# Interactive terminal review plan

Proposed extension to the [v1 CLI](../prqueue-spec.md#4-cli-reference). The selected interaction is a plain terminal prompt: type a letter, then Enter. This document plans the feature; implementation follows after review.

## Command and selection

```sh
prq review                            # All pending and blocked local findings
prq review --repo DustinVK/pr-queue    # One repository
prq review DustinVK/pr-queue#2         # One PR
```

The positional PR and `--repo` are mutually exclusive. Selection comes from the local queue, independent of agent-generation filters. The command does not invoke Claude. An empty selection prints a short message and succeeds without GitHub calls.

Capture a fixed list of candidate IDs at session start. Group by repository and PR, show the independent summary first within each PR, then other findings by chronological creation time and UUID. Reload each finding before displaying it. Skip entries that another command has already approved, rejected, published, or made obsolete; findings created during the session wait for the next invocation.

## Terminal flow

```text
DustinVK/pr-queue#2   Finding 3 of 7   12ab34cd   pending
major / correctness   internal/store/store.go:142 RIGHT
Transaction not rolled back on early return

<full proposed Markdown body>

[a] accept  [e] edit  [d] deny  [v] diff  [s] skip  [q] quit >
```

Show the kind, title, severity/category when present, full anchor, body, and any block reason. Label rationale as private context. Display the complete body without truncation. Escape terminal control characters in the display while preserving the original stored body.

| Action | Behavior |
| --- | --- |
| `a` accept | Approve this exact displayed finding locally using current identity/comparison/anchor checks, then advance. This does not select a GitHub review event or publish. |
| `e` edit | Open the body in `$EDITOR`, falling back to `vi`. Save through existing edit rules, then redisplay the result and wait for a separate acceptance. An unchanged save or editor failure leaves the finding untouched. |
| `d` deny | Reject this finding locally and advance. Use the existing rejection audit; no mandatory reason or extra confirmation prompt. |
| `v` diff | Fetch/display the current diff with the finding's full anchor and any staleness/validation error, then reload and redisplay the finding before asking for another action. A failed diff read still allows deny, skip, or quit. |
| `s` skip | Leave the finding unchanged and advance for this session. |
| `q` quit | Stop intentionally, show the session outcome, and return `0`; completed decisions remain saved. |

Accept only explicit, complete action lines. Blank input is a no-op; invalid input repeats the choices. EOF while waiting for a decision interrupts the session and returns `2`, without applying an unterminated action line. Print an interruption message and the saved-progress summary so it cannot be mistaken for normal completion. Earlier committed decisions and edits remain saved. Ctrl-C/context cancellation also terminates the session and active editor operation with exit `2`. Interruption uses this code even before the first decision; it identifies an unfinished session rather than an invalid invocation.

Once the selection is exhausted, finish successfully without reading another action. A complete final action followed by EOF therefore succeeds; EOF before the next required decision does not. A complete `q` line is the explicit successful early exit; an unterminated `q` followed by EOF is still an interruption.

Blocked findings show their reason and disable acceptance. Offer `g` to convert an inline finding to general through the existing `--as-general` behavior, then redisplay it for explicit acceptance. Editing a body alone does not repair an invalid anchor. Stale comparisons remain ineligible for approval until a new review is generated; conversion does not bypass that rule.

Each successful mutation commits immediately. There is no session-wide transaction or saved session cursor. A new invocation naturally resumes with pending/blocked findings, including skipped and edited-but-unaccepted items. On exit, report accepted, denied, skipped, externally changed/skipped items, and successful edits/conversions; edits can overlap later decisions. Publishing remains the existing separate command.

## Preserve the displayed decision

The existing [triage service](../internal/queue/triage.go) accepts finding IDs and reloads current rows. Interactive review adds a human reading interval: a concurrent rerun can preserve an ID while changing its anchor or review run. An action must not silently apply to a different finding version than the user saw.

- Add conditional variants of the existing approve/reject/edit operations that accept the displayed finding as a precondition. Share their implementation and validation with the current commands.
- Compare identity, review-run identity/comparison, body, full anchor, status/approval, and displayed metadata with the current row under the PR lock. Recheck the mutation precondition inside its database transaction. Existing columns are sufficient; no schema migration or revision counter is required.
- A mismatch performs no requested triage mutation or triage audit. Reload and redisplay actionable findings, or report and skip entries already decided/obsolete/published. Require fresh input for the new display. Live observation may still persist comparison invalidation when an approval is refused, as required by the current spec.
- Hold no PR or global run lock while waiting at the menu. Each action uses the existing PR lock. Editing retains the current per-PR lock across the external editor and save; other PRs remain usable. Database transactions never span a prompt, editor, or GitHub call.
- An unresolved publication does not prevent triage. Its snapshot stays frozen, and existing recovery rules preserve subsequent local edits/decisions.

Current remote eligibility and anchor checks remain authoritative even if a displayed item looked current. A conflict, busy lock, failed remote read, or canceled editor does not silently advance. Display the error and allow retrying the action, skipping, or quitting. Fatal storage/configuration failures terminate the session.

Each accept or diff operation uses fresh live GitHub checks, inheriting the standalone semantics with no cache across session actions. An action can require several `gh` subprocesses and network round-trips; repeated latency on the same PR is an accepted tradeoff at personal-repository scale. Viewing a diff never satisfies a later acceptance's live checks.

## Implementation shape

Put argument handling and the small session loop in `cmd/prq/review.go`, using `internal/queue` directly. Reuse `internal/editor` and the existing diff/triage services. Do not shell out to `prq` for each action. The loop needs small injectable input/output/service seams for tests; a general UI framework is unnecessary.

Require terminal input and output. Reject nonterminal input/output and `--json` before startup recovery or database mutation; retain the existing commands for scripted operation. Update the spec's universal `--json` statement to document this interactive-only exception. Check help/invalid flags before starting a session.

The stdin terminal check protects the explicit-human-approval boundary: `yes a | prq review` must exit `1` before applying any action. Check actual stdin, even when stdout or a controlling terminal is available; detecting `/dev/tty` is not a substitute for rejecting piped input.

Terminal input has one owner. Pause menu reads while the editor owns the terminal, avoid consuming future editor input through buffered read-ahead, and make waiting for input cancelable without accumulating reader goroutines. The existing one-shot consent reader should not simply be called repeatedly. Route editor output through the session terminal and restore the prompt after the editor exits.

Reuse available terminal-detection dependencies where possible; no external executable beyond the existing editor is needed. The interactive command's exit codes are:

| Code | Outcome |
| --- | --- |
| `0` | Completed selection, empty queue, or explicit quit. |
| `1` | Invalid invocation, including nonterminal input/output or `--json`, or a fatal configuration/storage failure. |
| `2` | Interrupted session: EOF while awaiting a decision, Ctrl-C, or context cancellation. Saved decisions and edits remain committed. |

This extends the existing partial-completion meaning of `2` to interactive sessions. Document it in the spec and README alongside the interactive JSON exception. Handled per-item lock contention stays in the session. Existing noninteractive exit-code meanings remain unchanged.

## Incremental delivery

Implement this as three dependent, reviewable changes above the current CLI branch, or above `main` after the existing stack lands. At each boundary, review the diff, fix findings, and rerun affected checks before continuing.

1. **Conditional triage actions.** Add displayed-finding preconditions to shared queue/store mutation paths. Test concurrent body/anchor/run/status changes, including a preserved ID with a moved anchor; verify rollback, exact audit bodies, and existing standalone-command behavior. Review lock/transaction boundaries before adding the terminal loop.
2. **Interactive command.** Start with an automated preflight regression: repeated `a` lines supplied through a real pipe must return `1` with no recovery, GitHub calls, or finding/audit changes. Provide terminal stdout so rejection proves the stdin protection rather than merely failing the output check. Then add selection/order, display, accept/edit/deny, skip/quit, immediate persistence, terminal preflight, and editor hand-off. Test scripted action sequences through injected I/O, edit-then-accept, unchanged/failed editing, explicit input, empty queues, external changes, and restarting after a partial session. Verify explicit quit returns `0`, EOF at a required prompt and cancellation return `2`, invalid invocation/fatal failures return `1`, unterminated input applies nothing, and a complete final action succeeds without another input read. Check interruption before the first decision and after saved progress, including preservation of committed edits and decisions. Add help and the basic README workflow. Review the complete loop before extending it.
3. **Context and acceptance.** Add diff viewing and blocked-to-general conversion. Finish error handling and spec/docs updates. Exercise actual editor/terminal hand-off with a focused pseudo-terminal test or recorded manual trial; cover Ctrl-C, queued input, and terminal restoration. Verify repeated accept/diff operations make fresh live reads and a comparison change between viewing a diff and accepting refuses approval. Verify pending summaries require their own acceptance and unresolved-publication edits retain recovery behavior. Review the complete feature before opening it for normal use.

Run formatting, `go vet ./...`, relevant tests, race checks for concurrent actions/input, and the cgo-free build. The final acceptance pass uses local fixtures and fake GitHub/editor operations; no paid agent call or real publication is needed for this feature.

## Planning review

The plan addresses the displayed-item race, explicit acceptance after editing, per-action persistence, blocked anchors, session restart behavior, editor input ownership, and the new interactive exception to JSON output. Review follow-up distinguishes intentional quit (`0`), interrupted sessions (`2`, with saved-progress reporting), and invocation/fatal failures (`1`). Further review records uncached live checks as an intentional latency tradeoff and makes piped-input rejection the first phase 2 regression. It adds no session table, publication workflow, or full-screen terminal framework. Implementation and its phase reviews are still pending.
