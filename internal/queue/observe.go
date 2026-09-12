package queue

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/DustinVK/pr-queue/internal/github"
	"github.com/DustinVK/pr-queue/internal/lock"
	"github.com/DustinVK/pr-queue/internal/store"
)

type ObservationStore interface {
	Tracked(context.Context, string) ([]store.PullRequest, error)
	Observe(context.Context, string, int, findings.Comparison, string) (store.PullRequest, bool, error)
}
type PRReader interface {
	ListOpen(context.Context, string) ([]github.PR, error)
	FetchPR(context.Context, string, int) (github.PR, error)
}
type Observer struct {
	Store    ObservationStore
	Remote   PRReader
	StateDir string
}
type Observation struct {
	Number   int                `json:"number"`
	Remote   *github.PR         `json:"pr,omitempty"`
	Local    *store.PullRequest `json:"local,omitempty"`
	Changed  bool               `json:"changed"`
	Eligible bool               `json:"eligible"`
	Reason   string             `json:"reason,omitempty"`
}
type Problem struct {
	Repo  string `json:"repo"`
	PR    int    `json:"pr,omitempty"`
	Error string `json:"error"`
}
type ObservationBatch struct {
	Observations []Observation `json:"observations"`
	Problems     []Problem     `json:"problems"`
}

// ObserveRepo fully lists before inferring any absence, then observes each PR
// under its own lock before applying filters. The caller holds the run lock.
func (o Observer) ObserveRepo(ctx context.Context, repo config.Repo, forcePR int) (ObservationBatch, error) {
	out := ObservationBatch{Observations: []Observation{}, Problems: []Problem{}}
	open, err := o.Remote.ListOpen(ctx, repo.Name)
	if err != nil {
		return out, err
	}
	tracked, err := o.Store.Tracked(ctx, repo.Name)
	if err != nil {
		return out, err
	}
	byNumber := map[int]github.PR{}
	numbers := map[int]bool{}
	for _, p := range open {
		if p.Repo != repo.Name || p.Number < 1 {
			return out, fmt.Errorf("PR list identity mismatch")
		}
		if numbers[p.Number] {
			return out, fmt.Errorf("duplicate PR in complete list")
		}
		byNumber[p.Number] = p
		numbers[p.Number] = true
	}
	for _, p := range tracked {
		numbers[p.Number] = true
	}
	if forcePR > 0 {
		numbers[forcePR] = true
	}
	order := make([]int, 0, len(numbers))
	for n := range numbers {
		order = append(order, n)
	}
	sort.Ints(order)
	for _, n := range order {
		var listed *github.PR
		if p, ok := byNumber[n]; ok {
			listed = &p
		}
		item, err := o.observeOne(ctx, repo, n, listed, forcePR)
		if err != nil {
			out.Problems = append(out.Problems, Problem{Repo: repo.Name, PR: n, Error: err.Error()})
			continue
		}
		out.Observations = append(out.Observations, item)
	}
	return out, nil
}

func (o Observer) observeOne(ctx context.Context, repo config.Repo, n int, listed *github.PR, forcePR int) (Observation, error) {
	r := Observation{Number: n}
	l, err := lock.Acquire(lock.PRPath(o.StateDir, repo.Name, n), fmt.Sprintf("%s#%d", repo.Name, n))
	if err != nil {
		var busy *lock.BusyError
		if errors.As(err, &busy) {
			r.Reason = err.Error()
			return r, nil
		}
		return r, err
	}
	defer l.Close()
	if listed == nil {
		p, err := o.Remote.FetchPR(ctx, repo.Name, n)
		if err != nil {
			return r, err
		}
		listed = &p
	}
	if listed.Repo != repo.Name || listed.Number != n {
		return r, fmt.Errorf("PR fetch identity mismatch")
	}
	local, changed, err := o.Store.Observe(ctx, repo.Name, n, listed.Comparison, "coordinator")
	if err != nil {
		return r, err
	}
	r.Remote = listed
	r.Local = &local
	r.Changed = changed
	switch {
	case listed.State != "open":
		r.Reason = "PR is " + listed.State
	case forcePR > 0 && n != forcePR:
		r.Reason = "not the explicitly requested PR"
	case forcePR == n:
		r.Eligible = true
	case listed.Draft:
		r.Reason = "PR is draft"
	case !MatchesFilters(*listed, repo.Filters):
		r.Reason = "excluded by filters"
	case local.LastReviewedKey != nil && *local.LastReviewedKey == listed.Key():
		r.Reason = "comparison already reviewed"
	default:
		r.Eligible = true
	}
	return r, nil
}

func MatchesFilters(p github.PR, f config.Filters) bool {
	if len(f.Authors) > 0 && !containsFold(f.Authors, p.Author) {
		return false
	}
	if f.RequestedReviewer != nil && !containsFold(p.RequestedReviewers, *f.RequestedReviewer) {
		return false
	}
	if len(f.BaseBranches) > 0 {
		match := false
		for _, b := range f.BaseBranches {
			if b == p.BaseBranch {
				match = true
			}
		}
		if !match {
			return false
		}
	}
	return true
}
func containsFold(list []string, value string) bool {
	for _, s := range list {
		if strings.EqualFold(s, value) {
			return true
		}
	}
	return false
}
