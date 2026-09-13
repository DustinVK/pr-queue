package queue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/DustinVK/pr-queue/internal/github"
	"github.com/DustinVK/pr-queue/internal/lock"
	"github.com/DustinVK/pr-queue/internal/store"
)

type PublishRemote interface {
	Remote
	SendReview(context.Context, string, int, github.ReviewRequest) (github.Review, error)
	FetchReview(context.Context, string, int, string) (github.Review, error)
	ListReviews(context.Context, string, int) ([]github.Review, error)
	ListReviewComments(context.Context, string, int, string) ([]github.SubmittedComment, error)
}
type PublicationResult struct {
	Noop        bool               `json:"noop"`
	Publication *store.Publication `json:"publication,omitempty"`
}

func (q Service) Publish(ctx context.Context, ref, event string) (PublicationResult, error) {
	var result PublicationResult
	repo, pr, err := ParsePR(ref)
	if err != nil {
		return result, err
	}
	l, err := lock.Acquire(lock.PRPath(q.StateDir, repo, pr), ref+" publish")
	if err != nil {
		return result, err
	}
	defer l.Close()
	snapshot, err := q.publicationCandidate(ctx, repo, pr, event, true)
	if err != nil {
		return result, err
	}
	if snapshot == nil {
		return PublicationResult{Noop: true}, nil
	}
	remote, ok := q.Remote.(PublishRemote)
	if !ok {
		return result, fmt.Errorf("GitHub client cannot publish or reconcile reviews")
	}
	p, err := q.Store.PreparePublication(ctx, *snapshot)
	if err != nil {
		return result, err
	}
	return q.sendPrepared(ctx, remote, p)
}

func (q Service) sendPrepared(ctx context.Context, remote PublishRemote, p store.Publication) (PublicationResult, error) {
	if err := q.Store.MarkSending(ctx, p.ID); err != nil {
		return PublicationResult{Publication: &p}, err
	}
	review, err := remote.SendReview(ctx, p.Snapshot.Repo, p.Snapshot.PR, p.Snapshot.Request)
	if err != nil {
		var submission *github.SubmissionError
		definite := errors.As(err, &submission) && submission.Definite
		return q.publicationFailure(p, err, !definite, review.ID)
	}
	if err := review.Matches(p.Snapshot.User, p.Snapshot.Request); err != nil {
		return q.publicationFailure(p, err, true, review.ID)
	}
	comments, err := remote.ListReviewComments(ctx, p.Snapshot.Repo, p.Snapshot.PR, review.ID)
	if err == nil {
		err = github.MatchComments(comments, review.ID, p.Snapshot.User, p.Snapshot.Request)
	}
	if err != nil {
		return q.publicationFailure(p, err, true, review.ID)
	}
	return q.finalize(p, review.ID)
}

func (q Service) publicationFailure(p store.Publication, cause error, uncertain bool, reviewID string) (PublicationResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var known *string
	if reviewID != "" {
		known = findings.Ptr(reviewID)
	}
	err := q.Store.FailPublication(ctx, p.ID, cause, uncertain, known)
	current, readErr := q.Store.Publication(ctx, p.ID)
	if readErr == nil {
		p = current
	}
	if uncertain {
		cause = fmt.Errorf("publication %s may have been sent; use prq publish %s#%d --resume: %w", p.ID, p.Snapshot.Repo, p.Snapshot.PR, cause)
	}
	return PublicationResult{Publication: &p}, errors.Join(cause, err, readErr)
}

func (q Service) finalize(p store.Publication, reviewID string) (PublicationResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := q.Store.FinalizePublication(ctx, p.ID, reviewID); err != nil {
		return q.publicationFailure(p, fmt.Errorf("confirmed review %s could not be finalized: %w", reviewID, err), true, reviewID)
	}
	current, err := q.Store.Publication(ctx, p.ID)
	return PublicationResult{Publication: &current}, err
}

