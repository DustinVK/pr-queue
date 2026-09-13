package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/editor"
	"github.com/DustinVK/pr-queue/internal/github"
	"github.com/DustinVK/pr-queue/internal/queue"
	"github.com/DustinVK/pr-queue/internal/store"
)

func (a *app) triage(ctx context.Context, command string, args []string) (any, error) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var asGeneral bool
	var reason string
	if command == "reject" {
		fs.StringVar(&reason, "reason", "", "private rejection reason")
	}
	if command == "edit" {
		fs.BoolVar(&asGeneral, "as-general", false, "convert inline to general")
	}
	if err := parseFlags(fs, args); err != nil {
		return nil, err
	}
	if fs.NArg() == 0 || (command == "edit" && fs.NArg() != 1) {
		return nil, fmt.Errorf("invalid finding selection for %s", command)
	}
	cfg, err := config.Load(a.paths.Config)
	if err != nil {
		return nil, err
	}
	s, err := store.Open(ctx, a.paths.Database)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	remote := a.remote
	if remote == nil {
		remote = github.New()
	}
	q := queue.Service{Store: s, Remote: remote, User: cfg.GitHub.User, StateDir: a.paths.State}
	var data any
	switch command {
	case "approve":
		data, err = q.Approve(ctx, fs.Args())
	case "reject":
		data, err = q.Reject(ctx, fs.Args(), reason)
	case "edit":
		e := editor.Editor{Command: os.Getenv("EDITOR"), In: a.in, Out: a.errOut}
		data, err = q.Edit(ctx, fs.Arg(0), asGeneral, e.Edit)
	default:
		return nil, fmt.Errorf("unknown triage command %s", command)
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}
