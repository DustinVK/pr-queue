package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/google/uuid"
)

func runFixture(t *testing.T) (*Store, PullRequest, findings.Document, findings.Diff) {
	t.Helper()
	s, _ := testStore(t)
	c := findings.Comparison{HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), State: "open"}
	p, _, err := s.Observe(t.Context(), "owner/repo", 1, c, "test")
	if err != nil {
		t.Fatal(err)
	}
	d := findings.Document{SchemaVersion: 1, Repo: p.Repo, PR: p.Number, HeadSHA: p.HeadSHA, Summary: "Summary", Verdict: "comment", Findings: []findings.Finding{
		{Kind: "inline", Anchor: findings.Anchor{Path: findings.Ptr("file.go"), Side: findings.Ptr("RIGHT"), Line: findings.Ptr(2)}, Severity: "major", Category: "correctness", Title: "A", Body: "A\r\nbody"},
		{Kind: "general", Severity: "minor", Category: "test-coverage", Title: "B", Body: "B"},
		{Kind: "general", Severity: "minor", Category: "design", Title: "C", Body: "C"},
	}}
	diff := findings.Diff{}
	diff.AddPatch("file.go", "@@ -1,3 +1,3 @@\n context\n-old\n+new\n tail\n")
	return s, p, d, diff
}
func startTestRun(t *testing.T, s *Store, p PullRequest) ReviewRun {
	t.Helper()
	r, err := s.StartRun(t.Context(), uuid.NewString(), p, "/diagnostics/findings.json")
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func ingestTest(t *testing.T, s *Store, p PullRequest, d findings.Document, diff findings.Diff) Ingestion {
	t.Helper()
	r, err := s.Ingest(t.Context(), startTestRun(t, s, p), d, diff)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func setDecision(t *testing.T, s *Store, id, status, key string) {
	t.Helper()
	var approval any
	if status == "approved" {
		approval = key
	}
	if _, err := s.DB.Exec("UPDATE findings SET status=?,approved_comparison_key=? WHERE id=?", status, approval, id); err != nil {
		t.Fatal(err)
	}
}

func TestRerunUsesLatestDecisionsAndDoesNotRepostPublished(t *testing.T) {
	s, p, d, diff := runFixture(t)
	first := ingestTest(t, s, p, d, diff)
	run := startTestRun(t, s, p)
	setDecision(t, s, first.Findings[1].ID, "approved", p.Key())
	setDecision(t, s, first.Findings[2].ID, "rejected", "")
	setDecision(t, s, first.Findings[3].ID, "published", "")
	second, err := s.Ingest(t.Context(), run, d, diff)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"pending", "approved", "rejected", "published"} {
		if second.Findings[i].ID != first.Findings[i].ID || second.Findings[i].Status != want {
			t.Fatalf("item %d: %+v", i, second.Findings[i])
		}
	}
	if second.Changed != 0 {
		t.Fatalf("identical rerun changed %d findings", second.Changed)
	}
	current, err := s.PR(t.Context(), p.Repo, p.Number)
	if err != nil || current.LastReviewedKey == nil || *current.LastReviewedKey != p.Key() {
		t.Fatalf("cursor %+v %v", current, err)
	}
}

func TestChangedAnchorOrBodyClearsDecisionAndAuditsBodies(t *testing.T) {
	for _, change := range []string{"line", "CRLF"} {
		t.Run(change, func(t *testing.T) {
			s, p, d, diff := runFixture(t)
			first := ingestTest(t, s, p, d, diff)
			id := first.Findings[1].ID
			setDecision(t, s, id, "approved", p.Key())
			oldBody := d.Findings[0].Body
			if change == "line" {
				d.Findings[0].Line = findings.Ptr(3)
			} else {
				d.Findings[0].Body = strings.ReplaceAll(oldBody, "\r\n", "\n")
			}
			second := ingestTest(t, s, p, d, diff)
			f := second.Findings[1]
			if f.ID != id || f.Status != "pending" || f.ApprovedComparisonKey != nil {
				t.Fatalf("changed finding %+v", f)
			}
			if change == "CRLF" {
				var n int
				if err := s.DB.QueryRow("SELECT count(*) FROM audit_log WHERE finding_id=? AND action='edited' AND body_snapshot IN (?,?)", id, oldBody, d.Findings[0].Body).Scan(&n); err != nil || n != 2 {
					t.Fatalf("replaced body audit %d %v", n, err)
				}
			}
		})
	}
}

func TestAmbiguousAndUnmatchedFindings(t *testing.T) {
	s, p, d, diff := runFixture(t)
	first := ingestTest(t, s, p, d, diff)
	setDecision(t, s, first.Findings[1].ID, "approved", p.Key())
	setDecision(t, s, first.Findings[3].ID, "published", "")
	d.Findings = []findings.Finding{d.Findings[0], d.Findings[0]}
	second := ingestTest(t, s, p, d, diff)
	for _, f := range second.Findings[1:] {
		if f.ID == first.Findings[1].ID || f.Status != "pending" {
			t.Fatal("ambiguous match reused identity/approval")
		}
	}
	for i, want := range map[int]string{1: "obsolete", 2: "obsolete", 3: "published"} {
		f, err := s.ResolveFinding(t.Context(), first.Findings[i].ID)
		if err != nil || f.Status != want {
			t.Fatalf("unmatched %d: %+v %v", i, f, err)
		}
	}
}

func TestStaleAndFailedRunsDoNotAdvanceCursor(t *testing.T) {
	s, p, d, diff := runFixture(t)
	run := startTestRun(t, s, p)
	changed := p.Comparison
	changed.BaseSHA = strings.Repeat("c", 40)
	if _, _, err := s.Observe(t.Context(), p.Repo, p.Number, changed, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ingest(t.Context(), run, d, diff); err == nil {
		t.Fatal("stale ingestion accepted")
	}
	if err := s.FailRun(t.Context(), run, "timed_out", context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	current, err := s.PR(t.Context(), p.Repo, p.Number)
	if err != nil || current.LastReviewedKey != nil {
		t.Fatal("failed run advanced cursor")
	}
	fs, err := s.ListFindings(t.Context(), p.Repo, "", 1)
	if err != nil || len(fs) != 0 {
		t.Fatal("stale findings activated")
	}
	var key, status string
	if err := s.DB.QueryRow("SELECT comparison_key,status FROM review_runs WHERE id=?", run.ID).Scan(&key, &status); err != nil || key != p.Key() || status != "timed_out" {
		t.Fatalf("captured run key/status %s %s %v", key, status, err)
	}
}

func TestIngestionAndCursorRollBackTogether(t *testing.T) {
	s, p, d, diff := runFixture(t)
	run := startTestRun(t, s, p)
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_cursor BEFORE UPDATE OF last_reviewed_key ON pull_requests BEGIN SELECT RAISE(ABORT,'fail cursor'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ingest(t.Context(), run, d, diff); err == nil {
		t.Fatal("expected injection failure")
	}
	for _, table := range []string{"findings", "audit_log"} {
		var n int
		if err := s.DB.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("partial %s mutation: %d %v", table, n, err)
		}
	}
}

func TestBlockedAnchorsStayVisibleAndPublishedMatchesKeepHistory(t *testing.T) {
	s, p, d, diff := runFixture(t)
	d.Findings[0].Line = findings.Ptr(99)
	first := ingestTest(t, s, p, d, diff)
	if first.Findings[1].Status != "blocked" || first.Findings[1].BlockReason == nil {
		t.Fatal("invalid anchor dropped/unblocked")
	}
	setDecision(t, s, first.Findings[2].ID, "published", "")
	oldRun := first.Findings[2].ReviewRunID
	second := ingestTest(t, s, p, d, diff)
	if second.Findings[2].ReviewRunID != oldRun {
		t.Fatal("published history overwritten by rerun")
	}
}

func TestCloneMemoryPreservesTablesAndIsolatesMutations(t *testing.T) {
	s, p, d, diff := runFixture(t)
	first := ingestTest(t, s, p, d, diff)
	setDecision(t, s, first.Findings[1].ID, "approved", p.Key())
	ro, err := OpenReadOnly(t.Context(), databasePath(t, s))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	clone, err := ro.CloneMemory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer clone.Close()
	changed := p.Comparison
	changed.Draft = true
	if _, _, err := clone.Observe(t.Context(), p.Repo, p.Number, changed, "dry-run"); err != nil {
		t.Fatal(err)
	}
	original, err := s.PR(t.Context(), p.Repo, 1)
	if err != nil || original.Draft || original.LastReviewedKey == nil {
		t.Fatal("clone altered original observation")
	}
	f, err := s.ResolveFinding(t.Context(), first.Findings[1].ID)
	if err != nil || f.Status != "approved" {
		t.Fatal("clone altered original decision")
	}
	var n int
	if err := clone.DB.QueryRow("SELECT count(*) FROM audit_log WHERE action='approval_cleared'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("clone audit %d %v", n, err)
	}
	if err := s.DB.QueryRow("SELECT count(*) FROM audit_log WHERE action='approval_cleared'").Scan(&n); err != nil || n != 0 {
		t.Fatal("clone audit escaped")
	}
}

func databasePath(t *testing.T, s *Store) string {
	t.Helper()
	var seq int
	var name, path string
	if err := s.DB.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInterruptedRunsRemainRetryable(t *testing.T) {
	s, p, _, _ := runFixture(t)
	run := startTestRun(t, s, p)
	if err := s.FailInterruptedRuns(t.Context()); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.DB.QueryRow("SELECT status FROM review_runs WHERE id=?", run.ID).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("%s %v", status, err)
	}
	if err := s.FailInterruptedRuns(t.Context()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB.QueryRow("SELECT count(*) FROM audit_log WHERE action='run_failed'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("duplicate audit %d %v", n, err)
	}
	if err := s.FailRun(t.Context(), run, "succeeded", fmt.Errorf("bad")); err == nil {
		t.Fatal("invalid failure state accepted")
	}
}
