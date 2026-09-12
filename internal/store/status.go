package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type RunStatus struct {
	ID         string  `json:"id"`
	Repo       string  `json:"repo"`
	PR         int     `json:"pr"`
	Status     string  `json:"status"`
	StartedAt  string  `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
	Error      *string `json:"error"`
}

func (s *Store) LastRuns(ctx context.Context) ([]RunStatus, error) {
	// Existing databases can contain variable-width RFC3339 fractions. SQLite's
	// date functions lose nanosecond precision, so compare parsed instants here.
	rows, err := s.DB.QueryContext(ctx, `SELECT r.id, p.repo, p.number, r.status, r.started_at, r.finished_at, r.error
 FROM review_runs r JOIN pull_requests p ON p.id = r.pr_id
 ORDER BY p.repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []RunStatus{}
	var latest time.Time
	for rows.Next() {
		var r RunStatus
		if err := rows.Scan(&r.ID, &r.Repo, &r.PR, &r.Status, &r.StartedAt, &r.FinishedAt, &r.Error); err != nil {
			return nil, err
		}
		started, err := time.Parse(time.RFC3339Nano, r.StartedAt)
		if err != nil {
			return nil, fmt.Errorf("run %s started_at: %w", r.ID, err)
		}
		if len(result) == 0 || !strings.EqualFold(result[len(result)-1].Repo, r.Repo) {
			result = append(result, r)
			latest = started
		} else if started.After(latest) || (started.Equal(latest) && r.ID > result[len(result)-1].ID) {
			result[len(result)-1] = r
			latest = started
		}
	}
	return result, rows.Err()
}

type BlockingPublication struct {
	ID        string  `json:"id"`
	Repo      string  `json:"repo"`
	PR        int     `json:"pr"`
	Status    string  `json:"status"`
	Uncertain bool    `json:"uncertain"`
	Error     *string `json:"error"`
}

func (s *Store) BlockingPublications(ctx context.Context) ([]BlockingPublication, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT b.id, p.repo, p.number, b.status, b.uncertain, b.error
 FROM publications b JOIN pull_requests p ON p.id = b.pr_id
 WHERE b.status IN ('prepared','sending') OR (b.status = 'failed' AND b.uncertain = 1)
 ORDER BY p.repo, p.number`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []BlockingPublication{}
	for rows.Next() {
		var p BlockingPublication
		if err := rows.Scan(&p.ID, &p.Repo, &p.PR, &p.Status, &p.Uncertain, &p.Error); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}
