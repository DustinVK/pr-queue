package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/DustinVK/pr-queue/internal/findings"
)

func rawPR(n int) map[string]any {
	return map[string]any{
		"number": n, "state": "open", "draft": false, "merged": false,
		"head": map[string]any{"sha": strings.Repeat("a", 40)}, "base": map[string]any{"sha": strings.Repeat("b", 40), "ref": "main"},
		"user": map[string]any{"login": "author"}, "requested_reviewers": []any{map[string]any{"login": "reviewer"}}, "changed_files": 1,
	}
}
func jsonBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func fakeClient(t *testing.T, reply func([]string) Response) *Client {
	t.Helper()
	return &Client{Exec: func(ctx context.Context, args []string, input []byte) Response {
		if args[0] != "api" || input != nil {
			t.Errorf("unexpected command/input: %v %s", args, input)
		}
		for flag, want := range map[string]string{"--hostname": "github.com", "--method": "GET"} {
			i := slices.Index(args, flag)
			if i < 0 || i+1 >= len(args) || args[i+1] != want {
				t.Errorf("%s not explicit: %v", flag, args)
			}
		}
		return reply(args)
	}}
}

func TestListFullyPaginatesUnfiltered(t *testing.T) {
	p1, p2 := rawPR(1), rawPR(2)
	p2["draft"] = true
	c := fakeClient(t, func(args []string) Response {
		if !slices.Contains(args, "--paginate") || !slices.Contains(args, "--slurp") {
			t.Error("pagination missing")
		}
		if args[len(args)-1] != "repos/owner/repo/pulls?state=open&per_page=100" {
			t.Fatalf("unexpected filter: %v", args)
		}
		return Response{Stdout: jsonBytes(t, [][]any{{p1}, {p2}})}
	})
	prs, err := c.ListOpen(t.Context(), "owner/repo")
	if err != nil || len(prs) != 2 || !prs[1].Draft {
		t.Fatalf("list: %+v %v", prs, err)
	}
}

func TestListFailureDoesNotReturnPartialResults(t *testing.T) {
	c := fakeClient(t, func([]string) Response {
		return Response{Stdout: jsonBytes(t, [][]any{{rawPR(1)}}), Err: errors.New("exit 1"), Stderr: []byte("page 2 failed")}
	})
	prs, err := c.ListOpen(t.Context(), "owner/repo")
	if err == nil || prs != nil {
		t.Fatalf("partial result escaped: %+v %v", prs, err)
	}
	for _, body := range []any{nil, []any{}, []any{nil}, [][]any{{rawPR(1)}, {rawPR(1)}}} {
		c = fakeClient(t, func([]string) Response { return Response{Stdout: jsonBytes(t, body)} })
		if _, err := c.ListOpen(t.Context(), "owner/repo"); err == nil {
			t.Fatalf("accepted invalid page sequence: %#v", body)
		}
	}
}

func TestIdentityAndObservationValidation(t *testing.T) {
	c := fakeClient(t, func([]string) Response { return Response{Stdout: []byte(`{"login":"Reviewer"}`)} })
	if _, err := c.Identity(t.Context(), "reviewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Identity(t.Context(), "someone-else"); err == nil {
		t.Fatal("identity mismatch accepted")
	}
	for _, change := range []func(map[string]any){
		func(p map[string]any) { delete(p, "draft") }, func(p map[string]any) { p["number"] = 2 },
		func(p map[string]any) { p["head"] = map[string]any{"sha": "short"} }, func(p map[string]any) { p["state"] = "unknown" },
	} {
		p := rawPR(1)
		change(p)
		c = fakeClient(t, func([]string) Response { return Response{Stdout: jsonBytes(t, p)} })
		if _, err := c.FetchPR(t.Context(), "owner/repo", 1); err == nil {
			t.Fatal("incomplete/mismatched observation accepted")
		}
	}
	p := rawPR(1)
	p["state"] = "closed"
	p["merged_at"] = "now"
	c = fakeClient(t, func([]string) Response { return Response{Stdout: jsonBytes(t, p)} })
	got, err := c.FetchPR(t.Context(), "owner/repo", 1)
	if err != nil || got.State != "merged" {
		t.Fatalf("merge observation: %+v %v", got, err)
	}
}

func TestFilePaginationAndUnavailablePatches(t *testing.T) {
	c := fakeClient(t, func(args []string) Response {
		if !slices.Contains(args, "--paginate") {
			t.Error("file pagination missing")
		}
		return Response{Stdout: jsonBytes(t, [][]any{{map[string]any{"filename": "new.go", "patch": "@@ -1 +1 @@\n-old\n+new\n", "additions": 1, "deletions": 1}}, {map[string]any{"filename": "image.png"}}})}
	})
	p := PR{Repo: "owner/repo", Number: 1, ChangedFiles: findings.Ptr(2)}
	d, err := c.FetchDiff(t.Context(), p)
	if err != nil || len(d.Files) != 2 || d.Files["image.png"].Error == "" {
		t.Fatalf("files: %+v %v", d, err)
	}
	if d.Files["new.go"].Error != "" {
		t.Fatal("complete patch was blocked")
	}
	p.ChangedFiles = findings.Ptr(3)
	if _, err := c.FetchDiff(t.Context(), p); err == nil {
		t.Fatal("truncated files listing accepted")
	}
}

func TestInvalidRepoNeverInvokesGH(t *testing.T) {
	c := fakeClient(t, func([]string) Response { t.Fatal("invoked gh for invalid repo"); return Response{} })
	if _, err := c.ListOpen(t.Context(), "owner/repo;echo"); err == nil {
		t.Fatal("invalid repository accepted")
	}
}

func TestResponseSizeLimitCannotBeBypassedByIOCopy(t *testing.T) {
	b := &limitedBuffer{limit: 3}
	_, err := io.Copy(b, io.LimitReader(strings.NewReader("too much"), 8))
	if err == nil {
		t.Fatal("io.Copy bypassed response size limit")
	}
}

func TestDiffRequiresFullMetadataAndDetectsMissingWholeHunk(t *testing.T) {
	c := fakeClient(t, func([]string) Response {
		return Response{Stdout: jsonBytes(t, [][]any{{map[string]any{"filename": "file.go", "patch": "@@ -1 +1 @@\n-old\n+new\n", "additions": 2, "deletions": 2}}})}
	})
	p := PR{Repo: "owner/repo", Number: 1}
	if _, err := c.FetchDiff(t.Context(), p); err == nil {
		t.Fatal("diff completeness accepted without total changed files")
	}
	p.ChangedFiles = findings.Ptr(1)
	d, err := c.FetchDiff(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if d.Files["file.go"].Error == "" {
		t.Fatal("patch omitting a whole hunk considered complete")
	}
}
