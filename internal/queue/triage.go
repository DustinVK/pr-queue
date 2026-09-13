package queue

import (
	"context"
	"fmt"

	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/DustinVK/pr-queue/internal/github"
	"github.com/DustinVK/pr-queue/internal/lock"
	"github.com/DustinVK/pr-queue/internal/store"
)

func (q Service) selected(ctx context.Context, ids []string) ([]store.Finding, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("select at least one finding")
	}
	var result []store.Finding
	seen := map[string]bool{}
	for _, id := range ids {
		f, err := q.Store.ResolveFinding(ctx, id)
		if err != nil {
			return nil, err
		}
		if len(result) > 0 && result[0].PRID != f.PRID {
			return nil, fmt.Errorf("all findings must belong to one PR")
		}
		if !seen[f.ID] {
			seen[f.ID] = true
			result = append(result, f)
		}
	}
	return result, nil
}

func (q Service) Approve(ctx context.Context, ids []string) ([]store.Finding, error) {
	return q.decide(ctx, ids, "approved", "")
}
func (q Service) Reject(ctx context.Context, ids []string, reason string) ([]store.Finding, error) {
	if err := findings.ValidateBody(reason); err != nil {
		return nil, fmt.Errorf("rejection reason: %w", err)
	}
	return q.decide(ctx, ids, "rejected", reason)
}

func (q Service) decide(ctx context.Context, ids []string, decision, reason string) ([]store.Finding, error) {
	selected, err := q.selected(ctx, ids)
	if err != nil {
		return nil, err
	}
	f := selected[0]
	l, err := lock.Acquire(lock.PRPath(q.StateDir, f.Repo, f.PR), fmt.Sprintf("%s#%d %s", f.Repo, f.PR, decision))
	if err != nil {
		return nil, err
	}
	defer l.Close()
	var key string
	var diff findings.Diff
	actor := q.User
	if decision == "approved" {
		actor, err = q.Remote.Identity(ctx, q.User)
		if err != nil {
			return nil, err
		}
		p, d, err := q.currentDiff(ctx, f.Repo, f.PR, true)
		if err != nil {
			return nil, err
		}
		if p.State != "open" || p.Draft {
			return nil, fmt.Errorf("approval requires an open, non-draft PR")
		}
		key = p.Key()
		diff = d
	}
	fullIDs := make([]string, len(selected))
	for i, f := range selected {
		fullIDs[i] = f.ID
	}
	return q.Store.Decide(ctx, fullIDs, decision, actor, reason, key, diff)
}

// currentDiff observes both sides of the diff read, even if it must refuse an
// operation afterward. No remote call runs inside a database transaction.
// Caller holds the PR lock; persist=false is reserved for read-only previews.
func (q Service) currentDiff(ctx context.Context, repo string, number int, persist bool) (github.PR, findings.Diff, error) {
	var diff findings.Diff
	p, err := q.Remote.FetchPR(ctx, repo, number)
	if err != nil {
		return p, diff, err
	}
	observe := func(p github.PR) error {
		if p.Number != number || p.Repo != repo {
			return fmt.Errorf("PR identity mismatch")
		}
		if err := p.Comparison.Validate(); err != nil {
			return err
		}
		if persist {
			_, _, err := q.Store.Observe(ctx, repo, number, p.Comparison, q.User)
			return err
		}
		return nil
	}
	if err := observe(p); err != nil {
		return p, diff, err
	}
	diff, err = q.Remote.FetchDiff(ctx, p)
	if err != nil {
		return p, diff, err
	}
	after, err := q.Remote.FetchPR(ctx, repo, number)
	if err != nil {
		return p, diff, err
	}
	if err := observe(after); err != nil {
		return p, diff, err
	}
	if p.Key() != after.Key() {
		return after, diff, fmt.Errorf("comparison changed while fetching the diff; retry")
	}
	return p, diff, nil
}

type BodyEditor func(context.Context, string) (string, error)
type EditResult struct {
	Finding store.Finding `json:"finding"`
	Changed bool          `json:"changed"`
}

func (q Service) Edit(ctx context.Context, id string, asGeneral bool, edit BodyEditor) (EditResult, error) {
	var result EditResult
	f, err := q.Store.ResolveFinding(ctx, id)
	if err != nil {
		return result, err
	}
	l, err := lock.Acquire(lock.PRPath(q.StateDir, f.Repo, f.PR), fmt.Sprintf("%s#%d edit", f.Repo, f.PR))
	if err != nil {
		return result, err
	}
	defer l.Close()
	f, err = q.Store.ResolveFinding(ctx, f.ID)
	if err != nil {
		return result, err
	}
	if f.Status == "published" {
		return result, fmt.Errorf("published findings cannot be edited")
	}
	if asGeneral && f.Kind != "inline" && f.Kind != "general" {
		return result, fmt.Errorf("only inline findings can be converted to general")
	}
	if edit == nil {
		return result, fmt.Errorf("no editor configured")
	}
	body, err := edit(ctx, f.Body)
	if err != nil {
		return result, err
	}
	if err := findings.ValidateBody(body); err != nil {
		return result, err
	}
	if body == f.Body && (!asGeneral || f.Kind == "general") {
		return EditResult{Finding: f}, nil
	}
	var diff findings.Diff
	if f.Kind == "inline" && !asGeneral {
		if _, err := q.Remote.Identity(ctx, q.User); err != nil {
			return result, err
		}
		_, d, err := q.currentDiff(ctx, f.Repo, f.PR, true)
		if err != nil {
			return result, err
		}
		diff = d
	}
	updated, err := q.Store.EditFinding(ctx, f, body, asGeneral, q.User, diff)
	return EditResult{Finding: updated, Changed: err == nil}, err
}
