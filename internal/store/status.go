package store

import "context"

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
	rows, err := s.DB.QueryContext(ctx, `SELECT id, repo, number, status, started_at, finished_at, error FROM (
 SELECT r.id, p.repo, p.number, r.status, r.started_at, r.finished_at, r.error,
 ROW_NUMBER() OVER (PARTITION BY p.repo ORDER BY r.started_at DESC, r.id DESC) AS rank
 FROM review_runs r JOIN pull_requests p ON p.id = r.pr_id
) WHERE rank = 1 ORDER BY repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []RunStatus{}
	for rows.Next() {
		var r RunStatus
		if err := rows.Scan(&r.ID, &r.Repo, &r.PR, &r.Status, &r.StartedAt, &r.FinishedAt, &r.Error); err != nil {
			return nil, err
		}
		result = append(result, r)
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
