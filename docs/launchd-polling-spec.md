# Automated polling with launchd

Proposed next extension to the [implemented v1](../prqueue-spec.md). Scheduling invokes the existing one-shot `prq run` and has no dependency on the [interactive review command](interactive-review-plan.md) or its delivery order. This document specifies behavior and implementation phases; the scheduling commands below are not implemented yet.

See the [implementation handoff](launchd-implementation-handoff.md) for the starting code paths, phase deliverables, known implementation traps, and continuation record.

## Scope and defaults

Install one per-user macOS LaunchAgent that periodically invokes the existing one-shot `prq run`. It collects PR changes, generates findings, and sends the existing local notifications. Human triage and publication remain separate commands. The runner's trusted-local-agent permissions and possible model costs apply equally to scheduled execution.

| Decision | First version |
| --- | --- |
| Interval | Five minutes by default; configurable during installation. |
| Session | The current user's GUI login session; no root or system daemon. Closing Terminal does not stop polling. Logout stops the job; a later GUI login loads it again. |
| Initial run | `RunAtLoad=false`: installation and login do not request an immediate review. An explicit kickstart is available for testing. |
| Sleep | Do not wake the Mac. Missed interval firings are discarded; there is no immediate catch-up pass promised on wake. |
| Battery | Use the same behavior on battery and AC power. No power-source gate in this version. |
| Overlap | Skip a firing while the launchd job is running. The existing global run lock also excludes simultaneous manual runs. |
| Selection | Plain `prq run` over all configured repositories, with their existing filters and unchanged-comparison checks. |
| Failure retry | Wait for the next normal interval. No immediate retry loop, exponential backoff, or automatic suspension. |

Five minutes and operation on battery are the user's selected defaults. The current [example plist](../com.dustinvk.prqueue.plist) uses two minutes and `/tmp` logs; implementation replaces that example to match this specification.

`StartInterval` is an opportunity to start a pass, not a completion deadline or an exact wall-clock schedule. A pass with several PRs can last longer than the interval; `agent.timeout` still bounds each review attempt and `agent.max_parallel_reviews` still controls concurrency within the pass. Sleep can also suspend an already running pass; this feature adds no wake assertion or special resume handling.

The installed `launchd.plist(5)` manual explicitly distinguishes missed `StartInterval` firings from calendar-trigger catch-up. Use `StartInterval` alone. Do not add `StartCalendarInterval`, `KeepAlive`, a shell loop, or a second scheduler inside Go. Apple recommends launchd for timed jobs; its [job creation guide](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html) includes this interval-based pattern.

## User workflow

Add three commands, all supporting the existing `--json` result envelope:

```sh
prq schedule install [--interval 5m] [--dry-run]
prq schedule status
prq schedule uninstall
```

`install` creates and loads the job, or updates the existing managed job. On the first installation, an omitted interval means `5m`; on an update, it preserves the installed interval. Accept Go-style durations that resolve to whole seconds, are at least one minute, and fit the launchd integer field. Reject unexpected positional arguments and unsupported flags, including repository/PR selectors. Repository selection belongs in the existing YAML configuration.

`install --dry-run` validates the proposed installation and prints its absolute binary path, interval, PATH, log paths, and complete plist. It may check GitHub identity, but does not invoke a review, load a job, create persistent files, recover runs, or modify the database. Actual installation prints the same effective settings and states that future intervals can invoke Claude without another prompt. Existing `init` consent is required; installing a schedule never creates that consent automatically.

Use a stable installed binary. For example, from the checkout after the commands are implemented:

```sh
mkdir -p "$HOME/.local/bin"
GOBIN="$HOME/.local/bin" go install ./cmd/prq
"$HOME/.local/bin/prq" schedule install --dry-run
"$HOME/.local/bin/prq" schedule install --interval 5m
"$HOME/.local/bin/prq" schedule status
```

The installed job records the absolute path of the executable running `schedule install`, preserving a stable installation symlink rather than pinning a versioned package-manager target. It does not copy the binary or install an updater. Running installation from `./bin/prq` would tie the schedule to that checkout's build; show the path clearly and recommend the stable installation above. The job does not depend on `prq` being on the interactive shell's PATH.

To deliberately request one immediate scheduled pass, use the native command:

```sh
launchctl kickstart -p "gui/$(id -u)/com.dustinvk.prqueue"
```

