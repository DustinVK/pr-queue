package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "queue ?# space.db")
	s, err := Create(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func seedPR(t *testing.T, s *Store) {
	t.Helper()
	_, err := s.DB.Exec(`INSERT INTO pull_requests(id,repo,number,head_sha,base_sha,draft,state,updated_at) VALUES (1,'owner/repo',1,'head','base',0,'open','2026-09-12T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestMigrateAndForeignKeysOnEveryConnection(t *testing.T) {
	s, path := testStore(t)
	var conns []*sql.Conn
	for range 3 {
		c, err := s.DB.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		defer c.Close()
		var fk, version int
		var journal string
		if err := c.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
			t.Fatalf("foreign_keys %d: %v", fk, err)
		}
		if err := c.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
			t.Fatalf("journal %s: %v", journal, err)
		}
		if err := c.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
			t.Fatalf("schema %d: %v", version, err)
		}
		if _, err := c.ExecContext(t.Context(), `INSERT INTO review_runs(id,pr_id,head_sha,comparison_key,status,started_at) VALUES ('bad',999,'h','k','running','now')`); err == nil {
			t.Fatal("orphan run accepted")
		}
	}
	for _, c := range conns {
		c.Close()
	}
	seedPR(t, s)
	if _, err := s.DB.Exec(`INSERT INTO pull_requests(repo,number,head_sha,base_sha,draft,state,updated_at) VALUES ('OWNER/REPO',1,'head','base',0,'open','now')`); err == nil {
		t.Fatal("repository casing created duplicate PR")
	}
	other, err := Create(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var n int
	if err := other.DB.QueryRow("SELECT count(*) FROM pull_requests").Scan(&n); err != nil || n != 1 {
		t.Fatalf("rerun lost data: %d %v", n, err)
	}
	if err := other.DB.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&n); err != nil || n != 5 {
		t.Fatalf("expected five tables: %d %v", n, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("database mode: %v %v", info, err)
	}
}

func TestBlockingPublicationIndexAndImmutability(t *testing.T) {
	for _, state := range []struct {
		status    string
		uncertain bool
		blocks    bool
	}{
		{"prepared", false, true}, {"sending", false, true}, {"failed", true, true}, {"failed", false, false}, {"published", false, false},
	} {
		t.Run(state.status+string(rune('0'+boolInt(state.uncertain))), func(t *testing.T) {
			s, _ := testStore(t)
			seedPR(t, s)
			insert := `INSERT INTO publications(id,pr_id,head_sha,event,status,uncertain,snapshot_json,marker,created_at) VALUES (?,1,'head','COMMENT',?,?,'{}',?,'now')`
			if _, err := s.DB.Exec(insert, "old", state.status, state.uncertain, "old-marker"); err != nil {
				t.Fatal(err)
			}
			_, err := s.DB.Exec(insert, "new", "prepared", false, "new-marker")
			if (err != nil) != state.blocks {
				t.Fatalf("blocks=%v: %v", state.blocks, err)
			}
			if _, err := s.DB.Exec("UPDATE publications SET snapshot_json='changed' WHERE id='old'"); err == nil {
				t.Fatal("snapshot mutation accepted")
			}
		})
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestWriteRollbackAndAppendOnlyAudit(t *testing.T) {
	s, _ := testStore(t)
	seedPR(t, s)
	sentinel := errors.New("stop")
	err := s.Write(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.Exec("UPDATE pull_requests SET head_sha='changed' WHERE id=1"); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO audit_log(pr_id,at,actor,action,body_snapshot) VALUES(1,'now','user','edited','text')"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	var head string
	var n int
	if err := s.DB.QueryRow("SELECT head_sha FROM pull_requests").Scan(&head); err != nil || head != "head" {
		t.Fatalf("rollback head %q %v", head, err)
	}
	if err := s.DB.QueryRow("SELECT count(*) FROM audit_log").Scan(&n); err != nil || n != 0 {
		t.Fatalf("rollback audit %d %v", n, err)
	}
	if _, err := s.DB.Exec("INSERT INTO audit_log(pr_id,at,actor,action) VALUES(1,'now','user','created')"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"UPDATE audit_log SET action='edited'", "DELETE FROM audit_log"} {
		if _, err := s.DB.Exec(query); err == nil {
			t.Fatal("audit rewrite accepted")
		}
	}
}

func TestFailedMigrationIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.db")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := connect(t.Context(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.DB.Exec("CREATE TABLE review_runs (sentinel TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(t.Context()); err == nil {
		t.Fatal("expected conflicting schema to fail")
	}
	var n int
	if err := s.DB.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='pull_requests'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial migration remained: %d %v", n, err)
	}
	if err := s.DB.QueryRow("PRAGMA user_version").Scan(&n); err != nil || n != 0 {
		t.Fatalf("schema version advanced: %d %v", n, err)
	}
}

func TestReadOnlyAndFutureSchema(t *testing.T) {
	s, path := testStore(t)
	ro, err := OpenReadOnly(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.DB.Exec("DELETE FROM findings"); err == nil {
		t.Fatal("read-only store accepted write")
	}
	ro.Close()
	if _, err := s.DB.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), path); err == nil {
		t.Fatal("opened newer schema")
	}
	if _, err := Create(t.Context(), path); err == nil {
		t.Fatal("migrated newer schema")
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err := Open(context.Background(), missing); err == nil {
		t.Fatal("opened missing database")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("open created missing database")
	}
}

func TestStatusKeepsLatestRunPerRepoAndBlockingAttempts(t *testing.T) {
	s, _ := testStore(t)
	seedPR(t, s)
	if _, err := s.DB.Exec(`INSERT INTO pull_requests(id,repo,number,head_sha,base_sha,draft,state,updated_at) VALUES(2,'other/repo',2,'h','b',0,'open','now');
INSERT INTO review_runs(id,pr_id,head_sha,comparison_key,status,started_at) VALUES
('old',1,'h','k','succeeded','2026-09-11T00:00:00Z'),
('new',1,'h','k','failed','2026-09-12T00:00:00Z'),
('other',2,'h','k','running','2026-09-10T00:00:00Z');
INSERT INTO publications(id,pr_id,head_sha,event,status,uncertain,snapshot_json,marker,created_at) VALUES
('cleared',1,'h','COMMENT','failed',0,'{}','old','now'),
('blocked',1,'h','COMMENT','failed',1,'{}','new','now');`); err != nil {
		t.Fatal(err)
	}
	runs, err := s.LastRuns(t.Context())
	if err != nil || len(runs) != 2 || runs[0].ID != "other" || runs[1].ID != "new" {
		t.Fatalf("latest runs: %+v %v", runs, err)
	}
	p, err := s.BlockingPublications(t.Context())
	if err != nil || len(p) != 1 || p[0].ID != "blocked" {
		t.Fatalf("blocking attempts: %+v %v", p, err)
	}
}
