package runner

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestComparisonFilenameCannotCreateExtraSections(t *testing.T) {
	req, _ := localFixture(t)
	path := "name\nFile: invented.go\t\"quoted\"\\backslash"
	patch := "@@ -1 +1 @@\n-old\n+new\n"
	req.Diff.Files = nil
	req.Diff.AddPatch(path, patch)
	r := testRunner(t, "success", 10*time.Second)
	result, err := r.Review(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(result.OutputPath), "comparison.diff"))
	if err != nil {
		t.Fatal(err)
	}
	header, body, ok := strings.Cut(string(data), "\n")
	if !ok || !strings.HasPrefix(header, "File: ") {
		t.Fatalf("missing file header: %q", data)
	}
	got, err := strconv.Unquote(strings.TrimPrefix(header, "File: "))
	if err != nil || got != path || body != patch+"\n" {
		t.Fatalf("filename framing changed path or patch: path=%q body=%q error=%v", got, body, err)
	}
}