This can incur model cost. Do not use `-k`, which would kill an existing pass. This first version uses uninstall/reinstall to stop and resume polling; it adds no separate pause state. Uninstall preserves configuration, the database, findings, audit history, and diagnostic logs.

## Job definition and environment

The managed file is `~/Library/LaunchAgents/com.dustinvk.prqueue.plist`, loaded into `gui/<current-uid>`. Generate XML with a plist encoder and validate with `/usr/bin/plutil -lint`; do not interpolate shell commands into XML. Generated paths are absolute strings with XML escaping, including paths containing spaces. Launchd does not expand `~`, `$HOME`, or shell syntax in those strings.

| Plist key | Value |
| --- | --- |
| `Label` | `com.dustinvk.prqueue` |
| `ProgramArguments` | Two array elements: the absolute installed binary path and `run`. |
| `StartInterval` | Duration in seconds, default `300`. |
| `RunAtLoad` | `false` |
| `LimitLoadToSessionType` | `Aqua` |
| `WorkingDirectory` | The user's absolute home directory. |
| `EnvironmentVariables` | Explicit `PATH` as described below; no copied authentication tokens. |
| `StandardInPath` | `/dev/null` |
| `StandardOutPath` | Absolute `~/.local/state/prqueue/logs/launchd.stdout.log`. |
| `StandardErrorPath` | Absolute `~/.local/state/prqueue/logs/launchd.stderr.log`. |
| `Umask` | Integer `63`, equivalent to octal `077`. |
| `ExitTimeOut` | `60` seconds for graceful termination before launchd's forced kill. |

Leave `KeepAlive`, `AbandonProcessGroup`, and resource-limit overrides unset. The agent remains a foreground child of `prq`; `prq` exits when its pass finishes. There is no `poll_interval` field in `config.yaml` and no schedule table in SQLite.

Launchd does not source `.zshrc`, activate Conda, or reproduce a Terminal session's environment. Capture only the installing command's PATH, preserve its directory order, reject empty/relative entries, and append missing standard directories (`/usr/bin`, `/bin`, `/usr/sbin`, `/sbin`). This supports Homebrew, the Claude installation, and interpreter dependencies such as Node without guessing their locations. Show the captured value in preview/status. Do not copy the rest of the shell environment or use `launchctl setenv` to change other jobs' environments.

Installation preflight must:

1. Require macOS, a reachable current-user GUI domain, an existing valid configuration with at least one repository, and the existing agent-risk acknowledgment.
2. Verify the selected binary is an executable file. Resolve `git`, `gh`, and the configured agent using the proposed PATH; require an absolute path if `agent.executable` contains a path separator. Report resolved executable paths. Run bounded version checks so a missing shebang interpreter is caught as well as a missing file.
3. Verify `github.user` through the existing GitHub identity check with terminal input disabled. Build a conservative preflight environment from the real user's home/login/temp-directory values and the proposed PATH, excluding shell-only credentials and overrides. Do not validate using credentials inherited only from the installer shell. Bound preflight subprocesses so installation cannot hang on login or network access. This checks the supported local-login setup, not every ambient setting in the eventual launchd session.
4. Validate the rendered plist before changing an existing installation. Report an unavailable notification helper as optional, consistently with manual operation.

Use the user's supported local `gh` and Claude authentication. Shell-only `GH_TOKEN`, `ANTHROPIC_API_KEY`, custom credential-directory variables, proxies, and similar environment customizations are not automatically carried into the job. This first version does not add a secret store or environment-file feature; document local login as the supported setup, and report dependencies on unsupported shell-only settings instead of silently copying them. Ambient credentials in the user session remain possible, as with the existing runner.

GitHub preflight can verify its account. Claude version checks only prove that the CLI starts: they do not prove a scheduled review will authenticate and finish. A deliberate real launchd invocation is the final environment check. Authentication that needs an interactive unlock or expired login fails normally and must be repaired in the user's session. Repository/filter changes are read on each pass; binary location, captured PATH, and interval changes require reinstalling the job. Replacing the binary at the same stable path takes effect on its next launch.

## Installation, updates, and stopping

Use a small `internal/launchd` package for rendering, preflight, and calls to `/bin/launchctl`; keep command parsing in `cmd/prq/schedule.go`. Inject command execution and filesystem roots for tests. Scheduling commands bypass the CLI's general startup-recovery hook: status/preview must remain read-only, and uninstall must work even when review configuration or authentication is broken.

