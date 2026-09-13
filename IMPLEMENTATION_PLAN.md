# Incremental implementation plan

Implement [the v1 specification](prqueue-spec.md) in the order below. [FUTURE.md](FUTURE.md) remains out of scope. This plan organizes delivery; it does not replace or relax the specification.

## Working agreement

Each phase is a reviewable change with a useful outcome. Before starting the next phase:

1. Validate the phase's behavior, including relevant failure paths. Use local repositories, temporary databases, and fake executable/API responses for automated checks.
2. Review the complete diff against the referenced spec sections. Check data integrity, exact approved text, stale comparisons, lock ownership, and external side effects where applicable.
3. Fix every issue found, rerun the affected checks, and review the fixes.
4. Record the validation, review findings, fixes, and remaining constraints in the phase review log below. Advance only when the phase has no unresolved review issues.

Use `gofmt`, `go vet ./...`, and `go test ./...` once Go code exists. Add `go test -race ./...` for concurrency phases, and verify `CGO_ENABLED=0 go build ./cmd/prq` to preserve the pure Go build. The race detector is a development check and may require a C toolchain; it is not a requirement for the shipped binary. Test behavior and failure boundaries, rather than duplicating implementation details in assertions.

Do not make paid agent calls or publish to real PRs as part of automated checks. A deliberately selected real-PR quality check follows the completed local acceptance coverage; a live publication requires explicitly approved findings and a human-selected event.

## Tooling preflight

Checked on 2026-09-12:

| Requirement | Result |
| --- | --- |
| macOS host | Available, Apple Silicon |
| Go | `go1.26.4 darwin/arm64` |
| Git | `2.50.1 (Apple Git-155)` |
| GitHub CLI | `2.96.0`; github.com authentication verified |
| Claude Code | `2.1.236`; local auth status reports logged in |
| C toolchain for development race checks | Apple Command Line Tools `clang` available via `xcrun` |
| Desktop notification fallback | `/usr/bin/osascript` available |
| Optional `terminal-notifier` | Not installed; not a blocker |

No required executable is missing. Authentication checks did not invoke an agent or test model capacity. Download and pin Go module dependencies in phase 1; installed tools alone do not prove dependencies can be fetched. If subsequent checks reveal a missing tool, report it for the user to install.

## Phase 0 — Documentation and project map

**Outcome:** a newcomer can understand the intended tool, prerequisites, trust boundary, and delivery order without assuming an executable already exists.

- Add `README.md` with requirements, intended workflow, config example, approval/recovery behavior, and links to the spec and deferred work.
- Add this implementation plan and its review log.
- Check existing documentation links and keep all committed examples generic.

**Validation/review:** compare the README and plan with the spec; check links and whitespace. No code tests are needed for this phase.

## Phase 1 — Executable, configuration, storage, and locks

**Depends on:** phase 0. **Spec:** §§4, 8–12.

**Outcome:** `prq init` creates a usable local foundation safely, and `prq status` reports it.

- Add `go.mod`/`go.sum`, `cmd/prq`, and the initial `internal/config` and `internal/store` packages. Pin compatible versions of `modernc.org/sqlite` and a YAML parser; prefer the standard library for other needs.
- Resolve and document config/state paths, the database filename, and private file permissions. Validate repository identifiers, GitHub user, positive timeout and concurrency, and filter fields; apply the spec's defaults.
- Implement idempotent initialization and explicit acknowledgment of trusted, unisolated agent execution before the first run. Record acknowledgment locally; rerunning `init` must preserve existing config and data. A noninteractive caller must explicitly acknowledge rather than receiving an implicit yes.
- Create the five specified tables and partial unique publication index with atomic migrations and SQLite's schema version facility. Enable WAL and enforce foreign keys on every connection.
- Implement reusable nonblocking global and per-PR file locks with holder information. Use deterministic safe filenames for PR keys, take the run lock first, and never hold multiple PR locks at once.
- Establish one-object JSON output, stderr diagnostics, exit-code handling, and CLI argument validation. Unsupported commands must fail clearly until their phase lands.
- Add build/test/install instructions to the README and a focused `.gitignore` for generated binaries and local artifacts.

**Validation/review:** temporary-home initialization and reruns; declined/missing consent; malformed config; migrations and rollback; foreign keys on multiple connections; publication uniqueness; cross-process lock contention and release after process exit; JSON output and exit codes. Confirm database transactions cannot span agent execution or remote calls.

