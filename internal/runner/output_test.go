package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeProviderOutputUsesStrictFindingsContract(t *testing.T) {
	input, _ := invocationFixture()
	valid := `{"schema_version":1,"repo":"owner/repo","pr":17,"head_sha":"` + input.HeadSHA + `","summary":"clear","verdict":"comment","findings":[]}`
	path := filepath.Join(t.TempDir(), "findings.json")
	if err := os.WriteFile(path, []byte(valid), 0644); err != nil {
		t.Fatal(err)
	}
	document, err := DecodeProviderOutput(path, input)
	if err != nil {
		t.Fatal(err)
	}
	if document.Summary != "clear" {
		t.Fatalf("summary = %q", document.Summary)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}

	invalid := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(invalid, []byte("```json\n"+valid+"\n```"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeProviderOutput(invalid, input); err == nil || !strings.Contains(err.Error(), "invalid findings JSON") {
		t.Fatalf("fenced output error = %v", err)
	}
}

func TestDecodeProviderOutputRejectsNonRegularAndMissing(t *testing.T) {
	input, _ := invocationFixture()
	dir := t.TempDir()
	if _, err := DecodeProviderOutput(dir, input); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory error = %v", err)
	}
	if _, err := DecodeProviderOutput(filepath.Join(dir, "missing"), input); err == nil || !strings.Contains(err.Error(), "did not write") {
		t.Fatalf("missing error = %v", err)
	}
}
