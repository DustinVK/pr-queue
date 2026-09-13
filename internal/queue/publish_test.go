package queue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/DustinVK/pr-queue/internal/github"
	"github.com/DustinVK/pr-queue/internal/lock"
	"github.com/DustinVK/pr-queue/internal/store"
)

type publishRemote struct {
	*fakeRemote
	posts                           int
	mode                            string
	reviews                         []github.Review
	comments                        []github.SubmittedComment
	reads                           []string
	listErr, reviewErr, commentsErr error
	onSend                          func(github.ReviewRequest)
}

func (r *publishRemote) Identity(ctx context.Context, user string) (string, error) {
	r.reads = append(r.reads, "identity")
	return r.fakeRemote.Identity(ctx, user)
}
func (r *publishRemote) SendReview(_ context.Context, _ string, _ int, request github.ReviewRequest) (github.Review, error) {
	if r.onSend != nil {
		r.onSend(request)
	}
	if r.mode == "crash before request" {
		panic("simulated sender death")
	}
	r.posts++
	if r.mode == "rejected" {
		return github.Review{}, &github.SubmissionError{Cause: errors.New("validation rejected"), Definite: true}
	}
	if r.mode == "uncertain no match" {
		return github.Review{}, errors.New("connection dropped")
	}
	review := github.Review{ID: "123", User: "reviewer", CommitID: request.CommitID, Body: request.Body, State: map[string]string{"COMMENT": "COMMENTED", "APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED"}[request.Event], SubmittedAt: findings.Ptr("2026-09-12T12:00:00Z")}
	r.reviews = append(r.reviews, review)
	r.comments = []github.SubmittedComment{}
	for i, c := range request.Comments {
		r.comments = append(r.comments, github.SubmittedComment{ID: fmt.Sprint(i + 1), ReviewID: review.ID, User: review.User, OriginalCommitID: request.CommitID, ReviewComment: c})
	}
	if r.mode == "crash after request" {
		panic("simulated sender death")
	}
	if r.mode == "accepted response lost" {
		return github.Review{}, errors.New("response lost")
	}
	return review, nil
}
func (r *publishRemote) FetchReview(_ context.Context, _ string, _ int, id string) (github.Review, error) {
	r.reads = append(r.reads, "review")
	if r.reviewErr != nil {
		return github.Review{}, r.reviewErr
	}
	for _, review := range r.reviews {
		if review.ID == id {
			return review, nil
		}
	}
	return github.Review{}, errors.New("review not found")
}
func (r *publishRemote) ListReviews(context.Context, string, int) ([]github.Review, error) {
	r.reads = append(r.reads, "reviews")
	return r.reviews, r.listErr
}
func (r *publishRemote) ListReviewComments(context.Context, string, int, string) ([]github.SubmittedComment, error) {
	r.reads = append(r.reads, "comments")
	return r.comments, r.commentsErr
}

func publicationService(t *testing.T) (Service, *publishRemote, []store.Finding) {
	t.Helper()
	_, q, base, fs := triageFixture(t)
	remote := &publishRemote{fakeRemote: base, reviews: []github.Review{}, comments: []github.SubmittedComment{}}
	q.Remote = remote
	return q, remote, fs
}
func prepareTestPublication(t *testing.T, q Service, event string) store.Publication {
	t.Helper()
	preview, err := q.PreviewPublication(t.Context(), "owner/repo#1", event)
	if err != nil || preview.Snapshot == nil {
		t.Fatalf("prepare preview: %+v %v", preview, err)
	}
	p, err := q.Store.PreparePublication(t.Context(), *preview.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func approveTestItems(t *testing.T, q Service, fs []store.Finding) {
	t.Helper()
	ids := []string{}
	for _, f := range fs {
		ids = append(ids, f.ID)
	}
	if _, err := q.Approve(t.Context(), ids); err != nil {
		t.Fatal(err)
	}
}
func catchSenderDeath(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Error("expected simulated process death")
		}
	}()
	fn()
}

