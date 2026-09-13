package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/DustinVK/pr-queue/internal/findings"
)

func sampleReviewJSON(body string) string {
	data, _ := json.Marshal(map[string]any{"id": 123, "user": map[string]string{"login": "reviewer"}, "commit_id": strings.Repeat("a", 40), "body": body, "state": "COMMENTED", "submitted_at": "2026-09-12T12:00:00Z"})
	return string(data)
}
func sampleRequest() ReviewRequest {
	return ReviewRequest{CommitID: strings.Repeat("a", 40), Event: "COMMENT", Body: "exact body\n", Comments: []ReviewComment{}}
}

func TestSendReviewOneExplicitPOSTWithStdin(t *testing.T) {
	calls := 0
	request := sampleRequest()
	c := Client{Exec: func(_ context.Context, args []string, input []byte) Response {
		calls++
		for _, want := range []string{"POST", "--include", "--input", "-", "github.com", "repos/owner/repo/pulls/1/reviews"} {
			if !slices.Contains(args, want) {
				t.Errorf("missing %s: %v", want, args)
			}
		}
		if slices.Contains(args, "--paginate") {
			t.Error("POST must not paginate")
		}
		var got ReviewRequest
		if err := json.Unmarshal(input, &got); err != nil || got.Event != request.Event || got.Body != request.Body || got.CommitID != request.CommitID {
			t.Fatalf("request changed: %s %v", input, err)
		}
		return Response{Stdout: []byte("HTTP/2.0 200 OK\r\nContent-Type: application/json\r\n\r\n" + sampleReviewJSON(request.Body))}
	}}
	r, err := c.SendReview(t.Context(), "owner/repo", 1, request)
	if err != nil || calls != 1 || r.ID != "123" || r.Matches("reviewer", request) != nil {
		t.Fatalf("send: %+v %d %v", r, calls, err)
	}
	request.Event = ""
	if _, err := c.SendReview(t.Context(), "owner/repo", 1, request); err == nil || calls != 1 {
		t.Fatal("implicit pending review reached transport")
	}
}

func TestSubmissionOutcomeClassificationNeverRetries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stdout   string
		err      error
		definite bool
		id       string
	}{
		{"validation", "HTTP/2.0 422 Unprocessable Entity\nContent-Type: application/json\n\n{\"message\":\"Validation Failed\",\"errors\":[]}", errors.New("exit 1"), true, ""},
		{"forbidden", "HTTP/2.0 403 Forbidden\n\n{\"message\":\"Forbidden\"}", errors.New("exit 1"), true, ""},
		{"gateway", "HTTP/2.0 502 Bad Gateway\n\n{\"message\":\"upstream failed\"}", errors.New("exit 1"), false, ""},
		{"partial rejection", "HTTP/2.0 422 Unprocessable Entity\n\n{\"message\":", errors.New("truncated"), false, ""},
		{"status only", "HTTP/2.0 403 Forbidden\n\n", errors.New("lost body"), false, ""},
		{"transport lost", "", context.DeadlineExceeded, false, ""},
		{"stderr is not proof", "", errors.New("HTTP 422"), false, ""},
		{"success response lost", "HTTP/2.0 200 OK\n\n" + sampleReviewJSON("exact body\n"), errors.New("connection ended"), false, "123"},
		{"invalid success", "HTTP/2.0 200 OK\n\n{\"id\":123}", nil, false, "123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := Client{Exec: func(context.Context, []string, []byte) Response {
				calls++
				return Response{Stdout: []byte(tc.stdout), Stderr: []byte("HTTP 422"), Err: tc.err}
			}}
			r, err := c.SendReview(t.Context(), "owner/repo", 1, sampleRequest())
			var submission *SubmissionError
			if !errors.As(err, &submission) || submission.Definite != tc.definite || calls != 1 || r.ID != tc.id {
				t.Fatalf("classification: %+v calls=%d %v", submission, calls, err)
			}
		})
	}
}