## Phase 2 — Findings contract and diff anchors

**Depends on:** phase 1. **Spec:** §§3, 5.

**Outcome:** agent output can be accepted or rejected deterministically without running an agent.

- Build dependency-independent `internal/findings` types and strict versioned JSON validation, including exact input repo/PR/head matching and closed enums.
- Enforce the 4 MiB file, 100 findings, and 60,000-byte body/summary limits without truncation. Validate required fields, anchor shape, and paired range fields. Turn the top-level summary into an independently actionable internal queue item.
- Implement comparison keys as compact JSON arrays with full SHAs and a boolean draft flag. Implement fingerprints with only the specified CRLF normalization, null absent fields, and excluded line numbers.
- Parse unified diffs and validate LEFT/RIGHT and multi-line anchors against actual hunks. Cover additions, deletions, context, renames, missing/truncated patches, and binary files; an unverifiable anchor becomes blocked, not silently relocated or converted.
- Separate whole-document failures from structurally valid findings with invalid diff anchors, retaining a useful block reason for the latter.

**Validation/review:** fixtures for valid single/multi-line findings, wrong PR/head, unknown enums, byte limits including multibyte text, malformed JSON, range boundaries, CRLF fingerprints, duplicates, and unavailable diff data. Confirm private metadata never enters public body rendering.

## Phase 3 — GitHub reads, observation, and queue inspection

**Depends on:** phases 1–2. **Spec:** §§3–4, 7, 9, 14.

**Outcome:** remote PR state can be observed safely and local findings can be inspected through `list`, `show`, and `diff`.

- Add `internal/github` with an injectable command executor around `gh`, explicit github.com targeting, authenticated-identity verification, PR/diff reads, and complete pagination. Invoke executables with fixed argument arrays.
- Fetch the unfiltered open-PR list completely before using absence information. Directly fetch previously tracked PRs absent from that list; never infer closure from absence, filtering, or request failures.
- Under each PR's lock, persist observed head/base/draft/state and atomically clear the cursor and approvals with audit records when the comparison changes. Apply filters and eligibility only after observation.
- Implement `internal/queue` read operations and CLI output. Resolve full UUIDs or unambiguous eight-hex prefixes, reporting candidates on ambiguity. `show` must have a place for historical publications separate from the current finding.
- Make observation logic usable with an in-memory store for future dry runs. Keep GitHub calls outside SQLite transactions.

**Validation/review:** multipage PR lists and failed later pages; direct-fetch failures; filter exclusions; base-only changes; observed draft/ready and closed/open transitions; busy per-PR locks leaving state untouched; UUID ambiguity and JSON reads. Verify no path can write to GitHub yet.

## Phase 4 — Worktrees, agent execution, and review ingestion

**Depends on:** phases 1–3. **Spec:** §§3, 5, 8–10.

**Outcome:** `prq run` produces a durable local queue, with failures safely retryable.

- Add `internal/runner`: fetch the captured PR head, create a fresh detached worktree, supply the findings schema/input and an explicit output path, run Claude Code, and capture diagnostics separately from JSON stdout.
- Instruct the agent not to commit, push, or invoke `gh`; preserve the documented full-user trust boundary. Enforce the timeout, terminate the process group, and clean up worktrees on normal and failed completion.
- Track worktree ownership outside the five-table model and clean interrupted runs' orphans only after confirming the owning process is dead. Account for PID reuse and preserve diagnostic output for failed non-dry runs.
- Implement run locking, selected-repo/PR argument rules, filter behavior, forced draft review, configured parallelism, and per-repo/PR failure accounting. Observation contention skips; ingestion contention is a partial failure.
- Capture each run's immutable comparison. Execute the agent outside the PR lock, then reacquire it, refresh remote state, and refuse stale ingestion. Successful ingestion, cursor advancement, and audit entries commit together.
- Match fingerprints only one-old-to-one-new. Preserve IDs and decisions according to exact bodies, complete anchors, and comparison rules; keep published matches published; assign fresh IDs to ambiguous matches; obsolete unmatched unpublished findings only after successful replacement.
- Read the latest local decisions inside ingestion, preserving edits/approvals made while the agent ran and auditing replaced bodies. Never advance the cursor on invalid output, timeout, crash, or changed comparison.
- Implement `run --dry-run` using temporary files and a consistent in-memory copy of queue state, with no persistent database changes, notifications, or coordinator GitHub writes.

