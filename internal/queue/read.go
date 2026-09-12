// Package queue is the application service for local review decisions.
package queue

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/DustinVK/pr-queue/internal/github"
	"github.com/DustinVK/pr-queue/internal/store"
)

type Remote interface {
	Identity(context.Context, string) (string, error)
	FetchPR(context.Context, string, int) (github.PR, error)
	FetchDiff(context.Context, github.PR) (findings.Diff, error)
}
type Service struct {
	Store    *store.Store
	Remote   Remote
	User     string
	StateDir string
}

func ParsePR(ref string) (string, int, error) {
	parts := strings.Split(ref, "#")
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("PR must be owner/name#number")
	}
	if err := config.ValidateRepo(parts[0]); err != nil {
		return "", 0, err
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil || n < 1 {
		return "", 0, fmt.Errorf("PR number must be a positive integer")
	}
	return parts[0], n, nil
}

func (q Service) List(ctx context.Context, repo, status string) ([]store.Finding, error) {
	if repo != "" {
		if err := config.ValidateRepo(repo); err != nil {
			return nil, err
		}
	}
	return q.Store.ListFindings(ctx, repo, status, 0)
}

type ShowResult struct {
	PR           store.PullRequest          `json:"pr"`
	Findings     []store.Finding            `json:"findings"`
	Publications []store.PublicationHistory `json:"historical_publications"`
}

func (q Service) Show(ctx context.Context, ref string) (ShowResult, error) {
	var r ShowResult
	repo, n, err := ParsePR(ref)
	if err != nil {
		return r, err
	}
	if r.PR, err = q.Store.PR(ctx, repo, n); err != nil {
		return r, fmt.Errorf("PR %s not in local queue: %w", ref, err)
	}
	if r.Findings, err = q.Store.ListFindings(ctx, repo, "", n); err != nil {
		return r, err
	}
	r.Publications, err = q.Store.PublicationHistory(ctx, r.PR.ID)
	return r, err
}

type DiffResult struct {
	Finding         store.Finding       `json:"finding"`
	Comparison      findings.Comparison `json:"current_comparison"`
	Stale           bool                `json:"stale"`
	ValidationError string              `json:"validation_error,omitempty"`
	Patch           string              `json:"patch,omitempty"`
}

func (q Service) Diff(ctx context.Context, id string) (DiffResult, error) {
	var r DiffResult
	f, err := q.Store.ResolveFinding(ctx, id)
	if err != nil {
		return r, err
	}
	if _, err := q.Remote.Identity(ctx, q.User); err != nil {
		return r, err
	}
	p, err := q.Remote.FetchPR(ctx, f.Repo, f.PR)
	if err != nil {
		return r, err
	}
	d, err := q.Remote.FetchDiff(ctx, p)
	if err != nil {
		return r, err
	}
	after, err := q.Remote.FetchPR(ctx, f.Repo, f.PR)
	if err != nil {
		return r, err
	}
	if p.Key() != after.Key() {
		return r, fmt.Errorf("PR comparison changed while reading its diff; retry")
	}
	r = DiffResult{Finding: f, Comparison: p.Comparison, Stale: f.RunComparisonKey != p.Key()}
	if err := d.Validate(f.Finding); err != nil {
		r.ValidationError = err.Error()
	}
	if f.Path != nil && d.Files[*f.Path] != nil {
		r.Patch = d.Files[*f.Path].Patch
	}
	return r, nil
}
