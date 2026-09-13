# Codex support implementation plan

Prepared 2026-09-12 from the [Codex support handoff](codex-support-handoff.md), repository guidance, the complete dogfood tooling source, and current GitHub/CLI inspection. **Status: implemented and validated on 2026-09-12.** The original plan below is retained as the design record. See the [completed delivery record](codex-support-handoff.md#completed-codex-delivery-2026-09-12) for exact commits, retained binary, checks, and the successful forced Codex review of PR #4.

Add one explicitly selected local review provider, `claude` or `codex`, through the existing runner/coordinator/store pipeline. Keep the findings contract, triage, and publication behavior. Launchd, watch, interactive review, automatic fallback, and the paused review/merge operation remain separate work.

## 1. Verified baseline and landing order

The original checkout is `prq/08-cli` at `a1edd1e3ff73317a30dd2d013fb83ab0c887ed78`, with modified README/FUTURE and untracked user documentation. Preserve all of it. This plan is a separate new file; implementation belongs in isolated task worktrees.

| Source | State checked during planning |
| --- | --- |
| Complete tooling | `/Users/dvk/prqueue-dogfood-ytido9ij/tooling`, clean `dogfood/tooling`, `185c278415f37a415243db884cd58c9ab56599f8`. |
| Runner layer | `/Users/dvk/prqueue-dogfood-ytido9ij/pr4`, clean `dogfood/pr4`, `55b17c129fd60de6942b057985ec739a4300ea6e`. |
| Missing from tooling | Exactly `578d0eb7b7b6efe18eb855c715d0510724311156` and `55b17c129fd60de6942b057985ec739a4300ea6e`, the diff/count/output-path and successful-child-cleanup tests. |
| GitHub | #2/#3 are merged; #4 is open against main at `a701895770a981c707744cb141af54196aaed58a`, with head `55b17c1`; #5–#9 remain open in the dependent stack. |

Remote state was read through `gh api repos/DustinVK/pr-queue/pulls`; recheck before branch creation or pushes. The relevant destinations are [runner PR #4](https://github.com/DustinVK/pr-queue/pull/4) and [CLI PR #9](https://github.com/DustinVK/pr-queue/pull/9).

Use two new task branches, with these proposed names:

1. Create `codex/tooling` from the complete tooling SHA and normally merge the current #4 head. This establishes the complete development baseline with all prior fixes and both final regressions.
2. Create `codex/runner` from that same current #4 head. Author shared configuration/runner changes here, with package tests available at #4. Merge its validated increments into `codex/tooling` for full CLI integration. This preserves the same core commit identities in both histories.
3. Author CLI-only commits on `codex/tooling`. Keep their manifest separate so they can be cherry-picked onto #9 after its turn incorporates main and all preceding PRs.

| Commit group | Contents | Landing destination |
| --- | --- | --- |
| R1 | Provider configuration, normalization, validation, compatibility tests | #4 |
| R2 | Shared gated process launch, versioned ownership, legacy recovery, lifecycle tests | #4 |
| R3 | Codex invocation, prompt/schema/final output, provenance, runner/coordinator tests, core documentation | #4 |
| C1 | CLI dependency messages, trust/help, actual-executable acceptance and dry-run tests | #9; depends on R1–R3 and the existing CLI stack |
| C2 | User workflow/configuration docs and complete CLI validation record | #9; depends on C1 |

R1 may expose provider parsing before R3, but the runner must explicitly reject Codex execution until its backend is ready; never send Claude arguments to a selected Codex executable. Do not publish an intermediate layer as usable Codex support.

After final validation, push the complete R1–R3 history normally to #4's existing head branch. Retain the full tooling branch and C1/C2 commits on an explicitly named task branch. Do not push CLI commits to #4 or merge the whole tooling history into it. Stage C1/C2 for #9's turn instead of applying them to a remote #9 head that lacks their dependencies. Agent 2 owns sequential merges. Its final manifest must track original and cherry-picked SHAs and verify all feature changes reach main.

## 2. Configuration and compatibility decisions

Add `agent.provider`, accepting exactly `claude` and `codex`; omission means Claude. Retain `agent.executable`, `agent.timeout`, and `agent.max_parallel_reviews`.

```yaml
github:
  user: your-login
agent:
  provider: codex
  executable: codex
  timeout: 30m
  max_parallel_reviews: 1
repos:
  - name: owner/name
```

This is proposed syntax until implemented. `executable` may be omitted to use the selected provider's default. Actual user migration must preserve their GitHub identity, repo objects, timeout **30m**, parallelism **1**, and other settings. In an existing config containing `executable: claude`, switching providers requires changing or removing that explicit executable too.

Implement in [config.go](../internal/config/config.go) and its tests:

- Choose the executable default after decoding the provider. Today `Parse` decodes over `Defaults()`, which has already set `claude`; retaining that order would misroute an omitted Codex executable.
- Track YAML key presence. Omitted values receive defaults; explicitly empty/null selectors or executables fail validation. Preserve `KnownFields(true)`, duplicate-key rejection, and exactly one YAML document.
- Preserve explicitly supplied executable paths, including wrappers and spaces; infer neither provider nor compatibility from a basename. The chosen executable must accept its provider's CLI contract. Do not rewrite custom paths or parse them as shell commands.
- Give direct runner callers the same omitted-provider Claude behavior and validate provider/executable/timeout before setup. Keep existing `config.Agent` fixture construction working.
- Keep timeout and concurrency defaults at 15m/1 for new configuration. Explicit settings survive parsing and `init` reruns.

Do not add model, profile, arbitrary argument, or permission fields to prq in this increment. Inherit the chosen CLI's existing model configuration without passing a model default. Provider-specific prq settings that do not exist remain unknown YAML fields. A future model/profile interface can be added separately when needed.

Keep `agent-consent-v1` and `trusted-local-agent-v1\n` valid. Its acknowledgment is for trusted local agent execution, and the existing full-user-permission risk is broader than the proposed Codex shell policy. Selecting a provider must not rewrite consent or require reinitialization. Update `init`'s notice to explain both providers and the publisher's actual approval guarantee; do not make `init` overwrite or newly depend on valid existing review configuration.

## 3. Provider-specific invocation and output

Keep [runner.go](../internal/runner/runner.go) responsible for checkout creation, comparison material, deadlines, environment filtering, diagnostics, and cleanup. Add a small internal invocation builder, with explicit Claude/Codex branches, plus a focused schema helper. No registry or provider framework is needed; `queue.Reviewer` remains unchanged.

The installed `/opt/homebrew/bin/codex` reported `codex-cli 0.154.0`. Its root/exec help includes the planned flags below; `codex login status` exited 0 and reported a saved ChatGPT login. Credential contents were not displayed. Help/version emitted a PATH-alias write warning under this planning sandbox but completed successfully. These checks establish installed options and saved authentication, not model access or runtime compatibility. Saved login reuse is documented in the [non-interactive guide](https://learn.chatgpt.com/docs/non-interactive-mode); authentication inspection is documented in the [authentication guide](https://learn.chatgpt.com/docs/auth).

Proposed Codex invocation, rendered as a shell example only; implementation passes an argument array and supplies the prompt on stdin:

```sh
codex --ask-for-approval never exec \
  --sandbox workspace-write \
  -c 'sandbox_workspace_write.network_access=false' \
  -c 'sandbox_workspace_write.writable_roots=[]' \
  --add-dir "$scratch_dir" \
  --ephemeral --color never --json \
  --output-schema "$schema_path" \
  --output-last-message "$output_path" -
```

Set cwd to prq's detached checkout. Resolve the selected executable before entering the launch shim, using the same environment as preflight. The root-level approval option precedes `exec`, as exposed by installed help. Final flag placement and inherited-setting precedence require the compatibility check below. The [CLI reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli) documents stdin prompts, schema/output files, configuration overrides, and ephemeral sessions.

Claude retains its existing arguments: `-p --verbose --output-format stream-json --no-session-persistence --session-id <run-id> --dangerously-skip-permissions`. Only the shared lifecycle changes. Do not supply Claude flags to Codex, select `exec review`, resume sessions, or introduce provider fallback.

### Authoritative findings file

For Codex, the CLI writes its final response directly to the existing `runs/<run-id>/findings.json` via `--output-last-message`. The prompt requests one JSON final response and explicitly tells the model to leave writing that file to the CLI. Claude keeps its prompt-directed file delivery. Share comparison/review instructions, changing only provider-specific output directions and the schema explanation.

Preserve `agent.jsonl` and `agent-stderr.log` as private diagnostics. Never extract findings from events, strip Markdown fences, fill missing values, or fall back to a second output source. `PRQUEUE_INPUT`/`PRQUEUE_OUTPUT` remain consistent with the selected run, including for Codex.

The schema should use one root object and nested `anyOf` variants for general, single-line inline, and multiline inline findings. Give each variant forms with and without `rationale`; every property present in a variant is required, and every object has `additionalProperties: false`. Optional anchors are absent, never null. This design accommodates the [Structured Outputs restrictions](https://developers.openai.com/api/docs/guides/structured-outputs) while preserving prq's rejection of null fields. Validate this exact union with the installed Codex backend before declaring support.

Pin `schema_version` and repo/PR/head identity in the schema using singleton enums. Keep the existing closed verdict/severity/category enums. The schema guides generation; [findings.DecodeFile](../internal/findings/findings.go) remains authoritative for identity, duplicate/unknown keys, Unicode, byte/count limits, and field shape. Diff validation remains separate, so valid-shaped but unverifiable anchors still become blocked. Do not change the findings version or introduce a provider dependency into `internal/findings`.

Precreate private files where appropriate and set the child umask to `077`. Keep diagnostics/scratch directories at `0700`, files at `0600`, and final output regular-file checks. Verify actual CLI file creation/replacement modes. Nonzero exit, cancellation, timeout, missing/partial output, or failed validation fails the attempt even if plausible JSON exists. Retain the raw bytes and diagnostics; failed output cannot advance the cursor or replace findings.

### Permissions and inherited behavior

Choose bounded `workspace-write` shell execution with approvals disabled and command network access explicitly disabled. Clear inherited extra writable roots, then grant only the per-run scratch directory in addition to the checkout and Codex's standard temporary roots. Read comparison/schema files from private diagnostics; the CLI itself writes the final file. Workspace writes support focused checks, with temporary caches placed in scratch when needed. Dependency installation or tests requiring additional access may be unavailable; record such limits in the review instead of silently escalating.

The permission asymmetry is intentional for this release: use Codex's native sandbox while preserving Claude's existing full-permission compatibility; tightening Claude would require a separate design and validation effort, and this plan makes no commitment to eventual parity or permanent divergence.

These are Codex shell settings, not whole-process isolation. Codex still needs model-network access and may load local instructions, rules, hooks, and configured tools. Preserve model/auth configuration, inspect effective settings during compatibility validation, and keep the agent trusted. A hook-trust or managed-policy failure must be reported; do not add bypass flags or discard user configuration to get past it. The [security documentation](https://learn.chatgpt.com/docs/agent-approvals-security) describes the distinction between sandbox and approval policies, writable temporary roots, and command networking.

Keep quoted `File: %q` comparison headers, exact patch bytes, head/base inputs, no-source-edit/no-GitHub-write instructions, private scratch guidance, and refs/status diagnostics. Continue filtering inherited Git context for Git helpers and agents, stripping nested `PRQUEUE_*` and GitHub-token variables, and preserving transport/model authentication. Never dump environment or credential/config contents into provenance.

## 4. Close the launch ownership window

Use a small fixed `/bin/sh` launch gate for **both providers**, implemented within `internal/runner`. It is a startup synchronization step and then `exec`s the selected agent; it is not a persistent supervisor or another `prq` invocation.

1. Reserve a version-2 owner file before disposable setup, retaining coordinator PID/start identity and run ID. Record that agent launch has not been released.
2. Create a dedicated pipe using an exec-safe mechanism that establishes close-on-exec (`FD_CLOEXEC`) before either descriptor can leak through a concurrent process launch. An unsynchronized pipe-then-fcntl sequence is insufficient. Pass only its read end to the launch shell through `exec.Cmd.ExtraFiles`; never pass or duplicate the write end into that shell, a Git helper, another agent, or any other subprocess. Keep the write end exclusively in the coordinator. The prompt keeps its separate stdin channel.
3. Start the fixed shell in a new process group. It blocks reading a complete release token from the control pipe. EOF or a malformed token exits without executing the agent. Its fixed script sets the private umask and passes the executable/arguments through quoted `"$@"`, with no interpolation of paths or model text into shell source.
4. Obtain a nonempty child process start time and verify it leads the intended group. Atomically save that PID/start time in the owner file before writing the release token.
5. Release the gate, close both control descriptors in their respective processes, and `exec` the agent. Exec preserves the recorded process identity and group. Save/identity/release failures close the gate and terminate/reap the owned child; they never proceed to review.

If the coordinator dies between child start and saving metadata, its exclusive pipe writer closes and the waiting shell exits without launching an agent. If it dies after release, recoverable PID/start metadata already exists. Test both descriptor boundaries explicitly: the read end is closed before agent exec, and the write end cannot survive in a concurrently spawned unrelated helper. For the latter, start a long-lived fake Git helper while the gate is pending, kill the coordinator before release, and require the gate to observe EOF and exit promptly without starting the agent while that helper is still alive. Include concurrent launches/race checks and identity-checked fixture cleanup. Do not rely on a session ID emitted after launch.

Version-2 cleanup uses saved PID, nonempty matching start time, and group-leader checks. Decode existing unversioned owner files as legacy and preserve their Claude `--session-id` interrupted-start discovery. Recovery chooses behavior from recorded metadata, independent of current config or installed provider. Unknown versions/malformed ownership retain evidence and report an error while other independent entries are still attempted.

Preserve cancellation, timeout classification, post-success child cleanup, and bounded waits. Keep cleanup failures visible and ownership evidence when cleanup cannot finish. Existing absent-leader/reused-PID cases must remain safe: never signal a stale numeric group whose ownership cannot be established. This gate closes startup ownership; it does not promise recovery of arbitrary detached descendants. Hard coordinator death during Git helpers remains the separately recorded launchd gap; retain current Git cancellation behavior without expanding this task into scheduling/helper supervision.

## 5. Provenance, eligibility, and CLI behavior

Create a versioned private `runs/<run-id>/agent-metadata.json` before agent release; failure to write required provenance fails the attempt. Record run/comparison identity, selected provider, configured and resolved executable, prq-supplied arguments/permission options, and that model selection is inherited. Record execution/release outcome so intended invocation is distinguishable from an agent that actually started. A coordinator crash may leave that outcome unknown; do not infer successful execution from prepared metadata.

After execution, add observed CLI/session/model identity only when reliable CLI output reports it. Retain the raw diagnostic source; unknown stays unknown. Configured and observed model fields must be separate. Do not invoke arbitrary wrappers with extra probe commands merely to fill optional metadata. Record the actual CLI version in compatibility/delivery evidence. File naming, the user's later config, and absent metadata are not proof of a historical provider.

No database migration is needed: the existing run ID and raw output path locate the metadata beside the findings file. Keep immutable run comparisons, audit history, approval matching, summary independence, and publication snapshots unchanged.

Provider/model changes do not alter comparison identity or automatically invalidate successful cursors or approvals. An ordinary run without `--pr` may still skip an unchanged comparison after a provider switch. **The existing `--pr N` option is itself the force mechanism; there is no separate `--force` flag.** The CLI passes that number as `forcePR` through the coordinator, and [Observer.observeOne](../internal/queue/observe.go) selects the requested PR as eligible before checking drafts, filters, or `last_reviewed_key`. Thus `--pr 4` bypasses the successful cursor even when the entire comparison is unchanged. It still respects PR locks and refuses closed/merged PRs.

A failed forced rerun preserves any earlier successful cursor, so each retry must retain `--pr 4`. Document Agent 2's required fresh command after delivery:

```sh
# --pr 4 forces a fresh attempt even if this comparison was already reviewed.
"$prq_bin" run --repo DustinVK/pr-queue --pr 4 --json
```

An exit code of 0 alone is insufficient validation: observation lock contention or an ineligible PR can leave no review attempt. Require this invocation's result to contain a new review-run ID for `DustinVK/pr-queue#4` with status `succeeded`; verify that same persisted run ID/comparison and its raw findings, plus private provenance demonstrating Codex execution. Record the ID and evidence paths. A skip, empty reviews list, prior successful run, or merely prepared agent metadata does not satisfy the acceptance check.

In [run.go](../cmd/prq/run.go), use the normalized provider configuration and require only Git, gh, and that provider's executable. A Codex installation does not need Claude. Keep fatal invocation/config/dependency errors at exit 1, per-PR failures at 2, lock contention at 3, and one JSON result on stdout. Agent output stays in diagnostic files. Generic recovery in [recovery.go](../cmd/prq/recovery.go) must work without loading current provider configuration.

## 6. Implementation checkpoints

At each checkpoint, review the complete diff against this plan and the handoff, fix confirmed issues, rerun affected checks, and record results before advancing. These are engineering checkpoints within implementation, not repeated user approval gates.

| Checkpoint | Work and exit evidence |
| --- | --- |
| A — Baseline/configuration (R1) | Establish both isolated branches and complete baseline; pass existing package regressions; add strict provider/default/wrapper compatibility tests and temporary rejection of unwired Codex runs. |
| B — Lifecycle (R2) | Implement gated launch with fake agents, versioned ownership, legacy recovery, and deterministic crash-window tests. Validate both provider layouts, process cleanup, and race behavior before attaching real Codex execution. |
| C — Output/invocation (R3) | Add explicit backend arguments, shared prompt variants, schema, final-file handling, private metadata, and actual runner-to-coordinator/store tests. Preserve both final #4 regressions. |
| D — Full CLI (C1) | Merge core into complete tooling; exercise provider selection through actual executable boundaries, error/retry behavior, old consent, recovery after a provider switch, dry-run state isolation, and notifications. |
| E — Compatibility/docs/delivery (R3 follow-ups, C2) | Verify installed Codex mechanically using isolated inputs; finish docs; validate both landing layers and complete tooling; retain commits/binary and fill Agent 1's delivery record. Any core fixes still originate in the #4 layer. |

Required offline coverage:

| Boundary | Cases that must pass |
| --- | --- |
| Config/argv | Legacy omitted provider; custom Claude wrapper; explicit Codex and default executable; preserved timeout/parallelism; unknown/empty/null fields; unsupported provider; spaces in executable/paths; no cross-provider flags or fallback. |
| Schema/file contract | Empty findings; general and LEFT/RIGHT single/multiline findings with/without rationale; exact identities; malformed/partial/prose/duplicate-key/unknown-field/null/oversize output; missing/nonregular file; nonzero exit with valid file; distinct final/JSONL/stderr channels. |
| Real shared pipeline | Actual fake-Codex runner feeds coordinator/store; unchanged bytes/comparison patches and quoted filenames; valid versus blocked anchors; returned/persisted paths and timed-out status; summary+finding changed counts and retirement-only zero count; failed attempts preserve findings/cursor. |
| Lifecycle | Success and failure with surviving children; timeout/cancellation; coordinator death before PID persistence, after persistence before release, and after release; nonempty start-time requirement; save/release failure; read end absent after agent exec; gate EOF after coordinator death while a concurrently spawned unrelated helper remains alive; legacy starting/saved Claude owners while Codex is selected; absent/reused leaders; independent corrupt entries. |
| CLI/state | Codex runs without Claude on PATH; missing selected tool fails; one JSON envelope and existing exit codes; retry of failed eligible runs; seed a successful fake-Claude review, switch to fake Codex without changing the comparison, verify an ordinary run skips and `--pr` actually invokes Codex once and returns/persists a new succeeded run with execution provenance; forced retries retain `--pr`; skipped attempts cannot satisfy acceptance; legacy consent preserved; dry-run snapshots of all five tables, observations, files, output paths, run summary, and notification calls. |

Use existing local Git sources, `config.ForHome`, injected remotes/notifications, and fake executables. Do not alter the user's actual config/queue. Ordinary tests leave `PRQ_RUNNER_REAL_SMOKE`, `PRQ_MANUAL_GITHUB_TRIAL`, and any new paid-test opt-in unset.

### Installed Codex compatibility check

Add a distinct opt-in `TestManualCodexSmoke`, gated by proposed `PRQ_CODEX_REAL_SMOKE=1`, with an absolute temporary diagnostics path via proposed `PRQ_CODEX_SMOKE_STATE`. It must not activate either existing Claude/GitHub test. Use the real shared runner and synthetic local repository/PR identity, no live GitHub comparison or publication, and an explicit short deadline (initial target: two minutes per invocation).

Verify actual stdin/argument handling, saved login, schema union acceptance, separate diagnostic/final channels, output validation/modes, launch identity, cleanup, and a harmless focused local check. Inspect effective approval/network/writable-root behavior, including inherited config, and any blocked tools. Use an offline sandbox probe when useful; do not spend additional model calls testing behavior already covered by fake processes. Record each cost-bearing call, configured/observed model, duration, output path, result, and limitations.

If schema or sandbox compatibility fails, diagnose and revise that small implementation decision before delivery; do not relax the findings decoder, automatically switch output modes/providers, or grant full access as a retry. This runtime evidence cannot be supplied by help output or a fake test. No model call was made while writing this plan.

### Final validation

On the complete implementation branch:

```sh
gofmt -l cmd internal
go vet ./...
go test ./...
go test -race ./...
CGO_ENABLED=0 go build -o bin/prq ./cmd/prq
./bin/prq help --json
git diff --check
```

Formatting must print no files. Validate the #4 layer separately with `gofmt -l internal`, vet/tests/race, `CGO_ENABLED=0 go build ./...`, and whitespace checks; that layer intentionally lacks `cmd/prq`.

Update README requirements/config/workflow, relevant spec sections (scope, runner, configuration, agent choices), help/trust text, and smoke-test documentation. Preserve prior user edits. Keep historical Claude evidence historical and scheduling/watch/interactive proposals distinct. If updating the untracked `how-it-works.md` or AGENTS guidance, carry those as explicit documentation work, never incidental staging. Check Markdown links/anchors, shell-example syntax, fences, and whitespace.

## 7. Delivery and continuation

Implementation is complete only after core and CLI layers pass their checks, the installed Codex compatibility check succeeds, and every feature commit has a retained landing destination. If an external prerequisite prevents compatibility validation, record the exact prerequisite and leave delivery incomplete; Agent 2 must not treat it as usable support.

Fill the [handoff delivery table](codex-support-handoff.md#completion-and-delivery-record-for-agent-2) and append the private progress log with exact branch/checkout/SHAs, baseline inclusion, R/C commit manifest, applied-versus-staged status, #9 conflict instructions, validation/review findings, Codex invocation/permissions/provenance, and supported configuration. Build from a clean complete source revision and record a stable absolute binary path, source SHA, and build command. Rebuild after any final code fix.

Agent 2 then verifies the delivered #4 code, intentionally updates provider/executable while preserving 30m/1 and other real settings, and performs the forced Codex review through the rebuilt prq using `--pr 4`. Require the new succeeded run and execution evidence described in section 5 before counting this as end-to-end validation. It resumes the existing review/merge contract and later applies C1/C2 at #9's turn. The eleven stale items and historical follow-up from the prior Claude pass are not fresh Codex evidence; use the [paused-state record](dogfood-codex-resume-handoff.md).

Planning validation: source/ancestry/worktree inspection, read-only GitHub PR metadata, official documentation, and local Codex help/version/auth-status checks were performed. Review follow-up checked the existing CLI-to-observer force path in both the original and complete tooling source, made close-on-exec ownership and both descriptor tests explicit, and recorded the deliberate permission asymmetry. All nine local links/anchors, four fenced blocks, three shell examples (syntax only), and whitespace checks passed. Application code, real prq configuration/queue, task branches, PRs, and binaries were not changed. Implementation and application tests remain pending.

Implementation completed with three `gpt-5.6-sol` coding subagents and coordinating review. Core is pushed to #4; the complete branch and three CLI-only commits are retained for #9’s later integration. Both landing layers passed applicable checks, the installed Codex smoke succeeded, and the user-authorized forced live review produced new succeeded run `6ae36d37-8ee3-4649-b31b-24237d5197a8` against head `90581b0b212c3190c3f8f601109fce8810230375`. It reported no actionable findings; its summary remains pending. See the linked delivery record for the native sandbox test limitation and full acceptance evidence.

PR delivery follow-up: the user requested a dedicated PR for the retained CLI layer. [PR #10](https://github.com/DustinVK/pr-queue/pull/10) now carries it, stacked after #9 with core in #4. This supersedes the original CLI cherry-pick landing procedure above; use #10 after preceding stack integration and do not duplicate its commits in #9. The source/binary revision and validation evidence are unchanged.
