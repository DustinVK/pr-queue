package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/DustinVK/pr-queue/internal/config"
)

func TestAgentMetadataLifecycleAndPrivateWrite(t *testing.T) {
	input, comparison := invocationFixture()
	m := PreparedMetadata("run-1", input, comparison, "codex", "wrapper path", "/resolved/wrapper path", []string{"exec", "-"})
	if m.State != "prepared" || m.ReleasedAt != nil || m.FinishedAt != nil || m.Observed != nil {
		t.Fatalf("unexpected prepared metadata: %#v", m)
	}
	path := filepath.Join(t.TempDir(), "run", MetadataFilename)
	if err := WritePreparedAgentMetadata(path, m); err != nil {
		t.Fatal(err)
	}
	if err := WritePreparedAgentMetadata(path, m); err == nil {
		t.Fatal("expected existing prepared metadata error")
	}
	m.MarkReleased()
	if m.State != "released" || m.ReleasedAt == nil || m.FinishedAt != nil {
		t.Fatalf("unexpected released metadata: %#v", m)
	}
	m.MarkFinished(&ObservedIdentity{CLI: "codex", Model: "observed-model", Session: "thread-id"})
	if m.State != "process_finished" {
		t.Fatalf("finished state overclaims execution: %q", m.State)
	}
	if err := WriteAgentMetadata(path, m); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got AgentMetadata
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ConfiguredExecutable != "wrapper path" || got.ResolvedExecutable != "/resolved/wrapper path" || got.ModelSelection != "inherited" || got.Observed == nil || got.Observed.Model != "observed-model" {
		t.Fatalf("metadata = %#v", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestObservedIdentityReaderIsBounded(t *testing.T) {
	line := []byte("{\"type\":\"other\"}\n")
	tooLate := bytes.Repeat(line, maxObservedIdentityBytes/len(line)+1)
	tooLate = append(tooLate, []byte("{\"type\":\"thread.started\",\"thread_id\":\"too-late\"}\n")...)
	if got := ObservedIdentityFromJSONLReader(config.ProviderCodex, bytes.NewReader(tooLate)); got != nil {
		t.Fatalf("identity beyond diagnostic bound = %#v", got)
	}
}

func TestObservedIdentityFromJSONLUsesAllowlistedEvents(t *testing.T) {
	codex := ObservedIdentityFromJSONL("codex", []byte("{\"type\":\"thread.started\",\"thread_id\":\"thread-1\",\"model\":\"untrusted-placement\"}\n"))
	if codex == nil || codex.Session != "thread-1" || codex.Model != "" || codex.CLI != "" {
		t.Fatalf("codex identity = %#v", codex)
	}
	claude := ObservedIdentityFromJSONL("claude", []byte("{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"session-1\",\"model\":\"claude-model\"}\n"))
	if claude == nil || claude.Session != "session-1" || claude.Model != "claude-model" {
		t.Fatalf("claude identity = %#v", claude)
	}
	for _, data := range [][]byte{[]byte("not json\n"), []byte("{\"type\":\"thread.started\",\"session_id\":\"wrong-field\"}\n"), []byte("{\"type\":\"other\",\"thread_id\":\"wrong-event\"}\n")} {
		if got := ObservedIdentityFromJSONL("codex", data); got != nil {
			t.Fatalf("unexpected identity from %q: %#v", data, got)
		}
	}
}
