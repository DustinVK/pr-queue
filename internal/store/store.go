// Package store owns SQLite persistence. Callers hold PR locks for mutations;
// transaction callbacks must never execute an agent or perform network I/O.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/DustinVK/pr-queue/internal/localfs"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

const SchemaVersion = 1

type Store struct{ DB *sql.DB }

func Create(ctx context.Context, path string) (*Store, error) {
	if err := localfs.PrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	s, err := connect(ctx, path, false)
	if err != nil {
		return nil, err
	}
	if err = s.migrate(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func Open(ctx context.Context, path string) (*Store, error) { return openExisting(ctx, path, false) }
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	return openExisting(ctx, path, true)
}

func openExisting(ctx context.Context, path string, readOnly bool) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("open database (run prq init first): %w", err)
	}
	s, err := connect(ctx, path, readOnly)
	if err != nil {
		return nil, err
	}
	var version int
	err = s.DB.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	if err == nil && version != SchemaVersion {
		err = fmt.Errorf("unsupported database schema %d (expected %d); run a compatible prq init", version, SchemaVersion)
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func connect(ctx context.Context, path string, readOnly bool) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Set("mode", "rw")
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(FULL)")
		q.Set("_txlock", "immediate")
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db}, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) Write(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) migrate(ctx context.Context) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		var version int
		if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
			return err
		}
		switch version {
		case SchemaVersion:
			return nil
		case 0:
			if _, err := tx.ExecContext(ctx, schema); err != nil {
				return fmt.Errorf("initialize schema: %w", err)
			}
			_, err := tx.ExecContext(ctx, "PRAGMA user_version = 1")
			return err
		default:
			return fmt.Errorf("unsupported database schema %d (expected %d)", version, SchemaVersion)
		}
	})
}
