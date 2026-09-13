# Dogfood prq and merge PRs #2–#9

Work in `/Users/dvk/code/dev-tools/pr-queue` on `DustinVK/pr-queue`. Use this document as the task prompt and carry the work through to completion.

## Objective and authorization

Use the actual `prq` CLI and Claude to review the eight existing PRs, verify the proposed findings yourself, post verified findings through `prq`, fix every confirmed issue, and repeat review/verification until each PR is ready. Merge them **strictly in order: #2, #3, #4, #5, #6, #7, #8, #9**. Finish the current PR before starting the next PR's review cycle. The final goal is all eight PRs merged into `main`, with the integrated application passing its checks.

I authorize real GitHub reads and paid Claude review calls, use of the local prq queue, independent verification and local approval/rejection/editing of findings, publication of verified findings as GitHub **COMMENT** reviews, code/test fixes, incremental commits and pushes, necessary PR-base updates, resolution of addressed review threads, and merge commits for these eight PRs. Proceed without asking for confirmation again for those actions. This delegates verification to you; do not claim I personally examined each finding. It does not authorize blind approval of agent output.

Do not stop after a plan, one review pass, posting comments, making fixes, opening another PR, or enabling auto-merge. Continue until the selected PRs are actually merged. If a real external blocker requires my intervention, complete the independent work you can, record the exact blocker and remaining work, and report it honestly. Do not bypass repository protections, disable checks, fabricate findings/results, or call a closed-but-unmerged PR complete.

## Context and scope

Read the original checkout's `AGENTS.md`, `prqueue-spec.md`, `IMPLEMENTATION_PLAN.md`, `docs/how-it-works.md`, and `docs/phase-1-7-handoff.md`. The original v1 implementation is complete. Launchd, watch, and interactive-review documents are separate proposals; implementing those features is outside this task. Use the existing noninteractive `list`, `show`, `diff`, `edit`, `approve`, `reject`, and `publish` commands.

At prompt creation, all eight PRs were open, non-draft, and mergeable. Merge commits were enabled, automatic head-branch deletion was disabled, and the authenticated repository permission was ADMIN. Recheck current state rather than treating this snapshot as a merge guarantee. PR #1 was a closed integration fixture and is outside scope; so are unrelated PRs beyond this list. A narrowly scoped corrective follow-up for a defect discovered after its owning PR has merged is part of finishing this task if it is necessary; explain and record it separately.

| Order | PR | Head branch | Original base | Scope |
| --- | --- | --- | --- | --- |
| 1 | https://github.com/DustinVK/pr-queue/pull/2 | `prq/01-foundation` | `main` | Configuration, SQLite, locks |
| 2 | https://github.com/DustinVK/pr-queue/pull/3 | `prq/02-observation` | `prq/01-foundation` | Findings validation and observation |
| 3 | https://github.com/DustinVK/pr-queue/pull/4 | `prq/03-runner` | `prq/02-observation` | Claude runner and ingestion |
| 4 | https://github.com/DustinVK/pr-queue/pull/5 | `prq/04-triage` | `prq/03-runner` | Approval, rejection, editing |
| 5 | https://github.com/DustinVK/pr-queue/pull/6 | `prq/05-preview` | `prq/04-triage` | Publication snapshots and preview |
| 6 | https://github.com/DustinVK/pr-queue/pull/7 | `prq/06-publication` | `prq/05-preview` | Submitted reviews and recovery |
| 7 | https://github.com/DustinVK/pr-queue/pull/8 | `prq/07-notifications` | `prq/06-publication` | Notifications and run summaries |
| 8 | https://github.com/DustinVK/pr-queue/pull/9 | `prq/08-cli` | `prq/07-notifications` | Complete CLI and acceptance tests |

This numbering is the delivery stack, not the original implementation-plan numbering. Judge findings against each PR's incremental scope: missing later-stage code is not automatically a defect. In particular, early branches intentionally have no `cmd/prq`; full command wiring is in #9.

## Preparation

