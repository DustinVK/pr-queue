package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/DustinVK/pr-queue/internal/store"
	"github.com/google/uuid"
)

func TestRecoveryWarningDoesNotDisableOtherCommands(t *testing.T) {
	a, _, out := runFixture(t)
	var diagnostics bytes.Buffer
	a.errOut = &diagnostics
	s, err := store.Open(t.Context(), a.paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.Observe(t.Context(), "owner/repo", 1, a.remote.(runRemote).pr.Comparison, "test")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.StartRun(t.Context(), uuid.NewString(), p, "interrupted-output")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	owners := filepath.Join(a.paths.State, "worktrees")
	if err := os.MkdirAll(owners, 0700); err != nil {
		t.Fatal(err)
	}
	owner := filepath.Join(owners, uuid.NewString()+".owner.json")
	if err := os.WriteFile(owner, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := a.run(t.Context(), []string{"status", "--json"}); code != 0 {
		t.Fatalf("orphan warning disabled status: code=%d stdout=%s stderr=%s", code, out.String(), diagnostics.String())
	}
	if !bytes.Contains(diagnostics.Bytes(), []byte("Startup recovery warning:")) {
		t.Fatalf("orphan cleanup failure was not reported: %s", diagnostics.String())
	}
	s, err = store.OpenReadOnly(t.Context(), a.paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	var status string
	err = s.DB.QueryRow("SELECT status FROM review_runs WHERE id=?", r.ID).Scan(&status)
	s.Close()
	if err != nil || status != "failed" {
		t.Fatalf("interrupted run was not failed despite cleanup warning: %q %v", status, err)
	}
	out.Reset()
	diagnostics.Reset()
	if code := a.run(t.Context(), []string{"run", "--json"}); code != 1 {
		t.Fatalf("orphan cleanup did not refuse a new review: code=%d stdout=%s stderr=%s", code, out.String(), diagnostics.String())
	}
}
