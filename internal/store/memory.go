package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

func Memory(ctx context.Context) (*Store, error) {
	db, err := sql.Open("sqlite", "file:prqueue-"+uuid.NewString()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{DB: db}
	if err := s.migrate(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// CloneMemory copies all five tables under one source read transaction. That
// transaction ends before any remote request or agent execution can begin.
func (s *Store) CloneMemory(ctx context.Context) (*Store, error) {
	dest, err := Memory(ctx)
	if err != nil {
		return nil, err
	}
	source, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		dest.Close()
		return nil, err
	}
	defer source.Rollback()
	err = dest.Write(ctx, func(tx *sql.Tx) error {
		for _, table := range []string{"pull_requests", "review_runs", "findings", "publications", "audit_log"} {
			if err := copyTable(ctx, source, tx, table); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		err = source.Commit()
	}
	if err != nil {
		dest.Close()
		return nil, err
	}
	return dest, nil
}

func copyTable(ctx context.Context, source, target *sql.Tx, table string) error {
	rows, err := source.QueryContext(ctx, "SELECT * FROM "+table)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, strings.Join(columns, ","), strings.TrimSuffix(strings.Repeat("?,", len(columns)), ","))
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		if _, err := target.ExecContext(ctx, query, values...); err != nil {
			return err
		}
	}
	return rows.Err()
}