func (q Service) ResumePublication(ctx context.Context, ref string, confirmedNotSent bool) (PublicationResult, error) {
	var result PublicationResult
	repo, pr, err := ParsePR(ref)
	if err != nil {
		return result, err
	}
	l, err := lock.Acquire(lock.PRPath(q.StateDir, repo, pr), ref+" resume publication")
	if err != nil {
		return result, err
	}
	defer l.Close()
	p, err := q.Store.BlockingPublication(ctx, repo, pr)
	if err != nil {
		return result, err
	}
	if p == nil {
		return PublicationResult{Noop: true}, nil
	}
	result.Publication = p
	// Check config before the identity request, then require the actual account
	// to match the recorded one before all other remote operations.
	if !strings.EqualFold(q.User, p.Snapshot.User) {
		return result, fmt.Errorf("configured user %q differs from publication user %q", q.User, p.Snapshot.User)
	}
	if _, err := q.Remote.Identity(ctx, p.Snapshot.User); err != nil {
		return result, err
	}
	remote, ok := q.Remote.(PublishRemote)
	if !ok {
		return result, fmt.Errorf("GitHub client cannot publish or reconcile reviews")
	}
	if p.Status == "prepared" {
		if confirmedNotSent {
			return q.clearPublication(ctx, *p, "explicitly cleared known-unsent prepared publication")
		}
		return q.resumePrepared(ctx, remote, *p)
	}
	if p.Status == "sending" {
		if err := q.Store.FailPublication(ctx, p.ID, fmt.Errorf("previous sender ended without a confirmed outcome"), true, p.ReviewID); err != nil {
			return result, err
		}
	}
	// Reconcile before fetching current PR state: a submitted review stays sent
	// even after the PR changes, becomes draft, or closes.
	var review *github.Review
	if p.ReviewID != nil {
		r, err := remote.FetchReview(ctx, repo, pr, *p.ReviewID)
		if err != nil {
			return q.publicationFailure(*p, err, true, "")
		}
		review = &r
	} else {
		reviews, err := remote.ListReviews(ctx, repo, pr)
		if err != nil {
			return q.publicationFailure(*p, err, true, "")
		}
		var matches []github.Review
		for _, r := range reviews {
			if strings.Contains(r.Body, p.Marker) {
				matches = append(matches, r)
			}
		}
		if len(matches) > 1 {
			return q.publicationFailure(*p, fmt.Errorf("multiple reviews contain publication marker %s", p.Marker), true, "")
		}
		if len(matches) == 1 {
			review = &matches[0]
		}
	}
	if review == nil {
		if confirmedNotSent {
			return q.clearPublication(ctx, *p, "user confirmed not sent after a complete marker search; duplicate risk acknowledged")
		}
		return q.publicationFailure(*p, fmt.Errorf("complete review list contains no matching marker; inspect the PR before --confirmed-not-sent"), true, "")
	}
	if err := review.Matches(p.Snapshot.User, p.Snapshot.Request); err != nil {
		return q.publicationFailure(*p, err, true, review.ID)
	}
	comments, err := remote.ListReviewComments(ctx, repo, pr, review.ID)
	if err == nil {
		err = github.MatchComments(comments, review.ID, p.Snapshot.User, p.Snapshot.Request)
	}
	if err != nil {
		return q.publicationFailure(*p, err, true, review.ID)
	}
	return q.finalize(*p, review.ID)
}

func (q Service) clearPublication(ctx context.Context, p store.Publication, reason string) (PublicationResult, error) {
	if err := q.Store.ClearPublication(ctx, p.ID, q.User, reason); err != nil {
		return PublicationResult{Publication: &p}, err
	}
	current, err := q.Store.Publication(ctx, p.ID)
	return PublicationResult{Publication: &current}, err
}

func (q Service) resumePrepared(ctx context.Context, remote PublishRemote, p store.Publication) (PublicationResult, error) {
	live, diff, err := q.currentDiff(ctx, p.Snapshot.Repo, p.Snapshot.PR, true)
	if live.Comparison.Validate() == nil && live.Key() != p.Snapshot.ComparisonKey {
		return q.publicationFailure(p, fmt.Errorf("prepared comparison is stale; create a new publication after reviewing current code"), false, "")
	}
	if err != nil {
		return PublicationResult{Publication: &p}, err
	}
	if live.State != "open" || live.Draft || (p.Event != "COMMENT" && strings.EqualFold(live.Author, p.Snapshot.User)) {
		return q.publicationFailure(p, fmt.Errorf("prepared publication no longer meets PR eligibility checks"), false, "")
	}
	current, err := q.Store.ListFindings(ctx, p.Snapshot.Repo, "", p.Snapshot.PR)
	if err != nil {
		return PublicationResult{Publication: &p}, err
	}
	byID := map[string]store.Finding{}
	snapshotIDs := map[string]bool{}
	for _, item := range p.Snapshot.Items {
		snapshotIDs[item.ID] = true
	}
	var stale []string
	for _, f := range current {
		byID[f.ID] = f
		if f.Status == "approved" && !snapshotIDs[f.ID] {
			stale = append(stale, f.ID)
		}
	}
	for _, item := range p.Snapshot.Items {
		f, ok := byID[item.ID]
		if !ok || !item.Matches(f) || diff.Validate(f.Finding) != nil {
			stale = append(stale, item.ID)
		}
	}
	if len(stale) > 0 {
		var clear []string
		for _, id := range stale {
			if f, ok := byID[id]; ok && f.Status == "approved" && f.RunComparisonKey == p.Snapshot.ComparisonKey && diff.Validate(f.Finding) != nil {
				clear = append(clear, id)
			}
		}
		if len(clear) > 0 {
			if err := q.Store.ClearApprovals(ctx, clear, q.User); err != nil {
				return PublicationResult{Publication: &p}, err
			}
		}
		return q.publicationFailure(p, fmt.Errorf("prepared findings changed: %s", strings.Join(stale, ", ")), false, "")
	}
	return q.sendPrepared(ctx, remote, p)
}
