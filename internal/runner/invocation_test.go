package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DustinVK/pr-queue/internal/findings"
)

func invocationFixture() (findings.Input, findings.Comparison) {
	input := findings.Input{Repo: "owner/repo", PR: 17, HeadSHA: strings.Repeat("a", 40)}
	return input, findings.Comparison{HeadSHA: input.HeadSHA, BaseSHA: strings.Repeat("b", 40), State: "open"}
}

func TestBuildCodexInvocation(t *testing.T) {
	input, comparison := invocationFixture()
	paths := InvocationPaths{Output: "/private/run/findings.json", Diff: "/private/run/comparison.diff", Scratch: "/private/run/scratch with space", Schema: "/private/run/findings-schema.json"}
	got, err := BuildInvocation("codex", "run-id", input, comparison, paths)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--ask-for-approval", "never", "exec", "--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=false", "-c", "sandbox_workspace_write.writable_roots=[]", "--add-dir", paths.Scratch, "--ephemeral", "--color", "never", "--json", "--output-schema", paths.Schema, "--output-last-message", paths.Output, "-"}
	if !reflect.DeepEqual(got.Args, want) {
		t.Fatalf("args = %#v, want %#v", got.Args, want)
	}
	if !strings.Contains(got.Prompt, `The supplied current PR diff is in: "/private/run/comparison.diff"`) {
		t.Fatal("prompt lost quoted diff path")
	}
	if !strings.Contains(got.Prompt, "do not write the findings file yourself") || strings.Contains(got.Prompt, "Write exactly one JSON document to this absolute file path") {
		t.Fatal("prompt has wrong Codex delivery instructions")
	}
	if !strings.Contains(got.Prompt, `temporary output: "/private/run/scratch with space"`) {
		t.Fatal("prompt lost exact quoted scratch path")
	}
}

func TestBuildClaudeInvocationCompatibility(t *testing.T) {
	input, comparison := invocationFixture()
	got, err := BuildInvocation("claude", "123", input, comparison, InvocationPaths{Output: "/out", Diff: "/diff"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-p", "--verbose", "--output-format", "stream-json", "--no-session-persistence", "--session-id", "123", "--dangerously-skip-permissions"}
	if !reflect.DeepEqual(got.Args, want) {
		t.Fatalf("args = %#v", got.Args)
	}
	if !strings.Contains(got.Prompt, `Write exactly one JSON document to this absolute file path: "/out"`) {
		t.Fatal("prompt lost Claude file delivery")
	}
}

func TestOutputSchemaFindingVariantsAndIdentity(t *testing.T) {
	input, _ := invocationFixture()
	data, err := OutputSchema(input)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]any)
	if got := properties["repo"].(map[string]any)["enum"].([]any)[0]; got != input.Repo {
		t.Fatalf("repo enum = %v", got)
	}
	items := properties["findings"].(map[string]any)["items"].(map[string]any)
	variants := items["anyOf"].([]any)
	if len(variants) != 6 {
		t.Fatalf("variant count = %d", len(variants))
	}
	for i, raw := range variants {
		variant := raw.(map[string]any)
		if variant["additionalProperties"] != false {
			t.Fatalf("variant %d permits extra properties", i)
		}
		required := variant["required"].([]any)
		props := variant["properties"].(map[string]any)
		if len(required) != len(props) {
			t.Fatalf("variant %d has optional or unrequired properties", i)
		}
	}
}

func TestWriteOutputSchemaRejectsExistingFileAndKeepsPrivateMode(t *testing.T) {
	input, _ := invocationFixture()
	path := filepath.Join(t.TempDir(), "nested", "schema.json")
	if err := WriteOutputSchema(path, input); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	if err := WriteOutputSchema(path, input); err == nil {
		t.Fatal("expected existing-file error")
	}
}
