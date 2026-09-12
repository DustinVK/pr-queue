# Phase 8 validation

Completed on 2026-09-12 on macOS arm64, using the tool versions recorded in the [implementation plan](../IMPLEMENTATION_PLAN.md#tooling-preflight).

## Automated acceptance coverage

The [CLI acceptance tests](../cmd/prq/acceptance_test.go) drive the actual command dispatcher, SQLite store, Git subprocesses, and GitHub/agent executable boundaries. GitHub and agent executables are fixture processes; Git sources and application state are temporary. Ordinary tests make no paid model calls or real GitHub writes.

| Spec scenario | Coverage |
| --- | --- |
| Approve A, reject B, publish | Only A's exact approved body and anchor reach the request; unapproved summary and B remain private. |
| Comparison and observed state transitions | Head/base changes, ready/draft and open/closed transitions invalidate approval; observation precedes author filtering and exclusions never imply closure. |
| Edit while publication is unresolved | Later edited text remains pending after reconciliation; the sent body comes from the frozen snapshot and is audited. |
| Agent crash or changed comparison | CLI crash/retry plus [coordinator tests](../internal/queue/run_test.go) verify no stale ingestion or cursor advancement. |
| Stop in prepared/sending or lose an accepted response | Prepared validation, no replay from sending, incomplete-read refusal, reconciliation after closure, and repeated finalization. [Publisher tests](../internal/queue/publish_test.go) inject failures at each commit boundary. |
| Zero approved findings | COMMENT no-op, REQUEST_CHANGES refusal, and explicit empty APPROVE. |
| Run and approve overlap | CLI test approves during a waiting agent and ingestion preserves that decision. [Triage tests](../internal/queue/triage_test.go) verify other-PR usability. |
| Existing blocking publication | Fresh publication refuses, missing marker remains blocked, and explicit clearance sends nothing. [Store tests](../internal/store/publications_test.go) cover index enforcement and immutable snapshots. |

Notification tests verify fallback, literal argument handling, quiet repeated failures, changed failures, and retention across lock skips. CLI checks cover repository failures without PR rows, nonfatal notification errors, and dry runs that preserve the summary file byte for byte. Existing dry-run tests compare all five database tables.

Final checks passed:

```sh
gofmt -l cmd internal   # no output
go vet ./...
go test -race ./...
CGO_ENABLED=0 go build -o bin/prq ./cmd/prq
./bin/prq help --json
git diff --check
```

Local documentation links/anchors and changed-document whitespace were checked separately, including untracked files. Scope review found no scheduler, staged reviews, automatic publication, sandbox subsystem, or multi-agent feature.

## Review findings and fixes

- Interrupted worktree setup could leave an unregistered checkout that `git worktree remove` refused. Cleanup now removes the exclusively owned disposable root even when Git metadata is incomplete. A regression reproduced the failure before the fix.
- Lexical timestamp sorting placed `.11Z` before `.1Z`, violating general-finding creation order in the public body. Rendering now compares parsed instants with UUID tie-breaking; new stored timestamps use fixed fractional precision. A regression reproduced the incorrect request order before the fix.
- Real-agent transcript inspection found a scratch experiment written to a fixed global temporary path. The prompt now directs scratch files into a subdirectory beside the run's output file. This remains an instruction within the documented full-user trust boundary, and the revised instruction has not had another paid trial.
- The manual GitHub test now requires a newly succeeded review, so a skipped or closed PR cannot falsely count as a successful agent trial.

The affected tests and final checks passed after the fixes. No implementation review issue remains open.

## Controlled real-GitHub/Claude trial

The user authorized `DustinVK/pr-queue` as the testing repository. [Fixture PR #1](https://github.com/DustinVK/pr-queue/pull/1) used two separate test branches and a small standalone Go module. Its one-line change replaced `cents - cents*percent/100` with `cents * (1 - percent/100)`. Existing tests exercised only 0% and 100%, leaving the introduced intermediate-percentage bug undetected.

| Item | Result |
| --- | --- |
| Base | `f4906cc12d92c5937b4b0f033665c47dab8c82bb` |
| Reviewed head | `b21c2884298d382f1414b9107dd8fee0587f43bc` |
| Test | `TestManualGitHubReviewTrial`, explicit opt-in; actual GitHub reads, runner, Claude, validator, and ingestion |
| Duration | 70.48 seconds end to end; agent 65.297 seconds, within the 15-minute timeout |
| Run | `6e3fe24e-30e0-480b-a5ac-af30c2067118` |
| Queue | One succeeded run; summary, inline finding, and general finding all pending; matching cursor; zero publications |
| Cleanup | Detached worktree and ownership metadata removed; saved Git status clean and before/after refs identical |
| GitHub state | PR closed unmerged after inspection; no submitted reviews |

Claude identified the intended defect: `percent/100` truncates to zero for integer percentages 1–99, so 1,000 cents at 25% returns 1,000 instead of 750. The inline finding targeted the changed line, `discount.go:6`, and passed normal anchor validation.

The draft was verbose, its critical severity was debatable for this fixture, and a separate test-coverage finding overlapped the inline report. Suggested integer-width changes also needed scrutiny on the 64-bit host. This is evidence of working integration and detection of one controlled bug, with human editing still necessary; broader review quality needs representative PRs.

All eight recorded Bash tool calls were inspected. They read the diff/source/history, ran existing tests and a small arithmetic experiment, wrote `findings.json`, and checked the anchor. No commit, push, `gh` invocation, or delegation appeared. Agent stderr was empty. The two identified scratch experiment files were removed after their contents were matched to the transcript. Raw diagnostics remain outside the repository in the trial's temporary state directory.

Live publication and recovery were exercised through fake GitHub responses, including accepted-but-lost responses and incomplete reads. This trial generated local drafts only; it did not test a real review POST or macOS notification delivery. Notification argument handling was checked separately, including a local `osascript` argument round trip.

## Repeating a manual trial

Choose an authorized open PR deliberately. This invokes Claude and can incur model cost; the test uses a separate queue and never approves or publishes findings. Replace the example repository, login, and PR number:

```sh
trial_state=$(mktemp -d)
PRQ_MANUAL_GITHUB_TRIAL=1 PRQ_MANUAL_STATE="$trial_state" \
  PRQ_MANUAL_REPO=owner/repo PRQ_MANUAL_USER=your-login PRQ_MANUAL_PR=123 \
  go test ./cmd/prq -run '^TestManualGitHubReviewTrial$' -timeout 20m -v -count=1
```

Inspect the printed queue, findings, transcript, and Git diagnostics afterward. Ordinary test runs skip this test. The earlier GitHub-independent [phase 4 smoke check](phase4-smoke.md) remains a separate, smaller integration check.