func TestPublishCommitsSendingBeforeSinglePOSTAndHoldsOnlyPRLock(t *testing.T) {
	q, remote, fs := publicationService(t)
	approveTestItems(t, q, fs)
	seedApproved(t, q.Store, remotePR(2))
	other, err := q.Store.ListFindings(t.Context(), "owner/repo", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	remote.onSend = func(request github.ReviewRequest) {
		p, err := q.Store.BlockingPublication(t.Context(), "owner/repo", 1)
		if err != nil || p == nil || p.Status != "sending" || p.Snapshot.Request.Body != request.Body {
			t.Errorf("sending was not durable: %+v %v", p, err)
		}
		if _, err := q.Store.DB.Exec("INSERT INTO audit_log(pr_id,at,actor,action) VALUES(1,'now','test','network_probe')"); err != nil {
			t.Error("database transaction held over POST:", err)
		}
		_, err = q.Reject(t.Context(), []string{fs[0].ID}, "")
		var busy *lock.BusyError
		if !errors.As(err, &busy) {
			t.Errorf("publisher did not hold PR lock: %v", err)
		}
		if _, err := q.Reject(t.Context(), []string{other[0].ID}, "other PR stays usable"); err != nil {
			t.Error(err)
		}
	}
	r, err := q.Publish(t.Context(), "owner/repo#1", "COMMENT")
	if err != nil || remote.posts != 1 || r.Publication == nil || r.Publication.Status != "published" {
		t.Fatalf("publish: %+v %d %v", r, remote.posts, err)
	}
	r, err = q.ResumePublication(t.Context(), "owner/repo#1", false)
	if err != nil || !r.Noop || remote.posts != 1 {
		t.Fatalf("resume after success: %+v %v", r, err)
	}
}

func TestLostResponseAndSenderDeathRecoverAfterPRCloses(t *testing.T) {
	for _, mode := range []string{"accepted response lost", "crash after request"} {
		t.Run(mode, func(t *testing.T) {
			q, remote, fs := publicationService(t)
			approveTestItems(t, q, fs)
			remote.mode = mode
			if strings.HasPrefix(mode, "crash") {
				catchSenderDeath(t, func() { q.Publish(t.Context(), "owner/repo#1", "COMMENT") })
			} else if _, err := q.Publish(t.Context(), "owner/repo#1", "COMMENT"); err == nil {
				t.Fatal("lost response accepted")
			}
			if _, err := q.Publish(t.Context(), "owner/repo#1", "COMMENT"); err == nil || remote.posts != 1 {
				t.Fatal("uncertain attempt allowed another POST")
			}
			closed := remote.fetch[1]
			closed.State = "closed"
			closed.HeadSHA = strings.Repeat("f", 40)
			closed.Draft = true
			remote.fetch[1] = closed
			fetches := len(remote.fetched)
			r, err := q.ResumePublication(t.Context(), "owner/repo#1", false)
			if err != nil || r.Publication == nil || r.Publication.Status != "published" || remote.posts != 1 || len(remote.fetched) != fetches {
				t.Fatalf("recovery: %+v posts=%d %v", r, remote.posts, err)
			}
		})
	}
}

func TestLostResponseDismissedApprovalReconciles(t *testing.T) {
	q, remote, fs := publicationService(t)
	approveTestItems(t, q, fs)
	remote.mode = "accepted response lost"
	if _, err := q.Publish(t.Context(), "owner/repo#1", "APPROVE"); err == nil {
		t.Fatal("expected lost response")
	}
	remote.reviews[0].State = "DISMISSED"
	remote.reviews[0].DismissedState = "APPROVED"
	r, err := q.ResumePublication(t.Context(), "owner/repo#1", false)
	if err != nil || r.Publication == nil || r.Publication.Status != "published" || remote.posts != 1 {
		t.Fatalf("dismissed approval recovery: %+v posts=%d %v", r, remote.posts, err)
	}
	if !strings.Contains(","+strings.Join(remote.reads, ",")+",", ",review,") {
		t.Fatalf("dismissed marker match was not fetched by ID: %v", remote.reads)
	}
}

func TestCrashBeforePOSTIsNeverBlindlyResent(t *testing.T) {
	q, remote, fs := publicationService(t)
	approveTestItems(t, q, fs)
	remote.mode = "crash before request"
	catchSenderDeath(t, func() { q.Publish(t.Context(), "owner/repo#1", "COMMENT") })
	if _, err := q.ResumePublication(t.Context(), "owner/repo#1", false); err == nil || remote.posts != 0 {
		t.Fatal("sending attempt resent")
	}
	p, err := q.Store.BlockingPublication(t.Context(), "owner/repo", 1)
	if err != nil || p == nil || p.Status != "failed" || !p.Uncertain {
		t.Fatalf("not blocked uncertain: %+v %v", p, err)
	}
}

func TestFailureBeforeSendingLeavesPreparedAndCanResumeOnce(t *testing.T) {
	q, remote, fs := publicationService(t)
	approveTestItems(t, q, fs)
	if _, err := q.Store.DB.Exec(`CREATE TRIGGER fail_sending BEFORE UPDATE OF status ON publications WHEN NEW.status='sending' BEGIN SELECT RAISE(ABORT,'injected death before sending'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Publish(t.Context(), "owner/repo#1", "COMMENT"); err == nil || remote.posts != 0 {
		t.Fatal("POST ran before sending commit")
	}
	p, err := q.Store.BlockingPublication(t.Context(), "owner/repo", 1)
	if err != nil || p == nil || p.Status != "prepared" {
		t.Fatalf("prepared not durable: %+v %v", p, err)
	}
	if _, err := q.Store.DB.Exec("DROP TRIGGER fail_sending"); err != nil {
		t.Fatal(err)
	}
	r, err := q.ResumePublication(t.Context(), "owner/repo#1", false)
	if err != nil || r.Publication.Status != "published" || remote.posts != 1 || remote.reviews[0].Body != p.Snapshot.Request.Body {
		t.Fatalf("resume prepared: %+v %v", r, err)
	}
}

func TestPreparationFailureMakesNoPOST(t *testing.T) {
	q, remote, fs := publicationService(t)
	approveTestItems(t, q, fs)
	if _, err := q.Store.DB.Exec(`CREATE TRIGGER fail_prepare BEFORE INSERT ON publications BEGIN SELECT RAISE(ABORT,'injected preparation failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Publish(t.Context(), "owner/repo#1", "COMMENT"); err == nil || remote.posts != 0 {
		t.Fatal("POST ran before preparation commit")
	}
	p, err := q.Store.BlockingPublication(t.Context(), "owner/repo", 1)
	if err != nil || p != nil {
		t.Fatalf("partial preparation: %+v %v", p, err)
	}
}

func TestDefiniteRejectionAllowsNewAttempt(t *testing.T) {
	q, remote, fs := publicationService(t)
	approveTestItems(t, q, fs)
	remote.mode = "rejected"
	r, err := q.Publish(t.Context(), "owner/repo#1", "COMMENT")
	if err == nil || r.Publication == nil || r.Publication.Status != "failed" || r.Publication.Uncertain {
		t.Fatalf("definite rejection: %+v %v", r, err)
	}
	firstID := r.Publication.ID
	remote.mode = ""
	r, err = q.Publish(t.Context(), "owner/repo#1", "COMMENT")
	if err != nil || r.Publication.ID == firstID || remote.posts != 2 {
		t.Fatalf("fresh publication: %+v %v", r, err)
	}
}

func TestFinalizationFailureRetainsKnownReviewAndResumesWithoutPOST(t *testing.T) {
	q, remote, fs := publicationService(t)
	approveTestItems(t, q, fs)
	if _, err := q.Store.DB.Exec(`CREATE TRIGGER fail_final BEFORE INSERT ON audit_log WHEN NEW.action='published' BEGIN SELECT RAISE(ABORT,'injected finalization failure'); END`); err != nil {
		t.Fatal(err)
	}
	r, err := q.Publish(t.Context(), "owner/repo#1", "COMMENT")
	if err == nil || r.Publication.ReviewID == nil || !r.Publication.Uncertain {
		t.Fatalf("known outcome: %+v %v", r, err)
	}
	if _, err := q.Store.DB.Exec("DROP TRIGGER fail_final"); err != nil {
		t.Fatal(err)
	}
	remote.reads = nil
	r, err = q.ResumePublication(t.Context(), "owner/repo#1", false)
	if err != nil || r.Publication.Status != "published" || remote.posts != 1 || strings.Join(remote.reads, ",") != "identity,review,comments" {
		t.Fatalf("known-ID recovery: %+v %v reads=%v", r, err, remote.reads)
	}
}

func TestKnownReviewReadFailureCannotBeManuallyCleared(t *testing.T) {
	q, remote, fs := publicationService(t)
	approveTestItems(t, q, fs)
	remote.commentsErr = errors.New("lost comment response after successful POST")
	r, err := q.Publish(t.Context(), "owner/repo#1", "COMMENT")
	if err == nil || r.Publication.ReviewID == nil {
		t.Fatal("known review ID was not retained")
	}
	remote.reviewErr = errors.New("review read returned 404")
	r, err = q.ResumePublication(t.Context(), "owner/repo#1", true)
	if err == nil || !r.Publication.Uncertain || remote.posts != 1 {
		t.Fatalf("known review read failure cleared: %+v %v", r, err)
	}
}

func TestPreparedSnapshotStalenessIncludesEmptyApproval(t *testing.T) {
	for _, change := range []string{"empty approval head changed", "body changed", "anchor changed", "rejected"} {
		t.Run(change, func(t *testing.T) {
			q, remote, fs := publicationService(t)
			event := "COMMENT"
			if strings.HasPrefix(change, "empty") {
				event = "APPROVE"
			} else {
				approveTestItems(t, q, fs)
			}
			p := prepareTestPublication(t, q, event)
			switch change {
			case "empty approval head changed":
				live := remote.fetch[1]
				live.HeadSHA = strings.Repeat("f", 40)
				remote.fetch[1] = live
			case "body changed":
				if _, err := q.Edit(t.Context(), fs[1].ID, false, func(context.Context, string) (string, error) { return "later body", nil }); err != nil {
					t.Fatal(err)
				}
			case "anchor changed":
				if _, err := q.Store.DB.Exec("UPDATE findings SET start_side='LEFT' WHERE id=?", fs[2].ID); err != nil {
					t.Fatal(err)
				}
			case "rejected":
				if _, err := q.Reject(t.Context(), []string{fs[1].ID}, "later rejection"); err != nil {
					t.Fatal(err)
				}
			}
			r, err := q.ResumePublication(t.Context(), "owner/repo#1", false)
			if err == nil || remote.posts != 0 || r.Publication.Status != "failed" || r.Publication.Uncertain {
				t.Fatalf("stale prepared: %+v %v", r, err)
			}
			stored, err := q.Store.Publication(t.Context(), p.ID)
			if err != nil || stored.Snapshot.Request.Body != p.Snapshot.Request.Body {
				t.Fatal("stale snapshot changed")
			}
		})
	}
}

func TestPreparedSnapshotRejectsNewApprovals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event string
		seed  bool
	}{
		{name: "comment", event: "COMMENT", seed: true},
		{name: "empty approval", event: "APPROVE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, remote, fs := publicationService(t)
			if tc.seed {
				if _, err := q.Approve(t.Context(), []string{fs[0].ID}); err != nil {
					t.Fatal(err)
				}
			}
			prepared := prepareTestPublication(t, q, tc.event)
			if _, err := q.Approve(t.Context(), []string{fs[1].ID}); err != nil {
				t.Fatal(err)
			}

			result, err := q.ResumePublication(t.Context(), "owner/repo#1", false)
			if err == nil || remote.posts != 0 {
				t.Fatalf("stale prepared selection was sent: result=%+v posts=%d err=%v", result, remote.posts, err)
			}
			stored, err := q.Store.Publication(t.Context(), prepared.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != "failed" || stored.Uncertain {
				t.Fatalf("stale prepared selection not failed definitely: %+v", stored)
			}
		})
	}
}

