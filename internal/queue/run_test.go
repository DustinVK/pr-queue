package queue

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/DustinVK/pr-queue/internal/github"
	"github.com/DustinVK/pr-queue/internal/lock"
	"github.com/DustinVK/pr-queue/internal/runner"
)

type reviewFunc func(context.Context, runner.Request) (runner.Result, error)

func (f reviewFunc) Review(ctx context.Context, r runner.Request) (runner.Result, error) {
	return f(ctx, r)
}
func fakeReview(req runner.Request) runner.Result {
	return runner.Result{Document: findings.Document{SchemaVersion: 1, Repo: req.Input.Repo, PR: req.Input.PR, HeadSHA: req.Input.HeadSHA, Summary: "Summary", Verdict: "comment", Findings: []findings.Finding{{Kind: "general", Severity: "major", Category: "correctness", Title: "A", Body: "A"}}}}
}
func runCoordinator(t *testing.T) (Coordinator, *fakeRemote) {
	t.Helper()
	s, state := queueStore(t)
	p := remotePR(1)
	remote := &fakeRemote{open: []github.PR{p}, fetch: map[int]github.PR{1: p}}
	c := Coordinator{Store: s, Remote: remote, Reviewer: reviewFunc(func(_ context.Context, r runner.Request) (runner.Result, error) { return fakeReview(r), nil }), StateDir: state, WorkDir: state, Parallel: 1}
	return c, remote
}
func repos() []config.Repo { return []config.Repo{{Name: "owner/repo"}} }

func TestCoordinatorPreservesDiffPathsAndChangeCounts(t *testing.T) {
	c, remote := runCoordinator(t)
	remote.diff.AddPatch("value.txt", "@@ -1 +1 @@\n-old\n+new\n")
	valid := findings.Finding{Kind: "inline", Severity: "minor", Category: "correctness", Title: "Valid", Body: "Valid body", Anchor: findings.Anchor{Path: findings.Ptr("value.txt"), Side: findings.Ptr("RIGHT"), Line: findings.Ptr(1)}}
	invalid := valid
	invalid.Title, invalid.Body, invalid.Line = "Invalid", "Invalid body", findings.Ptr(3)
	items := []findings.Finding{valid, invalid}
	c.Reviewer = reviewFunc(func(_ context.Context, req runner.Request) (runner.Result, error) {
		if err := req.Diff.Validate(valid); err != nil {
			t.Errorf("reviewer did not receive the fetched diff: %v", err)
		}
		if err := req.Diff.Validate(invalid); err == nil {
			t.Error("reviewer diff accepted a line outside the patch")
		}
		result := fakeReview(req)
		result.Document.Findings = items
		return result, nil
	})
	first, err := c.Run(t.Context(), repos(), 0)
	if err != nil {
		t.Fatal(err)
	}
	review := first.Repos[0].Reviews[0]
	if first.Changed != 3 || review.Ingestion == nil || review.Ingestion.Changed != 3 || len(review.Ingestion.Findings) != 3 {
		t.Fatalf("summary and two new findings not counted: %+v", first)
	}
	for i, want := range []string{"pending", "pending", "blocked"} {
		f := review.Ingestion.Findings[i]
		stored, err := c.Store.ResolveFinding(t.Context(), f.ID)
		if err != nil || f.Status != want || stored.Status != want {
			t.Fatalf("finding %d status: %+v %+v %v, want %s", i, f, stored, err, want)
		}
	}
	wantPath := runner.OutputPath(c.WorkDir, review.ID)
	var storedPath string
	if err := c.Store.DB.QueryRowContext(t.Context(), "SELECT raw_output_path FROM review_runs WHERE id=?", review.ID).Scan(&storedPath); err != nil || storedPath != wantPath || review.OutputPath != wantPath {
		t.Fatalf("recorded diagnostic destination: returned %q stored %q, want %q: %v", review.OutputPath, storedPath, wantPath, err)
	}
	// Drop the pending inline item while keeping the summary and blocked item
	// byte-identical. Only retirement occurs, so no new/changed item is counted.
	dropped := review.Ingestion.Findings[1].ID
	items = []findings.Finding{invalid}
	second, err := c.Run(t.Context(), repos(), 1)
	if err != nil {
		t.Fatal(err)
	}
	ingested := second.Repos[0].Reviews[0].Ingestion
	if second.Changed != 0 || ingested == nil || ingested.Changed != 0 {
		t.Fatalf("retirement counted as new or changed output: %+v", second)
	}
	retired, err := c.Store.ResolveFinding(t.Context(), dropped)
	if err != nil || retired.Status != "obsolete" {
		t.Fatalf("omitted finding was not retired: %+v %v", retired, err)
	}
}

