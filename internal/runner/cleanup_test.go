package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupContinuesAfterIndependentEntryFailures(t *testing.T) {
	r := Runner{StateDir: t.TempDir()}
	base := filepath.Join(r.StateDir, "worktrees")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	badIDs := []string{"00000000-0000-4000-8000-000000000000", "11111111-1111-4111-8111-111111111111"}
	for i, id := range badIDs {
		body := []string{"not JSON", `{}`}[i]
		if err := os.WriteFile(ownerPath(filepath.Join(base, id)), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	id := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	root := filepath.Join(base, id)
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	o, err := newOwner(id)
	if err != nil {
		t.Fatal(err)
	}
	o.Started = "previous owner with the same PID"
	if err := saveOwner(root, o); err != nil {
		t.Fatal(err)
	}
	cleaned, err := r.Cleanup(t.Context())
	if err == nil || len(cleaned) != 1 || cleaned[0] != id {
		t.Fatalf("independent cleanup: %v %v", cleaned, err)
	}
	for _, badID := range badIDs {
		if !strings.Contains(err.Error(), badID) {
			t.Fatalf("missing failure context for %s: %v", badID, err)
		}
		if _, err := os.Stat(ownerPath(filepath.Join(base, badID))); err != nil {
			t.Fatal("invalid ownership must remain available for diagnosis:", err)
		}
	}
	for _, path := range []string{root, ownerPath(root)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("valid stale entry remains at %s: %v", path, err)
		}
	}
}
