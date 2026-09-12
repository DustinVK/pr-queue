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
		if !errors.As(err, &partial) || result.Failed != 1 {
			t.Fatalf("failure: %+v %v", result, err)
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
