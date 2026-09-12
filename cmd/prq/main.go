package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/localfs"
	"github.com/DustinVK/pr-queue/internal/lock"
	"github.com/DustinVK/pr-queue/internal/notify"
	"github.com/DustinVK/pr-queue/internal/queue"
	"github.com/DustinVK/pr-queue/internal/store"
)

const trustNotice = `Claude Code runs with your full user permissions and can access ambient GitHub authentication.
A worktree is not a sandbox. The agent is instructed not to commit, push, or invoke gh;
prqueue's approval guarantee applies only to its own publisher, not to an agent acting independently.
`

const usage = `Usage: prq <command> [options] [--json]

Available:
  init [--accept-agent-risk]   Initialize config/database; acknowledge trusted agent execution
  status                      Show local state, locks, and unresolved publications
  list [--repo R] [--status S] List local findings
  show <owner/name#N>          Show findings and historical publications
  diff <finding-id>           Show a finding against the current PR diff
  run [--repo R] [--pr N] [--dry-run] Draft reviews into the local queue
  approve <finding-id>...      Approve current, valid findings for one PR
  reject <finding-id>... [--reason TEXT] Reject findings locally
  edit <finding-id> [--as-general] Edit the body with $EDITOR
  publish <owner/name#N> --event COMMENT|REQUEST_CHANGES|APPROVE [--dry-run]
  publish <owner/name#N> --resume [--confirmed-not-sent]
`

type app struct {
	paths         config.Paths
	in            io.Reader
	out           io.Writer
	errOut        io.Writer
	remote        queue.Remote
	source        func(string) string // Local Git fixture source; nil uses GitHub.
	notifications notify.Sink
}

