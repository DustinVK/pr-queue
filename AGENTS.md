# Working in prqueue

## Read first

- [README.md](README.md): setup, supported commands, and the user workflow.
- [prqueue-spec.md](prqueue-spec.md): the implemented v1 behavior and approval/publication contract.
- [Phases 1–7 handoff](docs/phase-1-7-handoff.md): architecture, important decisions, regression history, and the mapping between implementation phases and stacked PRs.
- [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md): phase definitions and completed review/validation records.
- [Launchd implementation handoff](docs/launchd-implementation-handoff.md): starting code paths, phase deliverables, and review/acceptance requirements for scheduled polling.

The v1 implementation is complete through phase 8. [Launchd polling](docs/launchd-polling-spec.md) is the next planned extension; [interactive review](docs/interactive-review-plan.md) is also planned. Neither is implemented at the time this guide was written. Check the current code and branch before relying on that status. Follow the user's current scope and the relevant extension document; do not treat proposed commands as available or restart completed phases. [FUTURE.md](FUTURE.md) records other deferred features.

## Repository and architecture

This is a Go 1.26+ CLI for macOS and github.com. The module is `github.com/DustinVK/pr-queue`; the executable is `prq`. There is no application server. `modernc.org/sqlite` provides pure Go SQLite; a database server and Docker are unnecessary.

| Area | Responsibility |
| --- | --- |
| `cmd/prq` | CLI parsing, result envelopes, startup recovery, dependency wiring, acceptance fixtures. |
| `internal/config`, `internal/localfs`, `internal/lock` | Validated configuration, private files, and nonblocking flock ownership. |
| `internal/findings` | Strict agent-output contract, comparison keys, fingerprints, complete diff anchors. Keep it independent of other project packages. |
| `internal/github` | Explicit `gh api` transport, identity/pagination, submitted-review transport and reconciliation reads. |
| `internal/runner` | Disposable Git checkout, Claude prompt/process, diagnostics, timeout and orphan cleanup. |
| `internal/store` | SQLite schema, transactions, audit history, ingestion, immutable publication snapshots. |
| `internal/queue` | Observation, orchestration, triage, publication, and run completion services. |
| `internal/editor`, `internal/notify` | Local editor invocation and desktop notification sinks. |

Keep business behavior in shared queue/store services instead of duplicating it in command handlers or shelling out to `prq` from `prq`. Use existing injectable remotes, executors, reviewers, and filesystem paths for tests. Prefer small additions over a general framework or a new subsystem.

## Build and validation

```sh
go mod download
CGO_ENABLED=0 go build -o bin/prq ./cmd/prq
./bin/prq help --json
go vet ./...
go test ./...
go test -race ./...
git diff --check
```

Format changed Go files with `gofmt`; `gofmt -l cmd internal` should print nothing. Race checks require the macOS C toolchain even though the shipped binary builds without cgo. Runtime reviews need Git, authenticated `gh`, and authenticated Claude Code. `terminal-notifier` is optional; `osascript` is the fallback. Launchd work also uses the macOS-provided `launchctl` and `plutil`. If required tooling is missing, report it for the user to install. Fetching pinned Go modules is normal development work; do not upgrade dependencies incidentally.

Use focused tests while changing behavior, then the applicable phase checks. Include race checks for concurrent mutation, locks, process cancellation, or input ownership. Documentation-only changes need link/anchor, example syntax, and whitespace checks, not a fresh paid trial or new implementation-mirroring tests. Report what actually ran, what passed, and any unverified behavior.

Automated tests use temporary application paths/databases, local Git repositories, and fake GitHub/agent/editor executables. Keep ordinary tests offline with respect to GitHub/model services. The manual tests are opt-in: `PRQ_RUNNER_REAL_SMOKE` invokes real Claude and `PRQ_MANUAL_GITHUB_TRIAL` invokes real GitHub/Claude. Leave them unset during routine validation; details and evidence are in [phase 4](docs/phase4-smoke.md) and [phase 8](docs/phase8-validation.md).

## Data and side effects

Configuration is `~/.config/prqueue/config.yaml`; state is `~/.local/state/prqueue/`, including `queue.db`, locks, consent, `run-summary.json`, and per-run artifacts. There are no production XDG/config-path override flags. Use `config.ForHome` and the existing injected app paths for test isolation. Treat the user's actual queue, config, and diagnostics as user data; do not use them as test fixtures or commit them.

