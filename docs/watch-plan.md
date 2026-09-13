# prq watch — design

## What it is

A long-running, terminal-resident dashboard you start once and leave open. It shows your configured repos, how many PRs in each need attention, and lets you drill into a repo, see its PRs, and jump straight into the existing `prq review` flow for one of them. It is the "leave it open and glance at it" surface that `prq run` (one-shot, externally scheduled) and `prq review` (one PR at a time, invoked deliberately) don't provide on their own.

## Non-goals — read this section first

`watch` does not:

- Poll GitHub. It never talks to the GitHub API. `prq run`, on its own launchd schedule, remains the only thing that does.
- Run the agent, ingest findings, or make any GitHub write call.
- Mutate any finding, approval, or publication. Every state change still happens inside `prq review` or `prq publish`, run as a child process.
- Own notifications. `prq run`'s `notify.Sink` is unchanged and is still the only source of desktop notifications. (Rationale below.)
- Introduce a second definition of "PR status." It reads the same tables `review` and `publish` already read and write, and derives status the same way they would.

Everything `watch` does is a read of local SQLite plus a terminal hand-off to a binary that already exists. That's deliberate: `run`/`review`/`publish` already solved polling, locking, and mutation safety as separate OS processes coordinating through the DB and flock. `watch` adds a fourth process that only reads.

**Prerequisite: `prq review` must exist before `watch` phase 1 can be built.** `watch`'s entire value is the hand-off into it; as of this checkout `review` is still unimplemented. Treat that as a hard blocking dependency, not a parallel-track item — don't start `watch` phase 1 until `review` is real and its exact invocation form (below) is stable.

## CLI surface

```
prq watch              # repo list screen
prq watch --repo OWNER/NAME   # jump straight to that repo's PR list
```

No other flags for v1. Config (repo list, DB path) comes from the same `config.yaml` `run` and `review` already use. An optional `watch.refresh_interval_ms` (default 2000; see Refresh model) is the only `watch`-specific setting — adding it requires a real change to the shared strict config parser (it currently rejects unknown keys), not just a documentation update.

`watch` requires a real terminal on both stdin and stdout and refuses to start otherwise, and there is no `--json` mode — it's an inherently interactive surface, the same safety property `review` already has for the same reason (a piped or redirected `watch` isn't a dashboard, it's a foot-gun).

## Screens

### 1. Repo list (home screen)

One row per configured repo:

| column | source |
|---|---|
| repo | `config.yaml` |
| pending | count of PRs with at least one actionable finding — no decision yet, or edited-but-not-approved (see status derivation below) |
| ready to publish | count of PRs where every finding has a terminal decision (approved or rejected) and nothing currently blocks publishing |
| needs attention | count of PRs with a blocking `publications` row — `prepared`, `sending`, or `uncertain` (see status derivation) |

There is no per-repo "last checked" column — `pull_requests` has no such field, and I'm not assuming `run-summary.json` is repo-scoped. Instead the screen shows one line above the table: "last run: `<time>`, from `run-summary.json`" (the same source `schedule status` already reads), labeled as the latest recorded pass, which may have been manual — exactly the caveat already established for `schedule status`.

Sorted with any repo that has `needs attention > 0` pinned to the top, then by `pending` descending. Arrow keys / `j`/`k` to move, `Enter` to drill in, `q` to quit the program.

### 2. PR list (per repo)

One row per tracked PR for the selected repo, most recently updated first:

| column | source |
|---|---|
| # | `pull_requests.number` |
| status | derived — see below |
| updated | `pull_requests.updated_at` |
| published | separate marker, not part of `status` — see below |

