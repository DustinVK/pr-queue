# prqueue — deferred work

Deferred work for [the v1 spec](prqueue-spec.md), with the reason each item is outside the current scope. Nothing here gates reviewing a real PR with v1.

**Whole-process isolation.** Credential separation, resource limits, and restricted mounts for the entire agent remain deferred. Codex uses its native command sandbox; Claude retains full-permission execution. Both remain trusted local agents, and prqueue enforces approval only in its own publisher; removing an environment token does not remove ambient `gh` authentication. An isolated runner would need to keep host credentials out of the agent's reach. Revisit when reviewing untrusted contributions becomes a real use case.

**Staged/pending GitHub reviews.** Creating a review without an `event`, editable in GitHub's own UI before someone submits it. Genuinely useful, but it reopens exactly the reconciliation problem v1 sidesteps by publishing in one shot (does the staged draft's comment set match what's currently approved locally, who owns edits made to it from GitHub's side). Worth it once triage volume makes "look it over in the GitHub UI before submitting" a real workflow, not before.

**Automated draft policy** (severity/category selection or staging). Revisit once a regular review cadence makes it useful. Unattended submission would change the human-approval promise and needs a separate product decision; a pending review alone does not provide human approval.

**Additional publication states and repair commands.** V1 already recovers `prepared`, treats interrupted `sending` as uncertain, blocks fresh publications until resolution, and preserves later edits when recording an older snapshot as sent. It uses the four existing states plus `uncertain`; no extra state machine is needed for those cases. Unexpected remote edits, marker conflicts, and duplicates after an incorrect manual `--confirmed-not-sent` remain operator investigations. Manual repair may be necessary; preserve frozen snapshots and audit evidence. Add formal repair workflows only after actual use shows the need.

**Generation counters / event-history reconstruction.** V1 records unfiltered observations, clears `last_reviewed_key` and approval on an observed comparison change, then applies review filters. That handles observed draft and close/reopen transitions without counters. Transitions entirely between polls are not reconstructed; revisit only if missing those transitions matters in practice.

**Cross-machine notification** (ntfy, Slack DM). Needed the moment execution moves off the machine you're sitting at — `osascript` has no session to reach on a headless box. Swap the `notify.Sink` implementation; nothing else changes, because `internal/notify` was designed as one interface from the start.

**Web UI.** A thin layer over `internal/queue` once CLI friction is actually felt rather than assumed in advance.

**Scheduling** (launchd on the Mac, a timer on a future headless host). Wrap the one-shot binary after manual operation works; the existing `com.dustinvk.prqueue.plist` is an optional example. Headless deployment also needs a supported notification path.

**Multi-agent support** (local-model escalation, provider failover). Explicit selection of Claude or Codex is supported. Combining reviewers and automatic fallback remain deferred pending experience with their findings on real PRs — see §14 of [the v1 spec](prqueue-spec.md).

**The 40-scenario acceptance matrix from the full-rigor draft.** Most of those scenarios are real and worth having *eventually* — but as regression tests written against working code, not as a spec to satisfy before code exists. §13 of the v1 spec keeps the eight that protect the core guarantee; the rest can come back as tests once there's something to test.