func TestReviewReadsRequireCompletePagination(t *testing.T) {
	c := Client{Exec: func(_ context.Context, args []string, _ []byte) Response {
		if !slices.Contains(args, "--paginate") || !slices.Contains(args, "--slurp") || !slices.Contains(args, "GET") {
			t.Errorf("read args: %v", args)
		}
		return Response{Stdout: []byte("[[" + sampleReviewJSON("one") + "],[]]")}
	}}
	r, err := c.ListReviews(t.Context(), "owner/repo", 1)
	if err != nil || len(r) != 1 {
		t.Fatalf("reviews: %v %v", r, err)
	}
	c.Exec = func(context.Context, []string, []byte) Response {
		return Response{Stdout: []byte("[[" + sampleReviewJSON("one") + "]]"), Err: errors.New("later page failed")}
	}
	if r, err := c.ListReviews(t.Context(), "owner/repo", 1); err == nil || r != nil {
		t.Fatal("partial reviews accepted")
	}
	for _, invalid := range []string{"[]", "[null]", "[[" + sampleReviewJSON("one") + "],[" + sampleReviewJSON("one") + "]]"} {
		c.Exec = func(context.Context, []string, []byte) Response { return Response{Stdout: []byte(invalid)} }
		if _, err := c.ListReviews(t.Context(), "owner/repo", 1); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
}

func TestDismissedReviewsRequireRecordedPreviousState(t *testing.T) {
	request := sampleRequest()
	request.Event = "APPROVE"
	dismissed := strings.Replace(sampleReviewJSON(request.Body), `"state":"COMMENTED"`, `"state":"DISMISSED"`, 1)
	for _, reviewID := range []string{"123", `"123"`} {
		t.Run(reviewID, func(t *testing.T) {
			c := Client{Exec: func(_ context.Context, args []string, _ []byte) Response {
				endpoint := args[len(args)-1]
				if strings.Contains(endpoint, "/events?") {
					return Response{Stdout: []byte(fmt.Sprintf(`[[{"event":"review_dismissed","dismissed_review":{"review_id":%s,"state":"approved"}}],[]]`, reviewID))}
				}
				return Response{Stdout: []byte(dismissed)}
			}}
			review, err := c.FetchReview(t.Context(), "owner/repo", 1, "123")
			if err != nil || review.DismissedState != "APPROVED" || review.Matches("reviewer", request) != nil {
				t.Fatalf("dismissed review: %+v %v", review, err)
			}
		})
	}

	for _, events := range []string{
		`[[]]`,
		`[[{"event":"review_dismissed","dismissed_review":{"review_id":123,"state":"changes_requested"}},{"event":"review_dismissed","dismissed_review":{"review_id":123,"state":"approved"}}]]`,
	} {
		c := Client{Exec: func(_ context.Context, args []string, _ []byte) Response {
			if strings.Contains(args[len(args)-1], "/events?") {
				return Response{Stdout: []byte(events)}
			}
			return Response{Stdout: []byte(dismissed)}
		}}
		if _, err := c.FetchReview(t.Context(), "owner/repo", 1, "123"); err == nil {
			t.Fatalf("accepted dismissal history %s", events)
		}
	}
}

func TestCommentsUseOriginalCompleteAnchorsAfterPush(t *testing.T) {
	raw := fmt.Sprintf(`[[{"id":8,"pull_request_review_id":999}],[{"id":9,"pull_request_review_id":123,"user":{"login":"reviewer"},"body":"exact","path":"file.go","side":"RIGHT","start_side":"LEFT","line":null,"start_line":null,"original_line":12,"original_start_line":10,"original_commit_id":%q,"commit_id":%q},{"id":10,"pull_request_review_id":123,"in_reply_to_id":9}]]`, strings.Repeat("a", 40), strings.Repeat("b", 40))
	c := Client{Exec: func(_ context.Context, args []string, _ []byte) Response {
		if args[len(args)-1] != "repos/owner/repo/pulls/1/comments?per_page=100" || !slices.Contains(args, "--paginate") {
			t.Errorf("comment endpoint: %v", args)
		}
		return Response{Stdout: []byte(raw)}
	}}
	comments, err := c.ListReviewComments(t.Context(), "owner/repo", 1, "123")
	if err != nil || len(comments) != 1 {
		t.Fatalf("comments: %+v %v", comments, err)
	}
	request := sampleRequest()
	request.Comments = []ReviewComment{{Body: "exact", Anchor: findings.Anchor{Path: findings.Ptr("file.go"), Side: findings.Ptr("RIGHT"), Line: findings.Ptr(12), StartLine: findings.Ptr(10), StartSide: findings.Ptr("LEFT")}}}
	if err := MatchComments(comments, "123", "reviewer", request); err != nil {
		t.Fatal(err)
	}
	comments[0].StartSide = findings.Ptr("RIGHT")
	if err := MatchComments(comments, "123", "reviewer", request); err == nil {
		t.Fatal("wrong start side accepted")
	}
	c.Exec = func(context.Context, []string, []byte) Response {
		return Response{Stdout: []byte(raw), Err: errors.New("later page failed")}
	}
	if _, err := c.ListReviewComments(t.Context(), "owner/repo", 1, "123"); err == nil {
		t.Fatal("incomplete comments accepted")
	}
}
