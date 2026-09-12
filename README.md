# prqueue

`prqueue` is a planned macOS CLI for drafting pull request reviews with Claude Code, triaging findings locally, and publishing the findings you explicitly approve. Its command is `prq`.

It runs once when invoked, reviews PRs in fresh Git worktrees, and keeps the review queue in SQLite. There is no server or daemon to deploy.

**Status:** specification and implementation planning. The `prq` executable is not implemented yet. The commands below describe the intended v1 workflow, not commands available in this checkout.

- [V1 specification](prqueue-spec.md): the source of truth for behavior.
- [Implementation plan](IMPLEMENTATION_PLAN.md): phases, validation, and review checkpoints.
- [Deferred work](FUTURE.md): features outside v1.

## Requirements

| Tool | Purpose | Requirement |
| --- | --- | --- |
| macOS | Supported v1 host | Required |
| Go | Build and test `prq` | Required for development; version will be pinned when the module is added |
| Git | Fetch PR revisions and create detached worktrees | Required |
| GitHub CLI (`gh`) | Access github.com as the configured account | Required, authenticated |
| Claude Code (`claude`) | Generate proposed reviews | Required, authenticated through its supported local login or environment |
| `terminal-notifier` | Local desktop notifications | Optional; falls back to macOS `osascript` |

No Docker or database server is required. SQLite uses the pure Go `modernc.org/sqlite` driver, so building the binary does not require a C toolchain. Development race checks do require one. Go module dependencies will be pinned during implementation.

Check the local tools and authentication without starting an agent review:

```sh
go version
git --version
gh --version
gh auth status
claude --version
claude auth status
command -v osascript
```

If authentication is missing, use `gh auth login --hostname github.com` or `claude auth login` as appropriate. `github.user` must match the authenticated GitHub account. `prq` does not manage model credentials.

## Intended workflow

Once implemented:

```sh
prq init
# Configure your GitHub login and repositories in:
# ~/.config/prqueue/config.yaml

prq run --repo owner/name
prq list --status pending
prq show owner/name#482
# Replace this with a finding UUID or an unambiguous 8-hex prefix from list:
finding_id=01234567
prq diff "$finding_id"
prq edit "$finding_id"
prq approve "$finding_id"
prq publish owner/name#482 --event COMMENT --dry-run
prq publish owner/name#482 --event COMMENT
```

`init` is safe to rerun and requires explicit acknowledgment of the agent's permissions before the first run. Configure repositories outside this checkout:

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
      authors: []
      requested_reviewer: null
      base_branches: []
```

The configuration lives at `~/.config/prqueue/config.yaml`; local state lives under `~/.local/state/prqueue/`. Keep credentials and actual repository configuration out of this repository.

`run` observes PR state before applying filters and skips an unchanged, successfully reviewed comparison. `run --repo owner/name --pr 482` forces a fresh review, including a draft, but never a closed or merged PR. Failed reviews remain eligible for retry.

Findings can be `pending`, `approved`, `rejected`, `published`, `obsolete`, or `blocked`. The agent's summary is a separate finding that needs its own approval. Invalid inline anchors remain visible as blocked findings; `edit --as-general` removes the anchor and requires approval of the resulting general finding.

All commands will support `--json`, with one result object on stdout and diagnostics elsewhere. Exit codes are `0` for success, `1` for a command/configuration failure or unresolved publication, `2` for a run with partial failures, and `3` for lock contention. See the [CLI reference](prqueue-spec.md#4-cli-reference) for the complete interface.

## Approval and publishing

The publisher sends only approved text and anchors verified against the current head, base, draft flag, and PR state. Editing a finding or observing a changed comparison clears its approval. Approval and publication require an open, non-draft PR and the configured GitHub identity.

Publication is one submitted GitHub review with an explicit human-selected event: `COMMENT`, `REQUEST_CHANGES`, or `APPROVE`. Titles, rationale, and the agent's verdict are private metadata. An approved summary and approved general findings form the review body; approved inline findings become individual comments. A recovery marker is appended to the body.

With no approved findings, `COMMENT` does nothing, `REQUEST_CHANGES` refuses, and `APPROVE` sends an explicit empty approval. Approving or requesting changes on your own PR is refused.

Publication snapshots are frozen before sending. If delivery is uncertain, a new publication is blocked until `prq publish owner/name#482 --resume` reconciles the original attempt. It never blindly resends a possibly submitted review. For an uncertain attempt, the `--confirmed-not-sent` recovery override requires manual inspection of the PR; the override sends nothing itself. See [publishing and crash recovery](prqueue-spec.md#7-publishing-and-crash-recovery).

`publish --dry-run` validates and previews the exact request without writing local or remote state. `run --dry-run` **does run the agent and can incur model cost**; the coordinator uses temporary files, persists no database changes, makes no GitHub writes, and sends no notification.

## Agent permissions

Claude Code runs as trusted local code with your full user permissions. A fresh worktree protects the working checkout from ordinary review activity; it is not a sandbox. The agent is instructed not to commit, push, or invoke `gh`, but it can access ambient GitHub authentication. The approval guarantee applies to `prqueue`'s publisher, not to actions a misbehaving agent might take independently, including during a dry run.

Docker isolation, a web UI, scheduling, other agents, and remote notifications are deferred. The existing [LaunchAgent example](com.dustinvk.prqueue.plist) is optional future scheduling material and is not installed by `init`.

## Development

Implementation follows the [phase plan](IMPLEMENTATION_PLAN.md). Each phase ends with validation, a review of the changes against the specification, fixes for any issues found, and a recorded result before the next phase begins.

The planned packages separate the CLI, configuration, findings validation, GitHub transport, SQLite store, agent runner, queue operations, and notification sink. This keeps the approval and publication rules testable without invoking a real agent or posting reviews. Build, test, and installation instructions will be added with the executable.
