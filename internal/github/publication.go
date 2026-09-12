package github

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/findings"
)

type Review struct {
	ID          string  `json:"id"`
	User        string  `json:"user"`
	CommitID    string  `json:"commit_id"`
	Body        string  `json:"body"`
	State       string  `json:"state"`
	SubmittedAt *string `json:"submitted_at,omitempty"`
}

type apiReview struct {
	ID   int64 `json:"id"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	CommitID    string  `json:"commit_id"`
	Body        *string `json:"body"`
	State       string  `json:"state"`
	SubmittedAt *string `json:"submitted_at"`
}

func (r apiReview) value() (Review, error) {
	v := Review{User: r.User.Login, CommitID: r.CommitID, State: r.State, SubmittedAt: r.SubmittedAt}
	if r.ID > 0 {
		v.ID = strconv.FormatInt(r.ID, 10)
	}
	if r.Body != nil {
		v.Body = *r.Body
	}
	if r.ID < 1 || r.User.Login == "" || r.Body == nil || r.State == "" {
		return v, fmt.Errorf("incomplete GitHub review")
	}
	return v, nil
}

func (r Review) Matches(user string, request ReviewRequest) error {
	state := map[string]string{"COMMENT": "COMMENTED", "APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED"}[request.Event]
	if r.ID == "" || state == "" || r.State != state || !strings.EqualFold(r.User, user) || r.CommitID != request.CommitID || r.Body != request.Body || r.SubmittedAt == nil {
		return fmt.Errorf("review %s does not match snapshot author, commit, submitted event, or exact body", r.ID)
	}
	if _, err := time.Parse(time.RFC3339, *r.SubmittedAt); err != nil {
		return fmt.Errorf("review %s has no valid submission time", r.ID)
	}
	return nil
}

type SubmissionError struct {
	Cause    error
	Definite bool // Proven local refusal or documented complete rejection response.
}

func (e *SubmissionError) Error() string { return e.Cause.Error() }
func (e *SubmissionError) Unwrap() error { return e.Cause }

func reviewEndpoint(repo string, pr int) (string, error) {
	if err := config.ValidateRepo(repo); err != nil {
		return "", err
	}
	if pr < 1 {
		return "", fmt.Errorf("PR number must be positive")
	}
	return fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, pr), nil
}
func validReviewID(id string) bool {
	n, err := strconv.ParseInt(id, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == id
}

func (c *Client) FetchReview(ctx context.Context, repo string, pr int, id string) (Review, error) {
	endpoint, err := reviewEndpoint(repo, pr)
	if err != nil {
		return Review{}, err
	}
	if !validReviewID(id) {
		return Review{}, fmt.Errorf("invalid review ID")
	}
	data, err := c.get(ctx, endpoint+"/"+id, false)
	if err != nil {
		return Review{}, err
	}
	var raw apiReview
	if err := decode(data, &raw); err != nil {
		return Review{}, err
	}
	r, err := raw.value()
	if err == nil && r.ID != id {
		err = fmt.Errorf("GitHub returned a different review ID")
	}
	return r, err
}

func (c *Client) ListReviews(ctx context.Context, repo string, pr int) ([]Review, error) {
	endpoint, err := reviewEndpoint(repo, pr)
	if err != nil {
		return nil, err
	}
	data, err := c.get(ctx, endpoint+"?per_page=100", true)
	if err != nil {
		return nil, err
	}
	var pages [][]apiReview
	if err := decode(data, &pages); err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("missing paginated reviews response")
	}
	result := []Review{}
	seen := map[string]bool{}
	for _, page := range pages {
		if page == nil {
			return nil, fmt.Errorf("invalid reviews page")
		}
		for _, raw := range page {
			r, err := raw.value()
			if err != nil {
				return nil, err
			}
			if seen[r.ID] {
				return nil, fmt.Errorf("duplicate review across pages; retry reconciliation")
			}
			seen[r.ID] = true
			result = append(result, r)
		}
	}
	return result, nil
}

type SubmittedComment struct {
	ID               string `json:"id"`
	ReviewID         string `json:"review_id"`
	User             string `json:"user"`
	OriginalCommitID string `json:"original_commit_id"`
	ReviewComment
}

// Read the full PR-comments endpoint because it supplies original line anchors
// needed after later pushes. Complete pagination is required even for no matches.
func (c *Client) ListReviewComments(ctx context.Context, repo string, pr int, id string) ([]SubmittedComment, error) {
	if _, err := reviewEndpoint(repo, pr); err != nil {
		return nil, err
	}
	if !validReviewID(id) {
		return nil, fmt.Errorf("invalid review ID")
	}
	data, err := c.get(ctx, fmt.Sprintf("repos/%s/pulls/%d/comments?per_page=100", repo, pr), true)
	if err != nil {
		return nil, err
	}
	var pages [][]struct {
		ID       int64 `json:"id"`
		ReviewID int64 `json:"pull_request_review_id"`
		User     struct {
			Login string `json:"login"`
		} `json:"user"`
		OriginalCommitID  string  `json:"original_commit_id"`
		Body              *string `json:"body"`
		Path              *string `json:"path"`
		Side              *string `json:"side"`
		OriginalLine      *int    `json:"original_line"`
		OriginalStartLine *int    `json:"original_start_line"`
		StartSide         *string `json:"start_side"`
	}
	if err := decode(data, &pages); err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("missing paginated review comments response")
	}
	result := []SubmittedComment{}
	seen := map[int64]bool{}
	for _, page := range pages {
		if page == nil {
			return nil, fmt.Errorf("invalid review comments page")
		}
		for _, raw := range page {
			if raw.ID < 1 || raw.ReviewID < 1 || seen[raw.ID] {
				return nil, fmt.Errorf("missing or duplicate review comment identity")
			}
			seen[raw.ID] = true
			reviewID := strconv.FormatInt(raw.ReviewID, 10)
			if reviewID != id {
				continue
			}
			if raw.Body == nil || raw.User.Login == "" || raw.OriginalCommitID == "" {
				return nil, fmt.Errorf("incomplete submitted comment")
			}
			anchor := findings.Anchor{Path: raw.Path, Side: raw.Side, Line: raw.OriginalLine, StartLine: raw.OriginalStartLine, StartSide: raw.StartSide}
			f := findings.Finding{Kind: "inline", Severity: "minor", Category: "correctness", Body: *raw.Body, Anchor: anchor}
			if err := f.Validate(); err != nil {
				return nil, fmt.Errorf("cannot verify original comment anchor: %w", err)
			}
			result = append(result, SubmittedComment{ID: strconv.FormatInt(raw.ID, 10), ReviewID: reviewID, User: raw.User.Login, OriginalCommitID: raw.OriginalCommitID, ReviewComment: ReviewComment{Anchor: anchor, Body: *raw.Body}})
		}
	}
	return result, nil
}

func MatchComments(comments []SubmittedComment, reviewID, user string, request ReviewRequest) error {
	if len(comments) != len(request.Comments) {
		return fmt.Errorf("review %s has %d comments; snapshot has %d", reviewID, len(comments), len(request.Comments))
	}
	matched := make([]bool, len(request.Comments))
	for _, actual := range comments {
		if actual.ReviewID != reviewID || !strings.EqualFold(actual.User, user) || actual.OriginalCommitID != request.CommitID {
			return fmt.Errorf("comment %s has unexpected author, review, or original commit", actual.ID)
		}
		found := false
		for i, want := range request.Comments {
			if !matched[i] && actual.Body == want.Body && actual.Anchor.Equal(want.Anchor) {
				matched[i] = true
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("comment %s has unexpected body or complete anchor", actual.ID)
		}
	}
	return nil
}

// SendReview executes exactly one submitted-review request and never retries.
func (c *Client) SendReview(ctx context.Context, repo string, pr int, request ReviewRequest) (Review, error) {
	endpoint, err := reviewEndpoint(repo, pr)
	if err == nil && !ValidEvent(request.Event) {
		err = fmt.Errorf("an explicit submitted event is required")
	}
	if err == nil {
		err = (findings.Comparison{HeadSHA: request.CommitID, BaseSHA: request.CommitID, State: "open"}).Validate()
	}
	if err == nil {
		err = findings.ValidateBody(request.Body)
	}
	if err != nil {
		return Review{}, &SubmissionError{Cause: err, Definite: true}
	}
	input, err := json.Marshal(request)
	if err != nil {
		return Review{}, &SubmissionError{Cause: err, Definite: true}
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	args := []string{"api", "--hostname", "github.com", "--method", "POST", "--header", "Accept: application/vnd.github+json", "--header", "Content-Type: application/json", "--header", "X-GitHub-Api-Version: 2026-03-10", "--include", "--input", "-", endpoint}
	execute := c.Exec
	if execute == nil {
		execute = Execute
	}
	response := execute(ctx, args, input)
	status, body, parseErr := includedResponse(response.Stdout)
	var raw apiReview
	decodeErr := decode(body, &raw)
	review, valueErr := raw.value()
	if parseErr == nil && response.Err == nil && status >= 200 && status < 300 && decodeErr == nil && valueErr == nil {
		return review, nil
	}
	definite := false
	if parseErr == nil && (status == 403 || status == 422) {
		// These documented rejection codes establish no review was created only
		// with a complete GitHub error object, never merely a status or stderr text.
		var proof struct {
			Message string          `json:"message"`
			ID      json.RawMessage `json:"id"`
		}
		definite = decode(body, &proof) == nil && proof.Message != "" && len(proof.ID) == 0
	}
	evidence := body
	if parseErr != nil {
		evidence = response.Stdout
	}
	cause := fmt.Errorf("GitHub review POST status=%d: %s", status, diagnostic(evidence, response.Stderr))
	cause = errors.Join(cause, response.Err, parseErr)
	if status >= 200 && status < 300 {
		cause = errors.Join(cause, decodeErr, valueErr)
	}
	return review, &SubmissionError{Cause: cause, Definite: definite}
}

// gh --include prints decoded body bytes after display headers, not an HTTP
// transfer stream. Parse headers without reapplying chunked/content encodings.
func includedResponse(data []byte) (int, []byte, error) {
	reader := bufio.NewReader(bytes.NewReader(data))
	header := textproto.NewReader(reader)
	line, err := header.ReadLine()
	if err != nil {
		return 0, nil, err
	}
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return 0, nil, fmt.Errorf("missing HTTP response status")
	}
	if _, _, ok := http.ParseHTTPVersion(parts[0]); !ok {
		return 0, nil, fmt.Errorf("invalid HTTP response status")
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil || status < 100 || status > 599 {
		return 0, nil, fmt.Errorf("invalid HTTP status code")
	}
	if _, err := header.ReadMIMEHeader(); err != nil {
		return status, nil, err
	}
	body, err := io.ReadAll(reader)
	return status, body, err
}

func diagnostic(body, stderr []byte) string {
	text := strings.TrimSpace(string(body) + "\n" + string(stderr))
	if len(text) > 8192 {
		text = text[:8192] + " [diagnostic truncated]"
	}
	return text
}
