package queue

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/DustinVK/pr-queue/internal/github"
	"github.com/DustinVK/pr-queue/internal/lock"
	"github.com/DustinVK/pr-queue/internal/store"
)

type fakeRemote struct {
	mu          sync.Mutex
	open        []github.PR
	listErr     error
	fetch       map[int]github.PR
	fetchErr    map[int]error
	fetched     []int
	diff        findings.Diff
	identityErr error
	afterFetch  func()
}

func (f *fakeRemote) ListOpen(context.Context, string) ([]github.PR, error) { return f.open, f.listErr }
func (f *fakeRemote) FetchPR(_ context.Context, _ string, n int) (github.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetched = append(f.fetched, n)
	if err := f.fetchErr[n]; err != nil {
		return github.PR{}, err
	}
	p, ok := f.fetch[n]
	if !ok {
		return p, fmt.Errorf("not found")
	}
	if f.afterFetch != nil {
		f.afterFetch()
	}
	return p, nil
}
func (f *fakeRemote) Identity(context.Context, string) (string, error) {
	return "reviewer", f.identityErr
}
func (f *fakeRemote) FetchDiff(context.Context, github.PR) (findings.Diff, error) { return f.diff, nil }
func remotePR(n int) github.PR {
	return github.PR{Repo: "owner/repo", Number: n, Author: "author", BaseBranch: "main", RequestedReviewers: []string{"reviewer"}, Comparison: findings.Comparison{HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), State: "open"}}
}
func queueStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	state := t.TempDir()
	s, err := store.Create(t.Context(), filepath.Join(state, "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, state
}

func seedApproved(t *testing.T, s *store.Store, p github.PR) store.PullRequest {
	t.Helper()
	local, _, err := s.Observe(t.Context(), p.Repo, p.Number, p.Comparison, "test")
	if err != nil {
		t.Fatal(err)
	}
	runID := fmt.Sprintf("run-%d", p.Number)
	_, err = s.DB.Exec(`INSERT INTO review_runs(id,pr_id,head_sha,comparison_key,status,started_at) VALUES(?,?,?,?,'succeeded','now')`, runID, local.ID, p.HeadSHA, p.Key())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec(`INSERT INTO findings(id,pr_id,review_run_id,kind,body,fingerprint,status,approved_comparison_key,created_at) VALUES(?,?,?,'general','approved body','fingerprint','approved',?,'now')`, fmt.Sprintf("%08x-0000-4000-8000-000000000001", p.Number), local.ID, runID, p.Key())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec("UPDATE pull_requests SET last_reviewed_key=? WHERE id=?", p.Key(), local.ID)
	if err != nil {
		t.Fatal(err)
	}
	return local
}

func assertApproved(t *testing.T, s *store.Store, p github.PR, want bool) {
	t.Helper()
	local, err := s.PR(t.Context(), p.Repo, p.Number)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := s.ListFindings(t.Context(), p.Repo, "", p.Number)
	if err != nil || len(fs) != 1 {
		t.Fatalf("findings %+v %v", fs, err)
	}
	if want {
		if fs[0].Status != "approved" || fs[0].ApprovedComparisonKey == nil || local.LastReviewedKey == nil {
			t.Fatal("approval/cursor unexpectedly cleared")
		}
	} else if fs[0].Status != "pending" || fs[0].ApprovedComparisonKey != nil || local.LastReviewedKey != nil {
		t.Fatal("stale approval/cursor remained")
	}
}

func TestObserveBeforeFilterAndRecordTransitions(t *testing.T) {
	s, state := queueStore(t)
	p := remotePR(1)
	seedApproved(t, s, p)
	remote := &fakeRemote{open: []github.PR{p}}
	o := Observer{Store: s, Remote: remote, StateDir: state}
	cfg := config.Repo{Name: p.Repo, Filters: config.Filters{Authors: []string{"excluded-author"}}}
	batch, err := o.ObserveRepo(t.Context(), cfg, 0)
	if err != nil || batch.Observations[0].Eligible {
		t.Fatalf("filter: %+v %v", batch, err)
	}
	assertApproved(t, s, p, true)
	draft := p
	draft.Draft = true
	remote.open = []github.PR{draft}
	batch, err = o.ObserveRepo(t.Context(), cfg, 0)
	if err != nil || !batch.Observations[0].Changed || batch.Observations[0].Eligible {
		t.Fatalf("draft observation: %+v %v", batch, err)
	}
	assertApproved(t, s, p, false)
	remote.open = []github.PR{p}
	cfg.Filters = config.Filters{}
	batch, err = o.ObserveRepo(t.Context(), cfg, 0)
	if err != nil || !batch.Observations[0].Eligible || !batch.Observations[0].Changed {
		t.Fatalf("ready again: %+v %v", batch, err)
	}
	var n int
	if err := s.DB.QueryRow("SELECT count(*) FROM audit_log WHERE action='approval_cleared' AND body_snapshot='approved body'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("invalidation audit %d %v", n, err)
	}
}

func TestMissingPRFetchedDirectlyNeverAssumedClosed(t *testing.T) {
	s, state := queueStore(t)
	p := remotePR(1)
	seedApproved(t, s, p)
	remote := &fakeRemote{open: []github.PR{}, fetchErr: map[int]error{1: errors.New("request failed")}}
	o := Observer{Store: s, Remote: remote, StateDir: state}
	cfg := config.Repo{Name: p.Repo}
	batch, err := o.ObserveRepo(t.Context(), cfg, 0)
	if err != nil || len(batch.Problems) != 1 || len(remote.fetched) != 1 {
		t.Fatalf("missing PR: %+v %v", batch, err)
	}
	assertApproved(t, s, p, true)
	remote.fetchErr = nil
	closed := p
	closed.State = "closed"
	remote.fetch = map[int]github.PR{1: closed}
	batch, err = o.ObserveRepo(t.Context(), cfg, 0)
	if err != nil || len(batch.Problems) > 0 || batch.Observations[0].Eligible {
		t.Fatalf("closed: %+v %v", batch, err)
	}
	assertApproved(t, s, p, false)
	remote.open = []github.PR{p}
	batch, err = o.ObserveRepo(t.Context(), cfg, 0)
	if err != nil || !batch.Observations[0].Eligible {
		t.Fatalf("reopen: %+v %v", batch, err)
	}
}

func TestHeadAndBaseChangesClearApprovalAndCursor(t *testing.T) {
	for _, field := range []string{"head", "base"} {
		t.Run(field, func(t *testing.T) {
			s, state := queueStore(t)
			p := remotePR(1)
			seedApproved(t, s, p)
			changed := p
			if field == "head" {
				changed.HeadSHA = strings.Repeat("c", 40)
			} else {
				changed.BaseSHA = strings.Repeat("c", 40)
			}
			remote := &fakeRemote{open: []github.PR{changed}}
			batch, err := (Observer{Store: s, Remote: remote, StateDir: state}).ObserveRepo(t.Context(), config.Repo{Name: p.Repo}, 0)
			if err != nil || len(batch.Problems) != 0 || len(batch.Observations) != 1 {
				t.Fatalf("observe: %+v %v", batch, err)
			}
			o := batch.Observations[0]
			if !o.Changed || !o.Eligible || o.Local.Key() != changed.Key() {
				t.Fatalf("changed comparison not eligible: %+v", o)
			}
			assertApproved(t, s, p, false)
			var n int
			if err := s.DB.QueryRow("SELECT count(*) FROM audit_log WHERE action='approval_cleared' AND body_snapshot='approved body'").Scan(&n); err != nil || n != 1 {
				t.Fatalf("invalidation audit %d %v", n, err)
			}
		})
	}
}

func TestIncompleteListDoesNotMutateObservations(t *testing.T) {
	s, state := queueStore(t)
	p := remotePR(1)
	seedApproved(t, s, p)
	changed := p
	changed.BaseSHA = strings.Repeat("c", 40)
	remote := &fakeRemote{open: []github.PR{changed}, listErr: errors.New("later page failed")}
	_, err := (Observer{Store: s, Remote: remote, StateDir: state}).ObserveRepo(t.Context(), config.Repo{Name: p.Repo}, 0)
	if err == nil {
		t.Fatal("list failure not returned")
	}
	assertApproved(t, s, p, true)
	local, _ := s.PR(t.Context(), p.Repo, 1)
	if local.Key() != p.Key() {
		t.Fatal("partial list persisted")
	}
}

func TestObservationContentionSkipsEvenForcedPR(t *testing.T) {
	s, state := queueStore(t)
	p := remotePR(1)
	seedApproved(t, s, p)
	changed := p
	changed.BaseSHA = strings.Repeat("c", 40)
	remote := &fakeRemote{open: []github.PR{changed, remotePR(2)}}
	l, err := lock.Acquire(lock.PRPath(state, p.Repo, 1), "triage")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	batch, err := (Observer{Store: s, Remote: remote, StateDir: state}).ObserveRepo(t.Context(), config.Repo{Name: p.Repo}, 1)
	if err != nil || len(batch.Problems) != 0 || len(batch.Observations) != 2 {
		t.Fatalf("contention: %+v %v", batch, err)
	}
	if batch.Observations[0].Local != nil || batch.Observations[0].Eligible {
		t.Fatal("busy PR was observed/reviewed")
	}
	if batch.Observations[1].Local == nil {
		t.Fatal("another PR was not observed")
	}
	assertApproved(t, s, p, true)
}

func TestForcedReviewBypassesFiltersAndDraftButNotClosed(t *testing.T) {
	s, state := queueStore(t)
	p := remotePR(1)
	p.Draft = true
	remote := &fakeRemote{open: []github.PR{p}}
	o := Observer{Store: s, Remote: remote, StateDir: state}
	cfg := config.Repo{Name: p.Repo, Filters: config.Filters{Authors: []string{"someone-else"}}}
	batch, err := o.ObserveRepo(t.Context(), cfg, 1)
	if err != nil || !batch.Observations[0].Eligible {
		t.Fatalf("force: %+v %v", batch, err)
	}
	p.State = "closed"
	remote.open = nil
	remote.fetch = map[int]github.PR{1: p}
	batch, err = o.ObserveRepo(t.Context(), cfg, 1)
	if err != nil || batch.Observations[0].Eligible {
		t.Fatalf("force closed: %+v %v", batch, err)
	}
}

func TestFiltersAreANDAndBranchNamesAreCaseSensitive(t *testing.T) {
	p := remotePR(1)
	f := config.Filters{Authors: []string{"AUTHOR"}, RequestedReviewer: findings.Ptr("Reviewer"), BaseBranches: []string{"main"}}
	if !MatchesFilters(p, f) {
		t.Fatal("matching filters rejected")
	}
	f.BaseBranches = []string{"Main"}
	if MatchesFilters(p, f) {
		t.Fatal("branch case was ignored")
	}
	f.BaseBranches = nil
	f.RequestedReviewer = findings.Ptr("other")
	if MatchesFilters(p, f) {
		t.Fatal("reviewer filter ignored")
	}
}

func TestObservationInvalidationRollsBackWithItsAudit(t *testing.T) {
	s, _ := queueStore(t)
	p := remotePR(1)
	seedApproved(t, s, p)
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_observation BEFORE UPDATE ON pull_requests
BEGIN SELECT RAISE(ABORT,'injected observation failure'); END`); err != nil {
		t.Fatal(err)
	}
	changed := p.Comparison
	changed.BaseSHA = strings.Repeat("c", 40)
	if _, _, err := s.Observe(t.Context(), p.Repo, p.Number, changed, "test"); err == nil {
		t.Fatal("injected failure ignored")
	}
	assertApproved(t, s, p, true)
	var n int
	if err := s.DB.QueryRow("SELECT count(*) FROM audit_log").Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial invalidation audit: %d %v", n, err)
	}
}
