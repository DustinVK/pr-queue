package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/github"
	"github.com/DustinVK/pr-queue/internal/queue"
	"github.com/DustinVK/pr-queue/internal/store"
)

func (a *app) readQueue(ctx context.Context, command string, args []string) (any, error) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var repo, status string
	if command == "list" {
		fs.StringVar(&repo, "repo", "", "repository filter")
		fs.StringVar(&status, "status", "", "finding status")
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if (command == "list" && fs.NArg() != 0) || (command != "list" && fs.NArg() != 1) {
		return nil, fmt.Errorf("invalid arguments for %s; use prq help", command)
	}
	c, err := config.Load(a.paths.Config)
	if err != nil {
		return nil, err
	}
	s, err := store.OpenReadOnly(ctx, a.paths.Database)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	remote := a.remote
	if remote == nil {
		remote = github.New()
	}
	q := queue.Service{Store: s, Remote: remote, User: c.GitHub.User, StateDir: a.paths.State}
	switch command {
	case "list":
		result, err := q.List(ctx, repo, status)
		if err != nil {
			return nil, err
		}
		return result, nil
	case "show":
		result, err := q.Show(ctx, fs.Arg(0))
		if err != nil {
			return nil, err
		}
		return result, nil
	case "diff":
		result, err := q.Diff(ctx, fs.Arg(0))
		if err != nil {
			return nil, err
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unknown queue read %s", command)
	}
}

func (a *app) printFinding(f store.Finding) {
	fmt.Fprintf(a.out, "%s %s#%d [%s] %s %s\n", f.ID, f.Repo, f.PR, f.Status, f.Kind, f.Title)
	if f.Path != nil && f.Side != nil && f.Line != nil {
		if f.StartLine != nil && f.StartSide != nil {
			fmt.Fprintf(a.out, "%s: %s %d through %s %d\n", *f.Path, *f.StartSide, *f.StartLine, *f.Side, *f.Line)
		} else {
			fmt.Fprintf(a.out, "%s:%d (%s)\n", *f.Path, *f.Line, *f.Side)
		}
	}
	fmt.Fprintln(a.out, f.Body)
	if f.BlockReason != nil {
		fmt.Fprintf(a.out, "Blocked: %s\n", *f.BlockReason)
	}
	if f.Rationale != nil {
		fmt.Fprintf(a.out, "Rationale (private): %s\n", *f.Rationale)
	}
	fmt.Fprintln(a.out)
}