**Validation/review:** use a fake agent and local Git remotes to test success, invalid output, timeout/child cleanup, stale comparisons, concurrent triage, duplicate fingerprints, CRLF-only body changes, moved anchors, published matches, orphan cleanup, and reruns after failure. Compare persistent state before/after dry runs. Run concurrency checks.

**Manual smoke check before phase 5:** make one real-Claude invocation through the actual `internal/runner` code using a synthetic diff in a disposable local worktree, with synthetic PR metadata and no GitHub dependency. Use the runner's real prompt, arguments, output path, and timeout; confirm `findings.json` passes the normal contract validator, execution finishes within timeout, and cleanup succeeds. Inspect the transcript and Git state by hand for attempted commits, pushes, or `gh` use, and record the result in the phase review log before proceeding. This checks CLI integration; phase 8 retains the separate real-PR quality trial.

## Phase 5 — Human triage and approval

**Depends on:** phase 4. **Spec:** §§4–5, 8–9.

**Outcome:** findings can be edited, approved, or rejected with an auditable decision history.

- Add `approve`, `reject`, and `edit`, using the queue service and per-PR locks. Validate every selected ID belongs to one PR before an all-or-nothing batch mutation.
- Approval verifies current authenticated identity, live PR eligibility, the finding's run comparison, and the complete anchor. Approve only valid pending/rejected findings, saving the exact comparison and body audit snapshot atomically.
- Launch `$EDITOR` with a temporary file and defined cancellation behavior. Cancelled or unchanged saves leave the row untouched. Changed bodies recompute fingerprints, clear approval, and revalidate. Published findings cannot be edited in place.
- Support `--as-general` by dropping all anchor fields and requiring approval of the converted body. Preserve the original and resulting bodies in the audit history.
- Ensure observing a comparison change during triage persists invalidation even when the requested approval is refused. Different PRs remain usable during a locked edit or approval.

**Validation/review:** multi-ID rollback, mixed PR IDs, identity mismatch, closed/draft/old-comparison refusals, invalid anchors, rejected-to-approved transitions, editor failure/unchanged save, conversion, and approval clearing. Exercise overlap between a running agent and triage, checking ingestion uses the latest decision.

## Phase 6 — Publication snapshots and read-only preview

**Depends on:** phase 5. **Spec:** §§7–9.

**Outcome:** `publish --dry-run` previews the precise request and the local publication state machine is testable. Live sending becomes available in phase 7, together with recovery.

- Build deterministic public payloads containing only independently approved items: summary first, then general bodies in creation order with UUID tie-breaking, and separate inline comments. Include no title, rationale, or verdict.
- Revalidate identity, live comparison, every included approval and anchor, and own-PR restrictions. Pin `commit_id` to the verified head. Enforce the final body's byte limit including separators and marker.
- Implement zero-item behavior: COMMENT no-op, REQUEST_CHANGES refusal, and explicit empty APPROVE. Check for a blocking publication before considering the no-op.
- Define immutable snapshots containing the recorded user, comparison/head, event, selected item text/complete anchors/approval keys, marker, and exact request. Implement atomic preparation and the partial-index conflict path locally.
- Build idempotent finalization that audits sent snapshot bodies and only marks still-matching approved current findings as published. Later edits and decisions must survive.
- Wire read-only preview through the same validation and rendering logic, without preparing or resolving a publication or persisting observed state. The preview includes the marker for its candidate request; a later separate publication allocates its own ID/marker.

**Validation/review:** approve A/reject B, summary independence, exact whitespace, deterministic ordering, empty events, stale approvals, own-PR restrictions, assembled-byte overflow, dry-run immutability, immutable snapshots, concurrent preparation, and finalization after edits. Verify there is still no live POST path.

## Phase 7 — Live publication and crash recovery

**Depends on:** phase 6. **Spec:** §§7–9, 13–14.

**Outcome:** `publish` and `publish --resume` can send and recover safely as one complete feature.

