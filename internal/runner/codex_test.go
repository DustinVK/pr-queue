package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DustinVK/pr-queue/internal/config"
)

func fakeCodexExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake codex")
	script := `#!/bin/sh
set -eu
schema=
output=
args_file="${PRQ_CODEX_ARGS_FILE:?}"
: > "$args_file"
while [ "$#" -gt 0 ]; do
  printf '%s\n' "$1" >> "$args_file"
  case "$1" in
    --output-schema) shift; schema=$1; printf '%s\n' "$1" >> "$args_file" ;;
    --output-last-message) shift; output=$1; printf '%s\n' "$1" >> "$args_file" ;;
  esac
  shift
done
[ -f "$schema" ]
prompt=$(sed -n '1,$p')
printf '%s' "$prompt" > "${PRQ_CODEX_PROMPT_FILE:?}"
printf '%s\n' '{"type":"thread.started","thread_id":"codex-thread-17"}'
printf '%s\n' 'separate stderr' >&2
case "${PRQ_CODEX_MODE:-success}" in
  missing) exit 0 ;;
  invalid) printf '%s' 'not JSON' > "$output"; exit 0 ;;
  nonzero) printf '%s' "${PRQ_CODEX_DOCUMENT:?}" > "$output"; exit 23 ;;
esac
printf '%s' "${PRQ_CODEX_DOCUMENT:?}" > "$output"
`
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// This is an explicit opt-in paid integration check, never an ordinary test.
func TestManualCodexSmoke(t *testing.T) {
	if os.Getenv("PRQ_RUNNER_REAL_CODEX_SMOKE") != "1" {
		t.Skip("set PRQ_RUNNER_REAL_CODEX_SMOKE=1 and PRQ_CODEX_SMOKE_STATE to run the paid local Codex smoke check")
	}
	state := os.Getenv("PRQ_CODEX_SMOKE_STATE")
	if !filepath.IsAbs(state) {
		t.Fatal("PRQ_CODEX_SMOKE_STATE must be an absolute private diagnostics directory")
	}
	executable := os.Getenv("PRQ_CODEX_SMOKE_EXECUTABLE")
	if executable == "" {
		executable = config.ProviderCodex
	}
	req, source := localFixture(t)
	r := Runner{StateDir: state, Agent: config.Agent{Provider: config.ProviderCodex, Executable: executable, Timeout: 2 * time.Minute, MaxParallelReviews: 1}}
	started := time.Now()
	result, err := r.Review(t.Context(), req)
	t.Logf("duration=%s output=%s log=%s metadata=%s", time.Since(started), result.OutputPath, result.LogPath, filepath.Join(filepath.Dir(result.OutputPath), MetadataFilename))
	if err != nil {
		t.Fatal(err)
	}
	if result.TimedOut {
		t.Fatal("real Codex agent timed out")
	}
	metadataData, err := os.ReadFile(filepath.Join(filepath.Dir(result.OutputPath), MetadataFilename))
	if err != nil {
		t.Fatal(err)
	}
	var metadata AgentMetadata
	if err := json.Unmarshal(metadataData, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Provider != config.ProviderCodex || metadata.State != "process_finished" || metadata.Observed == nil || metadata.Observed.Session == "" {
		t.Fatalf("Codex execution provenance incomplete: %#v", metadata)
	}
	head, err := exec.Command("git", "-C", source, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(head)) != req.Input.HeadSHA {
		t.Fatal("fixture source changed")
	}
}

func codexRunner(t *testing.T, req Request, mode string) Runner {
	t.Helper()
	document := map[string]any{
		"schema_version": 1, "repo": req.Input.Repo, "pr": req.Input.PR, "head_sha": req.Input.HeadSHA,
		"summary": "Codex synthetic review", "verdict": "comment",
		"findings": []any{
			map[string]any{"kind": "general", "severity": "minor", "category": "test-coverage", "title": "General", "body": "Add coverage."},
			map[string]any{"kind": "inline", "severity": "major", "category": "correctness", "title": "Inline", "body": "This line is wrong.", "path": "value.txt", "side": "RIGHT", "line": 1},
		},
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRQ_CODEX_MODE", mode)
	t.Setenv("PRQ_CODEX_DOCUMENT", string(data))
	t.Setenv("PRQ_CODEX_ARGS_FILE", filepath.Join(t.TempDir(), "args"))
	t.Setenv("PRQ_CODEX_PROMPT_FILE", filepath.Join(t.TempDir(), "prompt"))
	return Runner{StateDir: t.TempDir(), Agent: config.Agent{Provider: config.ProviderCodex, Executable: fakeCodexExecutable(t), Timeout: 10 * time.Second, MaxParallelReviews: 1}}
}

func TestCodexRunnerUsesAuthoritativeFinalFileAndSeparateDiagnostics(t *testing.T) {
	req, _ := localFixture(t)
	r := codexRunner(t, req, "success")
	result, err := r.Review(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Document.Repo != req.Input.Repo || result.Document.HeadSHA != req.Input.HeadSHA || len(result.Document.Findings) != 2 {
		t.Fatalf("document = %#v", result.Document)
	}
	diagnostics := filepath.Dir(result.OutputPath)
	stdout, err := os.ReadFile(result.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stdout), `"type":"thread.started"`) || strings.Contains(string(stdout), "Codex synthetic review") {
		t.Fatalf("JSONL mixed with final output: %s", stdout)
	}
	stderr, err := os.ReadFile(filepath.Join(diagnostics, "agent-stderr.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stderr) != "separate stderr\n" {
		t.Fatalf("stderr = %q", stderr)
	}
	final, err := os.ReadFile(result.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(final), "Codex synthetic review") {
		t.Fatalf("final = %s", final)
	}
	argsData, err := os.ReadFile(os.Getenv("PRQ_CODEX_ARGS_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(argsData)), "\n")
	wantOutput := []string{"--output-last-message", result.OutputPath}
	wantSchema := []string{"--output-schema", filepath.Join(diagnostics, "findings-schema.json")}
	if !containsAdjacent(args, wantOutput...) || !containsAdjacent(args, wantSchema...) || !containsAdjacent(args, "--ask-for-approval", "never") || !containsAdjacent(args, "--sandbox", "workspace-write") {
		t.Fatalf("Codex argv missing required values: %#v", args)
	}
	metadataData, err := os.ReadFile(filepath.Join(diagnostics, MetadataFilename))
	if err != nil {
		t.Fatal(err)
	}
	var metadata AgentMetadata
	if err := json.Unmarshal(metadataData, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Provider != config.ProviderCodex || metadata.ConfiguredExecutable != r.Agent.Executable || metadata.ResolvedExecutable == "" || metadata.State != "process_finished" || metadata.Observed == nil || metadata.Observed.Session != "codex-thread-17" {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func containsAdjacent(values []string, pair ...string) bool {
	for i := 0; i+len(pair) <= len(values); i++ {
		match := true
		for j := range pair {
			match = match && values[i+j] == pair[j]
		}
		if match {
			return true
		}
	}
	return false
}

func TestCodexRunnerRejectsFailedOrBrokenFinalOutput(t *testing.T) {
	for _, mode := range []string{"invalid", "missing", "nonzero"} {
		t.Run(mode, func(t *testing.T) {
			req, _ := localFixture(t)
			r := codexRunner(t, req, mode)
			result, err := r.Review(context.Background(), req)
			if err == nil {
				t.Fatalf("accepted %s output: %#v", mode, result)
			}
			if mode == "nonzero" {
				if _, statErr := os.Stat(result.OutputPath); statErr != nil {
					t.Fatalf("raw failed output was not retained: %v", statErr)
				}
			}
		})
	}
}
