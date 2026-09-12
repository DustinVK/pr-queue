package queue

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/DustinVK/pr-queue/internal/lock"
	"github.com/DustinVK/pr-queue/internal/runner"
	"github.com/DustinVK/pr-queue/internal/store"
)

func triageFixture(t *testing.T) (Coordinator, Service, *fakeRemote, []store.Finding) {
	t.Helper()
	c, remote := runCoordinator(t)
	remote.diff = findings.Diff{}
	remote.diff.AddPatch("value.txt", "@@ -1,2 +1,2 @@\n-old\n+new\n context")
	c.Reviewer = reviewFunc(func(_ context.Context, r runner.Request) (runner.Result, error) {
		result := fakeReview(r)
		result.Document.Findings = append(result.Document.Findings, findings.Finding{Kind: "inline", Severity: "minor", Category: "correctness", Title: "inline", Body: "  Keep exact Markdown.\n", Anchor: findings.Anchor{Path: findings.Ptr("value.txt"), Side: findings.Ptr("RIGHT"), Line: findings.Ptr(2), StartLine: findings.Ptr(1), StartSide: findings.Ptr("RIGHT")}})
		return result, nil
	})
	r, err := c.Run(t.Context(), repos(), 0)
	if err != nil {
		t.Fatal(err)
	}
	q := Service{Store: c.Store, Remote: remote, User: "reviewer", StateDir: c.StateDir}
	return c, q, remote, r.Repos[0].Reviews[0].Ingestion.Findings
}

