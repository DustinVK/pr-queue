package queue

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/DustinVK/pr-queue/internal/store"
)

func TestPreviewOnlyApprovedPublicTextAndIndependentSummary(t *testing.T) {
	_, q, _, fs := triageFixture(t)
	if _, err := q.Approve(t.Context(), []string{fs[1].ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Reject(t.Context(), []string{fs[2].ID}, "private reason"); err != nil {
		t.Fatal(err)
	}
	preview, err := q.PreviewPublication(t.Context(), "owner/repo#1", "COMMENT")
	if err != nil {
		t.Fatal(err)
	}
	s := preview.Snapshot
	if s == nil || s.Request.Body != fs[1].Body+"\n\n"+s.Marker || len(s.Request.Comments) != 0 || len(s.Items) != 1 {
		t.Fatalf("request: %+v", s)
	}
	if s.Request.CommitID != remotePR(1).HeadSHA {
		t.Fatal("request did not pin head")
	}
	if _, err := q.Approve(t.Context(), []string{fs[0].ID, fs[2].ID}); err != nil {
		t.Fatal(err)
	}
	preview, err = q.PreviewPublication(t.Context(), "owner/repo#1", "REQUEST_CHANGES")
	if err != nil {
		t.Fatal(err)
	}
	s = preview.Snapshot
	if s.Request.Body != fs[0].Body+"\n\n"+fs[1].Body+"\n\n"+s.Marker || len(s.Request.Comments) != 1 || s.Request.Comments[0].Body != fs[2].Body || !s.Request.Comments[0].Anchor.Equal(fs[2].Anchor) {
		t.Fatalf("independent summary/inline: %+v", s.Request)
	}
	raw, err := json.Marshal(s.Request)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{`"title"`, `"rationale"`, `"verdict"`, `"severity"`, `"category"`, "private reason"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("private metadata leaked: %s", raw)
		}
	}
}

func TestPreviewOrderingPreservesWhitespaceAndUUIDTies(t *testing.T) {
	_, q, _, fs := triageFixture(t)
	// Add another general item with the same creation time and a lower UUID.
	id := "00000000-0000-4000-8000-000000000001"
	_, err := q.Store.DB.Exec(`INSERT INTO findings(id,pr_id,review_run_id,kind,severity,category,title,body,fingerprint,status,created_at) SELECT ?,pr_id,review_run_id,kind,severity,category,'PRIVATE','first  '||char(10),'new-fingerprint','pending','tie' FROM findings WHERE id=?`, id, fs[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Store.DB.Exec("UPDATE findings SET created_at='tie' WHERE id=?", fs[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Store.DB.Exec("UPDATE findings SET created_at='zzz' WHERE id=?", fs[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Approve(t.Context(), []string{fs[1].ID, id, fs[0].ID}); err != nil {
		t.Fatal(err)
	}
	preview, err := q.PreviewPublication(t.Context(), "owner/repo#1", "COMMENT")
	if err != nil {
		t.Fatal(err)
	}
	want := fs[0].Body + "\n\nfirst  \n\n\n" + fs[1].Body + "\n\n" + preview.Snapshot.Marker
	if preview.Snapshot.Request.Body != want {
		t.Fatalf("order/bytes:\n%q\n%q", preview.Snapshot.Request.Body, want)
	}
}

func TestPreviewOrdersFractionalTimestampsChronologically(t *testing.T) {
	_, q, _, fs := triageFixture(t)
	id := "00000000-0000-4000-8000-000000000001"
	if _, err := q.Store.DB.Exec("UPDATE findings SET created_at='2026-09-12T12:00:00.1Z' WHERE id=?", fs[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Store.DB.Exec(`INSERT INTO findings(id,pr_id,review_run_id,kind,severity,category,title,body,fingerprint,status,created_at) SELECT ?,pr_id,review_run_id,kind,severity,category,title,'second','second','pending','2026-09-12T12:00:00.11Z' FROM findings WHERE id=?`, id, fs[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Approve(t.Context(), []string{id, fs[1].ID}); err != nil {
		t.Fatal(err)
	}
	p, err := q.PreviewPublication(t.Context(), "owner/repo#1", "COMMENT")
	if err != nil {
		t.Fatal(err)
	}
	want := fs[1].Body + "\n\nsecond\n\n" + p.Snapshot.Marker
	if p.Snapshot.Request.Body != want {
		t.Fatalf("fractional timestamps reordered bodies: %q", p.Snapshot.Request.Body)
	}
}

func TestEmptyEventsAndBlockingBeforeNoop(t *testing.T) {
	_, q, remote, _ := triageFixture(t)
	r, err := q.PreviewPublication(t.Context(), "owner/repo#1", "COMMENT")
	if err != nil || !r.Noop || r.Snapshot != nil {
		t.Fatalf("empty comment: %+v %v", r, err)
	}
	if _, err := q.PreviewPublication(t.Context(), "owner/repo#1", "REQUEST_CHANGES"); err == nil {
		t.Fatal("empty request changes accepted")
	}
	r, err = q.PreviewPublication(t.Context(), "owner/repo#1", "APPROVE")
	if err != nil || r.Noop || len(r.Snapshot.Items) != 0 || r.Snapshot.Request.Body != r.Snapshot.Marker {
		t.Fatalf("empty approval: %+v %v", r, err)
	}
	if _, err := q.Store.PreparePublication(t.Context(), *r.Snapshot); err != nil {
		t.Fatal(err)
	}
	remote.identityErr = context.Canceled
	if _, err := q.PreviewPublication(t.Context(), "owner/repo#1", "COMMENT"); err == nil || !strings.Contains(err.Error(), "--resume") {
		t.Fatalf("blocking no-op: %v", err)
	}
}

func TestPreviewOwnPRAndBodyOverflow(t *testing.T) {
	_, q, remote, fs := triageFixture(t)
	p := remote.fetch[1]
	p.Author = "REVIEWER"
	remote.fetch[1] = p
	if _, err := q.PreviewPublication(t.Context(), "owner/repo#1", "APPROVE"); err == nil {
		t.Fatal("own PR empty approval accepted")
	}
	if _, err := q.Approve(t.Context(), []string{fs[1].ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.PreviewPublication(t.Context(), "owner/repo#1", "REQUEST_CHANGES"); err == nil {
		t.Fatal("own PR request changes accepted")
	}
	if _, err := q.PreviewPublication(t.Context(), "owner/repo#1", "COMMENT"); err != nil {
		t.Fatal(err)
	}
	_, err := q.Edit(t.Context(), fs[1].ID, false, func(context.Context, string) (string, error) { return strings.Repeat("x", findings.MaxBodyBytes), nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Approve(t.Context(), []string{fs[1].ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.PreviewPublication(t.Context(), "owner/repo#1", "COMMENT"); err == nil || !strings.Contains(err.Error(), "assembled") {
		t.Fatalf("body limit ignored: %v", err)
	}
}

func TestStalePreviewIsReadOnlyAndPreparationInvalidates(t *testing.T) {
	_, q, remote, fs := triageFixture(t)
	if _, err := q.Approve(t.Context(), []string{fs[1].ID}); err != nil {
		t.Fatal(err)
	}
	before := auditCount(t, q.Store)
	p := remote.fetch[1]
	p.BaseSHA = strings.Repeat("e", 40)
	remote.fetch[1] = p
	if _, err := q.PreviewPublication(t.Context(), "owner/repo#1", "COMMENT"); err == nil {
		t.Fatal("stale preview accepted")
	}
	if f := getFinding(t, q.Store, fs[1].ID); f.Status != "approved" {
		t.Fatal("preview persisted invalidation")
	}
	local, err := q.Store.PR(t.Context(), p.Repo, p.Number)
	if err != nil || local.BaseSHA == p.BaseSHA || auditCount(t, q.Store) != before {
		t.Fatal("preview persisted observation/audit")
	}
	if _, err := q.publicationCandidate(t.Context(), p.Repo, p.Number, "COMMENT", true); err == nil {
		t.Fatal("stale publication preparation accepted")
	}
	if f := getFinding(t, q.Store, fs[1].ID); f.Status != "pending" || f.ApprovedComparisonKey != nil {
		t.Fatal("preparation left stale approval")
	}
	var count int
	if err := q.Store.DB.QueryRow("SELECT count(*) FROM publications").Scan(&count); err != nil || count != 0 {
		t.Fatal("failed validation prepared a publication")
	}
}

func TestFinalizationPreservesLaterEditsAndDecisions(t *testing.T) {
	_, q, _, fs := triageFixture(t)
	if _, err := q.Approve(t.Context(), []string{fs[0].ID, fs[1].ID, fs[2].ID}); err != nil {
		t.Fatal(err)
	}
	preview, err := q.PreviewPublication(t.Context(), "owner/repo#1", "COMMENT")
	if err != nil {
		t.Fatal(err)
	}
	p, err := q.Store.PreparePublication(t.Context(), *preview.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Store.MarkSending(t.Context(), p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Edit(t.Context(), fs[1].ID, false, func(context.Context, string) (string, error) { return "later approved body", nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Approve(t.Context(), []string{fs[1].ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Reject(t.Context(), []string{fs[2].ID}, "later rejection"); err != nil {
		t.Fatal(err)
	}
	if err := q.Store.FinalizePublication(t.Context(), p.ID, "123"); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"published", "approved", "rejected"} {
		f := getFinding(t, q.Store, fs[i].ID)
		if f.Status != want {
			t.Fatalf("finding %d: %+v", i, f)
		}
		var body string
		if err := q.Store.DB.QueryRow("SELECT body_snapshot FROM audit_log WHERE finding_id=? AND publication_id=? AND action='published'", f.ID, p.ID).Scan(&body); err != nil || body != fs[i].Body {
			t.Fatalf("sent audit uses later body: %q %v", body, err)
		}
	}
	before := auditCount(t, q.Store)
	if err := q.Store.FinalizePublication(t.Context(), p.ID, "123"); err != nil {
		t.Fatal(err)
	}
	if auditCount(t, q.Store) != before {
		t.Fatal("duplicate finalization audit")
	}
	show, err := q.Show(t.Context(), "owner/repo#1")
	if err != nil || len(show.Publications) != 1 {
		t.Fatalf("history missing: %+v %v", show, err)
	}
	var sent store.PublicationSnapshot
	if err := json.Unmarshal([]byte(show.Publications[0].Snapshot), &sent); err != nil || sent.Items[1].Body != fs[1].Body {
		t.Fatal("historical snapshot changed")
	}
}
