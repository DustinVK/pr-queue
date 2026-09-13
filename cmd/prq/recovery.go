package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/DustinVK/pr-queue/internal/lock"
	"github.com/DustinVK/pr-queue/internal/runner"
	"github.com/DustinVK/pr-queue/internal/store"
)

type recoveryWarning struct{ cause error }

func (w *recoveryWarning) Error() string { return w.cause.Error() }
func (w *recoveryWarning) Unwrap() error { return w.cause }

// Recover on the next invocation without preventing other commands during a run.
// Dry runs must not update persistent run records or remove persistent artifacts.
func (a *app) recoverInterrupted(ctx context.Context, command string, args []string) error {
	dryRun := false
	if command == "publish" {
		for _, arg := range args {
			if arg == "--" {
				break
			}
			option := strings.TrimLeft(arg, "-")
			if !strings.HasPrefix(arg, "-") {
				continue
			}
			if option == "dry-run" {
				dryRun = true
				continue
			}
			name, value, hasValue := strings.Cut(option, "=")
			if name == "dry-run" && hasValue {
				dry, err := strconv.ParseBool(value)
				if err == nil {
					dryRun = dry
				}
			}
		}
	}
	if dryRun {
		return nil
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
	_, cleanupErr := (runner.Runner{StateDir: a.paths.State}).Cleanup(ctx)
	if cleanupErr != nil {
		cleanupErr = fmt.Errorf("clean orphaned worktrees: %w", cleanupErr)
	}
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), cleanupErr)
	}
	if _, err := os.Stat(a.paths.Database); os.IsNotExist(err) {
		if cleanupErr != nil {
			return &recoveryWarning{cause: cleanupErr}
		}
		return nil
	} else if err != nil {
		return errors.Join(cleanupErr, err)
	}
	s, err := store.Open(ctx, a.paths.Database)
	if err != nil {
		return errors.Join(cleanupErr, err)
	}
	defer s.Close()
	if err := s.FailInterruptedRuns(ctx); err != nil {
		return errors.Join(cleanupErr, err)
	}
	if cleanupErr != nil {
		return &recoveryWarning{cause: cleanupErr}
	}
	return nil
}
