package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/DustinVK/pr-queue/internal/lock"
	"github.com/DustinVK/pr-queue/internal/runner"
	"github.com/DustinVK/pr-queue/internal/store"
	"github.com/google/uuid"
)

type RunRemote interface {
	Remote
	PRReader
}
type Reviewer interface {
	Review(context.Context, runner.Request) (runner.Result, error)
}
type Coordinator struct {
	Store    *store.Store
	Remote   RunRemote
	Reviewer Reviewer
	StateDir string
	WorkDir  string
	Parallel int
	Source   func(string) string
}
type ReviewResult struct {
	ID         string           `json:"id"`
	Repo       string           `json:"repo"`
	PR         int              `json:"pr"`
	Status     string           `json:"status"`
	OutputPath string           `json:"raw_output_path,omitempty"`
	Error      string           `json:"error,omitempty"`
	Ingestion  *store.Ingestion `json:"queue_changes,omitempty"`
}
type RepoResult struct {
	Repo string `json:"repo"`
	ObservationBatch
	Reviews []ReviewResult `json:"reviews"`
}
type RunResult struct {
	StartedAt  string       `json:"started_at"`
	FinishedAt string       `json:"finished_at"`
	DryRun     bool         `json:"dry_run"`
	Repos      []RepoResult `json:"repos"`
	Failed     int          `json:"failed"`
	Changed    int          `json:"changed"`
}
type PartialFailure struct{ Count int }

func (e *PartialFailure) Error() string {
	return fmt.Sprintf("run completed with %d repository/PR failures", e.Count)
}

// Run requires the caller's global run lock. Worker agents never hold PR locks.
func (c Coordinator) Run(ctx context.Context, repos []config.Repo, forcePR int) (RunResult, error) {
	out := RunResult{StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Repos: []RepoResult{}}
	if c.Parallel < 1 {
		return out, fmt.Errorf("parallel reviews must be positive")
	}
	type task struct {
		repoIndex int
		target    store.PullRequest
	}
	var tasks []task
	for _, repo := range repos {
		batch, err := (Observer{Store: c.Store, Remote: c.Remote, StateDir: c.StateDir}).ObserveRepo(ctx, repo, forcePR)
		if err != nil {
			batch.Problems = append(batch.Problems, Problem{Repo: repo.Name, Error: err.Error()})
		}
		index := len(out.Repos)
		out.Repos = append(out.Repos, RepoResult{Repo: repo.Name, ObservationBatch: batch, Reviews: []ReviewResult{}})
		out.Failed += len(batch.Problems)
		for _, o := range batch.Observations {
			if o.Eligible {
				tasks = append(tasks, task{repoIndex: index, target: *o.Local})
			}
		}
	}
	results := make([]ReviewResult, len(tasks))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(c.Parallel, len(tasks)) {
		wg.Go(func() {
			for i := range jobs {
				results[i] = c.review(ctx, tasks[i].target)
			}
		})
	}
	for i := range tasks {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	for i, result := range results {
		index := tasks[i].repoIndex
		out.Repos[index].Reviews = append(out.Repos[index].Reviews, result)
		if result.Error != "" {
			out.Failed++
		}
		if result.Ingestion != nil {
			out.Changed += result.Ingestion.Changed
		}
	}
	out.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	if out.Failed > 0 {
		return out, &PartialFailure{Count: out.Failed}
	}
	return out, nil
}

func (c Coordinator) review(ctx context.Context, p store.PullRequest) ReviewResult {
	id := uuid.NewString()
	out := ReviewResult{ID: id, Repo: p.Repo, PR: p.Number, Status: "failed", OutputPath: runner.OutputPath(c.WorkDir, id)}
	var attempt store.ReviewRun
	fail := func(err error, timedOut bool) ReviewResult {
		out.Error = err.Error()
		if timedOut {
			out.Status = "timed_out"
		}
		if attempt.ID != "" {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if e := c.Store.FailRun(cleanupCtx, attempt, out.Status, err); e != nil {
				out.Error = errors.Join(err, e).Error()
			}
		}
		return out
	}
	l, err := lock.Acquire(lock.PRPath(c.StateDir, p.Repo, p.Number), fmt.Sprintf("%s#%d start review", p.Repo, p.Number))
	if err != nil {
		return fail(err, false)
	}
	attempt, err = c.Store.StartRun(ctx, id, p, out.OutputPath)
	l.Close()
	if err != nil {
		return fail(err, false)
	}
	current, err := c.Remote.FetchPR(ctx, p.Repo, p.Number)
	if err != nil {
		return fail(err, false)
	}
	if current.Key() != p.Key() {
		l, err := lock.Acquire(lock.PRPath(c.StateDir, p.Repo, p.Number), fmt.Sprintf("%s#%d observe changed comparison", p.Repo, p.Number))
		if err != nil {
			return fail(err, false)
		}
		// Refresh under the lock so a newer observation cannot be overwritten.
		current, err = c.Remote.FetchPR(ctx, p.Repo, p.Number)
		if err == nil {
			_, _, err = c.Store.Observe(ctx, p.Repo, p.Number, current.Comparison, "coordinator")
		}
		l.Close()
		if err != nil {
			return fail(err, false)
		}
		return fail(fmt.Errorf("comparison changed before agent execution"), false)
	}
	diff, err := c.Remote.FetchDiff(ctx, current)
	if err != nil {
		return fail(err, false)
	}
	req := runner.Request{ID: id, Input: findings.Input{Repo: p.Repo, PR: p.Number, HeadSHA: p.HeadSHA}, Comparison: p.Comparison, Diff: diff}
	if c.Source != nil {
		req.Source = c.Source(p.Repo)
	}
	agent, err := c.Reviewer.Review(ctx, req)
	if err != nil {
		return fail(err, agent.TimedOut)
	}
	l, err = lock.Acquire(lock.PRPath(c.StateDir, p.Repo, p.Number), fmt.Sprintf("%s#%d ingest", p.Repo, p.Number))
	if err != nil {
		return fail(fmt.Errorf("ingestion lock: %w", err), false)
	}
	defer l.Close()
	current, err = c.Remote.FetchPR(ctx, p.Repo, p.Number)
	if err != nil {
		return fail(err, false)
	}
	observed, _, err := c.Store.Observe(ctx, p.Repo, p.Number, current.Comparison, "coordinator")
	if err != nil {
		return fail(err, false)
	}
	if observed.Key() != p.Key() {
		return fail(fmt.Errorf("comparison changed during agent execution; retained output at %s", out.OutputPath), false)
	}
	ingested, err := c.Store.Ingest(ctx, attempt, agent.Document, diff)
	if err != nil {
		return fail(err, false)
	}
	out.Status = "succeeded"
	out.Ingestion = &ingested
	return out
}
