# Codex runner validation

Codex support uses the existing runner, strict findings decoder, coordinator, and store. Ordinary tests use local Git repositories and fake executables; they make no model requests or GitHub writes.

## Deliberate local smoke check

`TestManualCodexSmoke` is a separate opt-in check using a synthetic local repository and PR identity. It invokes the installed Codex CLI through `Runner.Review`, with the production prompt, output schema, launch gate, and two-minute agent timeout. It inherits Codex's model configuration and saved authentication. It does not activate the historical Claude or GitHub manual tests.

To deliberately invoke Codex (this can incur model cost):

```sh
codex_state=$(mktemp -d)
PRQ_CODEX_REAL_SMOKE=1 PRQ_CODEX_SMOKE_STATE="$codex_state" \
  go test ./internal/runner -run '^TestManualCodexSmoke$' -v -count=1 -timeout 3m
```

The diagnostics directory must be absolute. Inspect the reported `findings.json`, `agent.jsonl`, `agent-stderr.log`, `agent-metadata.json`, and saved Git state afterward. The final response file is authoritative; a transcript containing plausible findings cannot rescue a missing or invalid final file, a failed process, or a timeout.

Codex receives `--ask-for-approval never exec --sandbox workspace-write`, explicit command-network denial and cleared inherited extra writable roots, the per-run scratch grant, `--ephemeral --color never --json`, and the schema/final output paths. The prompt arrives on stdin. No model, profile, hook-trust bypass, or provider fallback is supplied. The native command sandbox permits the checkout, scratch, and standard temporary roots; it does not isolate the entire CLI, configured tools, or model connection.

Metadata distinguishes preparation, gate release, and process completion. These states alone do not prove provider execution. Inspect observed session evidence, the validated final file, and the review result together. A model or CLI version not reported in the event stream remains unknown in per-run metadata; record the separately checked installed CLI version alongside compatibility evidence.

## Validation record

The local compatibility check passed on 2026-09-12 with `/opt/homebrew/bin/codex`, reporting `codex-cli 0.154.0`, and an existing saved ChatGPT login. One model call ran for 25.03 seconds under the two-minute deadline. The configured model was inherited (`gpt-6-astra`, `xhigh` reasoning); the event stream reported a session identity but no observed model or CLI version, so those metadata fields remained absent.

The installed backend accepted the full six-variant findings schema. The CLI consumed stdin, executed eight local inspection commands, and wrote a valid final document with the exact synthetic identity, a summary, and an empty findings array. The normal strict decoder accepted it. JSONL and final output stayed separate; stderr was empty. All retained artifact modes passed, the source and reviewed HEAD remained unchanged, saved refs matched, worktree status was empty, and the runner removed its worktree and ownership record.

A separate local `codex debug prompt-input` inspection with the same permission overrides rendered approval `never`, restricted network, and only the checkout, scratch, and standard macOS temporary writable roots. This establishes effective configuration; the smoke did not attempt an external connection or a denied write. The fixture had no test suite or dependencies to install. This check establishes invocation and output compatibility, not review quality or whole-process isolation.

Final offline package and CLI validation is recorded with the implementation delivery.
