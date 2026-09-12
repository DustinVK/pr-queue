package runner

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRunnerClearsCallerGitContext(t *testing.T) {
	req, _ := localFixture(t)
	_, caller := localFixture(t)
	// Make the caller's HEAD and staged contents differ from the review source.
	for _, args := range [][]string{
		{"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "caller"},
		{"rm", "--cached", "value.txt"},
	} {
		out, err := exec.Command("git", append([]string{"-C", caller, "-c", "core.hooksPath=/dev/null"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("caller setup: %v %s", err, out)
		}
	}
	callerGit := filepath.Join(caller, ".git")
	before := map[string][]byte{}
	for _, name := range []string{"config", "index", "refs/heads/main"} {
		data, err := os.ReadFile(filepath.Join(callerGit, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = data
	}
	global := filepath.Join(t.TempDir(), "global.gitconfig")
	if err := os.WriteFile(global, []byte("[prqueue]\n\ttestglobal = preserved\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := testRunner(t, "git-context", 10*time.Second)
	for key, value := range map[string]string{
		"GIT_DIR": callerGit, "GIT_WORK_TREE": caller, "GIT_COMMON_DIR": callerGit,
		"GIT_INDEX_FILE":       filepath.Join(callerGit, "index"),
		"GIT_OBJECT_DIRECTORY": filepath.Join(callerGit, "objects"),
		"GIT_CONFIG":           filepath.Join(callerGit, "config"),
		"GIT_CONFIG_COUNT":     "1", "GIT_CONFIG_KEY_0": "core.bare", "GIT_CONFIG_VALUE_0": "true",
		"GIT_CONFIG_PARAMETERS": "'core.bare'='true'", "GIT_CONFIG_GLOBAL": global,
		"GIT_NAMESPACE": "caller", "GIT_CEILING_DIRECTORIES": r.StateDir,
	} {
		t.Setenv(key, value)
	}
	result, err := r.Review(t.Context(), req)
	if err != nil {
		diagnostics, _ := os.ReadFile(filepath.Join(filepath.Dir(result.OutputPath), "agent-stderr.log"))
		t.Fatalf("review inherited caller context: %v\n%s", err, diagnostics)
	}
	if result.Document.HeadSHA != req.Input.HeadSHA {
		t.Fatal("review returned the wrong comparison")
	}
	for name, want := range before {
		got, err := os.ReadFile(filepath.Join(callerGit, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("caller %s changed: %v", name, err)
		}
	}
}