Repository configuration is a list of objects, for example `repos: [{name: owner/name}]`, not a list of strings. The defaults are a 15-minute agent timeout and one parallel review; do not replace an existing user's values with defaults. `init` preserves configuration and requires explicit trusted-agent consent. New application directories/files use `0700`/`0600`.

`prq` might not be on PATH: use the freshly built `./bin/prq` when appropriate. Do not run `init` or a recovery-capable command against the real home just to smoke-test a build. Current `status`, `list`, and other ordinary commands can perform startup orphan recovery; they are not necessarily filesystem/DB read-only. Inspect [cmd/prq/recovery.go](cmd/prq/recovery.go) when adding commands with stricter preflight or read-only requirements.

`run --dry-run` still invokes Claude and can incur cost. It computes queue changes in memory, retains no persistent review/observation changes, makes no coordinator GitHub writes, and sends no notification. `publish --dry-run` is a read-only publication preview. Never substitute a real agent call or live POST for a fixture test. The user designated `DustinVK/pr-queue` as a possible test repository; choose live trials deliberately within the current task's authorization. That designation does not select findings or a publication event, or turn automated tests into live tests.

Claude runs with full user permissions and `--dangerously-skip-permissions`. A worktree is not a sandbox; filtering token variables does not remove ambient authentication. Describe the approval guarantee as a guarantee of prqueue's publisher. The runner prompt's restrictions on source edits and `gh` apply to the spawned review agent; they do not prohibit authorized development work on this repository.

## Invariants to preserve

- Observe complete, unfiltered remote state before applying filters. Missing from a list is not proof of closure. Comparison identity is compact JSON `[head_sha, base_sha, draft, state]`; an observed change clears the cursor and approvals atomically with audits.
- Hold the global run lock for a review pass. Agents execute outside PR locks; ingestion reacquires the PR lock and rechecks live state. Observation contention skips; ingestion contention fails that attempt. If both scopes are needed, take the global lock first. Never hold multiple PR locks or unlink lock files.
- Keep GitHub calls, agent execution, and editor sessions outside SQLite transactions. The existing editor and publisher hold one PR lock during their operations; preserve that distinction from DB transaction duration.
- Reject a broken findings document as a whole; keep valid-shaped but unverifiable inline findings as `blocked`. Never silently truncate, repair, relocate, or auto-approve agent output. Failed attempts do not advance the successful-review cursor or replace findings from that output.
- Fingerprints normalize only CRLF in bodies and omit line numbers. A fingerprint match alone cannot preserve approval: require exact body, full anchor, and matching comparison. Read current decisions inside ingestion, and preserve published history.
- The summary requires independent approval. Public text contains approved bodies and anchors plus the recovery marker; titles, rationale, and verdict are private. Only a human-selected `publish --event` chooses the review event.
- Freeze the complete publication snapshot before sending. Commit `prepared`, then `sending`, then POST once. Never replay a possibly sent attempt. Reconcile uncertain delivery using recorded identity, marker, exact content, and complete pagination before current-PR eligibility checks.
- Preserve append-only audits, immutable run comparisons/publication snapshots, and the partial unique index for one blocking publication per PR. Finalization must preserve later edits and decisions. Use versioned atomic migrations if a task actually needs a schema change; do not edit an installed schema in place without migration handling.
- Preserve one JSON result on stdout and separate diagnostics. Current exit codes are `0` success, `1` invocation/fatal/unresolved-publication failure, `2` per-repo/PR run failures, and `3` lock contention. Planned interactive interruption behavior is not implemented yet.

## Delivery practice

Start with `git status` and inspect the current branch and relevant diff. Preserve pre-existing user changes. The original work uses dependent PRs, and its PR numbering differs from the implementation-plan numbering; consult the handoff and verify current GitHub state before choosing a base. Do not assume `main` contains the working CLI or flatten/rewrite the stack as housekeeping.

For phased work: implement a useful increment, validate it, review the complete diff against the spec, fix every issue found, rerun affected checks, and record the outcome before proceeding. Review checkpoints are engineering checks; continue within the user's authorized scope without adding repeated approval gates. When the task includes commits/PRs, keep commits coherent and incremental and explain each PR's dependency and validation. Update user-facing docs when behavior changes, and clearly separate completed behavior from proposals.
