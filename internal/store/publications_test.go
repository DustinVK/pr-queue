package store

import (
	"sync"
	"testing"

	"github.com/google/uuid"
)

func publicationFixture(t *testing.T) (*Store, PublicationSnapshot) {
	t.Helper()
	s, p, doc, diff := runFixture(t)
	r := ingestTest(t, s, p, doc, diff)
	id := uuid.NewString()
	snapshot := PublicationSnapshot{SchemaVersion: 1, ID: id, User: "reviewer", Repo: p.Repo, PR: p.Number, ComparisonKey: p.Key(), HeadSHA: p.HeadSHA, Event: "COMMENT", Marker: PublicationMarker(id), Items: []PublicationItem{}}
	for _, f := range r.Findings {
		setDecision(t, s, f.ID, "approved", p.Key())
		snapshot.Items = append(snapshot.Items, PublicationItem{ID: f.ID, Kind: f.Kind, Body: f.Body, Anchor: f.Anchor, RunComparisonKey: p.Key(), ApprovedComparisonKey: p.Key()})
	}
	var err error
	snapshot.Request, err = snapshot.Render()
	if err != nil {
		t.Fatal(err)
	}
	return s, snapshot
}

func TestConcurrentPreparationAndImmutableSnapshot(t *testing.T) {
	s, snapshot := publicationFixture(t)
	other := snapshot
	other.ID = uuid.NewString()
	other.Marker = PublicationMarker(other.ID)
	var err error
	other.Request, err = other.Render()
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, candidate := range []PublicationSnapshot{snapshot, other} {
		wg.Go(func() { _, err := s.PreparePublication(t.Context(), candidate); results <- err })
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("prepared %d attempts", successes)
	}
	p, err := s.BlockingPublication(t.Context(), snapshot.Repo, snapshot.PR)
	if err != nil || p == nil {
		t.Fatalf("blocking: %+v %v", p, err)
	}
	if _, err := s.DB.Exec("UPDATE publications SET snapshot_json='{}' WHERE id=?", p.ID); err == nil {
		t.Fatal("snapshot was mutable")
	}
	if err := s.MarkSending(t.Context(), p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSending(t.Context(), p.ID); err == nil {
		t.Fatal("sending transition repeated")
	}
}

func TestFinalizationRollsBackWithAudit(t *testing.T) {
	s, snapshot := publicationFixture(t)
	p, err := s.PreparePublication(t.Context(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinalizePublication(t.Context(), p.ID, "123"); err == nil {
		t.Fatal("known-unsent attempt finalized")
	}
	if err := s.MarkSending(t.Context(), p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_finalize BEFORE INSERT ON audit_log WHEN NEW.action='published' AND NEW.body_snapshot='C' BEGIN SELECT RAISE(ABORT,'injected audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.FinalizePublication(t.Context(), p.ID, "123"); err == nil {
		t.Fatal("injected failure ignored")
	}
	p, err = s.Publication(t.Context(), p.ID)
	if err != nil || p.Status != "sending" {
		t.Fatal("publication partially finalized")
	}
	fs, err := s.ListFindings(t.Context(), snapshot.Repo, "approved", snapshot.PR)
	if err != nil || len(fs) != len(snapshot.Items) {
		t.Fatal("findings partially finalized")
	}
	var count int
	if err := s.DB.QueryRow("SELECT count(*) FROM audit_log WHERE action='published'").Scan(&count); err != nil || count != 0 {
		t.Fatal("partial publication audit")
	}
}

func TestPreparationRejectsChangedApprovalOrPayload(t *testing.T) {
	s, snapshot := publicationFixture(t)
	bad := snapshot
	bad.Request.Body += "private unapproved addition"
	if _, err := s.PreparePublication(t.Context(), bad); err == nil {
		t.Fatal("unapproved payload addition accepted")
	}
	setDecision(t, s, snapshot.Items[0].ID, "pending", "")
	if _, err := s.PreparePublication(t.Context(), snapshot); err == nil {
		t.Fatal("changed approvals prepared")
	}
}
