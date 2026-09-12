package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/DustinVK/pr-queue/internal/github"
	"github.com/google/uuid"
)

type PublicationItem struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Body string `json:"body"`
	findings.Anchor
	RunComparisonKey      string `json:"run_comparison_key"`
	ApprovedComparisonKey string `json:"approved_comparison_key"`
}

func (item PublicationItem) Matches(f Finding) bool {
	return f.Status == "approved" && f.ID == item.ID && f.Kind == item.Kind && f.Body == item.Body && f.Anchor.Equal(item.Anchor) && f.RunComparisonKey == item.RunComparisonKey && f.ApprovedComparisonKey != nil && *f.ApprovedComparisonKey == item.ApprovedComparisonKey
}

type PublicationSnapshot struct {
	SchemaVersion int                  `json:"schema_version"`
	ID            string               `json:"id"`
	User          string               `json:"user"`
	Repo          string               `json:"repo"`
	PR            int                  `json:"pr"`
	ComparisonKey string               `json:"comparison_key"`
	HeadSHA       string               `json:"head_sha"`
	Event         string               `json:"event"`
	Items         []PublicationItem    `json:"items"`
	Marker        string               `json:"marker"`
	Request       github.ReviewRequest `json:"request"`
}

func PublicationMarker(id string) string { return "<!-- prqueue:" + id + " -->" }

// Render uses only frozen public bodies and complete anchors, preserving bytes.
func (s PublicationSnapshot) Render() (github.ReviewRequest, error) {
	r := github.ReviewRequest{CommitID: s.HeadSHA, Event: s.Event, Comments: []github.ReviewComment{}}
	var summary *string
	var bodies []string
	for _, item := range s.Items {
		switch item.Kind {
		case "summary":
			if summary != nil {
				return r, fmt.Errorf("multiple approved summaries; keep one before publishing")
			}
			body := item.Body
			summary = &body
		case "general":
			bodies = append(bodies, item.Body)
		case "inline":
			r.Comments = append(r.Comments, github.ReviewComment{Anchor: item.Anchor, Body: item.Body})
		default:
			return r, fmt.Errorf("invalid publication item kind %q", item.Kind)
		}
	}
	if summary != nil {
		bodies = append([]string{*summary}, bodies...)
	}
	bodies = append(bodies, s.Marker)
	r.Body = strings.Join(bodies, "\n\n")
	if err := findings.ValidateBody(r.Body); err != nil {
		return r, fmt.Errorf("assembled review body: %w", err)
	}
	return r, nil
}

func (s PublicationSnapshot) Validate() error {
	if s.SchemaVersion != 1 || !config.ValidLogin(s.User) || s.PR < 1 || !github.ValidEvent(s.Event) {
		return fmt.Errorf("invalid publication snapshot identity/event")
	}
	id, err := uuid.Parse(s.ID)
	if err != nil || id.String() != s.ID || s.Marker != PublicationMarker(s.ID) {
		return fmt.Errorf("invalid publication marker")
	}
	if err := config.ValidateRepo(s.Repo); err != nil {
		return err
	}
	var parts []json.RawMessage
	if err := json.Unmarshal([]byte(s.ComparisonKey), &parts); err != nil || len(parts) != 4 {
		return fmt.Errorf("invalid publication comparison")
	}
	var comparison findings.Comparison
	for i, dest := range []any{&comparison.HeadSHA, &comparison.BaseSHA, &comparison.Draft, &comparison.State} {
		if err := json.Unmarshal(parts[i], dest); err != nil {
			return err
		}
	}
	if err := comparison.Validate(); err != nil {
		return err
	}
	if comparison.Key() != s.ComparisonKey || comparison.HeadSHA != s.HeadSHA || comparison.State != "open" || comparison.Draft {
		return fmt.Errorf("publication comparison must be open and ready")
	}
	if len(s.Items) == 0 && s.Event != "APPROVE" {
		return fmt.Errorf("only APPROVE permits an empty publication")
	}
	seen := map[string]bool{}
	for _, item := range s.Items {
		id, err := uuid.Parse(item.ID)
		if err != nil || id.String() != item.ID || seen[item.ID] {
			return fmt.Errorf("invalid or duplicate snapshot item ID")
		}
		seen[item.ID] = true
		if item.RunComparisonKey != s.ComparisonKey || item.ApprovedComparisonKey != s.ComparisonKey {
			return fmt.Errorf("snapshot item has stale approval")
		}
		f := findings.Finding{Kind: item.Kind, Body: item.Body, Anchor: item.Anchor, Severity: "minor", Category: "correctness"}
		if err := f.Validate(); err != nil {
			return err
		}
	}
	rendered, err := s.Render()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(rendered, s.Request) {
		return fmt.Errorf("snapshot request does not match frozen items")
	}
	return nil
}
