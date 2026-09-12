CREATE TABLE pull_requests (
  id INTEGER PRIMARY KEY,
  repo TEXT NOT NULL COLLATE NOCASE,
  number INTEGER NOT NULL CHECK (number > 0),
  head_sha TEXT NOT NULL,
  base_sha TEXT NOT NULL,
  draft BOOLEAN NOT NULL CHECK (draft IN (0,1)),
  state TEXT NOT NULL CHECK (state IN ('open','closed','merged')),
  last_reviewed_key TEXT,
  updated_at TEXT NOT NULL,
  UNIQUE(repo, number)
);

CREATE TABLE review_runs (
  id TEXT PRIMARY KEY,
  pr_id INTEGER NOT NULL REFERENCES pull_requests(id),
  head_sha TEXT NOT NULL,
  comparison_key TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('running','succeeded','failed','timed_out')),
  started_at TEXT NOT NULL,
  finished_at TEXT,
  raw_output_path TEXT,
  error TEXT
);

CREATE TABLE findings (
  id TEXT PRIMARY KEY,
  pr_id INTEGER NOT NULL REFERENCES pull_requests(id),
  review_run_id TEXT NOT NULL REFERENCES review_runs(id),
  kind TEXT NOT NULL CHECK (kind IN ('inline','general','summary')),
  path TEXT, side TEXT, start_line INTEGER, start_side TEXT, line INTEGER,
  severity TEXT, category TEXT, title TEXT,
  body TEXT NOT NULL,
  rationale TEXT,
  fingerprint TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending','approved','rejected','published','obsolete','blocked')),
  block_reason TEXT,
  approved_comparison_key TEXT,
  published_review_id TEXT,
  created_at TEXT NOT NULL,
  CHECK ((status = 'approved') = (approved_comparison_key IS NOT NULL))
);

CREATE TABLE publications (
  id TEXT PRIMARY KEY,
  pr_id INTEGER NOT NULL REFERENCES pull_requests(id),
  head_sha TEXT NOT NULL,
  event TEXT NOT NULL CHECK (event IN ('COMMENT','REQUEST_CHANGES','APPROVE')),
  status TEXT NOT NULL CHECK (status IN ('prepared','sending','published','failed')),
  uncertain BOOLEAN NOT NULL DEFAULT 0 CHECK (uncertain IN (0,1)),
  snapshot_json TEXT NOT NULL,
  marker TEXT NOT NULL,
  github_review_id TEXT,
  created_at TEXT NOT NULL,
  finished_at TEXT,
  error TEXT
);

CREATE UNIQUE INDEX one_open_publication_per_pr ON publications(pr_id)
  WHERE status IN ('prepared','sending') OR (status = 'failed' AND uncertain = 1);

CREATE TABLE audit_log (
  id INTEGER PRIMARY KEY,
  pr_id INTEGER NOT NULL,
  finding_id TEXT,
  publication_id TEXT,
  at TEXT NOT NULL,
  actor TEXT NOT NULL,
  action TEXT NOT NULL,
  body_snapshot TEXT
);

CREATE INDEX findings_by_pr ON findings(pr_id, created_at, id);
CREATE INDEX runs_by_pr ON review_runs(pr_id, started_at);
CREATE INDEX audit_by_pr ON audit_log(pr_id, id);

CREATE TRIGGER audit_no_update BEFORE UPDATE ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;
CREATE TRIGGER audit_no_delete BEFORE DELETE ON audit_log
BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;
CREATE TRIGGER publication_snapshot_immutable
BEFORE UPDATE OF id, pr_id, head_sha, event, snapshot_json, marker, created_at ON publications
BEGIN SELECT RAISE(ABORT, 'publication snapshot is immutable'); END;
CREATE TRIGGER run_comparison_immutable
BEFORE UPDATE OF id, pr_id, head_sha, comparison_key ON review_runs
BEGIN SELECT RAISE(ABORT, 'run comparison is immutable'); END;
