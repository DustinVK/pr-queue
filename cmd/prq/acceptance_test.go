package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/DustinVK/pr-queue/internal/github"
	"github.com/DustinVK/pr-queue/internal/queue"
	"github.com/DustinVK/pr-queue/internal/store"
)

// The actual gh and agent subprocess boundaries are exercised without network.
func TestMain(m *testing.M) {
	if path := os.Getenv("PRQ_CLI_FIXTURE"); path != "" {
		switch filepath.Base(os.Args[0]) {
		case "gh":
			os.Exit(fixtureGitHub(path))
		case "fake-agent":
			os.Exit(fixtureAgent(path, "claude"))
		case "fake-codex":
			os.Exit(fixtureAgent(path, "codex"))
		}
	}
	os.Exit(m.Run())
}

type apiCall struct {
	Method string
	Path   string
}
type submittedFixture struct {
	ID      int
	Request github.ReviewRequest
}
type agentCall struct {
	Provider string
	Args     []string
}
type cliFixture struct {
	PR        github.PR
	Patch     string
	AgentMode string
	Fault     string
	Posts     int
	Reviews   []submittedFixture
	Calls     []apiCall
	Agents    []agentCall
}

func readFixture(path string) (cliFixture, error) {
	var f cliFixture
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, &f)
	}
	return f, err
}
func writeFixture(path string, f cliFixture) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "fixture-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
func lockedFixture(path string) (*os.File, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func fixturePR(p github.PR) map[string]any {
	return map[string]any{"number": p.Number, "state": p.State, "draft": p.Draft, "head": map[string]string{"sha": p.HeadSHA}, "base": map[string]string{"sha": p.BaseSHA, "ref": "main"}, "user": map[string]string{"login": "author"}, "requested_reviewers": []any{}, "changed_files": 1}
}
func fixtureReview(r submittedFixture) map[string]any {
	return map[string]any{"id": r.ID, "user": map[string]string{"login": "alice"}, "commit_id": r.Request.CommitID, "body": r.Request.Body, "state": map[string]string{"COMMENT": "COMMENTED", "APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED"}[r.Request.Event], "submitted_at": "2026-09-12T12:00:00Z"}
}

func fixtureGitHub(path string) int {
	l, err := lockedFixture(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 99
	}
	defer l.Close()
	f, err := readFixture(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 99
	}
	args := os.Args[1:]
	if len(args) < 2 || args[0] != "api" {
		fmt.Fprintln(os.Stderr, "unexpected gh command")
		return 99
	}
	method := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--method" {
			method = args[i+1]
		}
	}
	endpoint := args[len(args)-1]
	f.Calls = append(f.Calls, apiCall{Method: method, Path: endpoint})
	var response any
	code := 0
	include := false
	switch {
	case method == "GET" && endpoint == "user":
		response = map[string]string{"login": "alice"}
	case method == "GET" && strings.Contains(endpoint, "/pulls?state=open"):
		page := []any{}
		if f.PR.State == "open" {
			page = append(page, fixturePR(f.PR))
		}
		response = []any{page}
		if f.Fault == "repository unavailable" {
			code = 1
		}
	case method == "GET" && strings.HasSuffix(endpoint, "/pulls/1"):
		response = fixturePR(f.PR)
	case method == "GET" && strings.Contains(endpoint, "/files?"):
		response = []any{[]any{map[string]any{"filename": "value.txt", "patch": f.Patch, "additions": 1, "deletions": 1}}}
	case method == "GET" && strings.Contains(endpoint, "/reviews?"):
		page := []any{}
		for _, r := range f.Reviews {
			page = append(page, fixtureReview(r))
		}
		response = []any{page}
		if f.Fault == "incomplete reviews" {
			code = 1
		}
	case method == "GET" && strings.Contains(endpoint, "/reviews/"):
		for _, r := range f.Reviews {
			if strings.HasSuffix(endpoint, "/"+strconv.Itoa(r.ID)) {
				response = fixtureReview(r)
			}
		}
		if response == nil {
			response = map[string]string{"message": "Not Found"}
			code = 1
		}
	case method == "GET" && strings.Contains(endpoint, "/comments?"):
		page := []any{}
		for _, r := range f.Reviews {
			for i, c := range r.Request.Comments {
				page = append(page, map[string]any{"id": r.ID*100 + i + 1, "pull_request_review_id": r.ID, "user": map[string]string{"login": "alice"}, "body": c.Body, "path": c.Path, "side": c.Side, "original_line": c.Line, "original_start_line": c.StartLine, "start_side": c.StartSide, "original_commit_id": r.Request.CommitID, "line": nil, "start_line": nil})
			}
		}
		response = []any{page}
	case method == "POST" && strings.HasSuffix(endpoint, "/reviews"):
		include = true
		var request github.ReviewRequest
		if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil || !github.ValidEvent(request.Event) {
			fmt.Fprintln(os.Stderr, "invalid submitted request")
			return 99
		}
		f.Posts++
		if f.Fault == "reject" {
			response = map[string]string{"message": "Validation Failed"}
			code = 1
			break
		}
		if f.Fault == "lost not sent" {
			code = 1
			break
		}
		r := submittedFixture{ID: len(f.Reviews) + 1, Request: request}
		f.Reviews = append(f.Reviews, r)
		response = fixtureReview(r)
		if f.Fault == "lost response" {
			response = nil
			code = 1
		}
	default:
		fmt.Fprintln(os.Stderr, "unexpected API operation", method, endpoint)
		return 99
	}
	if err := writeFixture(path, f); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 99
	}
	if response != nil {
		if include {
			if f.Fault == "reject" {
				fmt.Print("HTTP/2.0 422 Unprocessable Entity\r\n\r\n")
			} else {
				fmt.Print("HTTP/2.0 200 OK\r\n\r\n")
			}
		}
		if err := json.NewEncoder(os.Stdout).Encode(response); err != nil {
			return 99
		}
	}
	if code != 0 {
		fmt.Fprintln(os.Stderr, "injected transport failure")
	}
	return code
}

