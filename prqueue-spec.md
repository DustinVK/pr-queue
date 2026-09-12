# prqueue — v1

A CLI tool that drafts PR reviews with a coding agent, keeps every finding in a local queue, and requires human approval before its publisher sends a review. Terminal-only: run it, get notified, triage, publish. No daemon, no web server, no sandboxing subsystem. Deferred work lives in [FUTURE.md](FUTURE.md).

---

## 1. What it is

`prq` polls configured repos, reviews eligible PRs with a coding agent in a fresh git worktree, and writes proposed findings to a local SQLite queue. The coordinator sends reviews only through `prq publish`: one submitted review containing findings you've explicitly approved, never a staged draft.

**The publisher's guarantee:** it sends only the exact text and anchors approved against the comparison it verifies immediately before sending (§3). Editing a finding or observing a changed comparison clears approval. The request names the verified commit; a push during the request does not make that review cover newer code. The agent is trusted local code instructed not to publish. It runs with your full user permissions and can access ambient `gh` authentication, so this guarantee does not constrain a misbehaving agent process. Process isolation remains deferred in `FUTURE.md`.

## 2. MVP scope

**In:** macOS, github.com, one configured account, one agent (Claude Code), manual `prq run`, full CLI triage, SQLite, optional desktop notification.

**Explicitly not in v1** (see `FUTURE.md` for why each is deferred, not forgotten): a scheduler/daemon, a web UI, Docker/sandboxed execution, cross-machine notification, an auto-publish policy, staged/pending GitHub reviews, multi-agent support.

**Host requirements:** `git`, authenticated `gh`, the agent's own executable and model auth in the environment. No server to deploy.

## 3. Execution model

`prq run [--repo owner/name] [--pr N] [--dry-run]` — one pass, then exit:

