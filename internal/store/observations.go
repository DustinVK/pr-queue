package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/findings"
)

type PullRequest struct {
	ID     int64  `json:"id"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	findings.Comparison
	LastReviewedKey *string `json:"last_reviewed_key"`
	UpdatedAt       string  `json:"updated_at"`
}

const prColumns = `id,repo,number,head_sha,base_sha,draft,state,last_reviewed_key,updated_at`

type scanner interface{ Scan(...any) error }

func scanPR(row scanner) (PullRequest, error) {
	var p PullRequest
	err := row.Scan(&p.ID, &p.Repo, &p.Number, &p.HeadSHA, &p.BaseSHA, &p.Draft, &p.State, &p.LastReviewedKey, &p.UpdatedAt)
	return p, err
}
func (s *Store) PR(ctx context.Context, repo string, number int) (PullRequest, error) {
	return scanPR(s.DB.QueryRowContext(ctx, "SELECT "+prColumns+" FROM pull_requests WHERE repo=? AND number=?", repo, number))
}
func (s *Store) Tracked(ctx context.Context, repo string) ([]PullRequest, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT "+prColumns+" FROM pull_requests WHERE repo=? ORDER BY number", repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PullRequest{}
	for rows.Next() {
		p, err := scanPR(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

// Observe is called under the PR lock, after remote I/O has finished.
func (s *Store) Observe(ctx context.Context, repo string, number int, c findings.Comparison, actor string) (PullRequest, bool, error) {
	var result PullRequest
	changed := false
	if err := config.ValidateRepo(repo); err != nil {
		return result, false, err
	}
	if number < 1 {
		return result, false, fmt.Errorf("PR number must be positive")
	}
	if err := c.Validate(); err != nil {
		return result, false, err
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		old, err := scanPR(tx.QueryRowContext(ctx, "SELECT "+prColumns+" FROM pull_requests WHERE repo=? AND number=?", repo, number))
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, `INSERT INTO pull_requests(repo,number,head_sha,base_sha,draft,state,updated_at) VALUES(?,?,?,?,?,?,?)`, repo, number, c.HeadSHA, c.BaseSHA, c.Draft, c.State, now)
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			changed = old.Comparison.Key() != c.Key()
			cursor := old.LastReviewedKey
			if changed {
				cursor = nil
				if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(pr_id,finding_id,at,actor,action,body_snapshot)
SELECT pr_id,id,?,?,'approval_cleared',body FROM findings WHERE pr_id=? AND status='approved'`, now, actor, old.ID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE findings SET status='pending',approved_comparison_key=NULL WHERE pr_id=? AND status='approved'`, old.ID); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE pull_requests SET head_sha=?,base_sha=?,draft=?,state=?,last_reviewed_key=?,updated_at=? WHERE id=?`, c.HeadSHA, c.BaseSHA, c.Draft, c.State, cursor, now, old.ID); err != nil {
				return err
			}
		}
		result, err = scanPR(tx.QueryRowContext(ctx, "SELECT "+prColumns+" FROM pull_requests WHERE repo=? AND number=?", repo, number))
		return err
	})
	return result, changed, err
}