type result struct {
	OK      bool   `json:"ok"`
	Command string `json:"command"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

func main() {
	paths, err := config.DefaultPaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	a := app{paths: paths, in: os.Stdin, out: os.Stdout, errOut: os.Stderr, notifications: notify.Desktop{}}
	code := a.run(ctx, os.Args[1:])
	cancel()
	os.Exit(code)
}

func (a *app) run(ctx context.Context, args []string) int {
	jsonMode, args := extractJSON(args)
	command := "help"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	var data any
	var err error
	switch command {
	case "init", "status", "list", "show", "diff", "approve", "reject", "edit", "publish":
		err = a.recoverInterrupted(ctx, args)
	}
	if err == nil {
		switch command {
		case "help", "--help", "-h":
			if len(args) != 0 {
				err = fmt.Errorf("help takes no arguments")
			} else {
				data = map[string]string{"usage": usage}
			}
		case "init":
			data, err = a.init(ctx, args)
		case "status":
			if len(args) != 0 {
				err = fmt.Errorf("status takes no arguments")
			} else {
				data, err = a.status(ctx)
			}
		case "list", "show", "diff":
			data, err = a.readQueue(ctx, command, args)
		case "run":
			data, err = a.runReviews(ctx, args)
		case "approve", "reject", "edit":
			data, err = a.triage(ctx, command, args)
		case "publish":
			data, err = a.publish(ctx, args)
		default:
			err = fmt.Errorf("unknown command %q; use prq help", command)
		}
	}
	code := 0
	r := result{OK: err == nil, Command: command, Data: data}
	if err != nil {
		code = 1
		var busy *lock.BusyError
		if errors.As(err, &busy) {
			code = 3
		}
		var partial *queue.PartialFailure
		if errors.As(err, &partial) {
			code = 2
		}
		r.Error = err.Error()
		fmt.Fprintln(a.errOut, err)
	}
	if jsonMode {
		if e := json.NewEncoder(a.out).Encode(r); e != nil {
			fmt.Fprintln(a.errOut, e)
			return 1
		}
	} else if err == nil || data != nil {
		if command == "help" || command == "--help" || command == "-h" {
			fmt.Fprint(a.out, usage)
		} else {
			a.human(command, data)
		}
	}
	return code
}

type initResult struct {
	Paths             config.Paths `json:"paths"`
	ConfigCreated     bool         `json:"config_created"`
	AgentAcknowledged bool         `json:"agent_acknowledged"`
}

func (a *app) init(ctx context.Context, args []string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	accept := fs.Bool("accept-agent-risk", false, "acknowledge trusted local agent execution")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 {
		return nil, fmt.Errorf("init takes no positional arguments")
	}
	l, err := lock.Acquire(lock.RunPath(a.paths.State), "init")
	if err != nil {
		return nil, err
	}
	defer l.Close()
	ack, err := a.paths.Consented()
	if err != nil {
		return nil, err
	}
	if !ack {
		fmt.Fprint(a.errOut, trustNotice)
		if !*accept {
			fmt.Fprint(a.errOut, "Accept this agent access? Type yes: ")
			line, readErr := readLine(ctx, a.in)
			if readErr != nil && readErr != io.EOF {
				return nil, readErr
			}
			if strings.TrimSpace(line) != "yes" {
				return nil, fmt.Errorf("agent access not acknowledged; rerun prq init and type yes, or use --accept-agent-risk")
			}
		}
	}
	created, err := localfs.WriteNew(a.paths.Config, []byte(config.DefaultYAML))
	if err != nil {
		return nil, err
	}
	if _, err = config.Load(a.paths.Config); err != nil {
		return nil, err
	}
	s, err := store.Create(ctx, a.paths.Database)
	if err != nil {
		return nil, err
	}
	if err = s.Close(); err != nil {
		return nil, err
	}
	if !ack {
		if _, err = localfs.WriteNew(a.paths.Consent, []byte(config.ConsentText)); err != nil {
			return nil, err
		}
	}
	return initResult{Paths: a.paths, ConfigCreated: created, AgentAcknowledged: true}, nil
}

func readLine(ctx context.Context, input io.Reader) (string, error) {
	type answer struct {
		line string
		err  error
	}
	ready := make(chan answer, 1)
	go func() {
		line, err := bufio.NewReader(io.LimitReader(input, 128)).ReadString('\n')
		ready <- answer{line, err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case a := <-ready:
		return a.line, a.err
	}
}

type statusResult struct {
	Paths             config.Paths                `json:"paths"`
	AgentAcknowledged bool                        `json:"agent_acknowledged"`
	ConfiguredRepos   []string                    `json:"configured_repos"`
	Locks             []lock.Holder               `json:"locks"`
	LastRuns          []store.RunStatus           `json:"last_runs"`
	Publications      []store.BlockingPublication `json:"publications_needing_resume"`
	LastPasses        []queue.RepoPass            `json:"last_passes"`
	NotificationError string                      `json:"notification_error,omitempty"`
	SummaryError      string                      `json:"summary_error,omitempty"`
}

func (a *app) status(ctx context.Context) (any, error) {
	c, err := config.Load(a.paths.Config)
	if err != nil {
		return nil, err
	}
	ack, err := a.paths.Consented()
	if err != nil {
		return nil, err
	}
	s, err := store.OpenReadOnly(ctx, a.paths.Database)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	r := statusResult{Paths: a.paths, AgentAcknowledged: ack, ConfiguredRepos: []string{}, Locks: []lock.Holder{}}
	for _, repo := range c.Repos {
		r.ConfiguredRepos = append(r.ConfiguredRepos, repo.Name)
	}
	if r.LastRuns, err = s.LastRuns(ctx); err != nil {
		return nil, err
	}
	if r.Publications, err = s.BlockingPublications(ctx); err != nil {
		return nil, err
	}
	summary, summaryErr := queue.LoadRunSummary(a.paths.State)
	r.LastPasses = []queue.RepoPass{}
	if summaryErr != nil {
		r.SummaryError = summaryErr.Error()
	} else {
		r.NotificationError = summary.NotificationError
		for _, pass := range summary.Passes {
			r.LastPasses = append(r.LastPasses, pass)
		}
		sort.Slice(r.LastPasses, func(i, j int) bool { return r.LastPasses[i].Repo < r.LastPasses[j].Repo })
	}
	lockPaths, err := filepath.Glob(filepath.Join(a.paths.State, "locks", "*.lock"))
	if err != nil {
		return nil, err
	}
	lockPaths = append([]string{lock.RunPath(a.paths.State)}, lockPaths...)
	for _, path := range lockPaths {
		h, err := lock.Inspect(path)
		if err != nil {
			return nil, err
		}
		if h != nil {
			r.Locks = append(r.Locks, *h)
		}
	}
	return r, nil
}

func (a *app) human(command string, data any) {
	switch r := data.(type) {
	case queue.PublicationResult:
		if r.Noop {
			fmt.Fprintln(a.out, "Nothing to publish or recover.")
			break
		}
		if r.Publication != nil {
			p := r.Publication
			fmt.Fprintf(a.out, "Publication %s: %s (uncertain=%t)\n", p.ID, p.Status, p.Uncertain)
			if p.ReviewID != nil {
				fmt.Fprintf(a.out, "GitHub review: %s\n", *p.ReviewID)
			}
		}
	case queue.PublicationPreview:
		if r.Noop {
			fmt.Fprintln(a.out, "No approved findings; nothing to publish.")
			break
		}
		fmt.Fprintln(a.out, "Publication preview:")
		encoder := json.NewEncoder(a.out)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(r.Snapshot.Request); err != nil {
			fmt.Fprintln(a.errOut, err)
		}
	case queue.EditResult:
		fmt.Fprintf(a.out, "Finding changed: %t\n", r.Changed)
		a.printFinding(r.Finding)
	case queue.RunResult:
		fmt.Fprintf(a.out, "Review pass finished: %d failures, %d new or changed findings (dry-run=%t).\n", r.Failed, r.Changed, r.DryRun)
		for _, repo := range r.Repos {
			for _, p := range repo.Problems {
				if p.PR > 0 {
					fmt.Fprintf(a.out, "%s#%d: %s\n", p.Repo, p.PR, p.Error)
				} else {
					fmt.Fprintf(a.out, "%s: %s\n", p.Repo, p.Error)
				}
			}
			for _, o := range repo.Observations {
				if !o.Eligible {
					fmt.Fprintf(a.out, "%s#%d: skipped: %s\n", repo.Repo, o.Number, o.Reason)
				}
			}
			for _, review := range repo.Reviews {
				fmt.Fprintf(a.out, "%s#%d: %s", review.Repo, review.PR, review.Status)
				if review.Error != "" {
					fmt.Fprintf(a.out, ": %s", review.Error)
				}
				fmt.Fprintln(a.out)
			}
		}
	case []store.Finding:
		if len(r) == 0 {
			fmt.Fprintln(a.out, "No findings.")
		}
		for _, f := range r {
			a.printFinding(f)
		}
	case queue.ShowResult:
		fmt.Fprintf(a.out, "%s#%d (%s, draft=%t)\n", r.PR.Repo, r.PR.Number, r.PR.State, r.PR.Draft)
		for _, f := range r.Findings {
			a.printFinding(f)
		}
		for _, p := range r.Publications {
			fmt.Fprintf(a.out, "Historical publication %s (%s):\n%s\n", p.ID, p.Event, p.Snapshot)
		}
	case queue.DiffResult:
		a.printFinding(r.Finding)
		fmt.Fprintf(a.out, "Stale comparison: %t\n", r.Stale)
		if r.ValidationError != "" {
			fmt.Fprintln(a.out, r.ValidationError)
		}
		fmt.Fprintln(a.out, r.Patch)
	case initResult:
		fmt.Fprintf(a.out, "Initialized prqueue.\nConfig: %s\nDatabase: %s\nSet github.user and repos in the config before reviewing.\n", r.Paths.Config, r.Paths.Database)
	case statusResult:
		fmt.Fprintf(a.out, "Config: %s\nDatabase: %s\nAgent access acknowledged: %t\nConfigured repositories: %d\n", r.Paths.Config, r.Paths.Database, r.AgentAcknowledged, len(r.ConfiguredRepos))
		for _, pass := range r.LastPasses {
			fmt.Fprintf(a.out, "Last pass for %s: %s\n", pass.Repo, pass.FinishedAt)
			for _, problem := range pass.Problems {
				fmt.Fprintf(a.out, "  %s\n", problem.Error)
			}
			for _, review := range pass.Reviews {
				if review.Error != "" {
					fmt.Fprintf(a.out, "  PR #%d: %s\n", review.PR, review.Error)
				}
			}
		}
		if r.NotificationError != "" {
			fmt.Fprintf(a.out, "Last notification failed: %s\n", r.NotificationError)
		}
		if r.SummaryError != "" {
			fmt.Fprintf(a.out, "Run summary unavailable: %s\n", r.SummaryError)
		}
		if len(r.LastRuns) == 0 {
			fmt.Fprintln(a.out, "No review runs yet.")
		}
		for _, run := range r.LastRuns {
			fmt.Fprintf(a.out, "%s#%d: %s (%s)\n", run.Repo, run.PR, run.Status, run.StartedAt)
			if run.Error != nil {
				fmt.Fprintln(a.out, *run.Error)
			}
		}
		if len(r.Locks) == 0 {
			fmt.Fprintln(a.out, "No locks held.")
		}
		for _, h := range r.Locks {
			fmt.Fprintf(a.out, "Lock: %s pid %d since %s\n", h.Key, h.PID, h.Since)
		}
		for _, p := range r.Publications {
			fmt.Fprintf(a.out, "%s#%d: publication %s is %s (uncertain=%t); use prq publish %s#%d --resume\n", p.Repo, p.PR, p.ID, p.Status, p.Uncertain, p.Repo, p.PR)
		}
	}
}