Serialize install/uninstall with a separate nonblocking `schedule.lock` in the private state directory. Follow the existing flock implementation and never unlink lock files. This lock protects job-management changes only; scheduling still relies on the existing `run.lock` for reviews. Do not hold a database transaction across a launchctl call.

For an actual install/update, acquire the management lock, then the global run lock before any persistent installation change. An active manual or scheduled pass returns `3` with no replacement or cancellation. Requiring the run lock even on a first installation is deliberate: creation and updates share one guarded path, avoiding installation-file changes during a manual review, although launchd itself does not require this exclusion. Recheck the installation after locking. Write the plist atomically with mode `0600`, identify it with a generated-file comment, and precreate the private log directory/files with modes `0700`/`0600`. Do not change permissions on the user's existing `~/Library/LaunchAgents` directory. Refuse symlinks or an unfamiliar existing definition at the managed path. If the label is already loaded without a matching managed file, report the conflict instead of taking it over.

Load with `launchctl bootstrap gui/<uid> <absolute-plist-path>`. Updating an idle managed job uses `bootout gui/<uid>/com.dustinvk.prqueue` before loading the replacement. Never boot out the GUI domain itself. Reinstalling an identical, already loaded definition is a successful no-op. Do not silently change a persistent `launchctl disable` override.

Bootstrap's exit status determines whether loading succeeded; its failure does not establish that the label is disabled. Preserve the exit status and raw diagnostics, follow the normal rollback path below, and include conditional troubleshooting: inspect `launchctl print-disabled gui/<uid>` by hand; **if the exact label is listed as disabled**, run `launchctl enable gui/<uid>/com.dustinvk.prqueue` and retry installation. The installer does not run that enable command or parse `print`, `print-disabled`, or bootstrap's prose into a disabled-state boolean. Generic failures such as an I/O error can have other causes and must not be reported as a confirmed disabled override. This keeps the failure path consistent with opaque launchctl diagnostics while naming the native call that exposes the override for human inspection.

`install --dry-run` never probes by bootstrapping a job. Report actual loading and persistent-disabled status as unverified in the preview; successful static/preflight checks do not guarantee bootstrap will succeed.

Filesystem replacement and launchctl operations are not one transaction. Retain the previous managed definition for rollback until loading succeeds. On failure, restore the previous file/load state where possible; a failed first install must remove its newly installed plist so a later login does not unexpectedly enable it. If rollback also fails, return `1` and print exactly which file/job remains and the native cleanup command. Never report a successful install solely because the file was written.

For uninstall, acquire only the management lock initially: waiting for the global run lock first would prevent stopping the very review holding it. Apply the same managed-file/service ownership checks as install; an unfamiliar definition must not be removed. Boot out only the exact service and verify its removal, then remove the managed plist to prevent loading at the next login. Keep the file until service removal is verified so a failed/interrupted bootout can be retried without losing the ownership evidence. Stop an active scheduled pass through launchd's SIGTERM path and wait with a bounded deadline (75 seconds total for stop/verification). Do not kill a manual `prq run`, send signals by process name, or use an unbounded `bootout --wait`. If stopping or deleting the file fails, return `1` and explicitly report that the remaining plist may load again at login. Repeating uninstall when both file and job are absent succeeds after any required orphan cleanup.

After stopping the job, acquire the global run lock and reuse orphan cleanup and interrupted-run persistence. The runner gives Git/Claude separate process groups, so launchd's cleanup of its own group alone does not guarantee those children disappeared after a hard coordinator kill. Verify the existing owner/PID-start/session checks before cleanup; preserve completed findings and diagnostic artifacts. If a concurrent manual run owns the lock, report that polling is removed but cleanup is deferred and return `3`. Other stop/cleanup failures return `1` with the actual remaining state. No successful uninstall may silently leave a known scheduled agent running. Cleanup must remain available without a valid review config or GitHub login.

Service removal alone is not proof that its process has exited. Retain available verified run-owner identity before bootout and use bounded owner/lock checks during termination; a still-live scheduled owner is still stopping, not an unrelated manual lock holder. The current owner metadata records Claude's process group but not each Git subprocess. Phase 2 must cover forced coordinator death during Git setup as well as Claude execution and extend the runner's scoped helper ownership/recovery where needed. Do not claim complete helper cleanup from the existing Claude-only regression.

