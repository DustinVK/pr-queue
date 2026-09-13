# Agent 1: plan and implement Codex support in prq

Prepared 2026-09-12. This is the first of two sequential handoffs. Implement and validate Codex CLI support, then leave a working delivery for [Agent 2](dogfood-codex-resume-handoff.md), who will resume the paused PR reviews and merges using Codex.

The user requested both planning and implementation. Make and record the necessary design decisions, implement useful increments, review the resulting diff, fix findings, and complete validation. A plan alone does not finish this task. Creating this document did not resume the paused dogfood operation; executing this first handoff starts the implementation task only.

After these handoffs were written, the user separately requested checking and posting the last #4 findings. Their verified coverage changes and existing fix commits were posted in a [historical follow-up PR comment](https://github.com/DustinVK/pr-queue/pull/4#issuecomment-5648878190), with exact remote text verified. No prq/Claude invocation or merge resumed. The stale queue items remain unchanged, and #4 still needs a fresh review after your implementation. See Agent 2's follow-up record before interpreting the original pause entry.

## Read first and preserve the existing work

Read the original checkout's [AGENTS.md](../AGENTS.md), [README](../README.md), [v1 spec](../prqueue-spec.md), [implementation plan](../IMPLEMENTATION_PLAN.md), [phase handoff](phase-1-7-handoff.md), [original dogfood instructions](dogfood-prs-2-9-handoff.md), and [Agent 2's paused-state record](dogfood-codex-resume-handoff.md). Some are untracked user documents available only in the original checkout. Launchd, watch, and interactive review are separate proposals and remain outside this task.

Start with Git status and worktree inspection. The original checkout is `/Users/dvk/code/dev-tools/pr-queue`, on `prq/08-cli` at `a1edd1e3ff73317a30dd2d013fb83ab0c887ed78` when paused. Its modified README/FUTURE and untracked documentation are user work. Do not reset, stash, delete, or commit them incidentally. Implement in an isolated task checkout.

The existing private task workspace is `/Users/dvk/prqueue-dogfood-ytido9ij`:

| Location | Recorded state and use |
| --- | --- |
| `repo` | Isolated clone of `DustinVK/pr-queue`; owns the task worktrees. |
| `pr4` | Clean `dogfood/pr4`, tracking `origin/prq/03-runner`, head `55b17c129fd60de6942b057985ec739a4300ea6e`. |
| `tooling` | Complete CLI, branch `dogfood/tooling`, head `185c278415f37a415243db884cd58c9ab56599f8`. Includes prior runtime fixes but lacks the final two PR #4 test commits. |
| `bin/prq` | Stable executable built from that tooling revision. Currently supports Claude only. |
| `progress.md`, `evidence/` | Detailed private history, validation, review artifacts, and dispositions. Read the paused entry and subsequent updates. |
| `original-files.json` | Hashes of ten pre-existing user files; preserve those contents. These two new handoffs are separate additions. |

PRs #2 and #3 are merged. `main` was `a701895770a981c707744cb141af54196aaed58a` at pause and does **not** contain the complete CLI. PR #4 is open against main; #5–#9 remain dependent and unmerged. Verify live refs and PR state before choosing branches. Neither a fresh checkout of main nor the original full-CLI branch alone is a sufficient implementation baseline.

Use the complete tooling history plus PR #4's final test commits `578d0eb` and `55b17c1` as the development baseline. Inspect ancestry and integrate those tests normally in an isolated branch; do not duplicate commits already incorporated by intervening work.

## Scope and delivery strategy

Add Codex as a selectable local review agent while retaining working Claude support and existing configuration compatibility. Prefer the Codex CLI execution model already suited to the current subprocess runner. An OpenAI API client, app-server integration, generic plugin framework, multi-agent review system, and automatic provider fallback are unnecessary for this request.

Prepare a concrete landing plan before implementation. The recommended split is:

1. Configuration, shared runner, prompt/output handling, process ownership, and package tests belong with the still-open runner PR #4 where they can be independently built and reviewed.
2. Full command wiring, CLI trust/help text, acceptance tests, and CLI-dependent documentation belong with #9.
3. Keep a complete tooling branch containing both layers so Agent 2 can use Codex immediately to review #4. Keep coherent commits for each layer and record their dependencies and exact destination. Stage the #9-only commits for its turn if applying them to the current remote head would leave that branch broken.

