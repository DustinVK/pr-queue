# prqueue

`prqueue` is a macOS CLI for drafting pull request reviews with Claude Code, triaging findings locally, and publishing the findings you explicitly approve. Its command is `prq`.

It runs once when invoked, reviews PRs in fresh Git worktrees, and keeps the review queue in SQLite. There is no server or daemon to deploy.

**Status:** all eight implementation phases are complete and reviewed. Offline acceptance checks and a controlled real-GitHub/Claude review trial passed; see the [validation record](docs/phase8-validation.md) for coverage and limitations.

- [V1 specification](prqueue-spec.md): the source of truth for behavior.
- [Implementation plan](IMPLEMENTATION_PLAN.md): phases, validation, and review checkpoints.
- [Deferred work](FUTURE.md): features outside v1.

## Requirements

| Tool | Purpose | Requirement |
| --- | --- | --- |
| macOS | Supported v1 host | Required |
| Go 1.26+ | Build and test `prq` | Required for development |
| Git | Fetch PR revisions and create detached worktrees | Required |
| GitHub CLI (`gh`) | Access github.com as the configured account | Required, authenticated |
| Claude Code (`claude`) | Generate proposed reviews | Required, authenticated through its supported local login or environment |
| `terminal-notifier` | Local desktop notifications | Optional; falls back to macOS `osascript` |

No Docker or database server is required. SQLite uses the pure Go `modernc.org/sqlite` driver, so building the binary does not require a C toolchain. Development race checks do require one. Go module versions and checksums are pinned in `go.mod` and `go.sum`.

Check the local tools and authentication without starting an agent review:

```sh
go version
git --version
gh --version
gh auth status
claude --version
claude auth status
command -v osascript
```

If authentication is missing, use `gh auth login --hostname github.com` or `claude auth login` as appropriate. `github.user` must match the authenticated GitHub account. `prq` does not manage model credentials.

## Build and initialize

From this checkout:

```sh
go mod download
CGO_ENABLED=0 go build -o bin/prq ./cmd/prq
./bin/prq init
./bin/prq status --json
```

`init` displays the agent-permissions notice and requires the exact answer `yes`. For explicit noninteractive acknowledgment, use `prq init --accept-agent-risk`. It preserves existing configuration and data on reruns. New configuration starts with `github.user: your-login` and an empty repository list; edit those before running reviews.

To install the executable in your Go binary directory, run `go install ./cmd/prq` and ensure that directory is on `PATH`.

## Review workflow

```sh
prq init
# Configure your GitHub login and repositories in:
# ~/.config/prqueue/config.yaml

prq run --repo owner/name
prq list --status pending
prq show owner/name#482
# Replace this with a finding UUID or an unambiguous 8-hex prefix from list:
finding_id=01234567
prq diff "$finding_id"
prq edit "$finding_id"
prq approve "$finding_id"
prq publish owner/name#482 --event COMMENT --dry-run
prq publish owner/name#482 --event COMMENT
```

`init` is safe to rerun and requires explicit acknowledgment of the agent's permissions before the first run. Configure repositories outside this checkout:

```yaml
github:
  user: your-login
agent:
  executable: claude
  timeout: 15m
  max_parallel_reviews: 1
repos:
  - name: owner/name
    filters:
      authors: []
      requested_reviewer: null
      base_branches: []
```

The configuration lives at `~/.config/prqueue/config.yaml`; the SQLite database is `~/.local/state/prqueue/queue.db`. The state directory also holds locks and the separate `agent-consent-v1` acknowledgment. Application directories use mode `0700`, and newly created configuration/state files use `0600`. Keep credentials and actual repository configuration out of this repository.

After each normal run, `run-summary.json` in the state directory records the latest pass for each selected repository, including failures that have no PR row. `prq status` reports these alongside PR runs, lock holders, and unresolved publications. New or changed findings and new failures produce one desktop notification containing counts and repository/PR identifiers. Identical repeated failures stay quiet; notification failures are reported without changing the run's exit code.

`run` observes PR state before applying filters and skips an unchanged, successfully reviewed comparison. `run --repo owner/name --pr 482` forces a fresh review, including a draft, but never a closed or merged PR. Failed reviews remain eligible for retry.

Each review keeps `findings.json`, the prompt, and separate agent diagnostics under `~/.local/state/prqueue/runs/<run-id>/`. Worktrees are removed after success or failure. The next normal invocation recovers worktrees and temporary dry-run diagnostics from dead coordinators, removes empty dry-run directories interrupted before ownership was recorded, and marks interrupted persistent runs failed; it skips recovery while another run holds the global lock. Successful dry runs retain no review artifacts or logical queue changes, but may update lock metadata and SQLite WAL coordination files.