Graceful logout/termination uses the existing signal cancellation and bounded runner cleanup. After an ungraceful coordinator death, the next normal recovery-capable invocation performs orphan recovery; this feature does not promise that a detached child is stopped immediately after SIGKILL or power loss. A recovery failure is visible and must not be mistaken for a healthy schedule.

To migrate a manually loaded copy of the old example, inspect and boot out that exact service and remove/move its old plist before using managed installation. Do not automatically overwrite an unrecognized file or sweep other LaunchAgents. For binary upgrades, uninstall, replace the stable binary, and reinstall; this provides an explicit way to avoid replacing a binary during its active pass.

## Results, diagnostics, and maintenance

Scheduled runs retain the existing exit meanings:

| Code | Meaning under polling |
| --- | --- |
| `0` | The pass completed without failures, including no eligible changes. |
| `1` | Fatal invocation/configuration/authentication/storage failure, or cancellation under current `run` semantics. |
| `2` | Repository/PR failures; other work may have completed. This includes invalid agent output. |
| `3` | Global run lock busy; this invocation did no review work. |

All return control to launchd. None activates a keep-alive retry. A failed new comparison remains eligible at the next interval; repeated malformed `findings.json` can therefore cause repeated paid agent calls. Unchanged comparisons that were already successfully reviewed still skip under existing cursor rules. The contract-failure behavior is unchanged: preserve previous findings, retain diagnostics, and do not advance the successful-review cursor. See [how it works](how-it-works.md#5-what-happens-when-output-is-invalid).

`schedule status` reports the installed definition's binary, interval, PATH, and log paths; file sizes/mtime; and native launchctl service diagnostics, including the command's exit status. Keep `launchctl print` output as opaque diagnostic text, including in JSON: Apple's manual explicitly says its format is not an API. Do not build lifecycle decisions by scraping its PID/state fields. Handle an absent job separately from an inaccessible GUI domain or a launchctl failure, and retain raw errors when the state cannot be determined. Never invent an exact next-fire timestamp.

Also display available timestamps/counts from the existing `run-summary.json`, without opening the database for mutation or triggering recovery. Label it as the latest recorded pass, which may have been manual. A loaded LaunchAgent means scheduling is configured, not that the latest poll succeeded. Fatal preflight errors can occur before `run-summary.json` and normal failure notifications are updated; the service diagnostics and stderr log are necessary to find those failures. A missing/malformed summary is reported as unavailable without preventing inspection of the job.

Keep the normal human `run` output in stdout and diagnostics in stderr; do not put full JSON finding bodies in the job log. Existing per-run prompt, findings, and agent stdout/stderr remain under `runs/<run-id>/`. Notifications keep their current new/changed-finding and distinct-failure rules; repeated identical failures stay quiet. Installation must not promise desktop delivery until macOS permissions and a real GUI-session test establish it.

Both job logs append and are private. **Automatic log rotation and run-artifact retention are deferred in this version**, an explicit storage tradeoff at personal-repository scale. Status exposes log sizes and points to the run-artifact directory. To clear job logs, uninstall first, truncate the two named log files, and reinstall; this preserves file permissions and avoids active-writer/renamed-file confusion. Do not automatically delete run artifacts, the database, or audit history. Stop polling and inspect disk use if repeated failures accumulate large agent diagnostics.

Scheduling-command exits are `0` for completed operations (including a successful read of a schedule whose last run failed), `1` for invalid invocation/preflight/launchctl/storage failure, and `3` for management/run-lock contention. Keep the existing JSON envelope; include partial management results when a failed uninstall has already removed the schedule. Preview/status must create no lock files or persistent state.

## Incremental implementation and review gates

Deliver three dependent PRs above the current CLI work, or above `main` once that stack lands. Each phase ends with a code review against this specification, fixes for every issue found, and rerunning the affected checks before continuing. No phase installs a production schedule as a side effect of tests.

1. **Render and preflight.** Add the launchd package, interval/path handling, proposed environment, plist encoding, and `schedule install --dry-run`. Test XML/path escaping, paths with spaces, interval boundaries, missing tools/interpreters, shell-only authentication, malformed configuration, missing consent, inaccessible GUI domain, and preview's lack of persistent mutations or agent calls. Test against the exact proposed subprocess environment. Validate generated plists with the installed `plutil`. Review the unattended invocation and permission boundary before adding installation.
2. **Lifecycle and diagnostics.** Add install/update/status/uninstall, private files, management locking, bounded launchctl calls, rollback, and targeted post-stop recovery. Use fake launchctl commands and temporary paths. Cover active-run refusal for both a first install and an update, simultaneous managers, existing unmanaged files/services, absent jobs, bootstrap/bootout failures, rollback failures, corrupt review config during uninstall, read-only status with missing/stale summary, and cleanup after a killed coordinator. For disabled-override failures, verify raw bootstrap diagnostics, normal rollback, conditional manual inspection/enable guidance, and no automatic enable; a generic bootstrap error must not become a confirmed disabled diagnosis. Verify preview performs no bootstrap, and changing native diagnostic formatting does not change lifecycle decisions. Verify no unrelated process/job/data is touched. Review partial-operation reporting and every cancellation/cleanup path.
3. **Real launchd acceptance and documentation.** On macOS in a GUI session, load a uniquely labeled disposable job that uses a harmless fake `prq`, a temporary state/log directory, and a short test interval. Record actual firings, environment, paths containing spaces, noninteractive stdin, all run exit codes, skipped overlap, log permissions/appending, idle reload, and active uninstall including helper-process cleanup. Remove that exact test job and plist even when the trial fails. Also exercise real launchd through a test harness running the actual CLI/run code with injected temporary paths and fake GitHub/agent fixtures to verify the global lock and ingestion path; the fake executable alone cannot prove these integration properties. Keep this injection in the test harness rather than adding production path-override flags. Record a manual sleep/wake and logout/login check for the documented session behavior. Finish README/help/native troubleshooting instructions, update the checked-in example, and review the complete diff.

At phase 3, also run one deliberately selected real scheduled review with the installed binary and normal local authentication to verify the launchd environment can reach GitHub and Claude and deliver notifications. Choose the repository configuration and explicitly kickstart it; retain the result and uninstall the test schedule afterward unless continued polling was requested. This is a manual opt-in trial that can incur model cost. It does not approve findings or post a GitHub review. The previously completed Terminal-based Claude smoke test does not substitute for this launchd test.

Run formatting, `go vet ./...`, relevant automated tests, race checks for management/cancellation paths, and a cgo-free build. Ordinary tests use temporary roots and fake tools, and must never load the production label or read the user's real queue. Final review verifies that no internal daemon, automatic approval/publication, wake scheduler, sandbox feature, or dependency on interactive review entered the change.

## Specification review and sources

This planning pass checked the current run lock, startup preflight, cancellation, orphan cleanup, summary persistence, and notification code. Review caught an invalid cross-reference and tightened uninstall ownership checks, bounded process-exit verification, and the Git-helper recovery requirement. The design explicitly covers dropped firings, repeated-failure costs, shell/launchd environment differences, active uninstall's lock ordering, separate child process groups, unstable launchctl diagnostic output, and log retention. Implementation and its phase reviews remain pending.

Review follow-up removed delivery-status wording from the interactive-review independence requirement, specified bootstrap failure plus human `print-disabled` inspection instead of programmatic disabled-state detection, and documented why a first install also requires the run lock. The phase 2 checks cover these decisions. The phase-8 completion record describes the original v1 implementation and its code reviews; at the checked-out `a1edd1e` revision, `prq review` is still absent from CLI dispatch. Scheduling's independence holds regardless of when that separate command lands.

The required launchd tooling is already available on the development Mac: `/bin/launchctl` and `/usr/bin/plutil`, alongside the existing Go/Git/gh/Claude tools. No additional scheduler package is needed.

Documentation validation passed: local links/anchors, Markdown fences/whitespace, and an XML round-trip plus `plutil -lint` for the specified plist values, including a path with spaces and an ampersand. This validates the proposed file shape only; no LaunchAgent was installed or loaded during planning.

Platform behavior was checked against the installed macOS 26.5.1 manuals on September 12, 2026: `man 5 launchd.plist` (especially `StartInterval`, `StartCalendarInterval`, `ExitTimeOut`, and `AbandonProcessGroup`) and `man 1 launchctl` (`bootstrap`, `bootout`, `kickstart`, `print`, and persistent `enable`/`disable`). These local manuals are authoritative for the target OS. Apple's archived [Creating Launch Daemons and Agents](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html) and [Scheduling Timed Jobs](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/ScheduledJobs.html) provide background; calendar-trigger wake behavior must not be generalized to `StartInterval`.
