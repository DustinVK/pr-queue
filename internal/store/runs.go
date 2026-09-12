package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/google/uuid"
)

type ReviewRun struct {
	ID            string `json:"id"`
	PRID          int64  `json:"pr_id"`
	HeadSHA       string `json:"head_sha"`
	ComparisonKey string `json:"comparison_key"`
	Status        string `json:"status"`
	OutputPath    string `json:"raw_output_path"`
}

func (s *Store) StartRun(ctx context.Context, id string, p PullRequest, outputPath string) (ReviewRun, error) {
	r := ReviewRun{ID: id, PRID: p.ID, HeadSHA: p.HeadSHA, ComparisonKey: p.Key(), Status: "running", OutputPath: outputPath}
	if _, err := uuid.Parse(id); err != nil {
		return r, err
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		current, err := scanPR(tx.QueryRowContext(ctx, "SELECT "+prColumns+" FROM pull_requests WHERE id=?", p.ID))
		if err != nil {
			return err
		}
		if current.Key() != p.Key() {
			return fmt.Errorf("comparison changed before starting review")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO review_runs(id,pr_id,head_sha,comparison_key,status,started_at,raw_output_path) VALUES(?,?,?,?,'running',?,?)`, id, p.ID, p.HeadSHA, p.Key(), timestamp(), outputPath)
		return err
	})
	return r, err
}

// Fixed precision keeps timestamps in chronological order in SQLite text indexes.
func timestamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z") }

func (s *Store) FailRun(ctx context.Context, r ReviewRun, status string, cause error) error {
	if status != "failed" && status != "timed_out" {
		return fmt.Errorf("invalid failure status %q", status)
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "UPDATE review_runs SET status=?,finished_at=?,error=? WHERE id=? AND status='running'", status, timestamp(), cause.Error(), r.ID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO audit_log(pr_id,at,actor,action,body_snapshot) VALUES(?,?,'coordinator','run_failed',?)`, r.PRID, timestamp(), cause.Error())
		return err
	})
}

type Ingestion struct {
	Findings []Finding `json:"findings"`
	Changed  int       `json:"changed"`
}

