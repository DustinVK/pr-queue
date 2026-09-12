package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/DustinVK/pr-queue/internal/github"
	"github.com/DustinVK/pr-queue/internal/store"
	"github.com/google/uuid"
)

type cliPublisher struct {
	runRemote
	posts        int
	loseResponse bool
	review       github.Review
}

func (r *cliPublisher) SendReview(_ context.Context, _ string, _ int, request github.ReviewRequest) (github.Review, error) {
	r.posts++
	r.review = github.Review{ID: "123", User: "alice", CommitID: request.CommitID, Body: request.Body, State: "COMMENTED", SubmittedAt: findings.Ptr("2026-09-12T12:00:00Z")}
	if r.loseResponse {
		return github.Review{}, errors.New("accepted but response lost")
	}
	return r.review, nil
}
func (r *cliPublisher) FetchReview(context.Context, string, int, string) (github.Review, error) {
	return r.review, nil
}
func (r *cliPublisher) ListReviews(context.Context, string, int) ([]github.Review, error) {
	return []github.Review{r.review}, nil
}
func (r *cliPublisher) ListReviewComments(context.Context, string, int, string) ([]github.SubmittedComment, error) {
	return []github.SubmittedComment{}, nil
}

func TestPublicationPreviewLeavesAllFiveTablesUnchanged(t *testing.T) {
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
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := a.run(t.Context(), []string{"approve", fs[0].ID, "--json"}); code != 0 {
		t.Fatalf("approve: %d %s", code, out)
	}
	p, err := s.PR(t.Context(), "owner/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	// A dry preview must not perform startup recovery, including with Go's
	// accepted single-dash option spelling.
	if _, err := s.StartRun(t.Context(), uuid.NewString(), p, "interrupted-output"); err != nil {
		t.Fatal(err)
	}
	before := databaseSnapshot(t, a.paths.Database)
	var previousMarker string
	for _, dryFlag := range []string{"--dry-run", "--dry-run=true", "-dry-run"} {
		out.Reset()
		if code := a.run(t.Context(), []string{"publish", "owner/repo#1", "--event", "COMMENT", dryFlag, "--json"}); code != 0 {
			t.Fatalf("preview: %d %s", code, out)
		}
		if after := databaseSnapshot(t, a.paths.Database); after != before {
			t.Fatalf("preview %s changed persistent state", dryFlag)
		}
		var response struct {
			OK   bool `json:"ok"`
			Data struct {
				Snapshot store.PublicationSnapshot `json:"snapshot"`
				DryRun   bool                      `json:"dry_run"`
			} `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &response); err != nil || !response.OK || !response.Data.DryRun {
			t.Fatalf("preview output: %s %v", out, err)
		}
		if response.Data.Snapshot.Marker == previousMarker {
			t.Fatal("separate previews reused an attempt ID")
		}
		previousMarker = response.Data.Snapshot.Marker
	}
}

func TestPublishNoopNeedsNoPostingClient(t *testing.T) {
	a, _, out := runFixture(t)
	if code := a.run(t.Context(), []string{"publish", "owner/repo#1", "--event", "COMMENT", "--json"}); code != 0 {
		t.Fatalf("no-op publish: %d %s", code, out)
	}
}

func TestCLIPublishLostResponseEditAndResume(t *testing.T) {
	a, _, out := runFixture(t)
	remote := &cliPublisher{runRemote: a.remote.(runRemote), loseResponse: true}
	a.remote = remote
	call := func(want int, args ...string) {
		t.Helper()
		out.Reset()
		if code := a.run(t.Context(), append(args, "--json")); code != want {
			t.Fatalf("%v: code=%d %s", args, code, out)
		}
	}
	call(0, "run")
	s, err := store.Open(t.Context(), a.paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fs, err := s.ListFindings(t.Context(), "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	id := fs[0].ID
	call(0, "approve", id)
	call(1, "publish", "owner/repo#1", "--event", "COMMENT")
	call(1, "publish", "owner/repo#1", "--event", "COMMENT")
	p, err := s.BlockingPublication(t.Context(), "owner/repo", 1)
	if err != nil || p == nil || !p.Uncertain {
		t.Fatalf("missing unresolved publication: %+v %v", p, err)
	}
	path := filepath.Join(t.TempDir(), "edit summary")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf 'later summary\\n' > \"$1\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", strconv.Quote(path))
	call(0, "edit", id)
	call(0, "publish", "owner/repo#1", "--resume")
	call(0, "publish", "owner/repo#1", "--resume")
	if remote.posts != 1 {
		t.Fatalf("sent %d reviews", remote.posts)
	}
	f, err := s.ResolveFinding(t.Context(), id)
	if err != nil || f.Status != "pending" || f.Body != "later summary\n" {
		t.Fatalf("later edit lost: %+v %v", f, err)
	}
	var body string
	if err := s.DB.QueryRow("SELECT body_snapshot FROM audit_log WHERE action='published'").Scan(&body); err != nil || body != fs[0].Body {
		t.Fatalf("wrong sent audit: %q %v", body, err)
	}
}

func TestPublishFlagCompatibility(t *testing.T) {
	a, _, out := runFixture(t)
	for _, args := range [][]string{
		{"publish", "owner/repo#1", "--resume", "--event", "COMMENT"},
		{"publish", "owner/repo#1", "--resume", "--dry-run"},
		{"publish", "owner/repo#1", "--resume", "--dry-run=false"},
		{"publish", "owner/repo#1", "--confirmed-not-sent", "--event", "COMMENT"},
		{"publish", "owner/repo#1"},
	} {
		out.Reset()
		if code := a.run(t.Context(), append(args, "--json")); code != 1 {
			t.Fatalf("flags %v: %d %s", args, code, out)
		}
	}
}
