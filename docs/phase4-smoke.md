# Phase 4 real-Claude smoke check

Passed on 2026-09-12 with Claude Code 2.1.236 on macOS arm64.

`TestManualClaudeSmoke` invoked the actual `internal/runner.Runner.Review` with its production prompt, arguments, output path, and default 15-minute timeout. The source was a disposable local Git repository containing two commits that changed `value.txt` from `old` to `new`. The PR metadata was synthetic (`owner/repo#1`); no GitHub repository or PR was needed.

- Runner duration: 29.22 seconds, within timeout.
- Run ID: `5ae1dcf8-b062-40e4-b7d3-bc9c60ff1c60`.
- The normal strict findings decoder accepted the output with matching repo, PR, and head SHA, a summary, and zero findings.
- Reviewed all five tool calls in the saved stream transcript. They read the diff and Git state, then wrote and parsed `findings.json`. No attempted commit, push, `gh` invocation, or delegation appeared. A failed macOS `cat -A` call was retried with `cat -v`.
- Source and reviewed HEADs remained at the captured commit. Saved refs before and after were identical; saved worktree status was empty. The runner removed the worktree and ownership metadata. Agent stderr was empty.

Raw diagnostics are retained locally in the temporary state directory printed by the manual test. They are not added to this repository. This check validates CLI plumbing, not review quality or an enforced permissions boundary.

To deliberately repeat this check (it invokes Claude and can incur model cost):

```sh
smoke_state=$(mktemp -d)
PRQ_RUNNER_REAL_SMOKE=1 PRQ_SMOKE_STATE="$smoke_state" \
  go test ./internal/runner -run '^TestManualClaudeSmoke$' -v -count=1
```

Inspect the printed diagnostic paths and transcript manually afterward. Ordinary test runs skip this check.