Do not merge the entire tooling branch into #4: that would pull the later stack into an early PR. Do not flatten/rewrite the stack, merge #9 early, push main, or leave the feature only in an untracked binary. You may prepare commits and normal pushes needed for the implementation, but leave the sequential review/merge operation to Agent 2. If the split needs adjustment after code inspection, resolve it and record an equally concrete plan that preserves PR order and ensures all feature commits reach main.

Keep the actual user's queue and configuration unchanged during ordinary implementation tests. At pause, `~/.config/prqueue/config.yaml` selected executable `claude`, timeout **30m**, and maximum parallel reviews **1**, with GitHub user `DustinVK` and a repository object for `DustinVK/pr-queue`. Do not replace these with defaults. Agent 2 will intentionally switch the real configuration using your delivered instructions. Do not run real-home `init`, `status`, or other recovery-capable commands as build smoke tests.

## Establish the supported Codex invocation

Documentation and local help were inspected while writing this handoff. `/opt/homebrew/bin/codex` reported **codex-cli 0.154.0**. Only version/help inspection ran; authentication, a model request, and runtime compatibility were not verified. Recheck the installed version/help and current official documentation when implementing.

`codex exec` supports unattended execution, JSONL progress with `--json`, a separate final response file with `--output-last-message`, and schema-constrained final output with `--output-schema`. Saved CLI authentication can be reused. See the official [non-interactive guide](https://learn.chatgpt.com/docs/non-interactive-mode).

The CLI reference documents stdin prompts via `-`, optional model selection, sandbox settings, and `--ephemeral`. The installed help also exposes these options. Treat these as building blocks, not a tested prq invocation. Check flag placement and behavior against the installed version. See the official [CLI reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli).

Check the supported authentication mode without exposing credential contents. Preserve a usable existing login; do not assume an API key is required or initiate a login flow unnecessarily. Record the chosen auth mechanism and any actual missing prerequisite. See the official [authentication documentation](https://learn.chatgpt.com/docs/auth).

Do not copy Claude's `--session-id`, `--output-format stream-json`, or permission flags into a Codex command. No caller-selected Codex session UUID has been verified here. Do not substitute `codex exec review` merely because its name matches the task; first establish whether it can honor prq's exact comparison, prompt, and findings contract. Ordinary `exec` is the initial implementation candidate.

## Decisions to make and implement

### Configuration and command wiring

Inspect `internal/config/config.go`, `cmd/prq/run.go`, `cmd/prq/main.go`, and their tests. A small explicit provider field, such as `agent.provider`, is a reasonable design candidate; **it is not an existing setting**. Choose and document the actual interface.

- Omitting the new selector must preserve existing Claude behavior, including custom `agent.executable` wrapper paths. Choose executable defaults based on the selected provider only when no executable was explicitly supplied; an inherited default must not accidentally launch Claude with Codex arguments.
- Preserve existing timeout and parallelism values and strict YAML validation. Reject unsupported providers and incompatible settings before agent execution. A Codex-only installation must not require the Claude executable for a Codex run.
- Decide whether model/profile configuration belongs in prq or remains inherited from the selected CLI. The user has not selected a model; preserve explicit settings and avoid pinning a fashionable model as an incidental default. Record configured versus observed model identity honestly.
- Keep provider selection explicit. If Codex fails, report a failed attempt; never silently run Claude instead.
- Retain one JSON result on prq stdout with diagnostics elsewhere. Make help, dependency errors, consent/trust explanations, and runtime messages accurate for the selected provider.
- Inspect the existing `agent-consent-v1` contract. Handle existing consent deliberately and document the execution permissions; do not silently claim that a worktree is a sandbox or that prq controls everything a trusted subprocess can do.

### Shared runner and strict findings contract

Inspect `internal/runner/runner.go`, `prompt.go`, `cleanup.go`, `internal/queue/run.go`, `internal/findings`, and the complete CLI's fixtures. Keep checkout creation, comparison preparation, timeouts, locks, ingestion, and publication in their existing shared services. Isolate only real provider differences, such as argument construction and final-output delivery.

The current runner sends a prompt on stdin, supplies `PRQUEUE_INPUT`/`PRQUEUE_OUTPUT`, captures `agent.jsonl` and stderr privately, and expects a regular `findings.json` validated with `findings.DecodeFile`. Codex JSONL events are diagnostics; they are not this findings document.

Choose and test one authoritative output path. A schema-constrained final response written by the CLI is a useful candidate, provided its shape exactly satisfies the current findings contract. Inspect schema restrictions, optional fields, null handling, and unknown-field rejection before adopting it. A prompt-directed file is another option if it preserves strict validation. Do not repair malformed model JSON, strip arbitrary prose, truncate output, relocate anchors, or synthesize a clean review from a partial transcript. Keep `internal/findings` independent of provider packages.

The final document must identify the expected repository, PR, and head. Reject a broken document as a whole; retain valid-shaped but unverifiable inline findings as blocked. Nonzero process exit, timeout, missing output, or invalid output must fail the attempt even if some plausible JSON exists. Failed attempts must not replace findings or advance the successful cursor. Summary approval remains independent.

Prompt changes must preserve the exact head/base comparison and complete diff anchors. Preserve the quoted filename format added during dogfooding. Tell the review agent to inspect and report, avoid source edits and GitHub writes, and leave publication to prq. Account for Codex loading repository/user instructions and tools; restrictions in a prompt are not an enforcement boundary. Retain private Git refs/status diagnostics so Agent 2 can inspect what happened.

Preserve inherited Git-context filtering for both Git helpers and agents, nested `PRQUEUE_*` isolation, transport/model authentication needed by the chosen backend, and the existing removal of GitHub token variables from the agent environment. Do not print secrets in diagnostic metadata. Preserve private directory/file modes (`0700`/`0600`) for prompts, schemas, output, logs, and ownership files.

### Noninteractive permissions, cancellation, and orphan recovery

Select and verify a permission policy that completes unattended, can read the checkout/comparison and produce output, and supports necessary review checks. Explain its actual filesystem/network access. Prefer a supported bounded policy where practical; do not describe full-access execution as isolated merely because prq created a disposable worktree. Do not add unrelated hook-trust or policy-bypass flags by habit. Check whether inherited configuration changes behavior or causes approval prompts.

This is the most sensitive compatibility area. `cleanup.go` currently recognizes the interrupted-start window by looking for a live process-group leader containing Claude's `--session-id <run UUID>` in its argv. Saved-PID recovery separately requires a nonempty matching process start time. Codex cannot simply inherit that Claude-specific discovery rule.

Design process ownership for both backends, including a coordinator crash after child start but before PID metadata is saved. A session identifier emitted only after launch does not itself close that window. Establish a verifiable ownership mechanism supported by the actual process layout; do not append unsupported flags or signal a numeric process group based on stale PID data. Preserve compatibility with existing Claude owner files during recovery even when the configured provider changes. If ownership metadata changes, make legacy decoding and version handling explicit.

Preserve cancellation of owned process groups, post-success child cleanup, timeout classification, best-effort cleanup of independent entries, and retained diagnostics on failure. Current documented recovery deliberately leaves descendants alone when their leader is absent and ownership cannot be established; a one-line change setting `Starting=true` does not identify those descendants. Do not regress to unsafe signaling or promise stronger recovery than the evidence supports.

### Provenance and review eligibility

Record enough private information to prove which provider and executable handled each run, with configured model/options and observed identity where available. Do not infer Codex usage solely from a filename or a future config value. Avoid a database migration unless required; if one is needed, follow the repository's atomic versioned migration rules and preserve historical rows.

Keep remote comparison identity `[head_sha, base_sha, draft, state]`, exact-body/anchor approval matching, immutable run comparisons, audit history, and frozen publication snapshots intact. Decide and document how provider changes interact with automatic skip behavior. At minimum, Agent 2 must use the existing forced single-PR command so prior Claude success cannot stand in for a fresh Codex review. Do not automatically approve, publish, or alter historical provenance during a provider switch.

## Implementation and validation checkpoints

Work incrementally: record the plan and landing split, implement configuration/invocation, implement output and lifecycle handling, wire the full CLI, then review the complete diff against the spec. Fix all confirmed issues from your own review and rerun affected checks. Continue through implementation without introducing repeated approval gates.

Add meaningful offline tests using local Git repositories, injected application paths, and fake Claude/Codex executables. Cover:

- Legacy config/custom executable behavior, explicit Codex selection/default executable, unsupported provider rejection, and provider-specific argv without cross-contamination.
- Real runner-to-coordinator-to-store behavior with valid Codex output, distinct JSONL/stderr/final output, invalid/partial/wrong-comparison output, and nonzero exit despite an output file.
- Success, timeout, cancellation, surviving children, interrupted start, stale/reused PID identity, and legacy Claude orphan recovery while Codex is selected. Include race checks for this lifecycle work.
- Exact diff forwarding and valid/blocked anchor ingestion, returned/persisted output paths, changed counts, and the last two PR #4 regressions.
- Full CLI Codex execution through the actual runner with fake tools; ordinary failure/retry behavior and dry-run isolation of the real fixture database, observations, output paths, and notifications.

Keep `PRQ_RUNNER_REAL_SMOKE` and `PRQ_MANUAL_GITHUB_TRIAL` unset during routine tests. Any new live test must be opt-in and provider-specific so selecting Codex cannot accidentally invoke a paid Claude test. After offline checks, use a deliberately bounded local Codex compatibility smoke if needed to verify actual flags, authentication, final-output shape, and process behavior. Use synthetic local Git input and temporary app paths; it must not review the live stack or publish to GitHub. Record exactly what ran, including cost-bearing calls and any unverified behavior. Missing tooling/auth is a concrete external prerequisite, not a reason to fall back to Claude.

On the complete implementation checkout, run the applicable repository checks:

```sh
gofmt -l cmd internal
go vet ./...
go test ./...
go test -race ./...
CGO_ENABLED=0 go build -o bin/prq ./cmd/prq
./bin/prq help --json
git diff --check
```

Formatting must list no files. Early #4 code has no `cmd/prq`; validate that staged layer with package tests and `CGO_ENABLED=0 go build ./...`. Update the relevant README/spec/help and test documentation in your task branch, preserving pre-existing user edits. Keep proposals distinct from implemented behavior.

## Completion and delivery record for Agent 2

Finish with working Codex support, passing applicable checks, coherent retained commits, a complete rebuilt binary at an explicit absolute path, and a verified landing plan. Do not resume the PR review/merge cycle in this task. Do not invoke live Claude for a compatibility check.

Append a completed delivery record here and to the private progress log. This section is intentionally **not completed at handoff creation**. Agent 2 must read the actual delivery before starting:

| Required delivery detail | Completed 2026-09-12 |
| --- | --- |
| Final code checkout, branch, full SHA | `/Users/dvk/prqueue-codex-20260912/tooling`, `codex/tooling`, `35918c78e612838462ad27c6914b34e6989f1d54`. Core checkout: sibling `runner`, `codex/runner`, `90581b0b212c3190c3f8f601109fce8810230375`. Both clean. |
| Baseline integration | Verified complete tooling `185c278415f37a415243db884cd58c9ab56599f8` plus `578d0eb7b7b6efe18eb855c715d0510724311156` and `55b17c129fd60de6942b057985ec739a4300ea6e` are ancestors of the final tooling revision. #4 contains no later CLI stack. |
| Commit manifest and landing destinations | Core commits are on #4. The complete branch `codex/tooling` now has [PR #10](https://github.com/DustinVK/pr-queue/pull/10), stacked after #9. It carries CLI commits `ee4ee8d7794ce97cf22b62e38f0b9e6c8b7bdf8e`, `e76bd18f1f49edf3400037e69ffdf7e4f5a0953a`, and `35918c78e612838462ad27c6914b34e6989f1d54`. Use #10 instead of the earlier cherry-pick plan. |
| Complete binary | `/Users/dvk/prqueue-codex-20260912/bin/prq`, built with `CGO_ENABLED=0 go build -o /Users/dvk/prqueue-codex-20260912/bin/prq ./cmd/prq` from `35918c78e612838462ad27c6914b34e6989f1d54`. Embedded VCS revision matches and `vcs.modified=false`; help emits one JSON result. |
| Exact supported configuration | `agent.provider: codex`, `agent.executable: codex`; omitted provider defaults to Claude and omitted executable defaults to the selected provider. Explicit wrappers are preserved. No model/profile override. Actual configuration was intentionally switched for the authorized trial, preserving DustinVK, the repository object, timeout 30m, parallelism 1, and existing consent. |
| Installed Codex and authentication | `/opt/homebrew/bin/codex`, codex-cli 0.154.0, saved ChatGPT login reused successfully. Installed config selects gpt-6-astra/xhigh; those settings were inherited. No credential contents disclosed. |
| Invocation/output/permissions | `--ask-for-approval never exec --sandbox workspace-write`, network false, inherited extra writable roots cleared, per-run scratch grant, ephemeral JSONL, schema and final-response output flags, prompt on stdin. Only CLI-written final JSON is ingested. Private `agent-metadata.json` beside findings records preparation/release/process completion and observed session; unreported model/CLI identity remains unknown. Native command sandbox is not whole-process isolation. |
| Recovery compatibility | Shared gate uses an exec-safe CLOEXEC pipe with coordinator-exclusive writer, closes reader before exec, persists v2 PID/start/group identity before release. Crash/descriptor/concurrent-helper and save/release/identity failures tested; legacy starting and saved Claude owners recover while Codex is selected. Unknown/malformed owners retain evidence. No DB migration. Missing/reused leader and hard-death Git-helper limitations remain documented. |
| Validation | Core and complete CLI formatting, vet, tests, race, cgo-free builds and whitespace checks passed; docs links/anchors/fences/shell syntax passed. Real synthetic Codex smoke passed in 25.03s. Forced live #4 review `6ae36d37-8ee3-4649-b31b-24237d5197a8` succeeded in 207.66s, with persisted exact comparison/output/session verified and zero actionable findings. |
| Next action | Verify the current #4 comparison and fresh succeeded Codex run, then continue Agent 2’s separate review/merge contract. If another review is needed, keep `--pr 4`. Review and land #10 only after #4–#9 are integrated; retarget to main after #9 if needed and record the eventual merge SHA. Do not also cherry-pick its CLI commits into #9. |

Agent 2's success requires Codex support to survive the eventual integration of the entire stack into main. Resolve that delivery requirement before declaring this implementation complete.

## Completed Codex delivery (2026-09-12)

The user subsequently authorized implementing the full plan with `gpt-5.6-sol` subagents and testing PR #4. Three such agents wrote the configuration/lifecycle, provider/output, and CLI/test increments while the coordinating agent reviewed each checkpoint. Confirmed review findings were fixed before delivery, including YAML alias/merge defaults, relative wrapper resolution, bounded provenance reads, malformed owner validation, descriptor inheritance, and lifecycle fixture assertions.

The explicit PR #4 test authorization superseded deferring that first fresh invocation to Agent 2. This task ran one synthetic Codex smoke and one forced live PR #4 review; no real Claude call, approval, publication, or PR merge occurred. The summary from the successful Codex review remains pending independently. Codex’s sandbox denied `ps` during its own process integration checks; host integration and race suites passed. Source and refs stayed unchanged, managed artifacts remained private, and the worktree/owner were removed. Earlier run rows, publication records and consent were verified unchanged.

Read the [complete retained delivery and landing manifest](/Users/dvk/prqueue-codex-20260912/delivery.md), [machine-readable manifest](/Users/dvk/prqueue-codex-20260912/delivery.json), and [live run verification](/Users/dvk/prqueue-codex-20260912/evidence/pr4-run-1-verified.json). Exact raw output and provenance paths are in that verification record. The retained binary’s SHA-256 is `9a6fd880f640d748f00fb1d1427af906f48b7c4a7c97ddedbe53f09a445f4a6d`.

Original README/FUTURE and all other pre-existing user documents were preserved. Only this handoff and the implementation plan were intentionally updated with delivery status. The original checkout remains on its pre-existing branch with its user changes; the implemented code is in the isolated retained checkouts and named remote branches above.

### PR follow-up

At the user’s request, the retained CLI delivery was opened as [PR #10](https://github.com/DustinVK/pr-queue/pull/10), `codex/tooling` → `prq/08-cli`, at unchanged source SHA `35918c78e612838462ad27c6914b34e6989f1d54`. Its body records the #4/#9 dependencies, existing validation, and sandbox limitation. This PR supersedes the original instructions to cherry-pick the three CLI commits into #9. No source changes, new model runs, or merges were made for PR creation.
