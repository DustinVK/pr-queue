package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/queue"
	"github.com/DustinVK/pr-queue/internal/store"
)

// Explicitly opt in with a deliberately selected PR. This invokes the real
// Claude and GitHub executables, keeps a separate local queue, and never publishes.
func TestManualGitHubReviewTrial(t *testing.T) {
	if os.Getenv("PRQ_MANUAL_GITHUB_TRIAL") != "1" {
		t.Skip("manual real-GitHub/Claude review trial")
	}
	root := os.Getenv("PRQ_MANUAL_STATE")
	repo := os.Getenv("PRQ_MANUAL_REPO")
	user := os.Getenv("PRQ_MANUAL_USER")
	pr, err := strconv.Atoi(os.Getenv("PRQ_MANUAL_PR"))
	if err != nil || pr < 1 || !filepath.IsAbs(root) || !config.ValidLogin(user) {
		t.Fatal("set absolute PRQ_MANUAL_STATE, PRQ_MANUAL_REPO, PRQ_MANUAL_PR, and PRQ_MANUAL_USER")
	}
	if err := config.ValidateRepo(repo); err != nil {
		t.Fatal(err)
	}
	paths := config.ForHome(root)
	var out, diagnostics bytes.Buffer
	a := app{paths: paths, in: strings.NewReader(""), out: &out, errOut: &diagnostics}
	if code := a.run(t.Context(), []string{"init", "--accept-agent-risk", "--json"}); code != 0 {
		t.Fatalf("init: %d %s", code, out.String())
	}
	quotedRepo, _ := json.Marshal(repo)
	quotedUser, _ := json.Marshal(user)
	cfg := fmt.Sprintf("github: {user: %s}\nagent: {executable: claude, timeout: 15m, max_parallel_reviews: 1}\nrepos: [{name: %s}]\n", quotedUser, quotedRepo)
	if err := os.WriteFile(paths.Config, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	diagnostics.Reset()
	started := time.Now()
	code := a.run(t.Context(), []string{"run", "--repo", repo, "--pr", strconv.Itoa(pr), "--json"})
	t.Logf("duration=%s queue=%s result=%s", time.Since(started), paths.Database, out.String())
	if code != 0 {
		t.Fatalf("run exit %d: %s", code, diagnostics.String())
	}
	var response struct {
		Data queue.RunResult `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	reviewed := false
	for _, repo := range response.Data.Repos {
		for _, review := range repo.Reviews {
			if review.PR == pr && review.Status == "succeeded" {
				reviewed = true
			}
		}
	}
	if !reviewed {
		t.Fatal("selected PR was skipped; this was not a real agent trial")
	}
	s, err := store.OpenReadOnly(t.Context(), paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fs, err := s.ListFindings(t.Context(), repo, "", pr)
	if err != nil || len(fs) == 0 {
		t.Fatalf("missing local findings: %v", err)
	}
	for _, f := range fs {
		if f.Status != "pending" && f.Status != "blocked" {
			t.Fatalf("unexpected automatic decision: %+v", f)
		}
	}
	var publications int
	if err := s.DB.QueryRow("SELECT count(*) FROM publications").Scan(&publications); err != nil || publications != 0 {
		t.Fatal("review trial prepared a publication")
	}
	t.Logf("%d local items; inspect findings, transcript, and Git diagnostics manually", len(fs))
}
