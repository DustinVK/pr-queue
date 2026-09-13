package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/DustinVK/pr-queue/internal/github"
	"github.com/DustinVK/pr-queue/internal/queue"
	"github.com/DustinVK/pr-queue/internal/store"
	"github.com/google/uuid"
)

type runRemote struct {
	pr   github.PR
	diff findings.Diff
}

func (r runRemote) Identity(context.Context, string) (string, error) { return "alice", nil }
func (r runRemote) ListOpen(context.Context, string) ([]github.PR, error) {
	return []github.PR{r.pr}, nil
}
func (r runRemote) FetchPR(context.Context, string, int) (github.PR, error)     { return r.pr, nil }
func (r runRemote) FetchDiff(context.Context, github.PR) (findings.Diff, error) { return r.diff, nil }

func runFixture(t *testing.T) (app, string, *bytes.Buffer) {
	t.Helper()
	paths := config.ForHome(t.TempDir())
	if code, r, _ := invoke(t, paths, "", "init", "--accept-agent-risk"); code != 0 {
		t.Fatalf("init: %+v", r)
	}
	source := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", source, "-c", "core.hooksPath=/dev/null", "-c", "user.name=Test", "-c", "user.email=test@example.invalid"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	for _, value := range []string{"old\n", "new\n"} {
		if err := os.WriteFile(filepath.Join(source, "value.txt"), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		git("add", "value.txt")
		git("commit", "-m", "fixture")
	}
	head, base := git("rev-parse", "HEAD"), git("rev-parse", "HEAD^")
	diff, err := findings.ParseDiff(git("diff", base, head))
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	agent := filepath.Join(bin, "fake-agent")
	// The executable accepts the real runner arguments and writes only the contract.
	script := `#!/bin/sh
cat >/dev/null
printf '%s' "$PRQUEUE_OUTPUT" > "$PRQ_TEST_MARKER"
if [ "$PRQ_TEST_FAIL" = 1 ]; then exit 42; fi
input=$PRQUEUE_INPUT
input=${input#\{}
input=${input%\}}
printf '{"schema_version":1,%s,"summary":"ok","verdict":"comment","findings":[]}\n' "$input" > "$PRQUEUE_OUTPUT"
`
	if err := os.WriteFile(agent, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	marker := filepath.Join(t.TempDir(), "agent-output-path")
	t.Setenv("PRQ_TEST_MARKER", marker)
	t.Setenv("PRQ_TEST_FAIL", "")
	quoted, _ := json.Marshal(agent)
	cfg := fmt.Sprintf("github: {user: alice}\nagent: {executable: %s, timeout: 10s}\nrepos: [{name: owner/repo}]\n", quoted)
	if err := os.WriteFile(paths.Config, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	var out, diagnostics bytes.Buffer
	remote := runRemote{pr: github.PR{Repo: "owner/repo", Number: 1, Comparison: findings.Comparison{HeadSHA: head, BaseSHA: base, State: "open"}}, diff: diff}
	return app{paths: paths, in: strings.NewReader(""), out: &out, errOut: &diagnostics, remote: remote, source: func(string) string { return source }}, marker, &out
}

func databaseSnapshot(t *testing.T, path string) string {
	t.Helper()
	s, err := store.OpenReadOnly(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	all := map[string][][]any{}
	for _, table := range []string{"pull_requests", "review_runs", "findings", "publications", "audit_log"} {
		rows, err := s.DB.Query("SELECT * FROM " + table + " ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(cols))
			pointers := make([]any, len(cols))
			for i := range pointers {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			all[table] = append(all[table], values)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	data, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRunDryRunIsolatesAllPersistentState(t *testing.T) {
	a, marker, out := runFixture(t)
	if code := a.run(t.Context(), []string{"run", "--json"}); code != 0 {
		t.Fatalf("initial run: %d %s", code, out)
	}
	before := databaseSnapshot(t, a.paths.Database)
	remote := a.remote.(runRemote)
	remote.pr.Draft = true // A forced dry run observes a transition and replaces findings in memory.
	a.remote = remote
	out.Reset()
	if code := a.run(t.Context(), []string{"run", "--repo", "owner/repo", "--pr", "1", "--dry-run", "--json"}); code != 0 {
		t.Fatalf("dry run: %d %s", code, out)
	}
	if after := databaseSnapshot(t, a.paths.Database); after != before {
		t.Fatalf("dry run changed database:\n%s\n%s", before, after)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	dryRunRoot := filepath.Join(a.paths.State, "dry-runs")
	rel, err := filepath.Rel(dryRunRoot, string(data))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		t.Fatalf("agent output escaped recovery-owned temporary root: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Dir(string(data))); !os.IsNotExist(err) {
		t.Fatal("temporary output remained")
	}
	if entries, err := os.ReadDir(filepath.Join(a.paths.State, "worktrees")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("dry-run recovery artifacts remained: %v", entries)
	}
	if entries, err := os.ReadDir(dryRunRoot); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("dry-run diagnostic roots remained: %v", entries)
	}
	var result struct {
		OK   bool `json:"ok"`
		Data struct {
			DryRun  bool `json:"dry_run"`
			Changed int  `json:"changed"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.OK || !result.Data.DryRun {
		t.Fatalf("dry output: %s %v", out, err)
	}
	if strings.Contains(out.String(), "raw_output_path") {
		t.Fatal("dry run advertised removed output")
	}
}

func TestRunFailureReturnsPartialExitAndRetries(t *testing.T) {
	a, _, out := runFixture(t)
	t.Setenv("PRQ_TEST_FAIL", "1")
	if code := a.run(t.Context(), []string{"run", "--json"}); code != 2 {
		t.Fatalf("failed run: %d %s", code, out)
	}
	t.Setenv("PRQ_TEST_FAIL", "")
	out.Reset()
	if code := a.run(t.Context(), []string{"run", "--json"}); code != 0 {
		t.Fatalf("retry: %d %s", code, out)
	}
}

func TestStatusRecoversInterruptedRun(t *testing.T) {
	a, _, out := runFixture(t)
	s, err := store.Open(t.Context(), a.paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, _, err := s.Observe(t.Context(), "owner/repo", 1, a.remote.(runRemote).pr.Comparison, "test")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.StartRun(t.Context(), uuid.NewString(), p, "output")
	if err != nil {
		t.Fatal(err)
	}
	if code := a.run(t.Context(), []string{"status", "--json"}); code != 0 {
		t.Fatalf("status: %d %s", code, out)
	}
	var status string
	if err := s.DB.QueryRow("SELECT status FROM review_runs WHERE id=?", r.ID).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("interrupted run: %s %v", status, err)
	}
}

func TestRunPreflightFailureStillRecoversInterruptedRun(t *testing.T) {
	a, _, out := runFixture(t)
	s, err := store.Open(t.Context(), a.paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, _, err := s.Observe(t.Context(), "owner/repo", 1, a.remote.(runRemote).pr.Comparison, "test")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.StartRun(t.Context(), uuid.NewString(), p, "interrupted-output")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.paths.Config, []byte("not: [valid"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := a.run(t.Context(), []string{"run", "--json"}); code != 1 {
		t.Fatalf("invalid config did not fail run: code=%d stdout=%s", code, out.String())
	}
	var status string
	if err := s.DB.QueryRow("SELECT status FROM review_runs WHERE id=?", r.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("preflight failure left interrupted run %q", status)
	}
}

func TestRunDryRunReportsArtifactCleanupFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		agentFail bool
	}{
		{name: "cleanup only"},
		{name: "partial and cleanup", agentFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, marker, out := runFixture(t)
			cfg, err := config.Load(a.paths.Config)
			if err != nil {
				t.Fatal(err)
			}
			inner := cfg.Agent.Executable + ".inner"
			if err := os.Rename(cfg.Agent.Executable, inner); err != nil {
				t.Fatal(err)
			}
			fail := ""
			if tc.agentFail {
				fail = "PRQ_TEST_FAIL=1 "
			}
			wrapper := fmt.Sprintf(`#!/bin/sh
%s%q "$@"
status=$?
root=$(dirname "$(dirname "$(dirname "$PRQUEUE_OUTPUT")")")
mkdir "$root/protected"
touch "$root/protected/file"
chmod 000 "$root/protected"
exit "$status"
	`, fail, inner)
			if err := os.WriteFile(cfg.Agent.Executable, []byte(wrapper), 0700); err != nil {
				t.Fatal(err)
			}
			code := a.run(t.Context(), []string{"run", "--repo", "owner/repo", "--pr", "1", "--dry-run", "--json"})
			data, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			workDir := filepath.Dir(filepath.Dir(filepath.Dir(string(data))))
			protected := filepath.Join(workDir, "protected")
			defer os.RemoveAll(workDir)
			defer os.Chmod(protected, 0700)
			if code != 1 {
				t.Fatalf("artifact cleanup failure was ignored: code=%d stdout=%s", code, out.String())
			}
			if tc.agentFail && (!strings.Contains(out.String(), "agent failed") || !strings.Contains(out.String(), "remove dry-run artifacts")) {
				t.Fatalf("combined failure lost an error: %s", out.String())
			}
		})
	}
}

func TestRunSummaryPersistenceFailureIsFatal(t *testing.T) {
	a, _, out := runFixture(t)
	if err := os.WriteFile(queue.SummaryPath(a.paths.State), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := a.run(t.Context(), []string{"run", "--json"}); code != 1 {
		t.Fatalf("invalid run summary was not fatal: code=%d stdout=%s", code, out.String())
	}
}
