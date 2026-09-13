# Launchd polling implementation handoff

Implement the reviewed [launchd polling specification](launchd-polling-spec.md) in three dependent phases. The spec defines behavior; this handoff identifies the code to reuse, implementation traps, and evidence needed to finish each phase. Read [AGENTS.md](../AGENTS.md) first. These are the launchd feature's phases 1–3, distinct from the completed original v1 phases.

## Starting point

At handoff creation on September 12, 2026, the checkout is `prq/08-cli` at `a1edd1e`. V1, including publication recovery and notifications, is implemented. There is no `schedule` command or `internal/launchd` package yet. The checked-in [plist](../com.dustinvk.prqueue.plist) is the old optional example: two-minute interval, `/usr/local/bin/prq`, and `/tmp` logs. Do not install it as the finished feature.

Start with `git status`, inspect the current CLI and relevant diffs, and verify the base branch before making changes. The original delivery stack may still be open; its mapping is in the [phases 1–7 handoff](phase-1-7-handoff.md#phase-numbering-and-branch-context). Base new work on the complete CLI, or on `main` after that stack has landed. Preserve the existing uncommitted documentation, including `docs/watch-plan.md`; writing this handoff did not commit, merge, or implement those proposals.

Scheduling has no dependency on interactive review or a watch dashboard. At this revision `prq review` is absent from CLI dispatch; original phase-8 completion referred to v1 and its code reviews. Recheck current code when resuming, but do not expand the scheduling task to implement either interface.

The required tools were available during planning: macOS 26.5.1, Go 1.26+, Git, `gh`, Claude Code, `/bin/launchctl`, and `/usr/bin/plutil`; the C toolchain is needed for race checks. `terminal-notifier` remains optional with `osascript` as fallback. Recheck availability before implementation and report missing required tooling for the user to install. Routine fixture tests need no live GitHub/model authentication. Historical tool checks are not proof that a future launchd job can authenticate.

## Decisions already made

| Area | Preserve this decision |
| --- | --- |
| Scheduled process | Exactly the installed absolute `prq` path plus `run`; all configured repos use their existing filters. No `--pr`, automatic approval/publication, wrapper loop, or internal daemon. |
| Timing | User-selected five-minute default, configurable whole seconds with a one-minute minimum. Preserve an installed interval when reinstalling without `--interval`. |
| Session/power | Current user's GUI LaunchAgent; runs on battery and AC, with no wake request. `RunAtLoad=false`; sleep/active-pass interval firings are skipped. |
| Commands | `schedule install [--interval 5m] [--dry-run]`, `schedule status`, `schedule uninstall`; existing JSON envelope. Stop/resume by uninstall/reinstall, without a separate pause state. |
| Installation | Stable absolute binary path and captured PATH, local supported authentication, existing agent-risk consent. No binary copier/updater, shell startup sourcing, token export, or schedule config/DB table. |
| Locking | Actual install/update takes `schedule.lock` then `run.lock`, including a first install. Uninstall takes `schedule.lock`, stops the scheduled owner, then acquires `run.lock` for recovery. Preview/status create no lock files. |
| Disabled override | Bootstrap failure follows rollback and retains raw diagnostics. Offer conditional human `print-disabled`/`enable` guidance; never parse native prose into a disabled-state boolean or enable automatically. |
| Diagnostics | Private append-only job logs plus existing run summaries/artifacts. No automatic rotation or artifact deletion in this version. Loaded is not equivalent to healthy. |
| Retries/cost | Use the next interval, without a separate backoff loop. Failed eligible comparisons can cause repeated paid calls; unchanged successful comparisons skip normally. |

Copy the exact plist keys, paths, and permission requirements from the [job definition](launchd-polling-spec.md#job-definition-and-environment). In particular, `Umask` is decimal `63`, `ExitTimeOut` is `60`, stdin is `/dev/null`, and the managed label is `com.dustinvk.prqueue` in `gui/<uid>`. A deliberate immediate run uses native `kickstart -p`, never `-k`.

## Code map and reuse boundaries

| Existing code | How it affects implementation |
| --- | --- |
| [cmd/prq/main.go](../cmd/prq/main.go), [flags.go](../cmd/prq/flags.go) | Add schedule dispatch/help and human/JSON rendering. Preserve one stdout result and existing error classification. Do not put schedule commands through generic startup recovery. |
| [cmd/prq/run.go](../cmd/prq/run.go) | Owns the global run lock, configuration/consent/tool/identity checks, coordinator wiring, and normal recovery. Read it for preflight requirements; do not execute `run` as an installation check. |
| [cmd/prq/recovery.go](../cmd/prq/recovery.go) | Existing wrapper acquires the run lock and silently skips recovery when busy. Uninstall needs explicit post-stop cleanup and a reported busy result, not this wrapper's skip semantics. |
| [internal/config/config.go](../internal/config/config.go) | Reuse parsing, validation, `Paths`, and `Consented`. Avoid creating consent or overwriting configuration. Uninstall/status must not require valid review config/auth. |
| [internal/github/client.go](../internal/github/client.go) | Reuse `Client.Identity` and the executor seam. Default execution inherits the calling environment; provide explicit-environment execution for installation preflight. |
| [internal/localfs/files.go](../internal/localfs/files.go) | Private state-file helpers are useful, but `WriteNew` and `Replace` call `PrivateDir`, which chmods the parent. Do not use them unchanged for the user's shared LaunchAgents directory. |
| [internal/lock/lock.go](../internal/lock/lock.go) | Reuse nonblocking flock and holder inspection; add the schedule-lock path here if useful. Inspection must not create locks; never unlink a lock inode. |
| [internal/runner/runner.go](../internal/runner/runner.go), [cleanup.go](../internal/runner/cleanup.go) | Existing cancellation, process groups, and owner recovery are the basis for active uninstall. Hard-kill coverage for Git helpers needs work, described below. |
| [internal/store/runs.go](../internal/store/runs.go) | Reuse `FailInterruptedRuns` after verified orphan cleanup while holding the run lock. No schema change is required for scheduling. |
| [internal/queue/completion.go](../internal/queue/completion.go) | `LoadRunSummary` provides a read-only summary source. Do not call `FinishRun` or the ordinary CLI status/recovery path from `schedule status`. |
| [cmd/prq/acceptance_test.go](../cmd/prq/acceptance_test.go), [run_test.go](../cmd/prq/run_test.go) | Reuse isolated app paths, fake executable boundaries, local Git sources, and injected notifications for integrated fixtures. |

Add `cmd/prq/schedule.go` and a small `internal/launchd` package. Separate rendering/preflight, native command execution, and lifecycle operations enough to test them independently; do not introduce a general service-manager framework. Suggested file names are `plist.go`, `preflight.go`, and `manager.go` with focused tests, but these are organizational suggestions, not new public interfaces.

## Phase 1 — Render and preflight

**Deliverable:** a working `prq schedule install --dry-run` with a validated proposed definition and no persistent changes. Suggested branch: `prq/launchd-01-preflight` above the complete CLI. Actual install/status/uninstall must fail clearly until phase 2 rather than claim success.

1. Parse schedule arguments before side effects. Resolve the current UID/home and installed binary path, and compute the spec's managed/log paths. Preserve a stable executable symlink instead of resolving it to a versioned package-manager target. Check the selected path is executable and print it so a checkout-local build is apparent.
2. Implement interval validation and reading of a recognized existing managed definition to preserve its interval. Render XML through an encoder, with proper escaping and argument arrays. Use `plutil -lint` on temporary output only; preview must not create application state, logs, lock files, or a LaunchAgent.
3. Capture PATH as specified: preserve absolute entry order, reject empty/relative entries, append missing standard system directories. Resolve Git/gh/the configured agent under that PATH, require absolute agent paths when they contain separators, and run bounded version checks with noninteractive input. Report optional notification-helper availability without turning it into a requirement.
4. Validate existing config/repositories/consent and the reachable GUI domain. Reuse the GitHub identity check under a conservative preflight environment containing the real user's home/login/temp values and proposed PATH. Exclude shell-only credentials/overrides. Version checks establish that Claude starts, not that a scheduled review authenticates or completes.
5. Return the effective path/interval/PATH/log paths and complete plist in human/JSON output. Mark actual loading and persistent-disabled status unverified. Never probe by bootstrap, call an agent review, or invoke ordinary startup recovery.

**Implementation trap:** changing `exec.Cmd.Env` does not change an executable path already resolved from the parent PATH. Resolve executables against the proposed PATH before starting them, then supply the explicit child environment. Do not mutate process-global environment variables to run preflight. The default GitHub executor is not sufficient merely because the plist contains a PATH.

**Required evidence:** duration boundaries; spaces/XML metacharacters and stable symlinks in paths; invalid PATH; missing tools and shebang interpreters; bad config/missing consent; inaccessible GUI domain; shell-token-only auth failing the supported local-login preflight; bounded cancellation; exactly one JSON result; and zero persistent files/database/recovery/agent/bootstrap effects. Compare filesystem/database snapshots where appropriate, including malformed-argument paths. Validate generated XML with the actual `plutil`.

Review the full diff against the spec, fix issues, rerun affected checks, and record the result before adding lifecycle mutations.

## Phase 2 — Lifecycle, status, and shutdown recovery

**Deliverable:** complete install/update/status/uninstall behavior with fixture-tested failure paths. Suggested branch: `prq/launchd-02-lifecycle`, based on phase 1. This phase includes the runner work required to make stopping a job reliable.

Implement actual installation under `schedule.lock` then `run.lock`. Refuse both a first install and an update with exit `3` if a review owns the run lock. This conservative first-install rule is intentional, not a launchd requirement. Recheck file/service ownership after locking; no takeover of the old manually managed example or an unfamiliar service. Write the plist atomically at mode `0600`, precreate private logs, and preserve existing LaunchAgents-directory permissions. Use an atomic-file operation whose parent-permission behavior is appropriate for that directory.

Load only the exact GUI service with native `bootstrap`; update an idle managed job with targeted `bootout` then bootstrap. Identical loaded definitions are no-ops. Never pass a bare GUI domain to bootout. Retain the old definition for rollback and report actual partial state if restoration fails. A failed first install removes its new plist so the next login does not unexpectedly load it.

Bootstrap failure is not proof of a disabled override. Preserve raw output/exit status, roll back, and supply manual `print-disabled gui/<uid>` inspection and conditional `enable gui/<uid>/com.dustinvk.prqueue` guidance. Neither `print` nor `print-disabled` nor bootstrap prose is a stable parsing API. Unknown/permission failures must not be collapsed into “absent” or “disabled”; verify the needed native command outcomes on the target OS and preserve uncertainty when they cannot be established.

For status, read the managed definition, log sizes/mtime, and available `run-summary.json`; retain launchctl diagnostics as opaque text even in JSON. A summary may come from a manual pass or be older than a fatal preflight failure. Missing/malformed summaries must not prevent job inspection. Report errors honestly without DB mutation, lock creation, recovery, or fabricated exact next-fire times. The command's successful inspection returns `0` even when the last scheduled run failed.

Implement uninstall in this order:

1. Acquire the management lock and verify managed ownership. Retain available verified run-owner identity before requesting stop. Do not acquire the global run lock yet.
2. Boot out the exact service and verify removal; keep its managed file until that succeeds, so an interrupted stop retains ownership evidence. Remove the file afterward to prevent loading at the next login. Bound stop/process-exit verification to the spec's 75-second deadline, allowing for launchd's 60-second termination grace. Never use an unbounded native wait or signal a process by name.
3. Verify process exit as well as service removal. A still-live scheduled owner is still stopping. Do not label it a concurrent manual run merely because the service vanished from launchctl.
4. Acquire the global run lock for owner-verified orphan cleanup and interrupted-run persistence. A concurrent manual owner is preserved; return `3` with “schedule removed, cleanup deferred” information. Other stop/cleanup failures return `1` with the actual remaining state. Preserve configuration, queue/history, and diagnostics. Repeating uninstall should complete remaining cleanup or succeed when nothing remains.

Extract a shared cleanup operation that assumes its caller holds the run lock if that simplifies reuse. Do not call `recoverInterrupted` while already holding that lock: it would try to acquire it again and can silently skip the required work. Keep cleanup usable with malformed review config or expired GitHub login, and bound its subprocesses/cancellation paths.

**Known runner gap:** Git and Claude run in separate process groups from their coordinator. Existing owner metadata records Claude, with a session-ID fallback around its startup, but does not record every active Git helper. Launchd killing the coordinator's group does not close that gap. Extend scoped helper ownership/recovery for forced coordinator death during Git setup as well as during Claude execution, including the crash window between spawning a helper and recording its PID. Reuse PID/start-time validation and support existing owner files; do not add broad process-name killing or a process-supervisor subsystem. A passed killed-Claude test is insufficient evidence for Git cleanup.

**Required evidence:** first-install/update lock refusal; simultaneous management calls; no-op/repeated operations; unknown/symlinked definitions; correctly scoped native arguments; permissions including an unchanged existing LaunchAgents directory; failed bootstrap/bootout/rollback; disabled guidance without automatic enable or guessed diagnoses; native diagnostic formatting changes that do not alter lifecycle decisions; read-only status; bad-config uninstall; bounded active stop; preserved manual runs and PID reuse; orphan recovery during both Git and Claude execution; and partial-result JSON/exit codes. Exercise faults after persistent changes, not just before the first write. Review cleanup, ownership, and rollback before phase 3.

## Phase 3 — Real launchd acceptance and user documentation

**Deliverable:** recorded macOS acceptance evidence and completed help/README/example updates. Suggested branch: `prq/launchd-03-acceptance`, based on phase 2. There are three distinct integration checks; passing one does not replace the others.

| Check | Evidence to collect |
| --- | --- |
| Native launchd with harmless fake `prq` | Unique disposable label, temporary paths, short test interval; actual firings, PATH/cwd/stdin, paths with spaces, codes 0–3, skipped overlap, private appended logs, idle update, and active stop/helper cleanup. |
| Native launchd with actual CLI/run code and fake external boundaries | Test harness injects temporary app paths, fake GitHub/agent executables, local Git source, and notifications. Prove run-lock exclusion and normal validation/ingestion through the real code. A fake `prq` executable alone cannot prove this. |
| Deliberately selected real scheduled review | Installed binary, normal local gh/Claude authentication, explicit kickstart, expected output/ingestion, and GUI notification delivery. Retain diagnostics and report any auth, permission, timeout, or notification failures. This call can incur model cost. |

Record manual sleep/wake and logout/login behavior with the disposable setup. Tests must always remove their exact test job and plist, including on failure; never load the production label in ordinary automated tests. Keep temporary-root injection in the test harness rather than adding production path-override flags or repurposing the user's actual home/config. Native launchd trials belong to explicit macOS integration checks, not every `go test` invocation.

For the real review, deliberately choose an authorized repository/configuration. `DustinVK/pr-queue` was offered for testing, but choose a suitable open comparison that will actually produce a new successful review; a skipped pass is not an agent smoke test. Do not put `--pr` in the managed job to force repeated reviews. Keep findings local; no approval or GitHub review POST is part of this check. Remove the trial schedule afterward unless continued polling was requested. The earlier Terminal-based real-Claude tests do not establish launchd authentication or notification behavior.

Finish native troubleshooting, installation at a stable binary path, migration of the old example, stop/reinstall/upgrade instructions, and log-maintenance instructions. Update the checked-in plist to the spec's interval/private-path pattern while keeping committed examples generic. Preserve the distinction between `schedule install --dry-run` (preflight/preview, no agent) and `run --dry-run` (agent execution with temporary state).

Review the complete feature for unintended scheduling inside Go, automatic triage/publication, wake scheduling, a sandbox subsystem, or a dependency on interactive review/watch. Record observed limits; do not mark a real acceptance check passed merely because its fake equivalent passed.

## Validation and continuation record

Use focused tests during each phase. Before advancing, run the applicable checks from [AGENTS.md](../AGENTS.md), review the complete diff, fix every issue found, and rerun affected checks. The final Go checks are:

```sh
gofmt -l cmd internal
go vet ./...
go test ./...
go test -race ./...
CGO_ENABLED=0 go build -o bin/prq ./cmd/prq
./bin/prq help --json
git diff --check
```

Formatting should report no files. Leave the existing paid-test opt-in variables unset during routine tests. Validate Markdown links/anchors and shell examples separately, including untracked files. Use `plutil` for actual rendered definitions; it validates syntax, not lifecycle correctness.

Keep coherent commits and three dependent reviewable PRs. At each boundary, update the table below with the branch/commit/PR, checks actually run, review findings/fixes, and any remaining limitation. Review checkpoints do not require repeated user confirmation when implementation is already authorized. Do not mark a phase complete while its required work remains.

| Phase | Status at handoff | Completion record |
| --- | --- | --- |
| 1 — Render/preflight | Not started | Record implementation, fixture validation, and review/fixes here. |
| 2 — Lifecycle/recovery | Not started | Include Git-helper recovery, rollback, cancellation, and status evidence. |
| 3 — Native acceptance/docs | Not started | Include all three integration checks, manual session checks, cleanup, and final review. |

If stopping mid-phase, append the exact branch/commit, changed files, passing/failing checks, unresolved issue, and next concrete action. Never let historical v1 validation substitute for the new feature's evidence.

This handoff was reviewed against the current spec and the referenced code. Documentation checks cover local links/anchors, shell-example syntax, fences, and whitespace. No schedule was installed, no agent was invoked, and no implementation phase was completed while writing it.