There is no stored PR title (`pull_requests` has no such column), so the list identifies PRs by number only for v1. Showing titles needs an explicit fetch-and-store change (a migration plus a place in `run`'s ingestion to capture it) — deferred, not part of this feature; `#526` is what you get until that lands.

**Status derivation.** Status reflects *current actionable state*, computed fresh each time, never a stale flag left over from a previous publish:

1. If a blocking publication row exists for this PR (`prepared`, `sending`, or `uncertain`) → status is `needs attention`. All three block a new publish attempt via the existing partial unique index, and all three can be the result of a killed publisher, not just `uncertain` — a process killed between writing the `prepared` row and actually sending, or mid-send, leaves exactly the same "something needs to be resolved" situation. So all three surface the same recovery action (below), not just `uncertain`.
2. Else, if any finding has no decision yet, or has been edited since its last approval → `N pending`.
3. Else, if there is at least one finding and every finding has a terminal decision (approved *or* rejected — a PR that's one approved finding and one rejected finding is fully decided, not incomplete) → `ready to publish`.
4. Else (PR tracked, no findings yet) → `no findings`.

This is deliberately independent of publication *history*. A PR that was published once, then got a new push with new findings, is `N pending` again — not `published`. Whether it has ever been published is shown as a separate `published` column (a timestamp, or blank), so that history isn't lost, but it never overrides what's actually still actionable right now.

`Enter` on a `needs attention` row offers two actions (small inline menu, not a new screen): `r` review normally, `p` run `prq publish ... --resume` recovery. `Enter` on any other row goes straight into review. `Esc`/`b` goes back to the repo list. `q` quits the whole program from either screen.

### 3. Hand-off to `prq review`

`watch` does not reimplement any part of the review TUI. On selection it:

1. Captures the selected PR's identity — `(repo, number)` — at the instant `Enter` is pressed, not re-derived afterward from whatever row happens to be highlighted (see Refresh model for why this matters).
2. Suspends its own display (releases the terminal — see Terminal ownership below).
3. Execs `prq review OWNER/NAME#N` as a child process, connected directly to the real terminal (stdin/stdout/stderr inherited, not piped) — matching `review`'s own established form: PR is given as a single `OWNER/NAME#N` positional argument, and `review` treats that positional and a `--repo` flag as mutually exclusive, so `watch` always uses the positional form and never passes `--repo` alongside it.
4. Waits for the child to actually exit — not just for a signal to be delivered to it (see Cancellation policy).
5. Resumes its own display and immediately re-queries the DB, so the screen reflects whatever `review` just changed.

`review`'s own startup already reads current DB state, so there's no staleness risk here even if the row `watch` displayed when you pressed Enter is a few seconds old by the time `review` launches — `review` never trusts what `watch` showed it, it re-reads.

`watch` does not interpret `review`'s exit code beyond zero/nonzero (see Child failure visibility, below) — `review`'s own interruption exit code has already changed once during implementation and shouldn't be hardcoded into a second command's spec.

### 4. Hand-off to `prq publish` (recovery)

Same mechanism as above, for the `needs attention` action: exec `prq publish OWNER/NAME#N --resume` (`publish` has no `--repo` flag; it's positional-only, same as `review`). If that comes back still `uncertain` and you decide by hand it's safe, you still run `--confirmed-not-sent` yourself from a real terminal — `watch` doesn't attempt to guess that call for you.

Because recovery is now offered for `prepared`/`sending` as well as `uncertain` (not just the latter), it's possible to invoke `--resume` against a publication that isn't actually stuck — the publisher is still legitimately running under a different `prq` invocation, or the PR lock is genuinely held. That's fine: `prq publish --resume`'s own PR lock refuses to run against an active publisher and reports that cleanly. `watch` doesn't need to distinguish "actually stuck" from "still legitimately in flight" itself — it offers the action whenever the state is blocking, and lets the lock be the arbiter of whether recovery can proceed right now.

## Child failure visibility

A child process (`review` or `publish --resume`) can exit nonzero for reasons that leave the database completely unchanged — a failed GitHub/Claude auth check, lock contention, or the process failing to start at all. Silently redrawing the dashboard in that case erases the only evidence of what happened; the next screen would look identical to whatever it looked like before you pressed Enter, with no way to tell "nothing happened" from "the thing you were trying to do worked."

So: on any nonzero child exit, `watch` shows a short inline message ("`prq review` exited with status N — press any key to continue") and waits for a keypress before restoring its own display and redrawing. On a zero exit, it redraws immediately with no extra step. This applies uniformly to both hand-offs; `watch` treats "zero vs. nonzero" as the only distinction it's entitled to make about a child it doesn't own the implementation of.

## Terminal ownership

Both hand-offs need to give the child process the real terminal and get it back cleanly. Two ways to do it:

- If `watch` is built on a TUI framework with a suspend/exec primitive (e.g. Bubble Tea's [`tea.ExecProcess`](https://github.com/charmbracelet/bubbletea/blob/master/exec.go)), use that — it's designed for exactly this and already handles restoring raw mode / alt-screen state afterward.
- If `watch` is a plain redraw-loop (no framework), do it by hand: turn off raw mode, exit the alt screen, `exec.Command(...).Run()` with inherited stdio, then re-enter raw mode / alt screen and force a redraw.

Either is fine; the framework route is less code to get wrong. Whichever is chosen, the suspend/resume boundary is the highest-risk plumbing in this whole feature and needs its own focused terminal tests, separate from the DB/status-derivation tests — a broken restore leaves the user's terminal in raw mode with no prompt back, which is a much worse failure than a wrong status label.

### Cancellation policy

This needs to be explicit, because "what does Ctrl-C do" has two different correct answers depending on who owns the terminal:

- While `watch` itself owns the terminal (repo list or PR list screens, nothing execed), Ctrl-C exits `watch` immediately, exit 0. There's no pending decision state living only in `watch`'s memory to lose.
- Once a child owns the terminal (inside `prq review`, inside its `$EDITOR`, or inside `prq publish --resume`), `watch`'s own input loop is not running — the framework's suspend primitive means `watch` isn't reading stdin at all during that window. A Ctrl-C delivered to the foreground terminal in that state goes to the child, and it is entirely the child's own responsibility to interpret it (which `review` already specifies for itself). `watch` never forwards, translates, or independently sends signals to the child.
- Either way, `watch` waits for the child process to actually exit before restoring its own display — it does not race ahead on the theory that a signal was sent. If the child needs a bounded moment to run its own cleanup, `watch` blocks through that; it doesn't paint over an interactive terminal that a child process is still mid-cleanup on.

## Refresh model

`watch` polls the local SQLite file, not GitHub. That's a fundamentally cheap operation (single-digit milliseconds, no rate limit, no network) so a short fixed interval is fine — default every 2 seconds, configurable via `watch.refresh_interval_ms`. This is not the same decision as "should `run` poll GitHub every 5 minutes" — those two numbers aren't related and shouldn't be confused.

Concretely: on a timer tick, re-run the same queries that built the current screen and re-render if anything changed (diff, don't blindly redraw, to avoid flicker while you're reading). Open a read-only connection with a short `busy_timeout` (e.g. 500ms); if a query fails because `run` or `review` holds a write lock at that instant, just skip that tick and try again next one — never block or error the UI over a transient SQLite lock.

**Selection is preserved by identity, not row position.** Rows can and will reorder between ticks — a repo moves to the top because it just gained a `needs attention` entry, a PR moves because its `updated_at` changed. If the highlighted row were tracked by numeric index, a refresh could silently move the cursor onto a different PR than the one you were looking at, and pressing `Enter` would open the wrong one — `review`'s own re-read-on-launch doesn't help here, because it correctly opens whatever PR number it's told to open; it has no way to know that wasn't the one you meant. So: track the current selection by `(repo, PR number)` (or just `repo` on the home screen). After each requery, find that identity in the new result set and keep it highlighted, wherever it now sorts. If it's gone entirely (the PR closed, or the repo was removed from config), fall back to the same index position, or the top row if the list is now shorter. Capture the identity at the moment `Enter` is pressed — described above under Hand-off — not re-derived after the fact, for the same reason.

No file-watcher (fsnotify on the DB/WAL) needed for v1. Polling a local file every 2 seconds is not a problem worth adding a dependency to avoid.

## Why notifications stay with `run`

This was the one open question left from the last round. Resolved: `run` keeps sole ownership of `notify.Sink`, unchanged. Two reasons:

1. `run` fires on its own schedule whether or not you have `watch` open — that's the common case, since `watch` is an opt-in thing you run when you're at your desk wanting to triage, not a requirement for `run` to be useful.
2. If you *do* have `watch` open and looking at it, a desktop notification for something the dashboard is already showing you live is noise, not signal. `watch` update-in-place is the notification, for as long as it's on screen.

So: no dedup marker, no second notify path, nothing added to `internal/notify`. `watch` only ever reads.

## Failure modes

- DB file doesn't exist yet — show a single-screen empty state: "No repos tracked yet — run `prq init` first," not an error. (`prq run` does not create the database; only `init` does.)
- SQLite busy on a poll tick — skip the tick silently (see Refresh model).
- Child process (`review`/`publish`) exits nonzero — see Child failure visibility above; the dashboard is not silently redrawn over it.
- Repo in `config.yaml` with zero tracked PRs yet — show it in the repo list with all counts at 0, not omitted.

## Locking

None held by `watch` itself — it only issues read queries and never takes the per-PR or global flock. Whatever locks `review` and `publish` already take, they take as their own independent process when execed; `watch` isn't in that critical section and doesn't need to coordinate with it beyond "wait for it to exit before redrawing." As noted above, a recovery action offered on a still-legitimately-active publication is expected to be safely refused by that process's own lock, not preempted by `watch`.

## Exit codes

`watch` mutates nothing on its own, so it has none of `review`'s careful exit-code semantics. `q` from the repo list, or Ctrl-C while `watch` owns the terminal, exits 0. There's no "session" to report as interrupted.

## Package layout

- `cmd/prq/watch.go` — command wiring, flag parsing, top-level loop.
- Read queries live as new methods on the existing `internal/queue` package (the one `review` already reads through) rather than a new package — one place that knows how to turn DB rows into PR/repo status, used by both `review` and `watch`, so the status-derivation logic in this doc has exactly one implementation, not a copy.
- No new `internal/watch` package needed unless the TUI rendering code itself gets large enough to want its own home; start with it inside `cmd/prq`.

## Incremental delivery

0. **Prerequisite, not a phase of this feature: `prq review` must exist and its invocation form (`prq review OWNER/NAME#N`) must be stable.** `watch` phase 1 cannot start before this.
1. **Static dashboard.** Repo list + PR list screens, manual refresh only (redraw after returning from a child process, no timer). Review and publish-recovery hand-offs both wired up, including selection-by-identity and child-failure visibility from the start — these aren't polish, they're correctness for the one interactive action this screen exists to trigger. This alone is usable — it's already better than running `prq status` by hand and remembering which PR you were mid-triage on.
2. **Live refresh.** Add the poll timer and diff-based redraw, including identity-preserving reselection across ticks.
3. **Nothing deferred to a phase 3 anymore** — publish recovery moved into phase 1 once it became clear `prepared`/`sending` need the same handling as `uncertain`; there's no smaller slice of "recovery" left to ship separately.

Ship (1) first and use it for real before building (2) — same reasoning as everything else in this project: let actual use tell you whether live-refresh polish is worth it before spending time on it.

## Open questions

None load-bearing. The one worth a second look once you've used it a while: whether the repo list's sort (attention-needed pinned to top) is actually the ordering you want, or whether you'd rather it stay in a fixed config-file order so repos don't visually jump around. Easy to flip later; not worth guessing now.