// Ingest reads decisions inside the final transaction, never from the snapshot
// captured before the agent ran. Caller holds the PR lock and verified live state.
func (s *Store) Ingest(ctx context.Context, run ReviewRun, doc findings.Document, diff findings.Diff) (Ingestion, error) {
	result := Ingestion{Findings: []Finding{}}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		p, err := scanPR(tx.QueryRowContext(ctx, "SELECT "+prColumns+" FROM pull_requests WHERE id=?", run.PRID))
		if err != nil {
			return err
		}
		var runKey, status string
		var prID int64
		if err := tx.QueryRowContext(ctx, "SELECT pr_id,comparison_key,status FROM review_runs WHERE id=?", run.ID).Scan(&prID, &runKey, &status); err != nil {
			return err
		}
		if status != "running" || prID != p.ID || runKey != run.ComparisonKey || p.Key() != run.ComparisonKey {
			return fmt.Errorf("review run is no longer active for this comparison")
		}
		if doc.Repo != p.Repo || doc.PR != p.Number || doc.HeadSHA != p.HeadSHA {
			return fmt.Errorf("ingestion document identity mismatch")
		}
		rows, err := tx.QueryContext(ctx, findingQuery+" WHERE f.pr_id=? ORDER BY f.created_at,f.id", p.ID)
		if err != nil {
			return err
		}
		var old []Finding
		for rows.Next() {
			f, err := scanFinding(rows)
			if err != nil {
				rows.Close()
				return err
			}
			old = append(old, f)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		oldByFP := map[string][]Finding{}
		newCounts := map[string]int{}
		for _, f := range old {
			oldByFP[f.Fingerprint] = append(oldByFP[f.Fingerprint], f)
		}
		items := doc.QueueItems()
		for _, f := range items {
			newCounts[f.Fingerprint()]++
		}
		matched := map[string]bool{}
		for _, item := range items {
			f := Finding{ID: uuid.NewString(), PRID: p.ID, ReviewRunID: run.ID, Repo: p.Repo, PR: p.Number, Finding: item, Fingerprint: item.Fingerprint(), Status: "pending", CreatedAt: timestamp(), RunComparisonKey: run.ComparisonKey}
			if err := diff.Validate(item); err != nil {
				f.Status = "blocked"
				f.BlockReason = findings.Ptr(err.Error())
			}
			var previous *Finding
			candidates := oldByFP[f.Fingerprint]
			if len(candidates) == 1 && newCounts[f.Fingerprint] == 1 {
				v := candidates[0]
				previous = &v
				f.ID = v.ID
				f.CreatedAt = v.CreatedAt
				matched[v.ID] = true
				if v.Status == "published" {
					// Published matches are history, even if a later run moves an anchor.
					result.Findings = append(result.Findings, v)
					continue
				}
				unchanged := v.Body == item.Body && v.Anchor.Equal(item.Anchor)
				if unchanged && f.Status != "blocked" {
					switch v.Status {
					case "rejected":
						f.Status = "rejected"
					case "approved":
						if v.RunComparisonKey == run.ComparisonKey && v.ApprovedComparisonKey != nil && *v.ApprovedComparisonKey == run.ComparisonKey {
							f.Status = "approved"
							f.ApprovedComparisonKey = v.ApprovedComparisonKey
						}
					}
				}
			}
			if previous != nil {
				if previous.Body != f.Body {
					if err := auditFinding(ctx, tx, *previous, "edited", "coordinator", previous.Body); err != nil {
						return err
					}
				}
				if previous.Status == "approved" && f.Status != "approved" {
					if err := auditFinding(ctx, tx, *previous, "approval_cleared", "coordinator", previous.Body); err != nil {
						return err
					}
				}
				if previous.Status != f.Status || previous.Body != f.Body || !previous.Anchor.Equal(f.Anchor) {
					result.Changed++
				}
				_, err = tx.ExecContext(ctx, `UPDATE findings SET review_run_id=?,kind=?,path=?,side=?,start_line=?,start_side=?,line=?,severity=?,category=?,title=?,body=?,rationale=?,fingerprint=?,status=?,block_reason=?,approved_comparison_key=? WHERE id=?`, f.ReviewRunID, f.Kind, f.Path, f.Side, f.StartLine, f.StartSide, f.Line, f.Severity, f.Category, f.Title, f.Body, f.Rationale, f.Fingerprint, f.Status, f.BlockReason, f.ApprovedComparisonKey, f.ID)
				if err != nil {
					return err
				}
				if previous.Body != f.Body {
					if err := auditFinding(ctx, tx, f, "edited", "coordinator", f.Body); err != nil {
						return err
					}
				}
			} else {
				_, err = tx.ExecContext(ctx, `INSERT INTO findings(id,pr_id,review_run_id,kind,path,side,start_line,start_side,line,severity,category,title,body,rationale,fingerprint,status,block_reason,approved_comparison_key,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, f.ID, f.PRID, f.ReviewRunID, f.Kind, f.Path, f.Side, f.StartLine, f.StartSide, f.Line, f.Severity, f.Category, f.Title, f.Body, f.Rationale, f.Fingerprint, f.Status, f.BlockReason, f.ApprovedComparisonKey, f.CreatedAt)
				if err != nil {
					return err
				}
				if err := auditFinding(ctx, tx, f, "created", "agent", f.Body); err != nil {
					return err
				}
				result.Changed++
			}
			result.Findings = append(result.Findings, f)
		}
		for _, f := range old {
			if matched[f.ID] || f.Status == "published" || f.Status == "obsolete" {
				continue
			}
			if f.Status == "approved" {
				if err := auditFinding(ctx, tx, f, "approval_cleared", "coordinator", f.Body); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, "UPDATE findings SET status='obsolete',approved_comparison_key=NULL WHERE id=?", f.ID); err != nil {
				return err
			}
			if err := auditFinding(ctx, tx, f, "obsolete", "coordinator", f.Body); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE pull_requests SET last_reviewed_key=? WHERE id=?", run.ComparisonKey, p.ID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE review_runs SET status='succeeded',finished_at=? WHERE id=?", timestamp(), run.ID)
		return err
	})
	return result, err
}

func auditFinding(ctx context.Context, tx *sql.Tx, f Finding, action, actor, body string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_log(pr_id,finding_id,at,actor,action,body_snapshot) VALUES(?,?,?,?,?,?)`, f.PRID, f.ID, timestamp(), actor, action, body)
	return err
}

// FailInterruptedRuns is called after acquiring the global run lock and
// cleaning orphaned worktrees; no previous coordinator can still own a run.
func (s *Store) FailInterruptedRuns(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, "SELECT id,pr_id,head_sha,comparison_key,status,COALESCE(raw_output_path,'') FROM review_runs WHERE status='running'")
	if err != nil {
		return err
	}
	var attempts []ReviewRun
	for rows.Next() {
		var r ReviewRun
		if err := rows.Scan(&r.ID, &r.PRID, &r.HeadSHA, &r.ComparisonKey, &r.Status, &r.OutputPath); err != nil {
			rows.Close()
			return err
		}
		attempts = append(attempts, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range attempts {
		if err := s.FailRun(ctx, r, "failed", fmt.Errorf("previous coordinator was interrupted")); err != nil {
			return err
		}
	}
	return nil
}
