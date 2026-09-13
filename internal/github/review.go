package github

import "github.com/DustinVK/pr-queue/internal/findings"

// ReviewRequest is the complete submitted-review request. Private finding
// metadata has no representation here. Live transport is added with recovery.
type ReviewRequest struct {
	CommitID string          `json:"commit_id"`
	Event    string          `json:"event"`
	Body     string          `json:"body"`
	Comments []ReviewComment `json:"comments"`
}

type ReviewComment struct {
	findings.Anchor
	Body string `json:"body"`
}

func ValidEvent(event string) bool {
	return event == "COMMENT" || event == "APPROVE" || event == "REQUEST_CHANGES"
}
