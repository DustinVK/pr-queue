package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/lock"
	"github.com/DustinVK/pr-queue/internal/store"
)

func invoke(t *testing.T, paths config.Paths, input string, args ...string) (int, result, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	a := app{paths: paths, in: strings.NewReader(input), out: &stdout, errOut: &stderr}
	code := a.run(t.Context(), append(args, "--json"))
	decoder := json.NewDecoder(&stdout)
	var r result
	if err := decoder.Decode(&r); err != nil {
		t.Fatalf("not JSON: %q (%v)", stdout.String(), err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("multiple stdout values: %v", err)
	}
	return code, r, stderr.String()
}

func TestInitConsentAndIdempotence(t *testing.T) {
	paths := config.ForHome(t.TempDir())
	code, r, diagnostics := invoke(t, paths, "yes\n", "init")
	if code != 0 || !r.OK {
		t.Fatalf("%d %+v", code, r)
	}
	if !strings.Contains(diagnostics, "full user permissions") || !strings.Contains(diagnostics, "workspace-write sandbox") || !strings.Contains(diagnostics, "approval guarantee") {
		t.Fatal("missing trust disclosure")
	}
	custom := "# custom comment\ngithub: {user: alice}\nrepos: [{name: owner/repo}]\n"
	if err := os.WriteFile(paths.Config, []byte(custom), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(t.Context(), paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO pull_requests(repo,number,head_sha,base_sha,draft,state,updated_at) VALUES('owner/repo',1,'h','b',0,'open','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO review_runs(id,pr_id,head_sha,comparison_key,status,started_at)
		SELECT 'interrupted',id,head_sha,'key','running',? FROM pull_requests WHERE repo='owner/repo' AND number=1`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	code, r, diagnostics = invoke(t, paths, "", "init")
	if code != 0 || !r.OK || diagnostics != "" {
		t.Fatalf("repeat init: %d %+v %q", code, r, diagnostics)
	}
	data, err := os.ReadFile(paths.Config)
	if err != nil || string(data) != custom {
		t.Fatalf("config replaced: %q %v", data, err)
	}
	s, err = store.Open(t.Context(), paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.DB.QueryRow("SELECT count(*) FROM pull_requests").Scan(&n); err != nil || n != 1 {
		t.Fatalf("data lost: %d %v", n, err)
	}
	var status string
	if err := s.DB.QueryRow("SELECT status FROM review_runs WHERE id='interrupted'").Scan(&status); err != nil || status != "failed" {
		t.Fatalf("init did not recover interrupted run: %q %v", status, err)
	}
	for path, mode := range map[string]os.FileMode{paths.Config: 0600, paths.Database: 0600, paths.Consent: 0600, paths.State: 0700, filepath.Dir(paths.Config): 0700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("mode %s: %v %v", path, info, err)
		}
	}
	code, r, _ = invoke(t, paths, "", "status")
	if code != 0 || !r.OK {
		t.Fatalf("status: %d %+v", code, r)
	}
	for _, args := range [][]string{{"list"}, {"list", "--status", "pending"}, {"show", "owner/repo#1"}} {
		code, r, _ := invoke(t, paths, "", args...)
		if code != 0 || !r.OK {
			t.Fatalf("read %v: %d %+v", args, code, r)
		}
	}
}

func TestInitCompletesSchemaZeroDatabase(t *testing.T) {
	paths := config.ForHome(t.TempDir())
	if err := os.MkdirAll(paths.State, 0700); err != nil {
		t.Fatal(err)
	}
	id := "00000000-0000-4000-8000-000000000001"
	root := filepath.Join(paths.State, "worktrees", id)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	owner := root + ".owner.json"
	body := []byte(`{"version":2,"id":"00000000-0000-4000-8000-000000000001","pid":999999,"started":"previous coordinator"}`)
	if err := os.WriteFile(owner, body, 0600); err != nil {
		t.Fatal(err)
	}
	badOwner := filepath.Join(paths.State, "worktrees", "00000000-0000-4000-8000-000000000002.owner.json")
	if err := os.WriteFile(badOwner, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Database, nil, 0600); err != nil {
		t.Fatal(err)
	}
	code, r, diagnostics := invoke(t, paths, "", "init", "--accept-agent-risk")
	if code != 0 || !r.OK {
		t.Fatalf("schema-0 init: %d %+v", code, r)
	}
	if !strings.Contains(diagnostics, "Startup recovery warning: clean orphaned worktrees") {
		t.Fatalf("cleanup warning missing: %q", diagnostics)
	}
	s, err := store.OpenReadOnly(t.Context(), paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.DB.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != store.SchemaVersion {
		t.Fatalf("schema version %d: %v", version, err)
	}
	for _, path := range []string{root, owner} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("init left orphan at %s: %v", path, err)
		}
	}
	if _, err := os.Stat(badOwner); err != nil {
		t.Fatal("invalid ownership evidence was not retained:", err)
	}
}

func TestConsentMustBeExplicit(t *testing.T) {
	for _, input := range []string{"", "no\n", "y\n"} {
		paths := config.ForHome(t.TempDir())
		code, r, _ := invoke(t, paths, input, "init")
		if code != 1 || r.OK {
			t.Fatalf("consent %q: %d %+v", input, code, r)
		}
		for _, path := range []string{paths.Config, paths.Database, paths.Consent} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("decline created %s", path)
			}
		}
	}
	paths := config.ForHome(t.TempDir())
	if code, r, _ := invoke(t, paths, "", "init", "--accept-agent-risk"); code != 0 {
		t.Fatalf("explicit flag: %d %+v", code, r)
	}
}

func TestJSONErrorsAndLockExitCode(t *testing.T) {
	paths := config.ForHome(t.TempDir())
	for _, args := range [][]string{{"unknown"}, {"run"}, {"status"}, {"init", "extra"}, {"init", "--wat"}} {
		code, r, _ := invoke(t, paths, "", args...)
		if code != 1 || r.OK || r.Error == "" {
			t.Fatalf("%v: %d %+v", args, code, r)
		}
	}
	l, err := lock.Acquire(lock.RunPath(paths.State), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	code, r, _ := invoke(t, paths, "", "init", "--accept-agent-risk")
	if code != 3 || !strings.Contains(r.Error, "lock held") {
		t.Fatalf("busy: %d %+v", code, r)
	}
}

func TestBadConfigDoesNotRecordConsent(t *testing.T) {
	paths := config.ForHome(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(paths.Config), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Config, []byte("github: {user: alice}\nunknown: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, _, _ := invoke(t, paths, "", "init", "--accept-agent-risk")
	if code != 1 {
		t.Fatalf("bad config exit %d", code)
	}
	if _, err := os.Stat(paths.Consent); !os.IsNotExist(err) {
		t.Fatal("recorded consent after failed init")
	}
}

type promptReader struct {
	io.Reader
	started chan struct{}
}

func (r *promptReader) Read(p []byte) (int, error) {
	select {
	case <-r.started:
	default:
		close(r.started)
	}
	return r.Reader.Read(p)
}

func TestInterruptedConsentPromptReleasesLock(t *testing.T) {
	paths := config.ForHome(t.TempDir())
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	reader := &promptReader{Reader: r, started: make(chan struct{})}
	a := app{paths: paths, in: reader, out: io.Discard, errOut: io.Discard}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.init(ctx, nil); done <- err }()
	select {
	case <-reader.started:
	case <-time.After(3 * time.Second):
		t.Fatal("prompt did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("prompt ignored cancellation")
	}
	h, err := lock.Inspect(lock.RunPath(paths.State))
	if err != nil || h != nil {
		t.Fatalf("lock not released: %v %v", h, err)
	}
}
