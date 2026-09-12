package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/DustinVK/pr-queue/internal/lock"
	"github.com/DustinVK/pr-queue/internal/runner"
	"github.com/DustinVK/pr-queue/internal/store"
)

// Recover on the next invocation without preventing other commands during a run.
// Dry runs must not update persistent run records or remove persistent artifacts.
func (a *app) recoverInterrupted(ctx context.Context, args []string) error {
	for _, arg := range args {
		option := strings.TrimLeft(arg, "-")
		if strings.HasPrefix(arg, "-") && (option == "dry-run" || strings.HasPrefix(option, "dry-run=")) {
			return nil
		}
	}
	if _, err := os.Stat(a.paths.Database); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	l, err := lock.Acquire(lock.RunPath(a.paths.State), "recover interrupted run")
	var busy *lock.BusyError
	if errors.As(err, &busy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer l.Close()
	if _, err := (runner.Runner{StateDir: a.paths.State}).Cleanup(ctx); err != nil {
		return fmt.Errorf("clean orphaned worktrees: %w", err)
	}
	s, err := store.Open(ctx, a.paths.Database)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.FailInterruptedRuns(ctx)
}