type repositoryFailureRemote struct{ *fakeRemote }

func (f repositoryFailureRemote) ListOpen(ctx context.Context, repo string) ([]github.PR, error) {
	if repo == "owner/unavailable" {
		return nil, errors.New("repository unavailable")
	}
	return f.fakeRemote.ListOpen(ctx, repo)
}

func TestCoordinatorRepositoryFailureDoesNotBlockHealthyRepository(t *testing.T) {
	c, remote := runCoordinator(t)
	c.Remote = repositoryFailureRemote{remote}
	var reviewed []string
	c.Reviewer = reviewFunc(func(_ context.Context, req runner.Request) (runner.Result, error) {
		reviewed = append(reviewed, req.Input.Repo)
		return fakeReview(req), nil
	})
	result, err := c.Run(t.Context(), []config.Repo{{Name: "owner/unavailable"}, {Name: "owner/repo"}}, 0)
	var partial *PartialFailure
	if !errors.As(err, &partial) || partial.Count != 1 || result.Failed != 1 || len(result.Repos) != 2 {
		t.Fatalf("repository failure result: %+v %v", result, err)
	}
	bad, good := result.Repos[0], result.Repos[1]
	if bad.Repo != "owner/unavailable" || len(bad.Problems) != 1 || bad.Problems[0].Repo != bad.Repo || bad.Problems[0].PR != 0 || len(bad.Reviews) != 0 {
		t.Fatalf("wrong failure attribution: %+v", bad)
	}
	if len(reviewed) != 1 || reviewed[0] != "owner/repo" || len(good.Problems) != 0 || len(good.Reviews) != 1 || good.Reviews[0].Status != "succeeded" {
		t.Fatalf("healthy repository did not complete: %+v; invoked %v", good, reviewed)
	}
	p, err := c.Store.PR(t.Context(), "owner/repo", 1)
	if err != nil || p.LastReviewedKey == nil || *p.LastReviewedKey != p.Key() {
		t.Fatalf("healthy review was not persisted: %+v %v", p, err)
	}
}

func TestCoordinatorSuccessfulRerunSkipsAndForceReviews(t *testing.T) {
	c, _ := runCoordinator(t)
	calls := 0
	c.Reviewer = reviewFunc(func(_ context.Context, r runner.Request) (runner.Result, error) { calls++; return fakeReview(r), nil })
	for _, force := range []int{0, 0, 1} {
		if _, err := c.Run(t.Context(), repos(), force); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("agent invoked %d times", calls)
	}
}