1. Take the run lock (`~/.local/state/prqueue/run.lock`, non-blocking). Held → print holder, exit `3`.
2. Fully paginate the **unfiltered** open-PR list. Record each PR's head, base, draft flag, and state under its per-PR lock before applying any review filters. If that lock is busy, report the PR as skipped, leave its stored state untouched, and skip reviewing it this pass (including with `--pr`); this is not a failure and observation is retried on the next run. For previously tracked PRs absent from the complete list, fetch each directly and record the returned state; absence, a failed request, or filtering never implies closure. A changed comparison clears `last_reviewed_key` and resets approved findings to `pending` with `approved_comparison_key = NULL`, recording the invalidation in the audit log. This records ready → draft → ready and open → closed → open transitions when their intermediate states are observed, even if those states are ineligible for review.
3. Apply author/reviewer/base filters (empty = no restriction, all filters AND), then exclude closed/merged PRs and drafts. The **comparison key** is the compact JSON encoding of `[head_sha, base_sha, draft, state]`, using full SHAs and a JSON boolean. Skip an eligible PR only when this key matches `last_reviewed_key`. A transition entirely between successful observations (including across skipped passes) cannot be detected; v1 does not reconstruct event history. Filter-only changes do not clear a successful review of otherwise unchanged code.
4. Capture the selected key in `review_runs.comparison_key`, prepare a fresh worktree at its head, run the agent, and validate `findings.json` (§5). Before ingestion, take that PR's lock and refresh its state again. If the comparison changed, retain the output for diagnostics, mark the attempt `failed`, and leave the new comparison eligible; do not activate stale findings. Otherwise write findings, advance `last_reviewed_key` to the captured key, and append audit entries in one transaction, using the latest local decisions. The agent executes outside the lock. A failed or timed-out run never advances the cursor.
5. Fire one desktop notification if anything needs a look (§6). Exit `0` (ok), `1` (fatal — bad config, no auth), or `2` (some repos/PRs failed, others didn't).

`--pr N` requires `--repo` and forces a fresh review even when the key matches, bypassing the configured filters and draft exclusion. It never permits reviewing a closed/merged PR. `approve` and `publish` still refuse a currently draft PR regardless of how it was reviewed. `--dry-run` runs the agent (real cost) with temporary worktree/output files; the coordinator computes observations and resulting queue changes in memory, persists no database changes (including observed head/base/draft/state, run records, cursor, and audit), makes no GitHub writes, and sends no notification. The agent's unisolated host access remains as stated in §10.

No scheduler is built in — `prq run` is invoked by hand, or later by launchd/systemd (`FUTURE.md`), unchanged either way.

## 4. CLI reference

```
prq init                          Write default config, initialize the database. Safe to rerun.
prq run [--repo R] [--pr N] [--dry-run]

prq list [--repo R] [--status S]  Status: pending | approved | rejected | published | obsolete | blocked
prq show <repo#pr>                Findings for one PR: ids, bodies, current status.
prq diff <finding-id>             The finding against the current diff — catches staleness before approving.

prq approve <finding-id>...       Requires: PR open, not draft, finding valid for the current comparison.
prq reject <finding-id>... [--reason "..."]
prq edit <finding-id> [--as-general]   Opens $EDITOR on the body; clears approval; re-validates on save.
                                        --as-general converts a blocked or inline finding to general,
                                        dropping its anchor; the converted body needs approval.

prq publish <repo#pr> --event COMMENT|REQUEST_CHANGES|APPROVE [--dry-run]
prq publish <repo#pr> --resume [--confirmed-not-sent]
                                   Recover the blocking prepared/sending/uncertain attempt (§7).
                                   --confirmed-not-sent clears it without sending; an uncertain attempt
                                   requires checking the PR yourself before using that override.
prq status                        Last run per repo, lock state, any publish needing --resume.
```

`--json` on any command for scripting. Exit codes: `0` ok, `1` command/configuration failure (including unresolved publication), `2` a run with partial failures, `3` lock held. JSON stdout is one result object; diagnostics and agent output go to stderr or logs.

`approve`/`reject` take multiple ids for one PR in one call, all-or-nothing after validating every id. Finding ids are UUIDs; an unambiguous 8-hex-char prefix works too, and an ambiguous one is an error listing candidates rather than guessing.

Approval verifies the authenticated identity, current comparison, and anchor, then records `approved_comparison_key` with the exact body in the audit log. The finding's run must have that same comparison; rerun before approving findings from older code. Pending/rejected valid findings can be approved; blocked, obsolete, and published findings cannot. A cancelled editor or unchanged save leaves the row untouched. Changed text or `--as-general` resets approval. Published findings are not edited in place. These mutations and their audit entries are atomic.

## 5. The findings contract

The agent writes one JSON file to a path `prq` supplies — never stdout, never prose you regex.

```json
{
  "schema_version": 1,
  "repo": "owner/name",
  "pr": 482,
  "head_sha": "9f3c1a8e...",
  "summary": "Markdown. Becomes the review body if approved. Never posted unapproved.",
  "verdict": "comment",
  "findings": [
    {
      "kind": "inline",
      "path": "internal/store/store.go",
      "side": "RIGHT",
      "start_line": 138,
      "start_side": "RIGHT",
      "line": 142,
      "severity": "major",
      "category": "correctness",
      "title": "Transaction not rolled back on early return",
      "body": "Markdown. Verbatim what posts as the inline comment.",
      "rationale": "Why the agent flagged it. Never posted — triage only."
    },
    {
      "kind": "general",
      "severity": "minor",
      "category": "test-coverage",
      "title": "Cover the transaction failure path",
      "body": "Add coverage showing a failed write releases the transaction."
    }
  ]
}
```

- `kind`: `inline` (needs `path`/`side`/`line`) or `general` (no anchor). `summary` is a third kind stored internally — the top-level `summary` string becomes its own queue item with its own approval, exactly like a finding; it is never posted just because an inline finding was approved.
- `severity`: `nit` < `minor` < `major` < `critical`. `category`: `correctness`, `security`, `performance`, `style`, `test-coverage`, `design`. Closed enums — a new value needs a `schema_version` bump.
- `start_line`/`start_side` appear together for a multi-line comment, or not at all for single-line.
- `verdict` is the agent's private hint. It never chooses the GitHub `event` — that's always a human argument to `prq publish`.
- No `suggestion` field: a GitHub suggestion block belongs inside `body` as a fenced ` ```suggestion ` block, so the exact public text is visible during triage instead of assembled later.
- Limits: 4 MiB per output file, 100 findings per run, 60,000 bytes per body/summary. Exceeding one fails the run — never silently truncated.
- The output's `repo`, `pr`, and `head_sha` must exactly match what the runner supplied as input for that run. Any mismatch rejects the entire document — an agent reporting on the wrong PR is a bug, not something to partially trust.
- A finding whose anchor doesn't land inside the diff is kept as `blocked` with the validation error attached — never dropped, never folded into the summary automatically. You convert it to `general` by hand if it's worth keeping.

**Identity across reruns:** each finding gets a random UUID (its address) plus a SHA-256 fingerprint over the JSON array `[kind, path, side, body]` with absent fields encoded as null and body CRLF normalized to LF. No other Markdown whitespace is removed. Line numbers are deliberately excluded. Recompute the fingerprint when an edit or conversion changes its inputs. Reuse an ID only for an unambiguous one-old-to-one-new fingerprint match within that PR; multiple candidates get fresh UUIDs. Preserve a decision only when the body is byte-identical and the complete anchor (`path`, `side`, `line`, `start_line`, `start_side`) is unchanged. Approval additionally requires the same comparison and a still-valid anchor. Changed or ambiguous findings return to `pending`, or `blocked` if invalid, and clear approval. Record changed bodies in the audit log before replacing them; ingestion must not restore decisions from a snapshot taken before the agent ran. Previously published matches remain published and are not automatically reposted. Unmatched unpublished findings become `obsolete` after a successful replacement run. Reworded findings can look new; this is an accepted v1 limitation.

## 6. Notifications

One optional local notification per run, only when something needs a look. Quiet on a clean run with nothing new; quiet on a repeated identical failure until it changes.

Prefer `terminal-notifier` on `$PATH`; fall back to `osascript`. Both are called with fixed argument arrays — never a shell string built from PR text. Message is counts and PR identifiers only, never finding bodies. If both sinks fail, the run's own result is unaffected — you'll see it next in `prq status`.

This only fires on the machine that ran `prq run`. Moving execution to a headless box means swapping the notify sink first (`FUTURE.md`) — there's no display session there for `osascript` to reach.

## 7. Publishing and crash recovery

One request: `POST /repos/{owner}/{repo}/pulls/{n}/reviews` with an explicit `event`, the approved body, and approved inline comments. V1 never creates, edits, or submits a staged (pending) review — one call, immediately visible, every time.

Immediately before sending, `publish` re-fetches the PR and recomputes its comparison key (§3). It must match every included finding's run comparison and `approved_comparison_key`; validate every selected anchor too. A mismatch refuses the whole publication and clears stale approval; review and approve the current comparison first. `commit_id` names the just-verified head. It pins the request's target if a push races the POST, but cannot prevent that push or make the review cover the later commit. Also verify the PR is open and not draft, and the authenticated `gh` identity matches `github.user`.

Select only approved, unpublished findings, including an independently approved summary. The public body is that summary followed by approved general findings in creation order, with UUID as a tie-breaker, joined by blank lines. Inline findings post as individual review comments. Check the assembled body, including separators and the publication marker, against the 60,000-byte limit before sending. Titles, rationale, and other private metadata are not added to public text. `publish --dry-run` performs read-only validation and previews this exact payload without persisting a publication or resolving an existing one.

**The failure this section exists to prevent:** the request succeeds on GitHub but the response is lost before `prq` records it. So every publish:

1. Under the per-PR lock, first check for a blocking publication. A normal `publish` refuses one and directs the user to `--resume`. Otherwise freeze a **snapshot** containing the authenticated user, comparison key, head SHA, event, each selected finding's ID/kind/body/complete anchor/approved comparison, and the complete rendered request. Append `<!-- prqueue:ID -->` to the public body and write the snapshot to a `prepared` row before sending anything. It is never rebuilt from later queue contents.
2. Commits `sending`, then sends once.
3. Confirmed success → finalize as described below. Confirmed rejection with no side effect → `failed`, `uncertain = false`; a new `publish` may prepare a new snapshot.
4. Timeout, dropped connection, or another outcome that does not establish whether the request took effect → `failed`, `uncertain = true`. Do not treat every HTTP error as proof that nothing was created.

**Recovery uses the existing four states.** `--resume` takes the per-PR lock and selects the single blocking row. If none exists, it succeeds without sending anything. Verify the recorded GitHub identity before remote operations. Reconcile a possibly sent request before checking current PR eligibility: a review that already posted must be recorded even if the PR has since changed or closed.

- `prepared` is known unsent because `sending` commits before the network request. Plain `--resume` may send that unchanged snapshot only when its comparison still equals the live key, all original selected findings remain approved and match its text/anchors/approved comparison, and normal pre-send checks pass. The snapshot comparison check applies even to an empty `APPROVE`. A stale snapshot becomes `failed, uncertain=false` without sending; the next publication can start fresh. `--confirmed-not-sent` on a prepared row simply clears that unsent attempt without sending.
- `sending` left behind after its lock is released is an uncertain outcome, including a process killed before an error handler ran. `--resume` first records `failed, uncertain=true`, then reconciles it exactly like any other uncertain failure. It never resends that request.
- `failed, uncertain=true` requires reconciliation. Read the known review ID when available; otherwise fully paginate this PR's reviews looking for the marker. Verify the submitted review's author, target commit, event, and content against the stored snapshot. A matching submitted review is finalized locally with no second POST. A missing marker, incomplete read, unexpected remote content, or multiple matches leaves the attempt blocked and reports the evidence for manual inspection.

`prepared`, `sending`, and uncertain failures block any new publication through the partial unique index (§8). For an uncertain attempt with no matching marker after a complete read, the user may inspect the PR and invoke `--resume --confirmed-not-sent`. Recheck first: an existing matching submitted review is finalized, and any marker conflict or read failure still refuses clearance. Otherwise mark the attempt `failed, uncertain=false`, record the explicit override, and send nothing. A separate `publish --event ...` creates a fresh snapshot. Manual confirmation is an accepted duplicate risk, not proof that an in-flight request cannot finish later; retain the old snapshot and marker for diagnosis.

**Finalization preserves later decisions.** In one transaction, set the publication to `published`, clear `uncertain`, retain its GitHub review ID, and append a `published` audit event for each snapshot item using the **snapshot body**. Change a current finding to `published` only if it is still `approved` and its kind, body, complete anchor, run comparison, and approved comparison still equal the snapshot. Otherwise leave its current text, status, and approval untouched. Thus, if A was sent and then edited to A′ while the outcome was uncertain, recovery records A as sent while A′ remains pending (or retains its later explicit decision). `show` presents that historical publication separately. Finalizing the same publication twice is a no-op; audit events are not duplicated.

After checking for a blocking publication, zero approved findings and no approved summary means: `COMMENT` is a no-op, `APPROVE` sends an explicit empty approval, and `REQUEST_CHANGES` refuses. Approval or request-changes on your own PR is refused before sending.

## 8. Data model

SQLite via `modernc.org/sqlite` (pure Go, no cgo), WAL mode, foreign-key enforcement on each connection. The five tables below remain the v1 model.

```sql
CREATE TABLE pull_requests (
  id INTEGER PRIMARY KEY,
  repo TEXT NOT NULL,
  number INTEGER NOT NULL,
  head_sha TEXT NOT NULL,
  base_sha TEXT NOT NULL,
  draft BOOLEAN NOT NULL,
  state TEXT NOT NULL,                 -- open | closed | merged
  last_reviewed_key TEXT,              -- comparison key of the last successful review
  updated_at TEXT NOT NULL,
  UNIQUE(repo, number)
);

CREATE TABLE review_runs (
  id TEXT PRIMARY KEY,
  pr_id INTEGER NOT NULL REFERENCES pull_requests(id),
  head_sha TEXT NOT NULL,
  comparison_key TEXT NOT NULL,        -- immutable input comparison; never copied from a later poll
  status TEXT NOT NULL,                -- running | succeeded | failed | timed_out
  started_at TEXT NOT NULL,
  finished_at TEXT,
  raw_output_path TEXT,
  error TEXT
);

CREATE TABLE findings (
  id TEXT PRIMARY KEY,                 -- UUID
  pr_id INTEGER NOT NULL REFERENCES pull_requests(id),
  review_run_id TEXT NOT NULL REFERENCES review_runs(id),
  kind TEXT NOT NULL,                  -- inline | general | summary
  path TEXT, side TEXT, start_line INTEGER, start_side TEXT, line INTEGER,
  severity TEXT, category TEXT, title TEXT,
  body TEXT NOT NULL,
  rationale TEXT,
  fingerprint TEXT NOT NULL,
  status TEXT NOT NULL,                -- pending | approved | rejected | published | obsolete | blocked
  block_reason TEXT,
  approved_comparison_key TEXT,        -- set on approve; publish refuses unless it still equals the
                                        -- PR's current comparison key (head+base+draft+state)
  published_review_id TEXT,
  created_at TEXT NOT NULL
);

CREATE TABLE publications (
  id TEXT PRIMARY KEY,
  pr_id INTEGER NOT NULL REFERENCES pull_requests(id),
  head_sha TEXT NOT NULL,
  event TEXT NOT NULL,
  status TEXT NOT NULL,                -- prepared | sending | published | failed
  uncertain BOOLEAN NOT NULL DEFAULT 0,
  snapshot_json TEXT NOT NULL,         -- identity, comparison, selected items, complete request;
                                        -- immutable at `prepared`, used for retry/recovery/audit
  marker TEXT NOT NULL,
  github_review_id TEXT,
  created_at TEXT NOT NULL,
  finished_at TEXT,
  error TEXT
);
-- One blocking row per PR: unsent, possibly in flight, or an uncertain failure.
-- --resume handles every included state; the snapshot survives resolution.
CREATE UNIQUE INDEX one_open_publication_per_pr ON publications(pr_id)
  WHERE status IN ('prepared','sending') OR (status = 'failed' AND uncertain = 1);

CREATE TABLE audit_log (            -- append-only; what makes this defensible on a work repo
  id INTEGER PRIMARY KEY,
  pr_id INTEGER NOT NULL,
  finding_id TEXT,
  publication_id TEXT,
  at TEXT NOT NULL,
  actor TEXT NOT NULL,
  action TEXT NOT NULL,               -- created | approved | rejected | edited | published |
                                       -- approval_cleared | obsolete | publication_cleared | run_failed
  body_snapshot TEXT
);
```

An edit updates `findings.body` in place, resets `status` to `pending`, and clears `approved_comparison_key`. Record the original agent body on `created`, the resulting body on `edited`/`approved`, and the sent snapshot body on `published`; ingestion also records any body it replaces. State changes and their audit entries commit together. Publication recovery never substitutes a later body for the frozen one. The audit records what was explicitly approved and sent; it does not prove that a person read it rather than invoking a script.

## 9. Locking

Two nonblocking `flock` scopes: a global run lock and a per-PR lock keyed by `repo#number`. Observation updates, approve/reject/edit, ingestion, and the entire publish/resume operation take the per-PR lock. The agent executes outside it. Take the run lock before any PR lock and never hold multiple PR locks at once. A standalone command returns `3` on contention; within a run, observation contention is a non-failing skip (§3), while ingestion contention is a partial failure that leaves the cursor unchanged and allows other PRs to continue. Different PRs remain usable. Hold no SQLite transaction across agent execution or a GitHub request.

Lock inspection queries the kernel without acquiring the lock. Holder metadata is a best-effort diagnostic: an incomplete record is reported as unavailable, and stale metadata alone never establishes ownership.

`max_parallel_reviews` defaults to `1`; raise it deliberately when provider capacity and local resources permit.

## 10. Agent runner

Every review runs in a fresh, detached git worktree — never the checkout you're actively using. It may read, run tests, and build; it is instructed not to commit, push, or invoke `gh`. **This is an instruction, not an enforced boundary.** V1's default mode runs the agent with your full user permissions, and withholding `prqueue`'s own GitHub credentials from its environment does not stop it from using `gh`'s ambient auth (keychain, or `~/.config/gh/hosts.yml`) if it decided to call `gh pr review` itself. The guarantee in §1 holds because `prqueue`'s publisher is separate code that only ever sends content you approved — it says nothing about what the agent process itself could do if it misbehaved.

This is the same trust boundary you already accept running `claude -p` directly against your own repos day to day — v1 doesn't claim to improve on it, and `prq init` says so plainly once, requiring an explicit yes before the first run. Closing this gap for real means process isolation — Docker mode, deferred in `FUTURE.md`.

Default timeout 15 minutes, worktree removed on completion including failure. An orphaned worktree from an interrupted run is cleaned up on the next invocation once its owning process is confirmed dead.

## 11. Config

`~/.config/prqueue/config.yaml`.

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
      authors: []              # empty = any
      requested_reviewer: null
      base_branches: []
```

Model auth (e.g. `ANTHROPIC_API_KEY`) comes from the invoking shell's environment — `prq` doesn't manage secrets. Keep committed examples generic: no employer-specific repo names, internal hosts, prompts, or source samples. Actual config lives outside the repo at the path above.

## 12. Package layout

```
cmd/prq/            CLI + JSON/human output
internal/config/    load, validate, defaults
internal/findings/  contract, validation, fingerprinting — no deps on the rest of the tree
internal/github/    poll, fetch diff, publish + reconcile
internal/store/     sqlite, migrations
internal/runner/    worktree lifecycle, agent exec, timeout
internal/queue/     approve/reject/edit/list/show — the seam a future UI would call
internal/notify/    Sink interface; desktop (macOS) ships in v1
internal/lock/      the two nonblocking flock scopes
internal/localfs/   private directories and atomic file installation/replacement
```

## 13. Acceptance scenarios

The short list that actually protects the guarantee in §1 — not a release gate, a sanity check before trusting this against a real repo:

| Scenario | Required result |
|---|---|
| Approve A, reject B, publish | Request contains A only, verbatim approved text. |
| Head/base changes or an observed ready → draft → ready / open → closed → open transition | Observe before filtering, clear the cursor and approvals, and review again. Stale publication is refused. Filter-only exclusion never implies closure. |
| Edit an approved finding, including while publication is unresolved | The edit becomes pending. Recovery audits the previously sent snapshot and preserves any later text or decision; original and edited bodies remain in the audit log. |
| Agent crashes or its comparison changes before ingestion | The attempt does not activate findings or advance `last_reviewed_key`; the next run retries eligible work. |
| Publisher stops in prepared/sending, or GitHub accepts but its response is lost | Prepared resumes only after validation; sending reconciles without replay. A matching submitted review finalizes once, even if the PR has since changed or closed. |
| Publish with zero approved findings | `COMMENT` no-ops; `REQUEST_CHANGES` refuses; `APPROVE` sends empty. |
| `prq run` and `prq approve` overlap on the same PR | Per-PR lock serializes them; other PRs unaffected. |
| A blocking publication exists for a PR | The index refuses a fresh publication. Marker absence alone never permits retry; explicit clearance sends nothing and preserves the old snapshot. |

## 14. V1 choices and deferred agent work

- GitHub transport: shell out to `gh` in v1 and verify the configured identity; changing the transport later stays within `internal/github`.
- Triggers: poll current state, record observed transitions before filtering, and include drafts becoming ready as specified in §3. No webhook-event reconstruction or generation counter.
- Agent: Claude Code in v1. Measure quality on deliberately selected real PRs before considering a local-model or escalation path; that work remains in `FUTURE.md`.