func TestResumeIdentityBeforeOtherRemoteOperations(t *testing.T) {
	q, remote, fs := publicationService(t)
	approveTestItems(t, q, fs)
	prepareTestPublication(t, q, "COMMENT")
	remote.reads = nil
	q.User = "different-user"
	if _, err := q.ResumePublication(t.Context(), "owner/repo#1", false); err == nil || len(remote.reads) != 0 {
		t.Fatal("config mismatch made remote calls")
	}
	q.User = "reviewer"
	remote.identityErr = errors.New("authenticated identity mismatch")
	if _, err := q.ResumePublication(t.Context(), "owner/repo#1", true); err == nil || strings.Join(remote.reads, ",") != "identity" || remote.posts != 0 {
		t.Fatal("wrong account reached resume operation")
	}
}

func TestConfirmedNotSentClearsOnlyPreparedOrCompleteNoMatch(t *testing.T) {
	for _, prepared := range []bool{true, false} {
		t.Run(fmt.Sprint(prepared), func(t *testing.T) {
			q, remote, fs := publicationService(t)
			approveTestItems(t, q, fs)
			if prepared {
				prepareTestPublication(t, q, "COMMENT")
			} else {
				remote.mode = "uncertain no match"
				if _, err := q.Publish(t.Context(), "owner/repo#1", "COMMENT"); err == nil {
					t.Fatal("expected unknown outcome")
				}
			}
			posts, fetches := remote.posts, len(remote.fetched)
			remote.reads = nil
			r, err := q.ResumePublication(t.Context(), "owner/repo#1", true)
			if err != nil || r.Publication.Status != "failed" || r.Publication.Uncertain || remote.posts != posts || len(remote.fetched) != fetches {
				t.Fatalf("clear: %+v %v", r, err)
			}
			var count int
			if err := q.Store.DB.QueryRow("SELECT count(*) FROM audit_log WHERE action='publication_cleared'").Scan(&count); err != nil || count != 1 {
				t.Fatal("clear not audited")
			}
			if prepared && strings.Join(remote.reads, ",") != "identity" {
				t.Fatal("known-unsent clear needed remote review reads")
			}
		})
	}
}

