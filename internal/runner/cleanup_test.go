package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCleanupInterruptedBeforeAgentPIDSaved(t *testing.T) {
	r := Runner{StateDir: t.TempDir()}
	id := uuid.NewString()
	root := filepath.Join(r.StateDir, "worktrees", id)
	if err := os.MkdirAll(filepath.Join(root, "checkout"), 0700); err != nil {
		t.Fatal(err)
	}
	o, err := newOwner(id)
	if err != nil {
		t.Fatal(err)
	}
	o.Started = "previous coordinator with the same PID"
	o.Starting = true
	if err := saveOwner(root, o); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// Long arguments also exercise finding the UUID late in ps's command field.
	cmd := exec.CommandContext(ctx, os.Args[0], strings.Repeat("padding", 40), "--session-id", id)
	cmd.Env = append(os.Environ(), "PRQ_RUNNER_TEST_MODE=starting-child", "PRQUEUE_OUTPUT="+ready)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			killGroup(cmd.Process.Pid)
			cmd.Wait()
		}
	})
	for {
		if data, err := os.ReadFile(ready); err == nil && string(data) == "ready" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("child never reached readiness")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cleaned, err := r.Cleanup(ctx)
	if err != nil || len(cleaned) != 1 || cleaned[0] != id {
		t.Fatalf("starting-window cleanup: %v %v", cleaned, err)
	}
	err = cmd.Wait()
	waited = true
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("child was not killed by recovery: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("test timeout, rather than recovery, terminated the child")
	}
	for _, path := range []string{root, ownerPath(root)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("recovery artifact remains at %s: %v", path, err)
		}
	}
}

func TestCleanupContinuesAfterIndependentEntryFailures(t *testing.T) {
	r := Runner{StateDir: t.TempDir()}
	base := filepath.Join(r.StateDir, "worktrees")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	badIDs := []string{"00000000-0000-4000-8000-000000000000", "11111111-1111-4111-8111-111111111111"}
	for i, id := range badIDs {
		body := []string{"not JSON", `{}`}[i]
		if err := os.WriteFile(ownerPath(filepath.Join(base, id)), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	id := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	root := filepath.Join(base, id)
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	o, err := newOwner(id)
	if err != nil {
		t.Fatal(err)
	}
	o.Started = "previous owner with the same PID"
	if err := saveOwner(root, o); err != nil {
		t.Fatal(err)
	}
	cleaned, err := r.Cleanup(t.Context())
	if err == nil || len(cleaned) != 1 || cleaned[0] != id {
		t.Fatalf("independent cleanup: %v %v", cleaned, err)
	}
	for _, badID := range badIDs {
		if !strings.Contains(err.Error(), badID) {
			t.Fatalf("missing failure context for %s: %v", badID, err)
		}
		if _, err := os.Stat(ownerPath(filepath.Join(base, badID))); err != nil {
			t.Fatal("invalid ownership must remain available for diagnosis:", err)
		}
	}
	for _, path := range []string{root, ownerPath(root)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("valid stale entry remains at %s: %v", path, err)
		}
	}
}
