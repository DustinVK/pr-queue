# Phases 1–7 implementation handoff

Recorded September 12, 2026, against CLI head `a1edd1e`. This is a continuation guide to the architecture and decisions established in phases 1–7 of [IMPLEMENTATION_PLAN.md](../IMPLEMENTATION_PLAN.md). It complements the [v1 specification](../prqueue-spec.md), which defines behavior, and [AGENTS.md](../AGENTS.md), which gives day-to-day repository guidance.

**Current state:** all original phases, including phase 8, are implemented and reviewed. Do not restart at phase 1 or assume live publication is missing. The next requested feature is [launchd polling](launchd-polling-spec.md); [interactive review](interactive-review-plan.md) is a separate, unimplemented plan. This handoff and those extension docs are documentation, not evidence their proposed commands exist.

## Phase numbering and branch context

The original implementation phases and the later eight-PR delivery stack use different numbering. Findings validation and observation were combined in one PR; notification work and final CLI wiring were split. Use this mapping when locating code or choosing a base:

| Plan phase | What landed | Delivery branch / PR |
| --- | --- | --- |
| 1 | Configuration, storage, locks, initial CLI foundation | `prq/01-foundation` — [#2](https://github.com/DustinVK/pr-queue/pull/2) |
| 2 | Findings contract and diff anchors | `prq/02-observation` — [#3](https://github.com/DustinVK/pr-queue/pull/3) |
| 3 | GitHub reads, observation, queue inspection | `prq/02-observation` — [#3](https://github.com/DustinVK/pr-queue/pull/3) |
| 4 | Worktrees, Claude, orchestration, ingestion | `prq/03-runner` — [#4](https://github.com/DustinVK/pr-queue/pull/4) |
| 5 | Human triage and editor integration | `prq/04-triage` — [#5](https://github.com/DustinVK/pr-queue/pull/5) |
| 6 | Immutable publication snapshots and preview | `prq/05-preview` — [#6](https://github.com/DustinVK/pr-queue/pull/6) |
| 7 | Live submission and uncertain-delivery recovery | `prq/06-publication` — [#7](https://github.com/DustinVK/pr-queue/pull/7) |
| 8 | Notifications and persistent run summaries | `prq/07-notifications` — [#8](https://github.com/DustinVK/pr-queue/pull/8) |
| 8 | Complete CLI wiring, acceptance tests, final docs | `prq/08-cli` — [#9](https://github.com/DustinVK/pr-queue/pull/9) |

GitHub was checked when writing this handoff: PRs #2–#9 were open, each based on the preceding branch; #2 targets `main`. The checkout was `prq/08-cli`, and local `main` still pointed to the initial README commit `db5728a`. Recheck with `git status`, `git log --oneline --decorate`, and `gh pr list --repo DustinVK/pr-queue` rather than treating this snapshot as permanent.

The code was implemented and validated incrementally, then arranged into 13 coherent commits across that stack. The full user-facing CLI wiring is in the final branch, even where earlier plan phases describe the command's behavior. New work needs the complete CLI base unless the existing stack has since landed. Merging in dependency order with ancestry-preserving merge commits keeps the stack connected; squash/rebase merges require updating descendants. Do not merge or rewrite branches merely to read this handoff.

At handoff creation, the working tree also contained earlier uncommitted README/FUTURE changes and the how-it-works, interactive-review, and launchd documents. Preserve them. Read the current diff before staging; untracked planning files are not disposable generated output.

## Architecture to keep in mind

`cmd/prq` wires configuration, storage, transports, and I/O into `internal/queue`. Queue services orchestrate domain operations; `internal/store` performs their atomic persistence. `internal/findings` is the independent validation layer. `internal/github`, `internal/runner`, `internal/editor`, and `internal/notify` are replaceable boundaries for external effects. There is no application server or background loop in v1.

The ordinary flow is remote observation → captured comparison/diff → disposable source checkout → agent output file → validation and live recheck → local triage → explicit publication. The agent never decides which finding is approved or which GitHub review event is sent. See [how it works](how-it-works.md) for the full invocation walkthrough.

Three identities serve different purposes:

| Identity | Purpose |
| --- | --- |
| Comparison key | Compact JSON `[head_sha, base_sha, draft, state]`. Defines the exact PR state that was observed, reviewed, approved, or published. |
| Finding UUID and fingerprint | UUID addresses the local item. SHA-256 of `[kind, path, side, body]`, with null absent fields and CRLF-only body normalization, can match it across reruns. Full anchors/body/comparison still determine whether decisions survive. |
| Publication UUID and marker | Identifies a frozen delivery attempt. `<!-- prqueue:ID -->` in the submitted body enables recovery when GitHub accepted a request but the response was lost. |

Do not collapse these identities into head SHA alone, fuzzy body matching, or current queue contents. Those shortcuts would lose base/draft/state invalidation, exact human approval, or recovery evidence.

## Phase 1 — Configuration, private storage, and locks

Start in [config.go](../internal/config/config.go), [store.go](../internal/store/store.go), [schema.sql](../internal/store/schema.sql), and [lock.go](../internal/lock/lock.go). The module uses Go 1.26+, pure Go SQLite, a strict YAML parser, and UUIDs. Config parsing rejects unknown fields and invalid users/repositories, timeouts, concurrency, and filter values. `repos` contains objects with `name`, not strings. Defaults are a 15-minute timeout and one parallel review.

`init` creates private application directories/files, preserves existing config/data, and separately records exact trusted-agent acknowledgment in `agent-consent-v1`. Paths are fixed under the real user's home: `~/.config/prqueue/config.yaml` and `~/.local/state/prqueue/queue.db`. There is no Docker/database service or production path-override CLI. Tests inject `config.Paths` instead of using the user's queue.

The five tables are `pull_requests`, `review_runs`, `findings`, `publications`, and `audit_log`. Schema versioning uses `PRAGMA user_version`; initialization is transactional. Connections enable WAL, foreign keys, a busy timeout, and immediate write transactions. Triggers enforce append-only audit history, immutable run comparisons, and immutable publication snapshots. The partial index permits only one prepared/sending/uncertain publication per PR. Keep these database protections even when service code also checks the same rule.

Locks are nonblocking flock scopes with holder metadata: one global `run.lock` and one hashed filename per case-normalized repo/PR. Never unlink a lock file; the inode is the synchronization primitive, and stale metadata is not proof of a held lock. Acquire the global lock before a PR lock when both are needed, and never hold two PR locks. Database transactions never span an agent, editor, or GitHub request.

Review fixes worth preserving: repository casing originally could split locks/rows, and interrupted consent input could keep waiting. Configuration/store/lock tests and [main_test.go](../cmd/prq/main_test.go) retain the regressions. All implemented commands use one JSON result envelope with stderr diagnostics; common exit codes are `0` success, `1` command/fatal failure, `2` run failures, and `3` lock contention.

## Phase 2 — Strict output contract and diff anchors

[findings.go](../internal/findings/findings.go) decodes one UTF-8 JSON document with exact schema and input identity. It rejects missing/unknown fields, invalid types/enums, malformed Unicode, excess documents, and exceeded limits: 4 MiB output, 100 findings, 60,000 UTF-8 bytes per body/summary. The top-level summary becomes an independently actionable internal `summary` finding; the agent may only emit `inline` and `general` items in its findings array. Verdict, title, and rationale never become public text automatically.

[diff.go](../internal/findings/diff.go) checks complete anchors, including both range endpoints, against actual LEFT/RIGHT hunk lines. It retains reasons for binary, unavailable, truncated, or otherwise unverifiable patches. A malformed finding shape rejects the document. A valid shape with an unverifiable location becomes `blocked`; there is no automatic relocation, conversion to general, or dropping of the item.

Fingerprints deliberately omit line numbers so a rerun can recognize a moved finding. That does **not** preserve its approval: matching requires one old and one new candidate, and a decision requires byte-identical body and complete anchor; approval also requires the same comparison. CRLF normalization is for fingerprint matching only, not permission to rewrite approved Markdown. Ambiguous matches get fresh UUIDs.

Phase review caught agent-emitted internal summary kinds, silently repaired Unicode surrogates, and overlapping hunks. See [findings tests](../internal/findings/findings_test.go) and [diff tests](../internal/findings/diff_test.go) before simplifying validation.

## Phase 3 — GitHub reads and observation before filters

[client.go](../internal/github/client.go) wraps `gh api` with fixed argument arrays, explicit github.com/method/version headers, identity checks, response bounds, and complete pagination. It can be replaced by an injected executor. GitHub owns PR metadata and changed-file patches; Claude is not asked to discover a PR through `gh`.

[observe.go](../internal/queue/observe.go) first obtains the complete unfiltered open-PR list. Tracked PRs absent from that list are fetched directly; a failed later page, missing list entry, or filter exclusion never establishes closure. Observation persists head/base/draft/state under the PR lock before selection. A changed comparison clears `last_reviewed_key` and approvals with audits in one transaction. Author/reviewer/base filters are ANDed afterward; filter changes alone do not force unchanged successful comparisons to be reviewed again.

Observation lock contention skips that PR without a failure or stored-state change. A forced `--pr` still cannot bypass this skip or review a closed/merged PR; it does bypass normal filters, drafts, and successful-comparison skips. Transitions entirely between successful observations cannot be reconstructed, an accepted v1 limitation.

Queue reads resolve full finding UUIDs or unambiguous eight-hex prefixes and separate historical publications from current findings. `diff` performs fresh live checks and can report staleness. Regressions protect against an embedded buffer allowing `io.Copy` to bypass a response limit, and a GitHub patch omitting a whole hunk despite locally valid remaining hunks. File totals and addition/deletion counts participate in completeness checks. See [client tests](../internal/github/client_test.go) and [observation tests](../internal/queue/observe_test.go).

## Phase 4 — Runner, worktrees, and atomic ingestion

[runner.go](../internal/runner/runner.go) makes a fresh bare repository for each attempt, fetches the captured head and base SHAs, and creates a detached worktree at the captured head. Claude has the tracked source tree and fetched history on disk, including unchanged code; the entire repository is not stuffed into its prompt. Dependencies/submodules are not installed automatically. PR discussion, descriptions, and previous reviews are not currently supplied.

The runner executes the configured binary directly with the equivalent arguments:

```sh
claude -p --verbose --output-format stream-json \
  --no-session-persistence --session-id "$run_id" \
  --dangerously-skip-permissions
```

Its cwd is the detached checkout. The [prompt](../internal/runner/prompt.go) arrives on stdin and names the exact repo/PR/head/base plus absolute diff/output paths and the JSON contract. `PRQUEUE_INPUT` and `PRQUEUE_OUTPUT` carry identity and destination too. The deliverable is `findings.json`; `agent.jsonl` and `agent-stderr.log` are separate diagnostics. No model override is selected. The prompt allows focused tests/builds and instructs against modifying source, committing, pushing, invoking `gh`, or contacting external review services. Scratch experiments belong beside the run's output.

The trusted-local-agent decision is deliberate. Claude uses full user permissions; stripping GitHub token environment variables does not remove local `gh` credentials. Worktrees and prompt instructions are not a sandbox. The runtime verifies that the reviewed HEAD is unchanged and records refs/status, but does not enforce every prompt restriction. The publisher's human-approval guarantee must not be described as preventing every possible agent side effect.

The [CLI run handler](../cmd/prq/run.go) acquires the global run lock and holds it through the pass. The [coordinator](../internal/queue/run.go) requires that caller-held lock, limits agent concurrency, and captures an immutable run comparison. The agent runs outside the PR lock. Before ingestion, the coordinator reacquires that lock and refreshes live state; changed comparisons fail without activating stale output. A busy ingestion lock is a per-PR failure, unlike observation's skip. [store/runs.go](../internal/store/runs.go) reloads current decisions inside the ingestion transaction, matches findings, audits replacements, obsoletes unmatched unpublished findings, and advances the cursor atomically. Published matches remain history and are not automatically reposted.

A missing/nonregular/invalid output, crash, timeout, or changed comparison fails the attempt. Keep diagnostics; do not advance the success cursor or replace/obsolete findings from that failed output. Earlier observation-driven approval invalidation still stands. Continue other PRs and return `2`, including when all attempted reviews fail. No repair prompt or immediate retry occurs. Later ordinary runs retry an eligible unsuccessful comparison; a failed forced rerun of an already successful comparison can still be skipped on the next ordinary pass because its old successful cursor remains valid.

Normal runs retain artifacts in `runs/<run-id>/` and clean disposable worktrees. [cleanup.go](../internal/runner/cleanup.go) uses owner PID/start time, agent process group, and a session-ID fallback around child startup. SIGTERM/context cancellation terminates owned process groups; bounded cleanup/persistence can outlive the canceled request context. Startup recovery checks ownership under the global lock, skips while another run owns it, and marks interrupted attempts failed. Some otherwise observational CLI commands invoke this recovery hook.

`run --dry-run` runs the real agent unless a fake is injected, but uses temporary files and a consistent in-memory database clone. It leaves persistent observations, cursors, findings, audits, run records, and notification summaries untouched. Do not equate dry run with zero model cost or process isolation.

Review fixed an ownership-creation crash window and a changed comparison detected before agent execution that failed to invalidate approvals. The [runner tests](../internal/runner/runner_test.go), [coordinator tests](../internal/queue/run_test.go), [ingestion tests](../internal/store/runs_test.go), and [CLI run tests](../cmd/prq/run_test.go) cover these boundaries. A real-Claude local synthetic test passed in 29.22 seconds **before phase 5**, with normal contract validation and manual transcript/Git-state inspection. [Evidence and opt-in rerun instructions](phase4-smoke.md) distinguish this mechanical test from review-quality evaluation.

## Phase 5 — Human triage and editor behavior

[queue/triage.go](../internal/queue/triage.go) and [store/triage.go](../internal/store/triage.go) implement approve/reject/edit. Multi-ID decisions must target one PR and commit together or not at all. Approval checks the live GitHub identity, open/non-draft state, finding-run comparison, and complete anchor, and records the exact approved body and comparison. A refused approval can still persist observation-driven invalidation; this is required behavior, not a partially committed approval.

Editing uses a private file via [editor.go](../internal/editor/editor.go). `$EDITOR` accepts quotes/arguments without shell expansion and falls back to `vi`. Cancellation, editor failure, or an unchanged save leaves the finding unchanged. Changed bodies and `--as-general` clear approval and audit original/resulting bodies; conversion removes every anchor field. Published findings cannot be edited in place. The existing editor holds the PR lock across the editor session but no DB transaction; other PRs remain usable and the agent can run concurrently.

Review regressions cover literal `--json` inside a rejection reason and failed edits rendering empty findings. Tests also protect atomic rollback, same-PR contention, other-PR usability, and ingestion preserving decisions made while an agent runs. The upcoming interactive mode must reuse these services with displayed-finding preconditions; current ID-only operations do not protect a human reading interval from another process replacing the displayed item.

## Phase 6 — Freeze publication before adding the POST

[queue/publication.go](../internal/queue/publication.go), [store/snapshot.go](../internal/store/snapshot.go), and [store/publications.go](../internal/store/publications.go) established rendering and persistence before a live transport was introduced. That split let fixtures exercise exact approved payloads and state transitions without a POST path.

The request contains an independently approved summary, then approved general bodies in chronological creation order with UUID tie-breaking; inline bodies and complete anchors are separate comments. Text is preserved exactly. Titles/rationale/verdict remain private. Revalidate every approval and the live comparison, pin `commit_id` to that head, and check the final body's 60,000-byte limit including separators and marker.

Check for a blocking publication before empty-selection behavior. With no approved items, COMMENT is a no-op, REQUEST_CHANGES refuses, and an explicit APPROVE sends an empty approval. APPROVE/REQUEST_CHANGES on the user's own PR are refused. These events are explicit human choices, never inferred from the agent's verdict.

The immutable snapshot holds the user, comparison/head, event, selected IDs/bodies/full anchors/approval keys, marker, and exact request. Finalization audits the sent snapshot and only marks still-matching approved current findings published, preserving later edits/decisions. Preview shares validation/rendering but persists no observation, preparation, or recovery. Each preview creates a candidate marker; a separate live invocation generates its own marker.

Review caught the accepted single-dash `-dry-run` spelling accidentally allowing startup recovery. A five-table CLI regression protects the read-only path. Read [publication tests](../internal/queue/publication_test.go), [store publication tests](../internal/store/publications_test.go), and [CLI publish tests](../cmd/prq/publish_test.go) before refactoring argument handling or finalization.

## Phase 7 — Submit once and reconcile uncertain delivery

[queue/publish.go](../internal/queue/publish.go) and [github/publication.go](../internal/github/publication.go) added one submitted-review POST with an explicit event. The PR lock spans the operation, but each local state transition commits before any network call. There are four publication states plus an uncertainty flag; no additional pending-review state machine was added.

| Recorded state | Recovery behavior |
| --- | --- |
| `prepared` | Known unsent. Resume may send the original frozen request only after identity, live comparison, original item approvals/text/anchors, and normal eligibility checks still pass. Even an empty APPROVE must match its snapshot comparison. |
| `sending` | May already have posted. Convert the abandoned attempt to uncertain failure and reconcile; never replay it. |
| `failed`, `uncertain=true` | Blocks fresh publication. Read a known review ID or search the complete review list by marker, fetch required inline comments, and compare the exact snapshot. |
| `failed`, `uncertain=false` | Does not block a new attempt. This requires evidence of rejection/no send, stale unsent preparation, or explicit clearance under the recovery rules. |
| `published` | Finalized history. Repeated finalization is idempotent. |

`prepared` commits, then `sending` commits, then POST occurs once. A timeout, malformed response, process death, or arbitrary HTTP error is not proof nothing was posted. The transport only classifies its supported complete rejection responses as definite; preserve raw diagnostic evidence instead of treating every non-2xx as safe to resend.

Resume verifies the snapshot's recorded account before remote work. Reconcile possible delivery **before** checking whether the PR is still open/current: a matching review can be finalized after a push, draft transition, or closure. Match author, commit, submitted event, body, marker, and all inline comment contents/anchors, including original anchors where GitHub relocates them after a push. Missing/conflicting/duplicate markers or incomplete reads stay unresolved.

`--confirmed-not-sent` sends nothing. It clears a known-unsent prepared attempt directly; for an uncertain attempt, a matching review is finalized, errors/conflicts are refused, and only a complete no-match read allows audited clearance after the user's manual inspection. It is not a retry or force-publish switch. An unresolved snapshot remains frozen while later triage continues.

Fault tests cover commit boundaries, lost accepted responses, death before/after POST, incomplete pagination, known-ID failures, conflicting markers, stale empty approvals, and repeated finalization. A CLI run/approve/publish/edit/resume fixture verifies one POST and preservation of the later edit. [Publisher tests](../internal/queue/publish_test.go) and [transport tests](../internal/github/publication_test.go) are the primary regression map. Phase 7 used fake POSTs plus a real read-only `gh --include` framing check; it did not post a live GitHub review.

## Phase 8 context already present in the checkout

The current code also has `internal/notify`, `queue.FinishRun`, `run-summary.json`, and full CLI acceptance fixtures. One local notification reports new/changed findings or distinct failures; identical repeated failures stay quiet. Summary persistence includes repository failures with no PR row without adding a sixth table. Notification errors do not change the run result. Fatal preflight can happen before summaries/notifications update, which matters for launchd status.

Phase 8 corrected cleanup of an interrupted, unregistered checkout and ordering across fractional timestamps. Public rendering compares parsed instants; new stored timestamps have fixed fractional precision. A real-GitHub/Claude trial on fixture PR #1 caught a seeded arithmetic bug; its draft needed human editing. The PR was closed unmerged with no submitted reviews. Offline fault tests cover live publication/recovery; actual review POST and macOS notification delivery were not validated by that trial. The scratch-directory prompt refinement after transcript inspection has not had another paid trial. See the full [validation record](phase8-validation.md).

## Picking up the next feature

The [launchd spec](launchd-polling-spec.md) records the selected five-minute interval and operation on battery. It proposes a per-user LaunchAgent around plain `prq run`, three schedule-management commands, private logs, explicit environment setup, and three implementation PRs. Sleep/active-run firings are skipped; ordinary eligibility can cause repeated paid calls after failures. There is no new daemon, automatic approval/publication, or internal polling loop. Existing `com.dustinvk.prqueue.plist` is only the old example, not the completed feature.

One implementation gap identified during launchd planning is concrete: runner owner metadata records Claude's group, not every Git helper. Graceful cancellation handles process groups, but hard coordinator death during Git setup needs targeted ownership/recovery work before claiming complete scheduled-uninstall cleanup. Do not infer this guarantee from the existing killed-Claude-coordinator test. Preview/status also need to avoid the CLI's normal recovery hook; uninstall must work with broken review config/auth and must not wait for the run lock before trying to stop its owner.

The [interactive plan](interactive-review-plan.md) is ready for separate work. Preserve explicit accept after edit, fresh uncached GitHub checks on accept/diff, per-action persistence, and displayed-finding preconditions. Reject actual nonterminal stdin/output before recovery or mutations; `yes a | prq review` must not approve anything. The selected interruption code is `2` for EOF at a required prompt/Ctrl-C, while explicit quit is `0` and invocation/fatal errors are `1`. These are planned additions, not current CLI behavior.

Use the review cycle and build commands in [AGENTS.md](../AGENTS.md). The original phases passed formatting, vet, relevant tests, race checks where applicable, and a cgo-free build; historical results are not validation of new edits. Each next phase needs its own complete diff review, fixes, affected checks, and recorded outcome. Ordinary automation must remain fixture-based. No production schedule, real agent invocation, or review publication was performed to write this handoff.

## Handoff review

Reviewed this document and AGENTS.md against the specification, phase review log, current source/tests, local branch history, and live PR metadata. The review clarified which layer acquires the global run lock and kept the phase/PR numbering, current recovery side effects, and planned helper-cleanup work explicit. Local links/anchors, shell-example syntax, Markdown fences, and whitespace checks passed. No Go code changed, so the application test suite was not rerun for this documentation task.