func TestCoordinatorFailuresStayRetryable(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		c, _ := runCoordinator(t)
		c.Reviewer = reviewFunc(func(context.Context, runner.Request) (runner.Result, error) {
			return runner.Result{TimedOut: timeout}, errors.New("agent failed")
		})
		result, err := c.Run(t.Context(), repos(), 0)
		var partial *PartialFailure
		if !errors.As(err, &partial) || partial.Count != 1 || result.Failed != 1 || len(result.Repos) != 1 || len(result.Repos[0].Reviews) != 1 {
			t.Fatalf("failure: %+v %v", result, err)
		}
		want := "failed"
		if timeout {
			want = "timed_out"
		}
		review := result.Repos[0].Reviews[0]
		if review.Status != want {
			t.Fatalf("review status %q, want %q", review.Status, want)
		}
		var status string
		if err := c.Store.DB.QueryRowContext(t.Context(), "SELECT status FROM review_runs WHERE id=?", review.ID).Scan(&status); err != nil || status != want {
			t.Fatalf("persisted run status %q, want %q: %v", status, want, err)
		}
		p, err := c.Store.PR(t.Context(), "owner/repo", 1)
		if err != nil || p.LastReviewedKey != nil {
			t.Fatal("failure advanced cursor")
		}
		c.Reviewer = reviewFunc(func(_ context.Context, r runner.Request) (runner.Result, error) { return fakeReview(r), nil })
		if _, err := c.Run(t.Context(), repos(), 0); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCoordinatorRefreshRejectsStaleOutput(t *testing.T) {
	c, remote := runCoordinator(t)
	c.Reviewer = reviewFunc(func(_ context.Context, r runner.Request) (runner.Result, error) {
		p := remote.fetch[1]
		p.BaseSHA = strings.Repeat("c", 40)
		remote.fetch[1] = p
		return fakeReview(r), nil
	})
	result, err := c.Run(t.Context(), repos(), 0)
	if err == nil || result.Failed != 1 || !strings.Contains(result.Repos[0].Reviews[0].Error, "comparison changed") {
		t.Fatalf("stale: %+v %v", result, err)
	}
	p, err := c.Store.PR(t.Context(), "owner/repo", 1)
	if err != nil || p.LastReviewedKey != nil || p.BaseSHA != strings.Repeat("c", 40) {
		t.Fatal("live comparison not observed")
	}
	fs, err := c.Store.ListFindings(t.Context(), "", "", 0)
	if err != nil || len(fs) != 0 {
		t.Fatal("stale output activated")
	}
}

func TestComparisonChangedBeforeAgentInvalidatesApprovals(t *testing.T) {
	c, remote := runCoordinator(t)
	first, err := c.Run(t.Context(), repos(), 0)
	if err != nil {
		t.Fatal(err)
	}
	id := first.Repos[0].Reviews[0].Ingestion.Findings[0].ID
	p, err := c.Store.PR(t.Context(), "owner/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store.DB.Exec("UPDATE findings SET status='approved',approved_comparison_key=? WHERE id=?", p.Key(), id); err != nil {
		t.Fatal(err)
	}
	changed := remote.fetch[1]
	changed.BaseSHA = strings.Repeat("d", 40)
	remote.fetch[1] = changed // The list still reports the old comparison.
	c.Reviewer = reviewFunc(func(context.Context, runner.Request) (runner.Result, error) {
		t.Error("stale comparison reached agent")
		return runner.Result{}, nil
	})
	result, err := c.Run(t.Context(), repos(), 1)
	if err == nil || result.Failed != 1 {
		t.Fatalf("changed: %+v %v", result, err)
	}
	p, err = c.Store.PR(t.Context(), "owner/repo", 1)
	if err != nil || p.BaseSHA != changed.BaseSHA || p.LastReviewedKey != nil {
		t.Fatal("changed observation was lost")
	}
	f, err := c.Store.ResolveFinding(t.Context(), id)
	if err != nil || f.Status != "pending" || f.ApprovedComparisonKey != nil {
		t.Fatalf("stale approval survived: %+v %v", f, err)
	}
}

func TestAgentOutsideLockAndLatestTriageDecisionPreserved(t *testing.T) {
	c, _ := runCoordinator(t)
	first, err := c.Run(t.Context(), repos(), 0)
	if err != nil {
		t.Fatal(err)
	}
	id := first.Repos[0].Reviews[0].Ingestion.Findings[1].ID
	c.Reviewer = reviewFunc(func(_ context.Context, r runner.Request) (runner.Result, error) {
		l, err := lock.Acquire(lock.PRPath(c.StateDir, r.Input.Repo, r.Input.PR), "triage during agent")
		if err != nil {
			return runner.Result{}, err
		}
		defer l.Close()
		if _, err := c.Store.DB.Exec("UPDATE findings SET status='rejected' WHERE id=?", id); err != nil {
			return runner.Result{}, err
		}
		return fakeReview(r), nil
	})
	second, err := c.Run(t.Context(), repos(), 1)
	if err != nil {
		t.Fatal(err)
	}
	f := second.Repos[0].Reviews[0].Ingestion.Findings[1]
	if f.ID != id || f.Status != "rejected" {
		t.Fatalf("later decision overwritten: %+v", f)
	}
}

func TestIngestionContentionIsPartialFailure(t *testing.T) {
	c, _ := runCoordinator(t)
	var held *lock.Lock
	c.Reviewer = reviewFunc(func(_ context.Context, r runner.Request) (runner.Result, error) {
		var err error
		held, err = lock.Acquire(lock.PRPath(c.StateDir, r.Input.Repo, r.Input.PR), "ongoing triage")
		return fakeReview(r), err
	})
	result, err := c.Run(t.Context(), repos(), 0)
	if held != nil {
		defer held.Close()
	}
	var partial *PartialFailure
	if !errors.As(err, &partial) || result.Failed != 1 || !strings.Contains(result.Repos[0].Reviews[0].Error, "ingestion lock") {
		t.Fatalf("contention: %+v %v", result, err)
	}
	p, _ := c.Store.PR(t.Context(), "owner/repo", 1)
	if p.LastReviewedKey != nil {
		t.Fatal("busy ingestion advanced cursor")
	}
}

func TestParallelReviewsRespectLimit(t *testing.T) {
	c, remote := runCoordinator(t)
	c.Parallel = 2
	remote.open = []github.PR{remotePR(1), remotePR(2), remotePR(3)}
	remote.fetch = map[int]github.PR{1: remotePR(1), 2: remotePR(2), 3: remotePR(3)}
	var active, peak atomic.Int32
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	c.Reviewer = reviewFunc(func(ctx context.Context, r runner.Request) (runner.Result, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return runner.Result{}, ctx.Err()
		}
		return fakeReview(r), nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Run(ctx, repos(), 0); done <- err }()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("reviews did not run in parallel")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 2 {
		t.Fatalf("peak concurrency %d", peak.Load())
	}
}