1. Inspect `git status`, local worktrees, remote PR metadata, and existing reviews/comments. Preserve the original checkout's modified and untracked documentation. Do not stash, reset, delete, or commit those files as incidental cleanup. Work in isolated clones or worktrees with dedicated task branches; if a target branch is already checked out in the original workspace, do not force it into another worktree. Push task commits explicitly to the correct existing PR head branch.
2. Verify Go, Git, gh, Claude, and the race-test toolchain. Check local gh/Claude authentication and ensure `github.user` matches the active GitHub identity. If required tools/auth are missing, report what the user must install or authenticate. `terminal-notifier` is optional.
3. Build a **complete working prq binary from the full CLI code**, initially `origin/prq/08-cli` (recorded head `a1edd1e` when this prompt was written), in an isolated tooling checkout. Store the binary at a stable absolute path outside the PR worktrees, in a task directory. Set a task variable such as `prq_bin` to that path; do not assume `prq` is on PATH. Do not try to build the CLI from early PR branches or add later CLI code to them just to make this task work.
4. Keep the tooling checkout separate from the branch under review. As relevant fixes land, incorporate them into the complete tooling source and rebuild when needed, recording the source revision. If a prq defect blocks dogfooding, repair it with regression coverage and local review, retain the repair in version control, and land it in the appropriate existing PR. A necessary tool repair may be prepared ahead of its PR's turn, but review/merge the PRs in the required order and do not lose that repair when updating branches.
5. Use the normal persistent prq configuration and queue; this task explicitly exercises them. Preserve existing records, approvals, publication history, and unrelated repository configuration. If initialization is missing, this task authorizes `init --accept-agent-risk` for trusted local Claude execution; configure `github.user` and a repo object such as `repos: [{name: DustinVK/pr-queue}]` without replacing existing user settings. Do not invent config-path flags or repurpose HOME. If making a database backup, use a SQLite-aware backup that includes committed WAL state.
6. Create a private progress log outside the source checkout. Record PR number, current base/head SHAs, prq binary revision, run IDs/output paths, finding dispositions, posted review URLs, fix commits, validation, and merge commit. Update it after each meaningful step so another session can resume safely. Keep routine tests isolated with their paid-test opt-in variables unset; deliberate dogfood calls are separate from those tests.

## Repeat this cycle for the current PR

### A. Prepare the branch and comparison

Set `pr_number` to the current target, starting with `2`, and fetch the latest remote state. If this PR is already merged, verify that fact and its inclusion in `main`, record it, and advance. Investigate an unexpected closed-but-unmerged PR; do not silently skip it.

For #2, the target is already `main`. For each subsequent PR, first verify the preceding PR is **merged into main**. Then retarget the current PR to `main` and incorporate current `origin/main` into its head branch, resolving conflicts and preserving both predecessor fixes and the current PR's changes. Prefer an ordinary merge from main and a fast-forward push to avoid rewriting shared branch history. Review conflict resolutions and run affected checks before requesting an agent review.

For example, after checking the PR identity and predecessor state:

```sh
gh pr edit "$pr_number" --repo DustinVK/pr-queue --base main
```

Do not merge #3 into `prq/01-foundation` (or later PRs into obsolete parent branches) and assume the work thereby reached main. Keep head branches while the stack is being processed; do not request branch deletion or blanket-retarget all open PRs at once. Recheck the actual remote diff after retargeting/integration. A base or head change invalidates prior comparison-based review evidence.

### B. Run actual prq on this PR only

Use a forced single-PR run so an old successful cursor cannot turn verification into a skipped pass:

```sh
"$prq_bin" run --repo DustinVK/pr-queue --pr "$pr_number" --json
"$prq_bin" show "DustinVK/pr-queue#$pr_number" --json
```

Do not start with an unfiltered repo-wide run that reviews all eight PRs concurrently. The forced command may observe other PRs under the normal execution model, but it must invoke the agent only for the selected PR.

Require a **new succeeded review run** for the intended current head/base/draft/state comparison. A skip, lock-contention exit, timeout, malformed findings file, or partial-failure result is not a clean review. Retain diagnostics and diagnose the cause; use the documented retry/recovery paths. Never repair `findings.json` or edit SQLite directly to make a failed attempt look successful. `run --dry-run` is not appropriate here: its temporary queue cannot provide the persistent triage/publication workflow.

### C. Independently verify every proposed finding

Read the latest run's raw `findings.json`, the local queue, source/diff, and relevant tests. Check the claim against the exact PR comparison and its incremental scope; reproduce defects with focused tests or a concrete code-path explanation where possible. Inspect pre-existing GitHub review threads too, because prq does not feed them to Claude automatically.

Give every proposed issue a recorded disposition: confirmed defect to fix, incorrect/duplicate/already-addressed claim with evidence, or intentional behavior outside this PR's scope with a specific spec/dependency reference. Do not dismiss a valid issue merely because it is minor or inconvenient, and do not implement a false positive just to make the model quiet. Verify the independent summary as well.

Use prq's own triage commands. Enumerate IDs after verification; do not pipe every pending ID into approval. If a body needs correction, use `prq edit` (a temporary noninteractive editor helper is fine), inspect the resulting exact text, then approve separately. Use `edit --as-general` only for a verified issue that warrants a general comment when its inline anchor cannot be verified. Reject incorrect claims with a concrete private reason. Do not rewrite queue/audit rows directly.

```sh
"$prq_bin" diff "$finding_id"
```

Choose the disposition after verification; these are alternatives for different findings:

```sh
"$prq_bin" approve "$finding_id"
```

```sh
"$prq_bin" reject "$finding_id" --reason "$verified_reason"
```

**Do not equate “no pending findings” with “fixed.”** Published matches remain published across reruns, even when the agent reports the same still-unfixed defect again. Inspect the latest raw document and correlate it with published history on every pass. Track repeated claims and their fix/rejection evidence, not only local status counts.

### D. Post verified findings through prq before fixing them

For each pass with new verified feedback, approve only independently checked bodies/anchors, including any summary selected for publication. Inspect the full preview, including any previously approved items already in this PR's queue, before sending:

```sh
"$prq_bin" publish "DustinVK/pr-queue#$pr_number" --event COMMENT --dry-run
"$prq_bin" publish "DustinVK/pr-queue#$pr_number" --event COMMENT
```

I select **COMMENT** as the publication event for this task. These PRs are authored by the same account; GitHub/prq refuses self-APPROVE and self-REQUEST_CHANGES. Local `prq approve` means approving a finding's exact text, which is distinct from GitHub's APPROVE event. Do not bypass prq with `gh pr review` to post the findings, or submit GitHub approval events to satisfy merge requirements.

Record and verify the submitted review URL/content. Avoid posting duplicate copies of findings already published. If a published claim was subsequently disproved, record the correction and explain it on that PR; do not erase audit history. An empty COMMENT publication is a no-op, not evidence that a review was posted.

Handle an existing or newly uncertain publication **before** preparing another one:

```sh
"$prq_bin" publish "DustinVK/pr-queue#$pr_number" --resume
```

Never blindly resend or discard an unresolved snapshot. `--confirmed-not-sent` sends nothing and is permitted only after independently inspecting the remote review/comment evidence and establishing the documented clearance conditions; uncertainty or a failed read is not proof of non-delivery. If the evidence remains ambiguous, preserve it and report the concrete blocker rather than bypassing recovery. Do not merge with unresolved delivery.

### E. Fix, test, push, and run prq again

Fix every confirmed issue in the current PR, including relevant existing reviewer findings. Add meaningful regression coverage for behavior/failure-boundary fixes. Keep changes consistent with the v1 spec and this PR's scope. Make coherent incremental commits, review your own diff, and fix issues from that review before pushing to the existing head branch. Verify fixes before resolving corresponding GitHub threads; include fix references where useful.

Run formatting, `go vet ./...`, and `go test ./...` on the current PR's code; run race checks for lock/concurrency/cancellation changes and the applicable build/documentation checks. Early branches lack `cmd/prq`, so validate their packages with `CGO_ENABLED=0 go build ./...` rather than treating the missing later CLI as a failure. Where the CLI exists, also build `CGO_ENABLED=0 go build -o bin/prq ./cmd/prq`. Record what was applicable and what actually passed.

After any code change, conflict resolution, relevant tool repair, PR retarget, or base/head update, get a fresh successful prq review of the resulting comparison and return to verification. Continue until no confirmed actionable issue remains. Repeated disproven claims do not require endless identical runs; reject them with evidence. A model verdict or an empty queue alone is never the merge gate.

## Merge gate and sequential progression

Merge the current PR only when all of the following hold:

- Its target is `main`, and all earlier PRs in the sequence are confirmed merged there.
- A fresh successful prq run covers the final current comparison, with every reported issue independently verified and no confirmed issue left unfixed. Previously posted findings are fixed or explicitly corrected with evidence.
- Tests/builds appropriate to this PR pass, required GitHub checks/review requirements are satisfied, and relevant conversations are resolved after verification. Distinguish no configured CI from failing or pending CI. Never use `--admin` or change branch protection to bypass requirements.
- No prepared/sending/uncertain publication is unresolved. Review and fix commit links plus final verification evidence are recorded.
- Immediately before merging, live head/base SHAs still match the reviewed comparison. Any change sends you back through integration and review.

Use a merge commit and guard the reviewed head:

```sh
gh pr merge "$pr_number" --repo DustinVK/pr-queue \
  --merge --match-head-commit "$verified_head_sha"
```

Do not squash or rebase-merge this dependent stack. Do not use branch deletion. If GitHub requires a merge queue, satisfy its normal requirements and wait for actual merge; queued/auto-merge-enabled is not complete. If a required independent approval is unavailable, report that external blocker rather than self-approving or bypassing it.

Read back PR state and merge commit, fetch `origin/main`, and verify inclusion before advancing to the next PR. Maintain the same complete-binary/tooling discipline and re-integrate predecessor fixes when preparing the next branch.

## Completion and final report

After #9 merges, validate integrated `origin/main` in a clean task worktree:

```sh
gofmt -l cmd internal
go vet ./...
go test ./...
go test -race ./...
CGO_ENABLED=0 go build -o bin/prq ./cmd/prq
./bin/prq help --json
git diff --check
```

Formatting should list no files. Confirm each PR #2–#9 is `MERGED` and each final reviewed head is contained in main's history. Preserve the original checkout's pre-existing documentation and user queue/audit data. Do not declare completion if integrated checks fail; investigate and complete the necessary corrective work through a reviewed PR rather than pushing directly to main or weakening tests.

Report one row per PR with review-run count, verified findings/fixes, posted review link(s), final validation, and merge commit. Mention any prq bugs discovered while dogfooding and how they were fixed. State explicitly whether **all eight PRs are merged and integrated validation passed**. If interrupted, leave the progress log with the exact current PR/comparison, unresolved publication state, outstanding issue, and next action so the next agent can resume without duplicate posting or skipped verification.