- Hold the PR lock for the complete operation. Commit `prepared`, commit `sending`, then make exactly one submitted-review POST with an explicit event. Before resume performs any remote reads or writes, verify the authenticated user matches the snapshot's recorded identity. Never keep a database transaction open during a remote request.
- Treat transport failures and ambiguous HTTP outcomes as uncertain unless the response establishes rejection without a side effect. Keep the frozen snapshot and error evidence.
- Resume a prepared attempt only after checking its live comparison (even an empty APPROVE), original item approvals, text/anchors, identity, and normal publication checks. Fail stale prepared attempts without sending.
- Treat an abandoned `sending` attempt as uncertain and never resend it. Reconcile before checking current PR eligibility so an already-submitted review can be finalized after a push, draft transition, or closure.
- Read a known review ID or fully paginate reviews by marker. Fetch all required inline comments and verify author, commit, submitted event, exact body, and complete comment contents/anchors against the snapshot. Missing markers, incomplete reads, unexpected content, and multiple matches remain blocked with diagnostic evidence.
- Implement `--confirmed-not-sent`: clear known-unsent prepared attempts directly; recheck uncertain attempts, finalize matches, refuse read errors/conflicts, and clear only after a complete no-match result. Record the override and send nothing.
- Add CLI argument compatibility rules, no-op resume when no attempt blocks, unresolved-publication reporting in `status`, and historical publication output in `show`.

**Validation/review:** fault injection before/after each committed state and around POST. Simulate accepted-but-response-lost, process death in sending, definite rejection, incomplete pagination, duplicate/conflicting markers, changed/closed PRs, prepared empty approvals becoming stale, manual clearance, and finalization repeated twice. Confirm at most one POST for every possibly sent attempt and preserved later decisions. Run concurrency checks.

## Phase 8 — Notifications and v1 integration

**Depends on:** phase 7. **Spec:** §§3–4, 6, 13.

**Outcome:** the complete one-shot CLI is ready for a deliberately selected real-PR trial.

- Add the `internal/notify` sink, preferring `terminal-notifier` and falling back to `osascript`. Use fixed argument arrays and messages containing counts/PR identifiers only.
- Notify once when a run needs attention; suppress clean runs and identical repeated failures until they change. Use persisted run outcomes for change detection, including a small local run-summary file for repo-level failures with no PR row, without adding a sixth table. Notification failures do not change the run result, and dry runs never notify or update that summary file.
- Complete `status` with last-run information per repo, lock holders, failures, and blocking publications. Check all commands' human/JSON output and specified exit codes.
- Exercise the spec's eight acceptance scenarios across the CLI using fake GitHub and agent executables, temporary SQLite state, and local Git repositories. Keep this as a focused sanity check rather than importing deferred scenarios wholesale.
- Update the README with actual build/install commands, implemented workflow, recovery examples, and verified limitations. Review real-PR findings quality only on PRs deliberately selected by the user; record it separately from offline correctness checks.

**Validation/review:** notification fallback and injection-shaped PR identifiers, repeated-error quieting, partial failures, dry-run isolation, CLI acceptance coverage, formatting/vet/tests, race checks, and a cgo-free build. Verify no scheduler, staged reviews, auto-publishing, sandbox subsystem, or multi-agent feature has entered v1.

## Phase review log