func fixtureAgent(path, provider string) int {
	l, err := lockedFixture(path)
	if err != nil {
		return 99
	}
	f, err := readFixture(path)
	if err != nil {
		l.Close()
		return 99
	}
	f.Agents = append(f.Agents, agentCall{Provider: provider, Args: append([]string(nil), os.Args[1:]...)})
	if err := writeFixture(path, f); err != nil {
		l.Close()
		return 99
	}
	l.Close()
	if err := os.WriteFile(path+".agent-started", []byte("started"), 0600); err != nil {
		return 99
	}
	if f.AgentMode == "crash" {
		return 42
	}
	if f.AgentMode == "wait" {
		for {
			if _, err := os.Stat(path + ".agent-release"); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	var input findings.Input
	if err := json.Unmarshal([]byte(os.Getenv("PRQUEUE_INPUT")), &input); err != nil {
		return 99
	}
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil || !bytes.Contains(prompt, []byte(input.Repo)) || !bytes.Contains(prompt, []byte(input.HeadSHA)) {
		fmt.Fprintln(os.Stderr, "invalid agent prompt")
		return 99
	}
	doc := findings.Document{SchemaVersion: 1, Repo: input.Repo, PR: input.PR, HeadSHA: input.HeadSHA, Summary: "SUMMARY stays private unless approved", Verdict: "request_changes", Findings: []findings.Finding{
		{Kind: "inline", Severity: "major", Category: "correctness", Title: "PRIVATE title A", Body: "Approved A\n", Rationale: findings.Ptr("PRIVATE rationale"), Anchor: findings.Anchor{Path: findings.Ptr("value.txt"), Side: findings.Ptr("RIGHT"), Line: findings.Ptr(1)}},
		{Kind: "general", Severity: "minor", Category: "test-coverage", Title: "PRIVATE title B", Body: "Rejected B"},
	}}
	data, err := json.Marshal(doc)
	if err != nil {
		return 99
	}
	output := os.Getenv("PRQUEUE_OUTPUT")
	if provider == "codex" {
		args := os.Args[1:]
		if len(args) < 2 || args[0] != "--ask-for-approval" || args[1] != "never" || !containsArgs(args, "exec", "--sandbox", "workspace-write", "sandbox_workspace_write.network_access=false", "sandbox_workspace_write.writable_roots=[]", "--ephemeral", "--json") || containsArgs(args, "--dangerously-skip-permissions") {
			fmt.Fprintln(os.Stderr, "unexpected Codex arguments")
			return 99
		}
		output = flagValue(args, "--output-last-message")
		if output == "" || output != os.Getenv("PRQUEUE_OUTPUT") || flagValue(args, "--output-schema") == "" || flagValue(args, "--add-dir") == "" || args[len(args)-1] != "-" {
			fmt.Fprintln(os.Stderr, "missing Codex output contract")
			return 99
		}
		fmt.Println(`{"type":"thread.started","thread_id":"fixture-thread"}`)
		fmt.Fprintln(os.Stderr, "fixture codex diagnostic")
	} else if !containsArgs(os.Args[1:], "--output-format", "stream-json", "--dangerously-skip-permissions") || containsArgs(os.Args[1:], "exec") {
		fmt.Fprintln(os.Stderr, "unexpected Claude arguments")
		return 99
	}
	switch f.AgentMode {
	case "missing-output":
		return 0
	case "malformed-output":
		data = []byte(`{"schema_version":`)
	}
	if err := os.WriteFile(output, data, 0600); err != nil {
		return 99
	}
	if f.AgentMode == "nonzero-with-output" {
		return 42
	}
	return 0
}

func flagValue(args []string, name string) string {
	for i := range len(args) - 1 {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func containsArgs(args []string, values ...string) bool {
	for _, value := range values {
		found := false
		for _, arg := range args {
			if arg == value {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func selectCodex(t *testing.T, a app) {
	t.Helper()
	cfg, err := config.Load(a.paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(a.paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	codexExecutable := filepath.Join(filepath.Dir(cfg.Agent.Executable), "fake-codex")
	updated := strings.Replace(string(data), "agent: {executable:", "agent: {provider: codex, executable:", 1)
	updated = strings.Replace(updated, cfg.Agent.Executable, codexExecutable, 1)
	if updated == string(data) {
		t.Fatalf("fixture config has unexpected agent form: %s", data)
	}
	if err := os.WriteFile(a.paths.Config, []byte(updated), 0600); err != nil {
		t.Fatal(err)
	}
}

func acceptanceApp(t *testing.T) (app, string, *bytes.Buffer) {
	t.Helper()
	a, _, out := runFixture(t)
	remote := a.remote.(runRemote)
	path := filepath.Join(t.TempDir(), "github.json")
	if err := writeFixture(path, cliFixture{PR: remote.pr, Patch: remote.diff.Files["value.txt"].Patch, Reviews: []submittedFixture{}, Calls: []apiCall{}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRQ_CLI_FIXTURE", path)
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	gh, err := exec.LookPath("gh")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(a.paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{gh, cfg.Agent.Executable, filepath.Join(filepath.Dir(cfg.Agent.Executable), "fake-codex")} {
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.Symlink(executable, target); err != nil {
			t.Fatal(err)
		}
	}
	a.remote = nil
	return a, path, out
}
func changeFixture(t *testing.T, path string, fn func(*cliFixture)) {
	t.Helper()
	l, err := lockedFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	f, err := readFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	fn(&f)
	if err := writeFixture(path, f); err != nil {
		t.Fatal(err)
	}
}
func cliCall(t *testing.T, a *app, out *bytes.Buffer, want int, args ...string) {
	t.Helper()
	out.Reset()
	if code := a.run(t.Context(), append(args, "--json")); code != want {
		t.Fatalf("%v: code=%d %s", args, code, out)
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not one JSON object: %s %v", out, err)
	}
}
func queueFindings(t *testing.T, a app) []store.Finding {
	t.Helper()
	s, err := store.OpenReadOnly(t.Context(), a.paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fs, err := s.ListFindings(t.Context(), "owner/repo", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}
func findingBody(t *testing.T, fs []store.Finding, body string) store.Finding {
	t.Helper()
	for _, f := range fs {
		if f.Body == body {
			return f
		}
	}
	t.Fatalf("finding body %q not found", body)
	return store.Finding{}
}

func runFilesSnapshot(t *testing.T, state string) string {
	t.Helper()
	var snapshot []string
	for _, root := range []string{filepath.Join(state, "runs"), queue.SummaryPath(state)} {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(state, path)
			if err != nil {
				return err
			}
			if info.IsDir() {
				snapshot = append(snapshot, rel+"/")
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			snapshot = append(snapshot, rel+":"+string(data))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(snapshot)
	return strings.Join(snapshot, "\n")
}

func TestAcceptanceExecutableApprovalSelectionAndLostResponseRecovery(t *testing.T) {
	a, path, out := acceptanceApp(t)
	cliCall(t, &a, out, 0, "run")
	fs := queueFindings(t, a)
	aID := findingBody(t, fs, "Approved A\n").ID
	bID := findingBody(t, fs, "Rejected B").ID
	cliCall(t, &a, out, 0, "approve", aID)
	cliCall(t, &a, out, 0, "reject", bID, "--reason", "not useful")
	changeFixture(t, path, func(f *cliFixture) { f.Fault = "lost response" })
	cliCall(t, &a, out, 1, "publish", "owner/repo#1", "--event", "COMMENT")
	cliCall(t, &a, out, 1, "publish", "owner/repo#1", "--event", "COMMENT")
	f, err := readFixture(path)
	if err != nil || f.Posts != 1 || len(f.Reviews) != 1 {
		t.Fatalf("POST count: %+v %v", f, err)
	}
	request := f.Reviews[0].Request
	if len(request.Comments) != 1 || request.Comments[0].Body != "Approved A\n" || strings.Contains(request.Body, "SUMMARY") || strings.Contains(request.Body, "Rejected B") {
		t.Fatalf("unapproved text sent: %+v", request)
	}
	editor := filepath.Join(t.TempDir(), "edit-later-body")
	if err := os.WriteFile(editor, []byte("#!/bin/sh\nprintf 'later A\\n' > \"$1\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", strconv.Quote(editor))
	cliCall(t, &a, out, 0, "edit", aID)
	changeFixture(t, path, func(f *cliFixture) { f.Fault = "incomplete reviews"; f.PR.Draft = true; f.PR.State = "closed" })
	cliCall(t, &a, out, 1, "publish", "owner/repo#1", "--resume", "--confirmed-not-sent")
	changeFixture(t, path, func(f *cliFixture) { f.Fault = "" })
	cliCall(t, &a, out, 0, "publish", "owner/repo#1", "--resume")
	cliCall(t, &a, out, 0, "publish", "owner/repo#1", "--resume")
	f, err = readFixture(path)
	if err != nil || f.Posts != 1 {
		t.Fatal("recovery reposted")
	}
	if findingBody(t, queueFindings(t, a), "later A\n").Status != "pending" {
		t.Fatal("recovery overwrote the later edit")
	}
	s, err := store.OpenReadOnly(t.Context(), a.paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var sentBody string
	if err := s.DB.QueryRow("SELECT body_snapshot FROM audit_log WHERE action='published'").Scan(&sentBody); err != nil || sentBody != "Approved A\n" {
		t.Fatalf("sent snapshot audit: %q %v", sentBody, err)
	}
}

func TestAcceptanceExecutableEmptyEventsAndExplicitClearance(t *testing.T) {
	a, path, out := acceptanceApp(t)
	cliCall(t, &a, out, 0, "publish", "owner/repo#1", "--event", "COMMENT")
	cliCall(t, &a, out, 1, "publish", "owner/repo#1", "--event", "REQUEST_CHANGES")
	changeFixture(t, path, func(f *cliFixture) { f.Fault = "lost not sent" })
	cliCall(t, &a, out, 1, "publish", "owner/repo#1", "--event", "APPROVE")
	cliCall(t, &a, out, 1, "publish", "owner/repo#1", "--resume")
	cliCall(t, &a, out, 1, "publish", "owner/repo#1", "--event", "COMMENT")
	cliCall(t, &a, out, 0, "publish", "owner/repo#1", "--resume", "--confirmed-not-sent")
	f, err := readFixture(path)
	if err != nil || f.Posts != 1 || len(f.Reviews) != 0 {
		t.Fatal("clearance sent a request")
	}
	changeFixture(t, path, func(f *cliFixture) { f.Fault = "" })
	cliCall(t, &a, out, 0, "publish", "owner/repo#1", "--event", "APPROVE")
	f, err = readFixture(path)
	if err != nil || f.Posts != 2 || len(f.Reviews) != 1 || len(f.Reviews[0].Request.Comments) != 0 || f.Reviews[0].Request.Event != "APPROVE" {
		t.Fatalf("empty approval: %+v %v", f, err)
	}
}

func TestAcceptanceExecutableCrashRetryAndComparisonTransitions(t *testing.T) {
	a, path, out := acceptanceApp(t)
	initial, err := readFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	changeFixture(t, path, func(f *cliFixture) { f.AgentMode = "crash" })
	cliCall(t, &a, out, 2, "run")
	if len(queueFindings(t, a)) != 0 {
		t.Fatal("crash activated findings")
	}
	changeFixture(t, path, func(f *cliFixture) { f.AgentMode = "" })
	cliCall(t, &a, out, 0, "run")
	aID := findingBody(t, queueFindings(t, a), "Approved A\n").ID
	cliCall(t, &a, out, 0, "approve", aID)
	changeFixture(t, path, func(f *cliFixture) { f.PR.BaseSHA = f.PR.HeadSHA })
	cliCall(t, &a, out, 1, "publish", "owner/repo#1", "--event", "COMMENT")
	if findingBody(t, queueFindings(t, a), "Approved A\n").Status != "pending" {
		t.Fatal("base change left approval")
	}
	cliCall(t, &a, out, 0, "run")
	for _, transition := range []string{"draft", "ready", "closed", "open"} {
		if transition == "draft" || transition == "closed" {
			cliCall(t, &a, out, 0, "approve", aID)
		}
		changeFixture(t, path, func(f *cliFixture) {
			switch transition {
			case "draft":
				f.PR.Draft = true
			case "ready":
				f.PR.Draft = false
			case "closed":
				f.PR.State = "closed"
			case "open":
				f.PR.State = "open"
			}
		})
		cliCall(t, &a, out, 0, "run")
		if findingBody(t, queueFindings(t, a), "Approved A\n").Status != "pending" {
			t.Fatalf("%s retained approval", transition)
		}
	}
	cliCall(t, &a, out, 0, "approve", aID)
	configBytes, err := os.ReadFile(a.paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	excluded := strings.Replace(string(configBytes), "repos: [{name: owner/repo}]", "repos: [{name: owner/repo, filters: {authors: [excluded]}}]", 1)
	if err := os.WriteFile(a.paths.Config, []byte(excluded), 0600); err != nil {
		t.Fatal(err)
	}
	changeFixture(t, path, func(f *cliFixture) { f.PR.HeadSHA = initial.PR.BaseSHA })
	cliCall(t, &a, out, 0, "run")
	if findingBody(t, queueFindings(t, a), "Approved A\n").Status != "pending" {
		t.Fatal("filter prevented head-change observation")
	}
	s, err := store.OpenReadOnly(t.Context(), a.paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.PR(t.Context(), "owner/repo", 1)
	s.Close()
	if err != nil || p.HeadSHA != initial.PR.BaseSHA || p.State != "open" || p.LastReviewedKey != nil {
		t.Fatalf("filtered head transition: %+v %v", p, err)
	}
	if err := os.WriteFile(a.paths.Config, configBytes, 0600); err != nil {
		t.Fatal(err)
	}
	cliCall(t, &a, out, 0, "run")
}

func TestAcceptanceExecutableProviderSwitchForceAndFailures(t *testing.T) {
	a, path, out := acceptanceApp(t)
	cliCall(t, &a, out, 0, "run")
	s, err := store.OpenReadOnly(t.Context(), a.paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	var initialRunID string
	err = s.DB.QueryRow("SELECT id FROM review_runs WHERE status='succeeded'").Scan(&initialRunID)
	s.Close()
	if err != nil || initialRunID == "" {
		t.Fatalf("initial persisted Claude run: %q %v", initialRunID, err)
	}
	initial, err := readFixture(path)
	if err != nil || len(initial.Agents) != 1 || initial.Agents[0].Provider != "claude" {
		t.Fatalf("initial Claude execution: %+v %v", initial.Agents, err)
	}

	selectCodex(t, a)
	cfg, err := config.Load(a.paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(cfg.Agent.Executable), "fake-agent")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(cfg.Agent.Executable)+string(os.PathListSeparator)+"/usr/bin:/bin")
	cliCall(t, &a, out, 0, "run")
	var ordinary struct {
		Data queue.RunResult `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &ordinary); err != nil || len(ordinary.Data.Repos) != 1 || len(ordinary.Data.Repos[0].Reviews) != 0 || len(ordinary.Data.Repos[0].Observations) != 1 || ordinary.Data.Repos[0].Observations[0].Eligible {
		t.Fatalf("unchanged provider switch was not an explicit no-review skip: %s %v", out, err)
	}
	skipped, err := readFixture(path)
	if err != nil || len(skipped.Agents) != 1 {
		t.Fatalf("provider switch should skip unchanged comparison: %+v %v", skipped.Agents, err)
	}

	cliCall(t, &a, out, 0, "run", "--repo", "owner/repo", "--pr", "1")
	var forced struct {
		Data queue.RunResult `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &forced); err != nil || len(forced.Data.Repos) != 1 || len(forced.Data.Repos[0].Reviews) != 1 {
		t.Fatalf("forced result did not contain one review: %s %v", out, err)
	}
	review := forced.Data.Repos[0].Reviews[0]
	if review.ID == "" || review.ID == initialRunID || review.Status != "succeeded" || review.OutputPath == "" {
		t.Fatalf("forced review was not a new persisted success: %+v", review)
	}
	f, err := readFixture(path)
	if err != nil || len(f.Agents) != 2 || f.Agents[1].Provider != "codex" {
		t.Fatalf("forced Codex invocation: %+v %v", f.Agents, err)
	}
	metadataPath := filepath.Join(a.paths.State, "runs", review.ID, "agent-metadata.json")
	metadata, err := os.ReadFile(metadataPath)
	var provenance struct {
		Provider string `json:"provider"`
		State    string `json:"state"`
	}
	decodeErr := json.Unmarshal(metadata, &provenance)
	if err != nil || decodeErr != nil || provenance.Provider != "codex" || provenance.State != "process_finished" {
		t.Fatalf("Codex provenance %s: %s %v", metadataPath, metadata, err)
	}
	runDir := filepath.Dir(metadataPath)
	events, eventsErr := os.ReadFile(filepath.Join(runDir, "agent.jsonl"))
	diagnostics, diagnosticsErr := os.ReadFile(filepath.Join(runDir, "agent-stderr.log"))
	final, finalErr := os.ReadFile(review.OutputPath)
	if eventsErr != nil || diagnosticsErr != nil || finalErr != nil || !bytes.Contains(events, []byte("thread.started")) || !bytes.Contains(diagnostics, []byte("fixture codex diagnostic")) || bytes.Contains(final, []byte("thread.started")) {
		t.Fatalf("Codex diagnostic/final channels were not separate: events=%q diagnostics=%q final=%q errors=%v/%v/%v", events, diagnostics, final, eventsErr, diagnosticsErr, finalErr)
	}
	s, err = store.OpenReadOnly(t.Context(), a.paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	var persistedStatus, persistedOutput, persistedComparison string
	err = s.DB.QueryRow("SELECT status,raw_output_path,comparison_key FROM review_runs WHERE id=?", review.ID).Scan(&persistedStatus, &persistedOutput, &persistedComparison)
	s.Close()
	if err != nil || persistedStatus != "succeeded" || persistedOutput != review.OutputPath || persistedComparison != initial.PR.Key() {
		t.Fatalf("persisted forced run: %q %q %q %v", persistedStatus, persistedOutput, persistedComparison, err)
	}

	for _, mode := range []string{"nonzero-with-output", "malformed-output", "missing-output"} {
		changeFixture(t, path, func(f *cliFixture) { f.AgentMode = mode })
		cliCall(t, &a, out, 2, "run", "--repo", "owner/repo", "--pr", "1")
	}
	f, err = readFixture(path)
	if err != nil || len(f.Agents) != 5 {
		t.Fatalf("forced retries did not each invoke Codex: %+v %v", f.Agents, err)
	}
	for _, call := range f.Agents[1:] {
		if call.Provider != "codex" {
			t.Fatalf("provider fallback occurred: %+v", f.Agents)
		}
	}
	changeFixture(t, path, func(f *cliFixture) {
		f.AgentMode = ""
		f.PR.State = "closed"
	})
	cliCall(t, &a, out, 0, "run", "--repo", "owner/repo", "--pr", "1")
	var closed struct {
		Data queue.RunResult `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &closed); err != nil || len(closed.Data.Repos) != 1 || len(closed.Data.Repos[0].Reviews) != 0 || len(closed.Data.Repos[0].Observations) != 1 || closed.Data.Repos[0].Observations[0].Eligible {
		t.Fatalf("closed forced PR could be mistaken for acceptance: %s %v", out, err)
	}
	f, err = readFixture(path)
	if err != nil || len(f.Agents) != 5 {
		t.Fatalf("ineligible forced PR invoked an agent: %+v %v", f.Agents, err)
	}
}

func TestAcceptanceMissingSelectedProviderDoesNotRequireOtherProvider(t *testing.T) {
	a, _, out := acceptanceApp(t)
	selectCodex(t, a)
	cfg, err := config.Load(a.paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg.Agent.Executable); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	code := a.run(t.Context(), []string{"run", "--repo", "owner/repo", "--pr", "1", "--json"})
	var response result
	decodeErr := json.Unmarshal(out.Bytes(), &response)
	if code != 1 || decodeErr != nil || !strings.Contains(response.Error, "required executable") || !strings.Contains(response.Error, cfg.Agent.Executable) || strings.Contains(response.Error, "fake-agent") {
		t.Fatalf("provider-aware dependency failure: code=%d %s", code, out)
	}
}

func TestAcceptanceExecutablePreparedAndSendingRecovery(t *testing.T) {
	for _, sending := range []bool{false, true} {
		t.Run(strconv.FormatBool(sending), func(t *testing.T) {
			a, path, out := acceptanceApp(t)
			cliCall(t, &a, out, 0, "run")
			aID := findingBody(t, queueFindings(t, a), "Approved A\n").ID
			cliCall(t, &a, out, 0, "approve", aID)
			s, err := store.Open(t.Context(), a.paths.Database)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err := s.DB.Exec(`CREATE TRIGGER stop_before_sending BEFORE UPDATE OF status ON publications WHEN NEW.status='sending' BEGIN SELECT RAISE(ABORT,'fault before sending'); END`); err != nil {
				t.Fatal(err)
			}
			cliCall(t, &a, out, 1, "publish", "owner/repo#1", "--event", "COMMENT")
			if _, err := s.DB.Exec("DROP TRIGGER stop_before_sending"); err != nil {
				t.Fatal(err)
			}
			if sending {
				if _, err := s.DB.Exec("UPDATE publications SET status='sending'"); err != nil {
					t.Fatal(err)
				}
				cliCall(t, &a, out, 1, "publish", "owner/repo#1", "--resume")
				f, err := readFixture(path)
				if err != nil || f.Posts != 0 {
					t.Fatal("sending recovery replayed the request")
				}
				cliCall(t, &a, out, 0, "publish", "owner/repo#1", "--resume", "--confirmed-not-sent")
			} else {
				cliCall(t, &a, out, 0, "publish", "owner/repo#1", "--resume")
				f, err := readFixture(path)
				if err != nil || f.Posts != 1 {
					t.Fatal("prepared recovery did not send once")
				}
			}
		})
	}
}

func TestAcceptanceExecutableAgentAndApprovalOverlap(t *testing.T) {
	a, path, out := acceptanceApp(t)
	cliCall(t, &a, out, 0, "run")
	aID := findingBody(t, queueFindings(t, a), "Approved A\n").ID
	changeFixture(t, path, func(f *cliFixture) { f.AgentMode = "wait" })
	if err := os.Remove(path + ".agent-started"); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	out.Reset()
	go func() { done <- a.run(t.Context(), []string{"run", "--repo", "owner/repo", "--pr", "1", "--json"}) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path + ".agent-started"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var triageOut, triageErr bytes.Buffer
	b := a
	b.out = &triageOut
	b.errOut = &triageErr
	cliCall(t, &b, &triageOut, 0, "approve", aID)
	if err := os.WriteFile(path+".agent-release", nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("forced run: %d %s", code, out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent run did not finish")
	}
	if findingBody(t, queueFindings(t, a), "Approved A\n").Status != "approved" {
		t.Fatal("ingestion restored a stale decision")
	}
}

func TestAcceptanceNotificationFailureDoesNotChangeExitAndDryRunKeepsSummary(t *testing.T) {
	a, path, out := acceptanceApp(t)
	sink := &cliNotification{err: fmt.Errorf("desktop unavailable")}
	a.notifications = sink
	cliCall(t, &a, out, 0, "run")
	selectCodex(t, a)
	beforeDatabase := databaseSnapshot(t, a.paths.Database)
	beforeFiles := runFilesSnapshot(t, a.paths.State)
	changeFixture(t, path, func(f *cliFixture) { f.PR.Draft = true })
	cliCall(t, &a, out, 0, "run", "--repo", "owner/repo", "--pr", "1", "--dry-run")
	var response struct {
		Data queue.RunResult `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil || len(response.Data.Repos) != 1 || len(response.Data.Repos[0].Reviews) != 1 || response.Data.Repos[0].Reviews[0].OutputPath != "" {
		t.Fatalf("dry-run output exposed a temporary path: %s %v", out, err)
	}
	if after := databaseSnapshot(t, a.paths.Database); after != beforeDatabase {
		t.Fatal("dry run changed one of the five persistent tables")
	}
	if after := runFilesSnapshot(t, a.paths.State); after != beforeFiles || len(sink.messages) != 1 {
		t.Fatal("dry run changed persisted run files/summary or notified")
	}
	fixture, err := readFixture(path)
	if err != nil || len(fixture.Agents) != 2 || fixture.Agents[1].Provider != "codex" {
		t.Fatalf("dry run did not exercise the selected executable: %+v %v", fixture.Agents, err)
	}
	s, err := store.OpenReadOnly(t.Context(), a.paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.PR(t.Context(), "owner/repo", 1)
	s.Close()
	if err != nil || p.Draft {
		t.Fatalf("dry-run observation escaped the clone: %+v %v", p, err)
	}
	cliCall(t, &a, out, 0, "status")
	if !strings.Contains(out.String(), "desktop unavailable") {
		t.Fatal("status omitted notification failure")
	}
}

func TestAcceptanceRepositoryFailureWithoutPRRowsIsVisibleAndQuietOnRepeat(t *testing.T) {
	a, path, out := acceptanceApp(t)
	sink := &cliNotification{}
	a.notifications = sink
	changeFixture(t, path, func(f *cliFixture) { f.Fault = "repository unavailable" })
	cliCall(t, &a, out, 2, "run")
	cliCall(t, &a, out, 2, "run")
	if len(sink.messages) != 1 {
		t.Fatalf("repeated repository failure notified %d times", len(sink.messages))
	}
	cliCall(t, &a, out, 0, "status")
	var response struct {
		Data statusResult `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil || len(response.Data.LastRuns) != 0 || len(response.Data.LastPasses) != 1 || len(response.Data.LastPasses[0].Problems) != 1 {
		t.Fatalf("missing repository failure: %s %v", out, err)
	}
	s, err := store.OpenReadOnly(t.Context(), a.paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var count int
	if err := s.DB.QueryRow("SELECT count(*) FROM pull_requests").Scan(&count); err != nil || count != 0 {
		t.Fatal("failed list invented a PR row")
	}
}

type cliNotification struct {
	messages []string
	err      error
}

func (n *cliNotification) Send(_ context.Context, message string) error {
	n.messages = append(n.messages, message)
	return n.err
}
