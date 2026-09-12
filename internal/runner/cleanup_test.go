package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCleanupLeavesUnidentifiedAgentGroupsAlone(t *testing.T) {
	for _, leaderGone := range []bool{false, true} {
		name := "reused PID with live leader"
		if leaderGone {
			name = "absent leader with surviving member"
		}
		t.Run(name, func(t *testing.T) {
			r := Runner{StateDir: t.TempDir()}
			childFile := filepath.Join(t.TempDir(), "child.pid")
			cmd := exec.Command("/bin/sh", "-c", `sleep 30 >/dev/null 2>&1 & echo "$!" > "$1"; wait`, "fixture", childFile)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			var child int
			var childStart string
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					// The unreaped fixture leader still reserves this group ID.
					killGroup(cmd.Process.Pid)
					cmd.Wait()
				} else if stamp, _ := processStart(context.Background(), child); stamp != "" && stamp == childStart {
					syscall.Kill(child, syscall.SIGKILL)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			for child == 0 {
				data, err := os.ReadFile(childFile)
				if err == nil {
					child, _ = strconv.Atoi(strings.TrimSpace(string(data)))
				}
				if child > 0 {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("fixture child never started")
				case <-time.After(10 * time.Millisecond):
				}
			}
			childStart, err := processStart(ctx, child)
			if err != nil || childStart == "" {
				t.Fatalf("fixture child not alive: %v", err)
			}
			group, err := syscall.Getpgid(child)
			if err != nil || group != cmd.Process.Pid {
				t.Fatalf("fixture group: %d %v", group, err)
			}
			if leaderGone {
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				cmd.Wait()
				if stamp, err := processStart(ctx, group); err != nil || stamp != "" {
					t.Fatalf("fixture leader not gone: %q %v", stamp, err)
				}
			}
			id := uuid.NewString()
			root := filepath.Join(r.StateDir, "worktrees", id)
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			o, err := newOwner(id)
			if err != nil {
				t.Fatal(err)
			}
			o.Started = "previous coordinator"
			o.AgentPID = group
			o.AgentStart = "previous agent, not this fixture"
			if err := saveOwner(root, o); err != nil {
				t.Fatal(err)
			}
			cleaned, err := r.Cleanup(ctx)
			if err != nil || len(cleaned) != 1 || cleaned[0] != id {
				t.Fatalf("cleanup: %v %v", cleaned, err)
			}
			// Allow a wrongly delivered SIGKILL to become observable.
			time.Sleep(50 * time.Millisecond)
			if stamp, err := processStart(ctx, child); err != nil || stamp != childStart {
				t.Fatalf("cleanup killed an unidentified member: %q %v", stamp, err)
			}
			for _, path := range []string{root, ownerPath(root)} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("owned artifact remains: %s %v", path, err)
				}
			}
		})
	}
}

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
