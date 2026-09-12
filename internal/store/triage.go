package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/DustinVK/pr-queue/internal/findings"
)

func (s *Store) ClearApprovals(ctx context.Context, ids []string, actor string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			f, err := scanFinding(tx.QueryRowContext(ctx, findingQuery+" WHERE f.id=?", id))
			if err != nil {
				return err
			}
			if f.Status != "approved" {
				continue
			}
			if _, err := tx.ExecContext(ctx, "UPDATE findings SET status='pending',approved_comparison_key=NULL WHERE id=?", id); err != nil {
				return err
			}
			if err := auditFinding(ctx, tx, f, "approval_cleared", actor, f.Body); err != nil {
				return err
			}
		}
		return nil
	})
}

// Decide validates and changes a whole single-PR batch in one transaction.
// Approval's remote comparison/diff were fetched by the caller under its PR lock.
func (s *Store) Decide(ctx context.Context, ids []string, decision, actor, reason, key string, diff findings.Diff) ([]Finding, error) {
	var result []Finding
	if len(ids) == 0 || (decision != "approved" && decision != "rejected") {
		return nil, fmt.Errorf("invalid decision batch")
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		var prID int64
		for _, id := range ids {
			f, err := scanFinding(tx.QueryRowContext(ctx, findingQuery+" WHERE f.id=?", id))
			if err != nil {
				return err
			}
			if prID != 0 && f.PRID != prID {
				return fmt.Errorf("all findings must belong to one PR")
			}
			prID = f.PRID
			if decision == "approved" {
				if f.Status != "pending" && f.Status != "rejected" {
					return fmt.Errorf("finding %s is %s; only pending/rejected findings can be approved", id, f.Status)
				}
				p, err := scanPR(tx.QueryRowContext(ctx, "SELECT "+prColumns+" FROM pull_requests WHERE id=?", f.PRID))
				if err != nil {
					return err
				}
				if p.State != "open" || p.Draft || p.Key() != key || f.RunComparisonKey != key {
					return fmt.Errorf("finding %s is not valid for the current open, ready comparison; rerun first", id)
				}
				if err := f.Finding.Validate(); err != nil {
					return err
				}
				if err := diff.Validate(f.Finding); err != nil {
					return fmt.Errorf("finding %s: %w", id, err)
				}
				f.ApprovedComparisonKey = findings.Ptr(key)
				f.BlockReason = nil
			} else {
				if f.Status == "published" || f.Status == "obsolete" {
					return fmt.Errorf("finding %s is %s and cannot be rejected", id, f.Status)
				}
				if f.Status == "approved" {
					if err := auditFinding(ctx, tx, f, "approval_cleared", actor, f.Body); err != nil {
						return err
					}
				}
				f.ApprovedComparisonKey = nil
			}
			f.Status = decision
			if _, err := tx.ExecContext(ctx, "UPDATE findings SET status=?,approved_comparison_key=?,block_reason=? WHERE id=?", f.Status, f.ApprovedComparisonKey, f.BlockReason, f.ID); err != nil {
				return err
			}
			body := f.Body
			if decision == "rejected" && reason != "" {
				// Rejection has optional private context; other body snapshots stay verbatim.
				data, err := json.Marshal(struct {
					Body   string `json:"body"`
					Reason string `json:"reason"`
				}{f.Body, reason})
				if err != nil {
					return err
				}
				body = string(data)
			}
			if err := auditFinding(ctx, tx, f, decision, actor, body); err != nil {
				return err
			}
			result = append(result, f)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// EditFinding changes only editable fields and their decision, leaving run identity intact.
// The caller holds the PR lock across the editor and any remote anchor validation.
func (s *Store) EditFinding(ctx context.Context, original Finding, body string, asGeneral bool, actor string, diff findings.Diff) (Finding, error) {
	var result Finding
	err := s.Write(ctx, func(tx *sql.Tx) error {
		f, err := scanFinding(tx.QueryRowContext(ctx, findingQuery+" WHERE f.id=?", original.ID))
		if err != nil {
			return err
		}
		if f.Status == "published" {
			return fmt.Errorf("published findings cannot be edited")
		}
		if f.Body != original.Body || f.Kind != original.Kind || !f.Anchor.Equal(original.Anchor) || f.ReviewRunID != original.ReviewRunID {
			return fmt.Errorf("finding changed while editor was open; retry")
		}
		previous := f
		f.Body = body
		if asGeneral {
			if f.Kind != "inline" && f.Kind != "general" {
				return fmt.Errorf("only inline findings can be converted to general")
			}
			f.Kind = "general"
			f.Anchor = findings.Anchor{}
		}
		if err := f.Finding.Validate(); err != nil {
			return err
		}
		if f.Body == previous.Body && f.Kind == previous.Kind && f.Anchor.Equal(previous.Anchor) {
			result = f
			return nil
		}
		f.Status = "pending"
		f.ApprovedComparisonKey = nil
		f.BlockReason = nil
		f.Fingerprint = f.Finding.Fingerprint()
		if err := diff.Validate(f.Finding); err != nil {
			f.Status = "blocked"
			f.BlockReason = findings.Ptr(err.Error())
		}
		if previous.Status == "approved" {
			if err := auditFinding(ctx, tx, previous, "approval_cleared", actor, previous.Body); err != nil {
				return err
			}
		}
		if err := auditFinding(ctx, tx, previous, "edited", actor, previous.Body); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE findings SET kind=?,body=?,path=?,side=?,line=?,start_line=?,start_side=?,fingerprint=?,status=?,block_reason=?,approved_comparison_key=NULL WHERE id=?`, f.Kind, f.Body, f.Path, f.Side, f.Line, f.StartLine, f.StartSide, f.Fingerprint, f.Status, f.BlockReason, f.ID); err != nil {
			return err
		}
		if err := auditFinding(ctx, tx, f, "edited", actor, f.Body); err != nil {
			return err
		}
		result = f
		return nil
	})
	return result, err
}
