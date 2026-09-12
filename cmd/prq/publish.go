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

func (a *app) publish(ctx context.Context, args []string) (any, error) {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	event := fs.String("event", "", "submitted review event")
	dry := fs.Bool("dry-run", false, "preview the exact request")
	resume := fs.Bool("resume", false, "recover the blocking publication")
	confirmed := fs.Bool("confirmed-not-sent", false, "clear after confirming no review was sent")
	if err := parseFlags(fs, args); err != nil {
		return nil, err
	}
	eventSet, drySet := false, false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "event" {
			eventSet = true
		}
		if f.Name == "dry-run" {
			drySet = true
		}
	})
	if fs.NArg() != 1 || (*resume && (eventSet || drySet)) || (!*resume && (!github.ValidEvent(*event) || *confirmed)) {
		return nil, fmt.Errorf("use publish owner/name#N --event COMMENT|REQUEST_CHANGES|APPROVE [--dry-run], or --resume [--confirmed-not-sent]")
	}
	cfg, err := config.Load(a.paths.Config)
	if err != nil {
		return nil, err
	}
	var s *store.Store
	if *dry {
		s, err = store.OpenReadOnly(ctx, a.paths.Database)
	} else {
		s, err = store.Open(ctx, a.paths.Database)
	}
	if err != nil {
		return nil, err
	}
	defer s.Close()
	remote := a.remote
	if remote == nil {
		remote = github.New()
	}
	q := queue.Service{Store: s, Remote: remote, User: cfg.GitHub.User, StateDir: a.paths.State}
	if *dry {
		result, err := q.PreviewPublication(ctx, fs.Arg(0), *event)
		if err != nil {
			return nil, err
		}
		return result, nil
	}
	if *resume {
		return q.ResumePublication(ctx, fs.Arg(0), *confirmed)
	}
	return q.Publish(ctx, fs.Arg(0), *event)
}
