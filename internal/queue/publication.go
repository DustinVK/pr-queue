package queue

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/DustinVK/pr-queue/internal/github"
	"github.com/DustinVK/pr-queue/internal/lock"
	"github.com/DustinVK/pr-queue/internal/store"
	"github.com/google/uuid"
)

type PublicationPreview struct {
	DryRun   bool                       `json:"dry_run"`
	Noop     bool                       `json:"noop"`
	Snapshot *store.PublicationSnapshot `json:"snapshot,omitempty"`
}

func (q Service) PreviewPublication(ctx context.Context, ref, event string) (PublicationPreview, error) {
	result := PublicationPreview{DryRun: true}
	repo, pr, err := ParsePR(ref)
	if err != nil {
		return result, err
	}
	l, err := lock.Acquire(lock.PRPath(q.StateDir, repo, pr), fmt.Sprintf("%s preview publication", ref))
	if err != nil {
		return result, err
	}
	defer l.Close()
	result.Snapshot, err = q.publicationCandidate(ctx, repo, pr, event, false)
	result.Noop = err == nil && result.Snapshot == nil
	return result, err
}

// publicationCandidate requires the PR lock. The live path will persist observed
// transitions before preparation; previews share validation without writing state.
func (q Service) publicationCandidate(ctx context.Context, repo string, pr int, event string, persist bool) (*store.PublicationSnapshot, error) {
	if !github.ValidEvent(event) {
		return nil, fmt.Errorf("event must be COMMENT, REQUEST_CHANGES, or APPROVE")
	}
	blocking, err := q.Store.BlockingPublication(ctx, repo, pr)
	if err != nil {
		return nil, err
	}
	if blocking != nil {
		return nil, fmt.Errorf("publication %s is unresolved; use prq publish %s#%d --resume", blocking.ID, repo, pr)
	}
	selected, err := q.Store.ListFindings(ctx, repo, "approved", pr)
	if err != nil {
		return nil, err
	}
	if len(selected) == 0 {
		if event == "COMMENT" {
			return nil, nil
		}
		if event == "REQUEST_CHANGES" {
			return nil, fmt.Errorf("REQUEST_CHANGES needs at least one approved finding")
		}
	}
	user, err := q.Remote.Identity(ctx, q.User)
	if err != nil {
		return nil, err
	}
	p, diff, err := q.currentDiff(ctx, repo, pr, persist)
	if err != nil {
		return nil, err
	}
	if p.State != "open" || p.Draft {
		return nil, fmt.Errorf("publication requires an open, non-draft PR")
	}
	if event != "COMMENT" && strings.EqualFold(p.Author, user) {
		return nil, fmt.Errorf("cannot %s on your own PR", event)
	}
	var invalid []string
	var problems []string
	for _, f := range selected {
		var reason error
		switch {
		case f.RunComparisonKey != p.Key() || f.ApprovedComparisonKey == nil || *f.ApprovedComparisonKey != p.Key():
			reason = fmt.Errorf("approval is for an older comparison; rerun and approve current findings")
		default:
			reason = f.Finding.Validate()
			if reason == nil {
				reason = diff.Validate(f.Finding)
			}
		}
		if reason != nil {
			invalid = append(invalid, f.ID)
			problems = append(problems, fmt.Sprintf("%s: %s", f.ID, reason))
		}
	}
	if len(invalid) > 0 {
		if persist {
			if err := q.Store.ClearApprovals(ctx, invalid, user); err != nil {
				return nil, err
			}
		}
		return nil, fmt.Errorf("refusing publication: %s", strings.Join(problems, "; "))
	}
	sort.Slice(selected, func(i, j int) bool {
		a, b := selected[i], selected[j]
		at, ae := time.Parse(time.RFC3339Nano, a.CreatedAt)
		bt, be := time.Parse(time.RFC3339Nano, b.CreatedAt)
		if ae == nil && be == nil {
			if !at.Equal(bt) {
				return at.Before(bt)
			}
		} else if a.CreatedAt != b.CreatedAt {
			return a.CreatedAt < b.CreatedAt
		}
		return a.ID < b.ID
	})
	id := uuid.NewString()
	snapshot := store.PublicationSnapshot{SchemaVersion: 1, ID: id, User: user, Repo: repo, PR: pr, ComparisonKey: p.Key(), HeadSHA: p.HeadSHA, Event: event, Items: []store.PublicationItem{}, Marker: store.PublicationMarker(id)}
	for _, f := range selected {
		snapshot.Items = append(snapshot.Items, store.PublicationItem{ID: f.ID, Kind: f.Kind, Body: f.Body, Anchor: f.Anchor, RunComparisonKey: f.RunComparisonKey, ApprovedComparisonKey: *f.ApprovedComparisonKey})
	}
	snapshot.Request, err = snapshot.Render()
	if err != nil {
		return nil, err
	}
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}
	return &snapshot, nil
}
