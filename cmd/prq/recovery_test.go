package main

import (
	"bytes"
	"fmt"
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

func TestRunCleanupFailureStillFailsInterruptedRuns(t *testing.T) {
	a, _, out := runFixture(t)
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
	if err := os.WriteFile(filepath.Join(owners, uuid.NewString()+".owner.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := a.run(t.Context(), []string{"run", "--json"}); code != 1 {
		t.Fatalf("cleanup failure did not refuse run: code=%d stdout=%s", code, out.String())
	}
	s, err = store.OpenReadOnly(t.Context(), a.paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var status string
	if err := s.DB.QueryRow("SELECT status FROM review_runs WHERE id=?", r.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("interrupted run remained %q after cleanup failure", status)
	}
}

func TestRejectReasonNamedDryRunDoesNotSkipRecovery(t *testing.T) {
	a, _, out := runFixture(t)
	if code := a.run(t.Context(), []string{"run", "--json"}); code != 0 {
		t.Fatalf("run: code=%d stdout=%s", code, out.String())
	}
	s, err := store.Open(t.Context(), a.paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	items, err := s.ListFindings(t.Context(), "", "", 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("findings: %v %+v", err, items)
	}
	p, err := s.PR(t.Context(), "owner/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.StartRun(t.Context(), uuid.NewString(), p, "interrupted-output")
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := a.run(t.Context(), []string{"reject", items[0].ID, "--reason", "--dry-run", "--json"}); code != 0 {
		t.Fatalf("reject: code=%d stdout=%s", code, out.String())
	}
	var status string
	if err := s.DB.QueryRow("SELECT status FROM review_runs WHERE id=?", r.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("reason named --dry-run left interrupted run %q", status)
	}
}

func TestCommandsCleanOrphansWithoutDatabase(t *testing.T) {
	for _, args := range [][]string{{"status", "--json"}, {"run", "--json"}} {
		t.Run(args[0], func(t *testing.T) {
			a, _, out := runFixture(t)
			id := uuid.NewString()
			root := filepath.Join(a.paths.State, "worktrees", id)
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			owner := filepath.Join(a.paths.State, "worktrees", id+".owner.json")
			body := fmt.Sprintf(`{"version":2,"id":%q,"pid":%d,"started":"previous coordinator"}`, id, os.Getpid())
			if err := os.WriteFile(owner, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(a.paths.Database, a.paths.Database+".missing"); err != nil {
				t.Fatal(err)
			}
			if code := a.run(t.Context(), args); code != 1 {
				t.Fatalf("missing database: code=%d stdout=%s", code, out.String())
			}
			if _, err := os.Stat(owner); !os.IsNotExist(err) {
				t.Fatalf("orphan owner remains without database: %v", err)
			}
		})
	}
}
