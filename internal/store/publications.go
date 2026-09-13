package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

type Publication struct {
	ID         string              `json:"id"`
	PRID       int64               `json:"pr_id"`
	HeadSHA    string              `json:"head_sha"`
	Event      string              `json:"event"`
	Status     string              `json:"status"`
	Uncertain  bool                `json:"uncertain"`
	Snapshot   PublicationSnapshot `json:"snapshot"`
	Marker     string              `json:"marker"`
	ReviewID   *string             `json:"github_review_id,omitempty"`
	CreatedAt  string              `json:"created_at"`
	FinishedAt *string             `json:"finished_at,omitempty"`
	Error      *string             `json:"error,omitempty"`
}

const publicationColumns = `b.id,b.pr_id,b.head_sha,b.event,b.status,b.uncertain,b.snapshot_json,b.marker,b.github_review_id,b.created_at,b.finished_at,b.error`

func scanPublication(row scanner) (Publication, error) {
	var p Publication
	var raw string
	err := row.Scan(&p.ID, &p.PRID, &p.HeadSHA, &p.Event, &p.Status, &p.Uncertain, &raw, &p.Marker, &p.ReviewID, &p.CreatedAt, &p.FinishedAt, &p.Error)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal([]byte(raw), &p.Snapshot); err != nil {
		return p, fmt.Errorf("decode publication %s: %w", p.ID, err)
	}
	if err := p.Snapshot.Validate(); err != nil {
		return p, fmt.Errorf("invalid publication %s: %w", p.ID, err)
	}
	if p.ID != p.Snapshot.ID || p.HeadSHA != p.Snapshot.HeadSHA || p.Event != p.Snapshot.Event || p.Marker != p.Snapshot.Marker {
		return p, fmt.Errorf("publication row does not match its snapshot")
	}
	return p, nil
}

func (s *Store) Publication(ctx context.Context, id string) (Publication, error) {
	return scanPublication(s.DB.QueryRowContext(ctx, "SELECT "+publicationColumns+" FROM publications b WHERE b.id=?", id))
}

func (s *Store) BlockingPublication(ctx context.Context, repo string, pr int) (*Publication, error) {
	p, err := scanPublication(s.DB.QueryRowContext(ctx, "SELECT "+publicationColumns+` FROM publications b JOIN pull_requests p ON p.id=b.pr_id WHERE p.repo=? AND p.number=? AND (b.status IN ('prepared','sending') OR (b.status='failed' AND b.uncertain=1))`, repo, pr))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// PreparePublication freezes the request in the same transaction that checks its
// original approvals. The caller owns the PR lock and verified the remote diff.
func (s *Store) PreparePublication(ctx context.Context, snapshot PublicationSnapshot) (Publication, error) {
	if err := snapshot.Validate(); err != nil {
		return Publication{}, err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return Publication{}, err
	}
	err = s.Write(ctx, func(tx *sql.Tx) error {
		p, err := scanPR(tx.QueryRowContext(ctx, "SELECT "+prColumns+" FROM pull_requests WHERE repo=? AND number=?", snapshot.Repo, snapshot.PR))
		if err != nil {
			return err
		}
		if p.Key() != snapshot.ComparisonKey {
			return fmt.Errorf("comparison changed before publication preparation")
		}
		var approved int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM findings WHERE pr_id=? AND status='approved'", p.ID).Scan(&approved); err != nil {
			return err
		}
		if approved != len(snapshot.Items) {
			return fmt.Errorf("approved selection changed before publication preparation")
		}
		for _, item := range snapshot.Items {
			f, err := scanFinding(tx.QueryRowContext(ctx, findingQuery+" WHERE f.id=?", item.ID))
			if err != nil {
				return err
			}
			if f.PRID != p.ID || !item.Matches(f) {
				return fmt.Errorf("finding %s changed before publication preparation", item.ID)
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO publications(id,pr_id,head_sha,event,status,uncertain,snapshot_json,marker,created_at) VALUES(?,?,?,?,'prepared',0,?,?,?)`, snapshot.ID, p.ID, snapshot.HeadSHA, snapshot.Event, string(raw), snapshot.Marker, timestamp())
		if err != nil {
			return fmt.Errorf("prepare publication (another attempt may require --resume): %w", err)
		}
		return nil
	})
	if err != nil {
		return Publication{}, err
	}
	return s.Publication(ctx, snapshot.ID)
}

func (s *Store) MarkSending(ctx context.Context, id string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "UPDATE publications SET status='sending' WHERE id=? AND status='prepared'", id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("publication is not prepared; use --resume")
		}
		return nil
	})
}

// FinalizePublication records the sent snapshot while preserving later decisions.
// Repeating finalization of the same confirmed review is a no-op.
func (s *Store) FinalizePublication(ctx context.Context, id, reviewID string) error {
	if n, err := strconv.ParseInt(reviewID, 10, 64); err != nil || n < 1 {
		return fmt.Errorf("invalid GitHub review ID")
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		p, err := scanPublication(tx.QueryRowContext(ctx, "SELECT "+publicationColumns+" FROM publications b WHERE b.id=?", id))
		if err != nil {
			return err
		}
		if p.Status == "published" {
			if p.ReviewID == nil || *p.ReviewID != reviewID {
				return fmt.Errorf("publication already finalized with a different review ID")
			}
			return nil
		}
		if p.Status != "sending" && !(p.Status == "failed" && p.Uncertain) {
			return fmt.Errorf("cannot finalize a known-unsent publication")
		}
		for _, item := range p.Snapshot.Items {
			f, err := scanFinding(tx.QueryRowContext(ctx, findingQuery+" WHERE f.id=?", item.ID))
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil && f.PRID == p.PRID && item.Matches(f) {
				if _, err := tx.ExecContext(ctx, "UPDATE findings SET status='published',approved_comparison_key=NULL,published_review_id=? WHERE id=?", reviewID, item.ID); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(pr_id,finding_id,publication_id,at,actor,action,body_snapshot) VALUES(?,?,?,?,?,'published',?)`, p.PRID, item.ID, p.ID, timestamp(), p.Snapshot.User, item.Body); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, "UPDATE publications SET status='published',uncertain=0,github_review_id=?,finished_at=?,error=NULL WHERE id=?", reviewID, timestamp(), id)
		return err
	})
}
