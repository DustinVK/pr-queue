// Package runner owns disposable worktrees and trusted local Claude execution.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/DustinVK/pr-queue/internal/localfs"
	"github.com/google/uuid"
)

type Request struct {
	ID         string
	Input      findings.Input
	Comparison findings.Comparison
	Diff       findings.Diff
	// Source is used for local fixture repositories; empty means github.com.
	Source string
}
type Result struct {
	Document   findings.Document
	OutputPath string
	LogPath    string
	TimedOut   bool
}
type Runner struct {
	StateDir string
	Agent    config.Agent
}

func OutputPath(state, id string) string { return filepath.Join(state, "runs", id, "findings.json") }

func (r Runner) Review(ctx context.Context, req Request) (result Result, err error) {
	if _, err := uuid.Parse(req.ID); err != nil {
		return result, err
	}
	if err := req.Comparison.Validate(); err != nil {
		return result, err
	}
	if req.Input.HeadSHA != req.Comparison.HeadSHA || req.Input.PR < 1 {
		return result, fmt.Errorf("invalid runner input")
	}
	if err := config.ValidateRepo(req.Input.Repo); err != nil {
		return result, err
	}
	if r.Agent.Timeout <= 0 {
		return result, fmt.Errorf("agent timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, r.Agent.Timeout)
	defer cancel()
	defer func() { result.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded) }()
	root := filepath.Join(r.StateDir, "worktrees", req.ID)
	diagnostics := filepath.Dir(OutputPath(r.StateDir, req.ID))
	if err := localfs.PrivateDir(filepath.Dir(root)); err != nil {
		return result, err
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		return result, fmt.Errorf("worktree path already exists or cannot be inspected: %s", root)
	}
	owner, err := newOwner(req.ID)
	if err != nil {
		return result, err
	}
	if err := reserveOwner(root, owner); err != nil {
		return result, err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if e := removeWorktree(cleanupCtx, root); e != nil {
			err = errors.Join(err, e)
		}
	}()
	if err := os.Mkdir(root, 0700); err != nil {
		return result, err
	}
	if err := localfs.PrivateDir(diagnostics); err != nil {
		return result, err
	}
	result.OutputPath = OutputPath(r.StateDir, req.ID)
	result.LogPath = filepath.Join(diagnostics, "agent.jsonl")
	bare := filepath.Join(root, "repo.git")
	checkout := filepath.Join(root, "checkout")
	if _, err := git(ctx, "", "init", "--bare", bare); err != nil {
		return result, err
	}
	source := req.Source
	if source == "" {
		source = "https://github.com/" + req.Input.Repo + ".git"
	}
	if _, err := git(ctx, bare, "remote", "add", "origin", source); err != nil {
		return result, err
	}
	if _, err := git(ctx, bare, "fetch", "--no-tags", "origin", req.Comparison.HeadSHA, req.Comparison.BaseSHA); err != nil {
		return result, err
	}
	if _, err := git(ctx, bare, "worktree", "add", "--detach", checkout, req.Comparison.HeadSHA); err != nil {
		return result, err
	}
	diffPath := filepath.Join(diagnostics, "comparison.diff")
	var diff strings.Builder
	paths := make([]string, 0, len(req.Diff.Files))
	for path := range req.Diff.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		f := req.Diff.Files[path]
		fmt.Fprintf(&diff, "File: %q\n%s\n", path, f.Patch)
		if f.Error != "" {
			fmt.Fprintf(&diff, "Anchor validation unavailable: %s\n", f.Error)
		}
	}
	if err := os.WriteFile(diffPath, []byte(diff.String()), 0600); err != nil {
		return result, err
	}
	prompt := Prompt(req.Input, req.Comparison.BaseSHA, result.OutputPath, diffPath)
	if err := os.WriteFile(filepath.Join(diagnostics, "prompt.txt"), []byte(prompt), 0600); err != nil {
		return result, err
	}
	before, err := git(ctx, bare, "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return result, err
	}
	if err := os.WriteFile(filepath.Join(diagnostics, "refs-before.txt"), before, 0600); err != nil {
		return result, err
	}
	stdout, err := os.OpenFile(result.LogPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, err
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(filepath.Join(diagnostics, "agent-stderr.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, err
	}
	defer stderr.Close()
	args := []string{"-p", "--verbose", "--output-format", "stream-json", "--no-session-persistence", "--session-id", req.ID, "--dangerously-skip-permissions"}
	cmd := exec.CommandContext(ctx, r.Agent.Executable, args...)
	cmd.Dir = checkout
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = agentEnv(result.OutputPath, req.Input)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd.Process.Pid) }
	cmd.WaitDelay = 2 * time.Second
	owner.Starting = true
	if err := saveOwner(root, owner); err != nil {
		return result, err
	}
	if err := cmd.Start(); err != nil {
		return result, err
	}
	owner.AgentPID = cmd.Process.Pid
	owner.AgentStart, err = processStart(ctx, owner.AgentPID)
	if err != nil {
		killGroup(cmd.Process.Pid)
		cmd.Wait()
		return result, err
	}
	owner.Starting = false
	if err := saveOwner(root, owner); err != nil {
		killGroup(cmd.Process.Pid)
		cmd.Wait()
		return result, err
	}
	runErr := cmd.Wait()
	_ = killGroup(cmd.Process.Pid) // Also stop any ordinary leftover child processes.
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if runErr != nil {
		return result, fmt.Errorf("agent failed (see %s): %w", result.LogPath, runErr)
	}
	if err := stdout.Sync(); err != nil {
		return result, err
	}
	head, err := git(ctx, "", "-C", checkout, "rev-parse", "HEAD")
	if err != nil {
		return result, err
	}
	after, err := git(ctx, bare, "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return result, err
	}
	if err := os.WriteFile(filepath.Join(diagnostics, "refs-after.txt"), after, 0600); err != nil {
		return result, err
	}
	status, statusErr := git(ctx, "", "-C", checkout, "status", "--porcelain=v1")
	if statusErr != nil {
		return result, statusErr
	}
	if err := os.WriteFile(filepath.Join(diagnostics, "worktree-status.txt"), status, 0600); err != nil {
		return result, err
	}
	if strings.TrimSpace(string(head)) != req.Input.HeadSHA {
		return result, fmt.Errorf("agent changed the reviewed commit")
	}
	info, err := os.Lstat(result.OutputPath)
	if err != nil {
		return result, fmt.Errorf("agent did not write findings.json: %w", err)
	}
	if !info.Mode().IsRegular() {
		return result, fmt.Errorf("findings output must be a regular file")
	}
	if err := os.Chmod(result.OutputPath, 0600); err != nil {
		return result, err
	}
	result.Document, err = findings.DecodeFile(result.OutputPath, req.Input)
	return result, err
}

func agentEnv(output string, input findings.Input) []string {
	var env []string
	for _, v := range os.Environ() {
		key, _, _ := strings.Cut(v, "=")
		if key == "GH_TOKEN" || key == "GITHUB_TOKEN" || key == "GH_ENTERPRISE_TOKEN" || key == "GITHUB_ENTERPRISE_TOKEN" || strings.HasPrefix(key, "PRQUEUE_") {
			continue
		}
		env = append(env, v)
	}
	data, _ := json.Marshal(input)
	return append(env, "PRQUEUE_OUTPUT="+output, "PRQUEUE_INPUT="+string(data))
}

func killGroup(pid int) error {
	if pid < 2 {
		return fmt.Errorf("refuse invalid process group %d", pid)
	}
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func git(ctx context.Context, bare string, args ...string) ([]byte, error) {
	fixed := []string{"-c", "core.hooksPath=/dev/null", "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential"}
	if bare != "" {
		fixed = append(fixed, "--git-dir", bare)
	}
	cmd := exec.CommandContext(ctx, "git", append(fixed, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd.Process.Pid) }
	cmd.WaitDelay = 2 * time.Second
	var out, diagnostics bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &diagnostics
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, diagnostics.String())
	}
	return out.Bytes(), nil
}
