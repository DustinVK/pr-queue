package queue

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
	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/DustinVK/pr-queue/internal/github"
	"github.com/DustinVK/pr-queue/internal/runner"
)

func codexQueueRepository(t *testing.T) (string, string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "source repo")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		all := append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null", "-c", "user.name=Test", "-c", "user.email=test@example.invalid"}, args...)
		out, err := exec.Command("git", all...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "value.txt"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "value.txt")
	run("commit", "-m", "base")
	base := run("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "value.txt"), []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "value.txt")
	run("commit", "-m", "head")
	return dir, base, run("rev-parse", "HEAD")
}

func codexQueueExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-codex")
	script := `#!/bin/sh
set -eu
output=
schema=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output-schema) shift; schema=$1 ;;
    --output-last-message) shift; output=$1 ;;
  esac
  shift
done
[ -f "$schema" ]
printf '%s\n' '{"type":"thread.started","thread_id":"queue-codex-thread"}'
case "${PRQ_QUEUE_CODEX_MODE:-success}" in
  invalid) printf '%s' 'not JSON' > "$output"; exit 0 ;;
  nonzero) printf '%s' "${PRQ_QUEUE_CODEX_DOCUMENT:?}" > "$output"; exit 19 ;;
esac
printf '%s' "${PRQ_QUEUE_CODEX_DOCUMENT:?}" > "$output"
`
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexRunnerCoordinatorPersistsCountsAnchorsAndProvenance(t *testing.T) {
	source, base, head := codexQueueRepository(t)
	document := findings.Document{
		SchemaVersion: 1, Repo: "owner/repo", PR: 1, HeadSHA: head, Summary: "Codex queue summary", Verdict: "comment",
		Findings: []findings.Finding{
			{Kind: "inline", Severity: "minor", Category: "correctness", Title: "Valid", Body: "Valid body", Anchor: findings.Anchor{Path: findings.Ptr("value.txt"), Side: findings.Ptr("RIGHT"), Line: findings.Ptr(1)}},
			{Kind: "inline", Severity: "major", Category: "correctness", Title: "Blocked", Body: "Blocked body", Anchor: findings.Anchor{Path: findings.Ptr("value.txt"), Side: findings.Ptr("RIGHT"), Line: findings.Ptr(3)}},
		},
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRQ_QUEUE_CODEX_DOCUMENT", string(data))
	s, state := queueStore(t)
	p := github.PR{Repo: "owner/repo", Number: 1, Author: "author", BaseBranch: "main", RequestedReviewers: []string{"reviewer"}, Comparison: findings.Comparison{HeadSHA: head, BaseSHA: base, State: "open"}}
	remote := &fakeRemote{open: []github.PR{p}, fetch: map[int]github.PR{1: p}}
	remote.diff.AddPatch("value.txt", "@@ -1 +1 @@\n-old\n+new\n")
	reviewer := runner.Runner{StateDir: state, Agent: config.Agent{Provider: config.ProviderCodex, Executable: codexQueueExecutable(t), Timeout: 10 * time.Second, MaxParallelReviews: 1}}
	c := Coordinator{Store: s, Remote: remote, Reviewer: reviewer, StateDir: state, WorkDir: state, Parallel: 1, Source: func(string) string { return source }}
	result, err := c.Run(context.Background(), []config.Repo{{Name: "owner/repo"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	review := result.Repos[0].Reviews[0]
	if review.Status != "succeeded" || result.Changed != 3 || review.Ingestion == nil || len(review.Ingestion.Findings) != 3 {
		t.Fatalf("result = %#v", result)
	}
	for i, want := range []string{"pending", "pending", "blocked"} {
		if got := review.Ingestion.Findings[i].Status; got != want {
			t.Fatalf("finding %d status = %q, want %q", i, got, want)
		}
	}
	metadataData, err := os.ReadFile(filepath.Join(filepath.Dir(review.OutputPath), runner.MetadataFilename))
	if err != nil {
		t.Fatal(err)
	}
	var metadata runner.AgentMetadata
	if err := json.Unmarshal(metadataData, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Provider != config.ProviderCodex || metadata.Observed == nil || metadata.Observed.Session != "queue-codex-thread" {
		t.Fatalf("metadata = %#v", metadata)
	}
	var storedStatus, storedHead, storedKey, storedOutput string
	if err := s.DB.QueryRowContext(t.Context(), "SELECT status,head_sha,comparison_key,raw_output_path FROM review_runs WHERE id=?", review.ID).Scan(&storedStatus, &storedHead, &storedKey, &storedOutput); err != nil {
		t.Fatal(err)
	}
	if storedStatus != "succeeded" || storedHead != head || storedKey != p.Key() || storedOutput != review.OutputPath {
		t.Fatalf("stored run = status %q head %q key %q output %q", storedStatus, storedHead, storedKey, storedOutput)
	}
	logPath := filepath.Join(filepath.Dir(review.OutputPath), "agent.jsonl")
	if log, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(log), "queue-codex-thread") {
		t.Fatalf("stored diagnostics = %q, %v", log, err)
	}
	wantDiff := "File: \"value.txt\"\n@@ -1 +1 @@\n-old\n+new\n\n"
	if diff, err := os.ReadFile(filepath.Join(filepath.Dir(review.OutputPath), "comparison.diff")); err != nil || string(diff) != wantDiff {
		t.Fatalf("comparison diagnostics = %q, %v", diff, err)
	}
	var cursor string
	if err := s.DB.QueryRowContext(t.Context(), "SELECT last_reviewed_key FROM pull_requests WHERE repo=? AND number=?", p.Repo, p.Number).Scan(&cursor); err != nil || cursor != p.Key() {
		t.Fatalf("cursor = %q, %v", cursor, err)
	}
	var findingCount int
	if err := s.DB.QueryRowContext(t.Context(), "SELECT count(*) FROM findings").Scan(&findingCount); err != nil || findingCount != 3 {
		t.Fatalf("finding count = %d, %v", findingCount, err)
	}
	for _, mode := range []string{"invalid", "nonzero"} {
		t.Setenv("PRQ_QUEUE_CODEX_MODE", mode)
		failed, err := c.Run(context.Background(), []config.Repo{{Name: "owner/repo"}}, 1)
		if err == nil || failed.Failed != 1 || len(failed.Repos[0].Reviews) != 1 || failed.Repos[0].Reviews[0].Status != "failed" {
			t.Fatalf("forced %s result = %#v, %v", mode, failed, err)
		}
		var gotCursor string
		if err := s.DB.QueryRowContext(t.Context(), "SELECT last_reviewed_key FROM pull_requests WHERE repo=? AND number=?", p.Repo, p.Number).Scan(&gotCursor); err != nil || gotCursor != cursor {
			t.Fatalf("cursor after %s = %q, %v", mode, gotCursor, err)
		}
		var gotCount int
		if err := s.DB.QueryRowContext(t.Context(), "SELECT count(*) FROM findings").Scan(&gotCount); err != nil || gotCount != findingCount {
			t.Fatalf("findings after %s = %d, %v", mode, gotCount, err)
		}
	}
}
