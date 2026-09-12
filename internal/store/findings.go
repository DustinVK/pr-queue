package store

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/DustinVK/pr-queue/internal/findings"
)

var Statuses = []string{"pending", "approved", "rejected", "published", "obsolete", "blocked"}

type Finding struct {
	ID          string `json:"id"`
	PRID        int64  `json:"pr_id"`
	ReviewRunID string `json:"review_run_id"`
	Repo        string `json:"repo"`
	PR          int    `json:"pr"`
	findings.Finding
	Fingerprint           string  `json:"fingerprint"`
	Status                string  `json:"status"`
	BlockReason           *string `json:"block_reason,omitempty"`
	ApprovedComparisonKey *string `json:"approved_comparison_key,omitempty"`
	PublishedReviewID     *string `json:"published_review_id,omitempty"`
	CreatedAt             string  `json:"created_at"`
	RunComparisonKey      string  `json:"run_comparison_key"`
}

const findingQuery = `SELECT f.id,f.pr_id,f.review_run_id,p.repo,p.number,f.kind,f.path,f.side,f.start_line,f.start_side,f.line,
COALESCE(f.severity,''),COALESCE(f.category,''),COALESCE(f.title,''),f.body,f.rationale,f.fingerprint,f.status,f.block_reason,
f.approved_comparison_key,f.published_review_id,f.created_at,r.comparison_key
FROM findings f JOIN pull_requests p ON p.id=f.pr_id JOIN review_runs r ON r.id=f.review_run_id`

func scanFinding(row scanner) (Finding, error) {
	var f Finding
	err := row.Scan(&f.ID, &f.PRID, &f.ReviewRunID, &f.Repo, &f.PR, &f.Kind, &f.Path, &f.Side, &f.StartLine, &f.StartSide, &f.Line,
		&f.Severity, &f.Category, &f.Title, &f.Body, &f.Rationale, &f.Fingerprint, &f.Status, &f.BlockReason, &f.ApprovedComparisonKey,
		&f.PublishedReviewID, &f.CreatedAt, &f.RunComparisonKey)
	return f, err
}

func (s *Store) ListFindings(ctx context.Context, repo, status string, pr int) ([]Finding, error) {
	if status != "" && !slices.Contains(Statuses, status) {
		return nil, fmt.Errorf("invalid finding status %q", status)
	}
	query := findingQuery + " WHERE 1=1"
	args := []any{}
	if repo != "" {
		query += " AND p.repo=?"
		args = append(args, repo)
	}
	if status != "" {
		query += " AND f.status=?"
		args = append(args, status)
	}
	if pr > 0 {
		query += " AND p.number=?"
		args = append(args, pr)
	}
	query += " ORDER BY p.repo,p.number,f.created_at,f.id"
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Finding{}
	for rows.Next() {
		f, err := scanFinding(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, f)
	}
	return result, rows.Err()
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var prefixPattern = regexp.MustCompile(`^[0-9a-f]{8}$`)

func (s *Store) ResolveFinding(ctx context.Context, id string) (Finding, error) {
	id = strings.ToLower(id)
	query := findingQuery
	if uuidPattern.MatchString(id) {
		query += " WHERE f.id=?"
	} else if prefixPattern.MatchString(id) {
		query += " WHERE substr(f.id,1,8)=?"
	} else {
		return Finding{}, fmt.Errorf("finding id must be a UUID or eight hex characters")
	}
	rows, err := s.DB.QueryContext(ctx, query+" ORDER BY f.id", id)
	if err != nil {
		return Finding{}, err
	}
	defer rows.Close()
	var result Finding
	var candidates []string
	for rows.Next() {
		f, err := scanFinding(rows)
		if err != nil {
			return Finding{}, err
		}
		result = f
		candidates = append(candidates, f.ID)
	}
	if err := rows.Err(); err != nil {
		return Finding{}, err
	}
	if len(candidates) == 0 {
		return Finding{}, fmt.Errorf("finding %s not found", id)
	}
	if len(candidates) > 1 {
		return Finding{}, fmt.Errorf("ambiguous finding prefix %s; candidates: %s", id, strings.Join(candidates, ", "))
	}
	return result, nil
}

type PublicationHistory struct {
	ID         string  `json:"id"`
	Event      string  `json:"event"`
	Snapshot   string  `json:"snapshot_json"`
	ReviewID   *string `json:"github_review_id"`
	FinishedAt *string `json:"finished_at"`
}

func (s *Store) PublicationHistory(ctx context.Context, prID int64) ([]PublicationHistory, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,event,snapshot_json,github_review_id,finished_at FROM publications WHERE pr_id=? AND status='published' ORDER BY created_at,id`, prID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PublicationHistory{}
	for rows.Next() {
		var p PublicationHistory
		if err := rows.Scan(&p.ID, &p.Event, &p.Snapshot, &p.ReviewID, &p.FinishedAt); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}
