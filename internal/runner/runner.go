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
	StateDir     string
	Agent        config.Agent
	launchFaults *launchFaults
}

func OutputPath(state, id string) string { return filepath.Join(state, "runs", id, "findings.json") }

func (r Runner) Review(ctx context.Context, req Request) (result Result, err error) {
	agent, err := r.Agent.Normalized()
	if err != nil {
		return result, err
	}
	resolvedExecutable, err := resolveExecutable(agent.Executable)
	if err != nil {
		return result, fmt.Errorf("find agent executable %q: %w", agent.Executable, err)
	}
	r.Agent = agent
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
		var cleanupFailure *launchCleanupError
		if errors.As(err, &cleanupFailure) {
			return
		}
		if e := removeWorktree(root); e != nil {
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
	scratch := filepath.Join(diagnostics, "scratch")
	if err := localfs.PrivateDir(scratch); err != nil {
		return result, err
	}
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
	schemaPath := filepath.Join(diagnostics, "findings-schema.json")
	if agent.Provider == config.ProviderCodex {
		if err := WriteOutputSchema(schemaPath, req.Input); err != nil {
			return result, err
		}
	}
	invocation, err := BuildInvocation(agent.Provider, req.ID, req.Input, req.Comparison, InvocationPaths{Output: result.OutputPath, Diff: diffPath, Scratch: scratch, Schema: schemaPath})
	if err != nil {
		return result, err
	}
	prompt := invocation.Prompt
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
	metadataPath := filepath.Join(diagnostics, MetadataFilename)
	metadata := PreparedMetadata(req.ID, req.Input, req.Comparison, agent.Provider, agent.Executable, resolvedExecutable, invocation.Args)
	if err := WritePreparedAgentMetadata(metadataPath, metadata); err != nil {
		return result, err
	}
	process, err := newGatedProcess(ctx, resolvedExecutable, invocation.Args)
	if err != nil {
		return result, err
	}
	cmd := process.cmd
	cmd.Dir = checkout
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = agentEnv(result.OutputPath, req.Input)
	afterRelease := func() error {
		metadata.MarkReleased()
		return WriteAgentMetadata(metadataPath, metadata)
	}
	if err := process.start(root, &owner, afterRelease, r.launchFaults); err != nil {
		return result, err
	}
	runErr := cmd.Wait()
	cleanupErr := killGroup(cmd.Process.Pid) // Also stop any ordinary leftover child processes.
	if cleanupErr != nil {
		return result, &launchCleanupError{err: cleanupErr}
	}
	if err := stdout.Sync(); err != nil {
		return result, err
	}
	var observed *ObservedIdentity
	logReader, openErr := os.Open(result.LogPath)
	var closeErr error
	if openErr == nil {
		observed = ObservedIdentityFromJSONLReader(agent.Provider, logReader)
		closeErr = logReader.Close()
	}
	readErr := errors.Join(openErr, closeErr)
	metadata.MarkFinished(observed)
	metadataErr := WriteAgentMetadata(metadataPath, metadata)
	if ctx.Err() != nil {
		return result, errors.Join(ctx.Err(), readErr, metadataErr)
	}
	if runErr != nil {
		return result, errors.Join(fmt.Errorf("agent failed (see %s): %w", result.LogPath, runErr), readErr, metadataErr)
	}
	if err := errors.Join(readErr, metadataErr); err != nil {
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
	result.Document, err = DecodeProviderOutput(result.OutputPath, req.Input)
	return result, err
}

func resolveExecutable(configured string) (string, error) {
	resolved, err := exec.LookPath(configured)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

func agentEnv(output string, input findings.Input) []string {
	var env []string
	for _, v := range repositoryEnv() {
		key, _, _ := strings.Cut(v, "=")
		if key == "GH_TOKEN" || key == "GITHUB_TOKEN" || key == "GH_ENTERPRISE_TOKEN" || key == "GITHUB_ENTERPRISE_TOKEN" || strings.HasPrefix(key, "PRQUEUE_") {
			continue
		}
		env = append(env, v)
	}
	data, _ := json.Marshal(input)
	return append(env, "PRQUEUE_OUTPUT="+output, "PRQUEUE_INPUT="+string(data))
}

// A hook or caller may export context for its own repository. Neither Git nor
// the agent should carry that context into the disposable review checkout.
// Keep global configuration and transport/authentication settings available.
func repositoryEnv() []string {
	var env []string
	for _, v := range os.Environ() {
		key, _, _ := strings.Cut(v, "=")
		switch key {
		case "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
			"GIT_OBJECT_DIRECTORY", "GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE",
			"GIT_GRAFT_FILE", "GIT_INDEX_FILE", "GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE",
			"GIT_PREFIX", "GIT_SHALLOW_FILE", "GIT_COMMON_DIR", "GIT_NAMESPACE",
			"GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM":
			continue
		}
		if strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			continue
		}
		env = append(env, v)
	}
	return env
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
	cmd.Env = append(repositoryEnv(), "GIT_TERMINAL_PROMPT=0")
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