Findings can be `pending`, `approved`, `rejected`, `published`, `obsolete`, or `blocked`. The agent's summary is a separate finding that needs its own approval. Invalid inline anchors remain visible as blocked findings; `edit --as-general` removes the anchor and requires approval of the resulting general finding.

`approve` and `reject` accept multiple finding IDs for one PR and commit the whole selection atomically. `reject --reason "..."` keeps the reason in the private audit history. `edit` uses `$EDITOR` (or `vi` when unset); quoted executable paths and arguments such as `code --wait` are supported without shell expansion. Editor failure or an unchanged save leaves the finding untouched. A changed body or conversion clears approval and keeps both bodies in the audit history; published findings cannot be edited in place.

All implemented commands support `--json`, with one result object on stdout and diagnostics elsewhere. Exit codes are `0` for success, `1` for a command/configuration failure or unresolved publication, `2` for a run with partial failures, and `3` for lock contention. See the [CLI reference](prqueue-spec.md#4-cli-reference) for the complete interface.

## Approval and publishing

The publisher sends only approved text and anchors verified against the current head, base, draft flag, and PR state. Editing a finding or observing a changed comparison clears its approval. Approval and publication require an open, non-draft PR and the configured GitHub identity.

Publication is one submitted GitHub review with an explicit human-selected event: `COMMENT`, `REQUEST_CHANGES`, or `APPROVE`. Titles, rationale, and the agent's verdict are private metadata. An approved summary and approved general findings form the review body; approved inline findings become individual comments. A recovery marker is appended to the body.

With no approved findings, `COMMENT` does nothing, `REQUEST_CHANGES` refuses, and `APPROVE` sends an explicit empty approval. Approving or requesting changes on your own PR is refused.

Publication snapshots are frozen before sending. If delivery is uncertain, a new publication is blocked until `prq publish owner/name#482 --resume` reconciles the original attempt. It never blindly resends a possibly submitted review. For an uncertain attempt, the `--confirmed-not-sent` recovery override requires manual inspection of the PR; the override sends nothing itself. See [publishing and crash recovery](prqueue-spec.md#7-publishing-and-crash-recovery).

`publish --dry-run` validates and previews the exact request without changing logical queue/publication data or remote state. It may update persistent lock metadata and SQLite WAL coordination files. Each preview allocates a candidate recovery marker; a later separate publication allocates its own marker. An existing unresolved publication blocks preview, including an otherwise empty `COMMENT`. `run --dry-run` **does run the agent and can incur model cost**; the coordinator uses temporary files, persists no logical database changes, makes no GitHub writes, and sends no notification.

Recover an interrupted publication separately from starting a new one:

```sh
prq status
prq publish owner/name#482 --resume
# Only after inspecting the PR and confirming no review was sent:
prq publish owner/name#482 --resume --confirmed-not-sent
```

Recovery verifies the recorded account before reading reviews. It can finalize a matching submitted review after the PR changes or closes. Conflicting content or incomplete reads remain unresolved, even with `--confirmed-not-sent`. An unsent prepared snapshot is revalidated before sending; stale prepared attempts fail without sending. `--resume` cannot be combined with `--event` or `--dry-run`.

## Agent permissions

Claude Code runs as trusted local code with your full user permissions, using `--dangerously-skip-permissions` for unattended execution. A fresh worktree protects the working checkout from ordinary review activity; it is not a sandbox. The agent is instructed not to commit, push, or invoke `gh`, but it can access ambient GitHub authentication. The approval guarantee applies to `prqueue`'s publisher, not to actions a misbehaving agent might take independently, including during a dry run.

Docker isolation, a web UI, scheduling, other agents, and remote notifications are deferred. The existing [LaunchAgent example](com.dustinvk.prqueue.plist) is optional future scheduling material and is not installed by `init`.

## Development

Implementation follows the [phase plan](IMPLEMENTATION_PLAN.md). Each phase ends with validation, a review of the changes against the specification, fixes for any issues found, and a recorded result before the next phase begins.

The packages separate the CLI, configuration, findings validation, GitHub transport, SQLite store, agent runner, queue operations, and notification sink. This keeps the approval and publication rules testable without invoking a real agent or posting reviews.

```sh
gofmt -w cmd internal
go vet ./...
go test ./...
go test -race ./...
CGO_ENABLED=0 go build -o bin/prq ./cmd/prq
```

Automated tests use temporary application directories, databases, local Git repositories, and fake GitHub/agent executables. They do not read your actual queue, invoke Claude Code, or publish real reviews. Separate manual tests require explicit environment opt-in and can incur model cost. The [phase 4 runner smoke check](docs/phase4-smoke.md) and [phase 8 integration trial](docs/phase8-validation.md) both passed. The latter detected an intentionally introduced arithmetic bug; its draft still needed human editing. Live review submission and recovery are covered by fault-injected fixtures, without a real GitHub review POST.
