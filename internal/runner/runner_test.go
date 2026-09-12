package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/google/uuid"
)

func TestMain(m *testing.M) {
	if os.Getenv("PRQ_TEST_COORDINATOR") == "1" && os.Getenv("PRQUEUE_OUTPUT") == "" {
		var req Request
		if err := json.Unmarshal([]byte(os.Getenv("PRQ_TEST_REQUEST")), &req); err != nil {
			panic(err)
		}
		r := Runner{StateDir: os.Getenv("PRQ_TEST_STATE"), Agent: config.Agent{Executable: os.Args[0], Timeout: time.Minute}}
		_, err := r.Review(context.Background(), req)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if mode := os.Getenv("PRQ_RUNNER_TEST_MODE"); mode != "" && os.Getenv("PRQUEUE_OUTPUT") != "" {
		os.Exit(fakeAgent(mode))
	}
	os.Exit(m.Run())
}

func fakeAgent(mode string) int {
	output := os.Getenv("PRQUEUE_OUTPUT")
	if mode == "starting-child" {
		if err := os.WriteFile(output, []byte("ready"), 0600); err != nil {
			panic(err)
		}
		time.Sleep(time.Minute)
		return 0
	}
	if mode == "sleep-child" {
		time.Sleep(time.Minute)
		return 0
	}
	if mode == "timeout" {
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "PRQ_RUNNER_TEST_MODE=sleep-child")
		if err := child.Start(); err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(filepath.Dir(output), "child.pid"), []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
			panic(err)
		}
		time.Sleep(time.Minute)
		return 0
	}
	if mode == "crash" {
		return 42
	}
	if mode == "missing" {
		return 0
	}
	if mode == "invalid" {
		os.WriteFile(output, []byte("not JSON"), 0600)
		return 0
	}
	var input findings.Input
	if err := json.Unmarshal([]byte(os.Getenv("PRQUEUE_INPUT")), &input); err != nil {
		panic(err)
	}
	if os.Getenv("GH_TOKEN") != "" || os.Getenv("GITHUB_TOKEN") != "" {
		return 43
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "test-model-auth" {
		return 44
	}
	info, err := os.Stat(".git")
	if err != nil || !info.Mode().IsRegular() {
		return 45
	}
	if mode == "commit" {
		if out, err := exec.Command("git", "-c", "core.hooksPath=/dev/null", "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "unexpected").CombinedOutput(); err != nil {
			fmt.Fprintln(os.Stderr, string(out))
			return 46
		}
	}
	if mode == "git-context" {
		for _, check := range []struct {
			args []string
			want string
		}{
			{[]string{"rev-parse", "HEAD"}, input.HeadSHA},
			{[]string{"status", "--porcelain=v1"}, ""},
			{[]string{"config", "--get", "prqueue.testglobal"}, "preserved"},
		} {
			out, err := exec.Command("git", check.args...).CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != check.want {
				fmt.Fprintf(os.Stderr, "agent git %v: %q %v; want %q\n", check.args, out, err, check.want)
				return 47
			}
		}
	}
	d := findings.Document{SchemaVersion: 1, Repo: input.Repo, PR: input.PR, HeadSHA: input.HeadSHA, Summary: "Synthetic review", Verdict: "comment", Findings: []findings.Finding{}}
	data, err := json.Marshal(d)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(output, data, 0600); err != nil {
		panic(err)
	}
	fmt.Println("This stdout is diagnostics, not the findings document.")
	return 0
}

