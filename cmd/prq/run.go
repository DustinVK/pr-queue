package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/github"
	"github.com/DustinVK/pr-queue/internal/lock"
	"github.com/DustinVK/pr-queue/internal/queue"
	"github.com/DustinVK/pr-queue/internal/runner"
	"github.com/DustinVK/pr-queue/internal/store"
)

func (a *app) runReviews(ctx context.Context, args []string) (any, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", "", "repository")
	pr := fs.Int("pr", 0, "force a PR")
	dry := fs.Bool("dry-run", false, "use temporary output and in-memory state")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	prSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "pr" {
			prSet = true
		}
	})
	if fs.NArg() != 0 || (prSet && (*pr < 1 || *repo == "")) {
		return nil, fmt.Errorf("run --pr requires a positive PR number and --repo")
	}
	if *repo != "" {
		if err := config.ValidateRepo(*repo); err != nil {
			return nil, err
		}
	}
	l, err := lock.Acquire(lock.RunPath(a.paths.State), "run")
	if err != nil {
		return nil, err
	}
	defer l.Close()
	cfg, err := config.Load(a.paths.Config)
	if err != nil {
		return nil, err
	}
	consented, err := a.paths.Consented()
	if err != nil {
		return nil, err
	}
	if !consented {
		return nil, fmt.Errorf("run prq init to acknowledge trusted agent execution first")
	}
	repos := cfg.Repos
	if *repo != "" {
		repos = []config.Repo{{Name: *repo}}
		for _, configured := range cfg.Repos {
			if strings.EqualFold(configured.Name, *repo) {
				repos = []config.Repo{configured}
				break
			}
		}
	}
	if len(repos) == 0 {
		return nil, fmt.Errorf("configure at least one repository or pass --repo")
	}
	for _, tool := range []string{"git", "gh", cfg.Agent.Executable} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("required executable %q: %w", tool, err)
		}
	}
	var remote queue.RunRemote
	if a.remote == nil {
		remote = github.New()
	} else {
		var ok bool
		remote, ok = a.remote.(queue.RunRemote)
		if !ok {
			return nil, fmt.Errorf("GitHub client cannot list PRs")
		}
	}
	if _, err := remote.Identity(ctx, cfg.GitHub.User); err != nil {
		return nil, err
	}
	var s *store.Store
	workDir := a.paths.State
	if *dry {
		source, err := store.OpenReadOnly(ctx, a.paths.Database)
		if err != nil {
			return nil, err
		}
		s, err = source.CloneMemory(ctx)
		source.Close()
		if err != nil {
			return nil, err
		}
		workDir, err = os.MkdirTemp("", "prqueue-dry-")
		if err != nil {
			s.Close()
			return nil, err
		}
		defer os.RemoveAll(workDir)
	} else {
		s, err = store.Open(ctx, a.paths.Database)
		if err != nil {
			return nil, err
		}
	}
	defer s.Close()
	agent := runner.Runner{StateDir: workDir, Agent: cfg.Agent}
	if !*dry {
		_, cleanupErr := agent.Cleanup(ctx)
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("clean orphaned worktrees: %w", cleanupErr)
		}
		if err := errors.Join(cleanupErr, s.FailInterruptedRuns(ctx)); err != nil {
			return nil, err
		}
	}
	coordinator := queue.Coordinator{Store: s, Remote: remote, Reviewer: agent, StateDir: a.paths.State, WorkDir: workDir, Parallel: cfg.Agent.MaxParallelReviews, Source: a.source}
	r, err := coordinator.Run(ctx, repos, *pr)
	r.DryRun = *dry
	if *dry {
		for i := range r.Repos {
			for j := range r.Repos[i].Reviews {
				r.Repos[i].Reviews[j].OutputPath = ""
			}
		}
	} else if completionErr := queue.FinishRun(ctx, a.paths.State, r, a.notifications); completionErr != nil {
		fmt.Fprintf(a.errOut, "Run completed; notification/summary warning: %s\n", completionErr)
	}
	return r, err
}
