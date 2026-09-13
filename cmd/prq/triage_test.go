package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/DustinVK/pr-queue/internal/lock"
	"github.com/DustinVK/pr-queue/internal/store"
)

func TestCLITriageJSONEditorAndReasonFlags(t *testing.T) {
	a, _, out := runFixture(t)
	if code := a.run(t.Context(), []string{"run", "--json"}); code != 0 {
		t.Fatalf("run: %d %s", code, out)
	}
	s, err := store.Open(t.Context(), a.paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fs, err := s.ListFindings(t.Context(), "", "", 0)
	if err != nil || len(fs) != 1 {
		t.Fatalf("findings: %v %v", fs, err)
	}
	id := fs[0].ID[:8]
	out.Reset()
	if code := a.run(t.Context(), []string{"reject", id, "--reason", "--json", "--json"}); code != 0 {
		t.Fatalf("reject: %d %s", code, out)
	}
	var body string
	if err := s.DB.QueryRow("SELECT body_snapshot FROM audit_log WHERE action='rejected'").Scan(&body); err != nil {
		t.Fatal(err)
	}
	var rejected struct {
		Body   string `json:"body"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(body), &rejected); err != nil || rejected.Reason != "--json" || rejected.Body != fs[0].Body {
		t.Fatalf("reason corrupted: %s %v", body, err)
	}
	out.Reset()
	if code := a.run(t.Context(), []string{"approve", id, "--json"}); code != 0 {
		t.Fatalf("approve: %d %s", code, out)
	}
	path := filepath.Join(t.TempDir(), "fake editor")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf 'edited summary\\n' > \"$1\"\nprintf 'editor diagnostic\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", strconv.Quote(path))
	out.Reset()
	if code := a.run(t.Context(), []string{"edit", id, "--json"}); code != 0 {
		t.Fatalf("edit: %d %s", code, out)
	}
	var response struct {
		OK   bool `json:"ok"`
		Data struct {
			Changed bool          `json:"changed"`
			Finding store.Finding `json:"finding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil || !response.OK || !response.Data.Changed || response.Data.Finding.Status != "pending" || response.Data.Finding.Body != "edited summary\n" {
		t.Fatalf("edit output: %s %v", out, err)
	}
	if strings.Contains(out.String(), "editor diagnostic") {
		t.Fatal("editor contaminated JSON stdout")
	}
	t.Setenv("EDITOR", "/usr/bin/false")
	out.Reset()
	if code := a.run(t.Context(), []string{"edit", id}); code != 1 || out.Len() != 0 {
		t.Fatalf("failed edit printed a finding: %d %q", code, out.String())
	}
	l, err := lock.Acquire(lock.PRPath(a.paths.State, "owner/repo", 1), "edit held")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	out.Reset()
	if code := a.run(t.Context(), []string{"approve", id, "--json"}); code != 3 {
		t.Fatalf("contention: %d %s", code, out)
	}
}

func TestParseTriageRejectsUnknownTrailingOptions(t *testing.T) {
	a, _, out := runFixture(t)
	for _, args := range [][]string{{"edit", "12345678", "--wat"}, {"approve", "12345678", "--reason", "why"}, {"edit", "12345678", "87654321"}, {"reject", "--reason"}} {
		out.Reset()
		if code := a.run(t.Context(), append(args, "--json")); code != 1 {
			t.Fatalf("args %v: %d %s", args, code, out)
		}
	}
}