func localFixture(t *testing.T) (Request, string) {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source repo")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		all := append([]string{"-C", source, "-c", "core.hooksPath=/dev/null", "-c", "user.name=Test", "-c", "user.email=test@example.invalid"}, args...)
		out, err := exec.Command("git", all...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(source, "value.txt"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "value.txt")
	run("commit", "-m", "base")
	base := run("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(source, "value.txt"), []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "value.txt")
	run("commit", "-m", "head")
	head := run("rev-parse", "HEAD")
	diff, err := findings.ParseDiff(run("diff", base, head))
	if err != nil {
		t.Fatal(err)
	}
	return Request{ID: uuid.NewString(), Input: findings.Input{Repo: "owner/repo", PR: 1, HeadSHA: head}, Comparison: findings.Comparison{HeadSHA: head, BaseSHA: base, State: "open"}, Source: source, Diff: diff}, source
}

func testRunner(t *testing.T, mode string, timeout time.Duration) Runner {
	t.Helper()
	t.Setenv("PRQ_RUNNER_TEST_MODE", mode)
	t.Setenv("GH_TOKEN", "test-github-auth")
	t.Setenv("GITHUB_TOKEN", "test-github-auth")
	t.Setenv("ANTHROPIC_API_KEY", "test-model-auth")
	return Runner{StateDir: t.TempDir(), Agent: config.Agent{Executable: os.Args[0], Timeout: timeout}}
}

func TestRunnerWritesContractAndRemovesDetachedWorktree(t *testing.T) {
	req, source := localFixture(t)
	r := testRunner(t, "success", 10*time.Second)
	result, err := r.Review(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Document.HeadSHA != req.Input.HeadSHA || result.Document.Summary != "Synthetic review" {
		t.Fatalf("result %+v", result)
	}
	if _, err := os.Stat(filepath.Join(r.StateDir, "worktrees", req.ID)); !os.IsNotExist(err) {
		t.Fatal("worktree left after success")
	}
	data, err := os.ReadFile(result.LogPath)
	if err != nil || !strings.Contains(string(data), "diagnostics") {
		t.Fatal("missing separate agent log")
	}
	prompt, err := os.ReadFile(filepath.Join(filepath.Dir(result.OutputPath), "prompt.txt"))
	if err != nil || !strings.Contains(string(prompt), "commit, push, invoke gh") {
		t.Fatal("review instructions missing")
	}
	info, err := os.Stat(result.OutputPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("output permissions")
	}
	head, err := exec.Command("git", "-C", source, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(head)) != req.Input.HeadSHA {
		t.Fatal("source checkout changed")
	}
}

func TestRunnerCleansUpOnAgentFailures(t *testing.T) {
	for _, mode := range []string{"crash", "missing", "invalid", "commit"} {
		t.Run(mode, func(t *testing.T) {
			req, _ := localFixture(t)
			r := testRunner(t, mode, 10*time.Second)
			result, err := r.Review(t.Context(), req)
			if err == nil {
				t.Fatal("agent failure accepted")
			}
			if result.TimedOut {
				t.Fatal("ordinary failure reported as timeout")
			}
			if _, err := os.Stat(filepath.Join(r.StateDir, "worktrees", req.ID)); !os.IsNotExist(err) {
				t.Fatal("worktree left after failure")
			}
		})
	}
}

func TestTimeoutKillsAgentProcessGroup(t *testing.T) {
	req, _ := localFixture(t)
	r := testRunner(t, "timeout", 1500*time.Millisecond)
	result, err := r.Review(t.Context(), req)
	if err == nil || !result.TimedOut {
		t.Fatalf("timeout: %+v %v", result, err)
	}
	pidBytes, err := os.ReadFile(filepath.Join(filepath.Dir(result.OutputPath), "child.pid"))
	if err != nil {
		t.Fatal("child did not start:", err)
	}
	pid, _ := strconv.Atoi(string(pidBytes))
	deadline := time.Now().Add(3 * time.Second)
	for {
		stamp, err := processStart(t.Context(), pid)
		if err != nil {
			t.Fatal(err)
		}
		if stamp == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent child survived timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(r.StateDir, "worktrees", req.ID)); !os.IsNotExist(err) {
		t.Fatal("timed-out worktree remained")
	}
}

func TestCleanupPreservesLiveOwnerAndHandlesPIDReuse(t *testing.T) {
	r := Runner{StateDir: t.TempDir()}
	id := uuid.NewString()
	root := filepath.Join(r.StateDir, "worktrees", id)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	o, err := newOwner(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveOwner(root, o); err != nil {
		t.Fatal(err)
	}
	cleaned, err := r.Cleanup(t.Context())
	if err != nil || len(cleaned) != 0 {
		t.Fatalf("removed live owner: %v %v", cleaned, err)
	}
	o.Started = "older process with the same PID"
	if err := saveOwner(root, o); err != nil {
		t.Fatal(err)
	}
	cleaned, err = r.Cleanup(t.Context())
	if err != nil || len(cleaned) != 1 {
		t.Fatalf("stale owner: %v %v", cleaned, err)
	}
}

func TestCleanupAfterCoordinatorKilled(t *testing.T) {
	req, _ := localFixture(t)
	r := testRunner(t, "timeout", time.Minute)
	outerOutput := filepath.Join(t.TempDir(), "findings.json")
	t.Setenv("PRQUEUE_OUTPUT", outerOutput)
	t.Setenv("PRQUEUE_INPUT", "outer review input")
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0])
	// This child is a coordinator, even when go test runs inside a review agent.
	cmd.Env = append(os.Environ(), "PRQUEUE_OUTPUT=", "PRQUEUE_INPUT=", "PRQ_TEST_COORDINATOR=1", "PRQ_TEST_STATE="+r.StateDir, "PRQ_TEST_REQUEST="+string(data))
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	root := filepath.Join(r.StateDir, "worktrees", req.ID)
	var o owner
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, _ := os.ReadFile(ownerPath(root))
		o = owner{}
		_ = json.Unmarshal(data, &o)
		if o.AgentPID > 0 && !o.Starting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	cleaned, err := r.Cleanup(t.Context())
	if err != nil || len(cleaned) != 1 {
		t.Fatalf("orphan cleanup: %v %v", cleaned, err)
	}
	stamp, err := processStart(t.Context(), o.AgentPID)
	if err != nil || stamp != "" {
		t.Fatalf("agent survived coordinator recovery: %s %v", stamp, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(outerOutput), "child.pid")); !os.IsNotExist(err) {
		t.Fatal("test wrote diagnostics into the enclosing review output directory")
	}
}

func TestCleanupInterruptedBeforeWorktreeCreation(t *testing.T) {
	r := Runner{StateDir: t.TempDir()}
	id := uuid.NewString()
	root := filepath.Join(r.StateDir, "worktrees", id)
	o, err := newOwner(id)
	if err != nil {
		t.Fatal(err)
	}
	o.Started = "previous process"
	if err := reserveOwner(root, o); err != nil {
		t.Fatal(err)
	}
	if err := reserveOwner(root, o); err == nil {
		t.Fatal("ownership overwritten")
	}
	cleaned, err := r.Cleanup(t.Context())
	if err != nil || len(cleaned) != 1 {
		t.Fatalf("cleanup: %v %v", cleaned, err)
	}
	if _, err := os.Stat(ownerPath(root)); !os.IsNotExist(err) {
		t.Fatal("orphan metadata remained")
	}
}

func TestCleanupInterruptedDuringWorktreeCreation(t *testing.T) {
	r := Runner{StateDir: t.TempDir()}
	id := uuid.NewString()
	root := filepath.Join(r.StateDir, "worktrees", id)
	o, err := newOwner(id)
	if err != nil {
		t.Fatal(err)
	}
	o.Started = "previous coordinator"
	if err := reserveOwner(root, o); err != nil {
		t.Fatal(err)
	}
	// A killed git worktree add can create checkout before registration finishes.
	if err := os.MkdirAll(filepath.Join(root, "checkout"), 0700); err != nil {
		t.Fatal(err)
	}
	cleaned, err := r.Cleanup(t.Context())
	if err != nil || len(cleaned) != 1 {
		t.Fatalf("incomplete setup cleanup: %v %v", cleaned, err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("incomplete worktree remained")
	}
}

// This is an explicit manual integration check, never part of automated tests.
func TestManualClaudeSmoke(t *testing.T) {
	if os.Getenv("PRQ_RUNNER_REAL_SMOKE") != "1" {
		t.Skip("set PRQ_RUNNER_REAL_SMOKE=1 and PRQ_SMOKE_STATE to run the paid local Claude smoke check")
	}
	state := os.Getenv("PRQ_SMOKE_STATE")
	if !filepath.IsAbs(state) {
		t.Fatal("PRQ_SMOKE_STATE must be an absolute diagnostics directory")
	}
	req, source := localFixture(t)
	agent := config.Defaults().Agent
	r := Runner{StateDir: state, Agent: agent}
	started := time.Now()
	result, err := r.Review(t.Context(), req)
	t.Logf("duration=%s output=%s log=%s", time.Since(started), result.OutputPath, result.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.TimedOut {
		t.Fatal("real agent timed out")
	}
	if _, err := os.Stat(filepath.Join(state, "worktrees", req.ID)); !os.IsNotExist(err) {
		t.Fatal("worktree remained")
	}
	head, err := exec.Command("git", "-C", source, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(head)) != req.Input.HeadSHA {
		t.Fatal("fixture source changed")
	}
	t.Logf("validated repo=%s pr=%d head=%s findings=%d; inspect transcript and saved Git state manually", result.Document.Repo, result.Document.PR, result.Document.HeadSHA, len(result.Document.Findings))
}
