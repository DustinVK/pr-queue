package queue

import (
	"strings"
	"testing"

	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/DustinVK/pr-queue/internal/github"
)

func TestUUIDResolutionAndQueueFilters(t *testing.T) {
	s, _ := queueStore(t)
	p := remotePR(1)
	local := seedApproved(t, s, p)
	id := "00000001-0000-4000-8000-000000000001"
	f, err := s.ResolveFinding(t.Context(), "00000001")
	if err != nil || f.ID != id {
		t.Fatalf("prefix: %+v %v", f, err)
	}
	if _, err := s.ResolveFinding(t.Context(), "0000000"); err == nil {
		t.Fatal("short prefix accepted")
	}
	_, err = s.DB.Exec(`INSERT INTO findings(id,pr_id,review_run_id,kind,body,fingerprint,status,created_at) VALUES('00000001-0000-4000-8000-000000000002',?,'run-1','general','another body','new','pending','later')`, local.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveFinding(t.Context(), "00000001"); err == nil || !strings.Contains(err.Error(), id) || !strings.Contains(err.Error(), "000000000002") {
		t.Fatalf("ambiguous candidates missing: %v", err)
	}
	if _, err := s.ResolveFinding(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	q := Service{Store: s}
	fs, err := q.List(t.Context(), "OWNER/REPO", "approved")
	if err != nil || len(fs) != 1 {
		t.Fatalf("list: %+v %v", fs, err)
	}
	if _, err := q.List(t.Context(), "", "invalid"); err == nil {
		t.Fatal("invalid status accepted")
	}
	show, err := q.Show(t.Context(), "owner/repo#1")
	if err != nil || len(show.Findings) != 2 || show.Publications == nil {
		t.Fatalf("show: %+v %v", show, err)
	}
}

func TestDiffShowsStalenessWithoutMutatingDecisions(t *testing.T) {
	s, _ := queueStore(t)
	p := remotePR(1)
	seedApproved(t, s, p)
	_, err := s.DB.Exec(`UPDATE findings SET kind='inline',path='file.go',side='RIGHT',line=2,severity='major',category='correctness',title='bug'`)
	if err != nil {
		t.Fatal(err)
	}
	current := p
	current.BaseSHA = strings.Repeat("c", 40)
	d := findings.Diff{}
	d.AddPatch("file.go", "@@ -1,2 +1,2 @@\n context\n-old\n+new\n")
	remote := &fakeRemote{fetch: map[int]github.PR{1: current}, diff: d}
	q := Service{Store: s, Remote: remote, User: "reviewer"}
	r, err := q.Diff(t.Context(), "00000001")
	if err != nil || !r.Stale || r.ValidationError != "" || r.Patch == "" {
		t.Fatalf("diff: %+v %v", r, err)
	}
	assertApproved(t, s, p, true)
	remote.afterFetch = func() { next := current; next.Draft = true; remote.fetch[1] = next }
	if _, err := q.Diff(t.Context(), "00000001"); err == nil {
		t.Fatal("changing comparison during diff read accepted")
	}
}