func auditCount(t *testing.T, s *store.Store) int {
	t.Helper()
	var count int
	if err := s.DB.QueryRow("SELECT count(*) FROM audit_log").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
func getFinding(t *testing.T, s *store.Store, id string) store.Finding {
	t.Helper()
	f, err := s.ResolveFinding(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestApprovalExactBodyAndRejectedTransition(t *testing.T) {
	_, q, _, fs := triageFixture(t)
	id := fs[2].ID
	if _, err := q.Reject(t.Context(), []string{id[:8]}, "already covered"); err != nil {
		t.Fatal(err)
	}
	approved, err := q.Approve(t.Context(), []string{id, id[:8]})
	if err != nil || len(approved) != 1 {
		t.Fatalf("approve: %+v %v", approved, err)
	}
	f := approved[0]
	if f.Status != "approved" || f.ApprovedComparisonKey == nil || *f.ApprovedComparisonKey != f.RunComparisonKey || f.Body != fs[2].Body {
		t.Fatalf("wrong approval: %+v", f)
	}
	var body string
	if err := q.Store.DB.QueryRow("SELECT body_snapshot FROM audit_log WHERE finding_id=? AND action='approved'", id).Scan(&body); err != nil || body != fs[2].Body {
		t.Fatalf("approval audit: %q %v", body, err)
	}
	if f := getFinding(t, q.Store, fs[0].ID); f.Status != "pending" {
		t.Fatal("summary was approved implicitly")
	}
}

func TestApprovalBatchRollbackAndMixedPRs(t *testing.T) {
	_, q, remote, fs := triageFixture(t)
	if _, err := q.Store.DB.Exec("UPDATE findings SET status='blocked' WHERE id=?", fs[2].ID); err != nil {
		t.Fatal(err)
	}
	before := auditCount(t, q.Store)
	if _, err := q.Approve(t.Context(), []string{fs[1].ID, fs[2].ID}); err == nil {
		t.Fatal("blocked batch approved")
	}
	if f := getFinding(t, q.Store, fs[1].ID); f.Status != "pending" || f.ApprovedComparisonKey != nil {
		t.Fatal("partial batch committed")
	}
	if auditCount(t, q.Store) != before {
		t.Fatal("partial batch audit committed")
	}
	seedApproved(t, q.Store, remotePR(2))
	other, err := q.Store.ListFindings(t.Context(), "owner/repo", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	fetched := len(remote.fetched)
	if _, err := q.Reject(t.Context(), []string{fs[1].ID, other[0].ID}, "mixed"); err == nil {
		t.Fatal("mixed PR batch accepted")
	}
	if f := getFinding(t, q.Store, fs[1].ID); f.Status != "pending" {
		t.Fatal("mixed batch mutated")
	}
	if len(remote.fetched) != fetched {
		t.Fatal("mixed selection made remote calls")
	}
}

func TestApprovalObservesTransitionsEvenWhenRefused(t *testing.T) {
	for _, change := range []string{"draft", "closed", "merged", "base", "head"} {
		t.Run(change, func(t *testing.T) {
			_, q, remote, fs := triageFixture(t)
			if _, err := q.Approve(t.Context(), []string{fs[1].ID}); err != nil {
				t.Fatal(err)
			}
			p := remote.fetch[1]
			switch change {
			case "draft":
				p.Draft = true
			case "closed", "merged":
				p.State = change
			case "base":
				p.BaseSHA = strings.Repeat("c", 40)
			case "head":
				p.HeadSHA = strings.Repeat("d", 40)
			}
			remote.fetch[1] = p
			if _, err := q.Approve(t.Context(), []string{fs[2].ID}); err == nil {
				t.Fatal("invalid comparison approved")
			}
			f := getFinding(t, q.Store, fs[1].ID)
			if f.Status != "pending" || f.ApprovedComparisonKey != nil {
				t.Fatal("stale approval survived refused command")
			}
			local, err := q.Store.PR(t.Context(), p.Repo, p.Number)
			if err != nil || local.Key() != p.Key() || local.LastReviewedKey != nil {
				t.Fatal("changed observation lost")
			}
		})
	}
}

func TestApprovalRefusesIdentityFailureAndChangingDiff(t *testing.T) {
	_, q, remote, fs := triageFixture(t)
	before := auditCount(t, q.Store)
	remote.identityErr = errors.New("identity mismatch")
	if _, err := q.Approve(t.Context(), []string{fs[1].ID}); err == nil {
		t.Fatal("wrong user approved")
	}
	if auditCount(t, q.Store) != before {
		t.Fatal("identity failure mutated audit")
	}
	remote.identityErr = nil
	if _, err := q.Approve(t.Context(), []string{fs[1].ID}); err != nil {
		t.Fatal(err)
	}
	remote.afterFetch = func() { p := remote.fetch[1]; p.BaseSHA = strings.Repeat("e", 40); remote.fetch[1] = p }
	if _, err := q.Approve(t.Context(), []string{fs[2].ID}); err == nil || !strings.Contains(err.Error(), "changed while") {
		t.Fatalf("moving diff: %v", err)
	}
	if f := getFinding(t, q.Store, fs[1].ID); f.Status != "pending" {
		t.Fatal("moving diff left stale approval")
	}
}

func TestApproveRevalidatesPendingAnchor(t *testing.T) {
	_, q, _, fs := triageFixture(t)
	if _, err := q.Store.DB.Exec("UPDATE findings SET line=99 WHERE id=?", fs[2].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Approve(t.Context(), []string{fs[2].ID}); err == nil {
		t.Fatal("invalid pending anchor approved")
	}
}

func TestEditorCancellationAndUnchangedSavePreserveEverything(t *testing.T) {
	_, q, remote, fs := triageFixture(t)
	if _, err := q.Approve(t.Context(), []string{fs[2].ID}); err != nil {
		t.Fatal(err)
	}
	before := auditCount(t, q.Store)
	remote.identityErr = errors.New("must not need a remote read for an unchanged edit")
	unchanged := func(_ context.Context, body string) (string, error) { return body, nil }
	r, err := q.Edit(t.Context(), fs[2].ID, false, unchanged)
	if err != nil || r.Changed || r.Finding.Status != "approved" {
		t.Fatalf("unchanged: %+v %v", r, err)
	}
	if _, err := q.Edit(t.Context(), fs[2].ID, true, func(context.Context, string) (string, error) { return "partial change", errors.New("cancelled") }); err == nil {
		t.Fatal("cancel error lost")
	}
	if f := getFinding(t, q.Store, fs[2].ID); f.Status != "approved" || f.Body != fs[2].Body || !f.Anchor.Equal(fs[2].Anchor) {
		t.Fatal("cancel mutated finding")
	}
	if auditCount(t, q.Store) != before {
		t.Fatal("unchanged/cancelled edit wrote audit")
	}
}

func TestEditClearsApprovalAuditsBodiesAndConvertsBlockedAnchor(t *testing.T) {
	_, q, _, fs := triageFixture(t)
	if _, err := q.Approve(t.Context(), []string{fs[2].ID}); err != nil {
		t.Fatal(err)
	}
	r, err := q.Edit(t.Context(), fs[2].ID, false, func(context.Context, string) (string, error) { return "edited\r\n", nil })
	if err != nil || !r.Changed || r.Finding.Status != "pending" || r.Finding.ApprovedComparisonKey != nil || r.Finding.Fingerprint == fs[2].Fingerprint {
		t.Fatalf("edit: %+v %v", r, err)
	}
	var original, edited int
	if err := q.Store.DB.QueryRow("SELECT count(*) FROM audit_log WHERE action='edited' AND body_snapshot=?", fs[2].Body).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if err := q.Store.DB.QueryRow("SELECT count(*) FROM audit_log WHERE action='edited' AND body_snapshot=?", "edited\r\n").Scan(&edited); err != nil {
		t.Fatal(err)
	}
	if original != 1 || edited != 1 {
		t.Fatal("edit did not preserve both bodies")
	}
	if _, err := q.Store.DB.Exec("UPDATE findings SET line=99,status='blocked',block_reason='invalid' WHERE id=?", fs[2].ID); err != nil {
		t.Fatal(err)
	}
	r, err = q.Edit(t.Context(), fs[2].ID, true, func(_ context.Context, body string) (string, error) { return body, nil })
	if err != nil || !r.Changed || r.Finding.Kind != "general" || !r.Finding.Anchor.Empty() || r.Finding.Status != "pending" || r.Finding.BlockReason != nil {
		t.Fatalf("conversion: %+v %v", r, err)
	}
	if _, err := q.Approve(t.Context(), []string{fs[2].ID}); err != nil {
		t.Fatal(err)
	}
}

func TestEditRollbackAndPublishedRefusal(t *testing.T) {
	_, q, _, fs := triageFixture(t)
	if _, err := q.Approve(t.Context(), []string{fs[1].ID}); err != nil {
		t.Fatal(err)
	}
	before := auditCount(t, q.Store)
	if _, err := q.Store.DB.Exec(`CREATE TRIGGER fail_edit BEFORE INSERT ON audit_log WHEN NEW.action='edited' AND NEW.body_snapshot='edited' BEGIN SELECT RAISE(ABORT,'injected audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Edit(t.Context(), fs[1].ID, false, func(context.Context, string) (string, error) { return "edited", nil }); err == nil {
		t.Fatal("injected failure ignored")
	}
	f := getFinding(t, q.Store, fs[1].ID)
	if f.Body != fs[1].Body || f.Status != "approved" || f.ApprovedComparisonKey == nil || auditCount(t, q.Store) != before {
		t.Fatal("edit partially committed")
	}
	if _, err := q.Store.DB.Exec("UPDATE findings SET status='published',approved_comparison_key=NULL WHERE id=?", fs[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Edit(t.Context(), fs[1].ID, false, func(context.Context, string) (string, error) {
		t.Error("opened editor on published finding")
		return "edited", nil
	}); err == nil {
		t.Fatal("published finding editable")
	}
}

func TestTriageDuringAgentAndOtherPRDuringEditor(t *testing.T) {
	c, q, _, fs := triageFixture(t)
	c.Reviewer = reviewFunc(func(ctx context.Context, r runner.Request) (runner.Result, error) {
		_, err := q.Reject(ctx, []string{fs[1].ID}, "reviewed while agent running")
		return fakeReview(r), err
	})
	run, err := c.Run(t.Context(), repos(), 1)
	if err != nil || run.Repos[0].Reviews[0].Ingestion.Findings[1].Status != "rejected" {
		t.Fatalf("concurrent triage: %+v %v", run, err)
	}
	seedApproved(t, q.Store, remotePR(2))
	other, err := q.Store.ListFindings(t.Context(), "owner/repo", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	_, err = q.Edit(t.Context(), fs[1].ID, false, func(ctx context.Context, body string) (string, error) {
		_, err := q.Reject(ctx, []string{fs[0].ID}, "")
		var busy *lock.BusyError
		if !errors.As(err, &busy) {
			t.Errorf("same PR not locked: %v", err)
		}
		if _, err := q.Reject(ctx, []string{other[0].ID}, "independent PR"); err != nil {
			return "", err
		}
		return body + " edited", nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
