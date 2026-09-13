package store

import (
	"context"
	"database/sql"
	"fmt"
)

func (s *Store) FailPublication(ctx context.Context, id string, cause error, uncertain bool, reviewID *string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		p, err := scanPublication(tx.QueryRowContext(ctx, "SELECT "+publicationColumns+" FROM publications b WHERE b.id=?", id))
		if err != nil {
			return err
		}
		if p.Status == "published" {
			return nil
		}
		if p.Uncertain && !uncertain {
			return fmt.Errorf("uncertain publication requires explicit reconciliation or clearance")
		}
		_, err = tx.ExecContext(ctx, "UPDATE publications SET status='failed',uncertain=?,github_review_id=COALESCE(?,github_review_id),finished_at=?,error=? WHERE id=?", uncertain, reviewID, timestamp(), cause.Error(), id)
		return err
	})
}

// ClearPublication is called only for a prepared attempt, or after a complete
// no-match reconciliation with the user's explicit --confirmed-not-sent flag.
func (s *Store) ClearPublication(ctx context.Context, id, actor, reason string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		p, err := scanPublication(tx.QueryRowContext(ctx, "SELECT "+publicationColumns+" FROM publications b WHERE b.id=?", id))
		if err != nil {
			return err
		}
		if p.Status != "prepared" && !(p.Status == "failed" && p.Uncertain) {
			return fmt.Errorf("publication is not awaiting clearance")
		}
		if _, err := tx.ExecContext(ctx, "UPDATE publications SET status='failed',uncertain=0,finished_at=?,error=? WHERE id=?", timestamp(), reason, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_log(pr_id,publication_id,at,actor,action,body_snapshot) VALUES(?,?,?,?,'publication_cleared',?)`, p.PRID, p.ID, timestamp(), actor, reason)
		return err
	})
}