| Phase | Status | Validation and review result |
| --- | --- | --- |
| 0 | Complete | 2026-09-12: tooling/authentication preflight and documentation review completed. Fixed both stale spec links in FUTURE.md, shell placeholders in the README, and recovery-identity/notification-state details in this plan. All 12 local links/anchors, both README shell examples (syntax only), and new-document whitespace checks passed. No code exists yet, so code tests do not apply. |
| 1 | Complete | Added Go module, init/status CLI, validated config, private files, explicit consent, five-table SQLite migration, and nonblocking locks. Review found casing could split a PR's lock/row and an interrupted consent prompt could keep waiting; regression tests reproduced both, fixes were reviewed and passed. Formatting, vet, package tests, race tests, cgo-free build, CLI JSON help smoke check, and documentation checks passed. Status fixtures also verify latest runs and unresolved publications. |
| 2 | Complete | Added strict findings decoding, byte/count limits, independent summary items, comparison keys, fingerprints, full-anchor equality, and unified-diff validation. Fixtures cover additions/deletions/context, renames and quoted paths, ranges, unavailable/truncated patches, malformed JSON, wrong identities, and byte boundaries. Review exposed internal summary kinds accepted from the agent, silently repaired Unicode surrogates, and overlapping hunks; regression tests reproduced each and passed after fixes. Formatting, vet, the full test suite, and cgo-free build passed. |
| 3 | Complete | Added explicit GET-only gh transport, complete pagination and identity checks, observation before filtering, atomic comparison invalidation, UUID resolution, and list/show/diff. Tests cover failed later pages, direct-fetch failures, observed state transitions, forced reviews, lock skips, filter behavior, stale diff reads, and rollback of invalidation with audit entries. Review found io.Copy could bypass an embedded buffer's size limit and a patch could omit a whole hunk; fixed with a non-embedded bounded buffer, required file totals, and addition/deletion checks. Formatting, vet, full tests, race checks, cgo-free build, CLI JSON smoke, and a real read-only client identity/list call against the project repository passed. |
| 4 | Complete | Added disposable detached worktrees, real Claude invocation and strict ingestion, parallel review coordination, process-group timeout handling, interrupted-run recovery, and in-memory dry runs. Review found an ownership setup crash window and a comparison change detected before agent execution that did not invalidate approvals; fixed both with regression coverage. CLI tests compare all five tables after a real fake-agent dry run and verify removed temporary artifacts, partial-failure exit codes, and retries. Formatting, vet, full race tests, and a cgo-free build passed. The one-time real-Claude invocation passed in 29.22 seconds; normal contract validation, unchanged Git state, cleanup, and manual transcript inspection passed. See [smoke evidence](docs/phase4-smoke.md). |
| 5 | Complete | Added atomic single-PR approve/reject batches, exact approval audit snapshots, live comparison/diff revalidation, private rejection reasons, and editor-driven body changes/conversion. Tests cover mixed-ID rollback, invalid/old/draft/closed comparisons, identity errors, summary independence, invalid anchors, cancellation and unchanged saves, edit audit rollback, published refusal, same-PR contention, other-PR usability, and triage during agent execution. Review found global --json extraction could consume a literal rejection reason and failed edits could render an empty finding; regression tests pass after fixes. Formatting, vet, full race tests, and cgo-free build passed. |
| 6 | Complete | Added exact public request rendering, read-only publish preview, immutable publication snapshots, atomic preparation, sending transition, and idempotent local finalization. Tests cover approval/summary independence, exact whitespace and ordering, full anchors, empty events, own-PR restrictions, body-plus-marker limits, stale approval invalidation, concurrent preparation, snapshot immutability, audit rollback, and preservation of later edits/decisions. Review found the accepted single-dash -dry-run spelling could trigger startup recovery; a five-table CLI regression reproduced the write and passed after the fix. Formatting, vet, full race tests, cgo-free build, and whitespace checks passed. Confirmed the GitHub client still has only a GET transport; live POST remains phase 7. |
| 7 | Complete | Added one explicit submitted-review POST, documented-rejection versus uncertain-outcome handling, recorded-identity checks, prepared resume, uncertain reconciliation, and audited manual clearance. Fully paginated PR comments retain original anchors for verification after pushes. Fault tests cover preparation/sending/finalization commits, death before/after POST, lost responses, stale prepared empty approvals, changed bodies/anchors, definite rejection, incomplete reads, conflicting/multiple markers, known-ID failures, and confirmed-not-sent behavior. A CLI run/approve/publish/edit/resume test preserves the later edit and sends exactly one POST through its fake transport. Review verified transaction/lock boundaries and strengthened retention of malformed HTTP evidence. Formatting, vet, full race tests, cgo-free build, and whitespace checks passed. A real read-only gh --include call confirmed the installed CLI's response framing; no live review has been posted. |
| 8 | Complete | Added desktop notification fallback, persisted run summaries, repeated-failure suppression, and status reporting for repository failures without PR rows. CLI acceptance checks use actual subprocess boundaries with fake gh/agent executables and local Git, covering the eight spec scenarios with supporting queue/store fault and concurrency tests. Review fixed interrupted setup cleanup for an unregistered worktree and incorrect public-body ordering across fractional timestamps; regression tests reproduced both and passed after fixes. Formatting, vet, full race tests, cgo-free build, JSON help, and documentation checks passed. A real-GitHub/Claude trial on the authorized repository caught a seeded arithmetic bug in 70.48 seconds; findings remained local and the fixture PR was closed unmerged. Transcript review prompted a scratch-directory instruction refinement. No deferred subsystem entered v1. See [validation and quality limits](docs/phase8-validation.md). |

Plan review follow-up: added and reviewed the manual real-Claude integration check in phase 4, then completed it before phase 5. Automated checks continue to use fake agents.

Implementation and review are complete through phase 8, with fixes at each boundary. The phase 4 synthetic local runner check and phase 8 controlled GitHub PR trial are recorded separately. The trial establishes integration and detection of one seeded bug; it does not establish broad review quality. No real GitHub review was published.