func TestConfirmedNotSentFinalizesMatchesAndRefusesConflicts(t *testing.T) {
	for _, change := range []string{"match", "multiple markers", "wrong body", "wrong author", "wrong event", "wrong commit", "wrong comment", "missing comment", "read failure", "comment read failure"} {
		t.Run(change, func(t *testing.T) {
			q, remote, fs := publicationService(t)
			approveTestItems(t, q, fs)
			remote.mode = "accepted response lost"
			if _, err := q.Publish(t.Context(), "owner/repo#1", "COMMENT"); err == nil {
				t.Fatal("expected lost response")
			}
			switch change {
			case "multiple markers":
				other := remote.reviews[0]
				other.ID = "124"
				remote.reviews = append(remote.reviews, other)
			case "wrong body":
				remote.reviews[0].Body = "unexpected " + remote.reviews[0].Body
			case "wrong author":
				remote.reviews[0].User = "other"
			case "wrong event":
				remote.reviews[0].State = "PENDING"
			case "wrong commit":
				remote.reviews[0].CommitID = strings.Repeat("e", 40)
			case "wrong comment":
				remote.comments[0].Body = "changed"
			case "missing comment":
				remote.comments = nil
			case "read failure":
				remote.listErr = errors.New("incomplete review list")
			case "comment read failure":
				remote.commentsErr = errors.New("incomplete comments")
			}
			r, err := q.ResumePublication(t.Context(), "owner/repo#1", true)
			if change == "match" {
				if err != nil || r.Publication.Status != "published" {
					t.Fatalf("matching override: %+v %v", r, err)
				}
			} else {
				if err == nil || r.Publication == nil || !r.Publication.Uncertain {
					t.Fatalf("conflict cleared: %+v %v", r, err)
				}
			}
			if remote.posts != 1 {
				t.Fatal("recovery sent another POST")
			}
		})
	}
}
