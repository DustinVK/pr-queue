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

	"github.com/DustinVK/pr-queue/internal/config"
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
	r := Runner{StateDir: t.TempDir(), Agent: config.Agent{Provider: config.ProviderCodex}}
	id := uuid.NewString()
	root := filepath.Join(r.StateDir, "worktrees", id)
	if err := os.MkdirAll(filepath.Join(root, "checkout"), 0700); err != nil {
		t.Fatal(err)
	}
	o, err := newOwner(id)
	if err != nil {
		t.Fatal(err)
	}
	o.Version = 0 // Exercise an unversioned owner written by the legacy Claude runner.
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

func TestCleanupLegacySavedAgentWhileCodexConfigured(t *testing.T) {
	r := Runner{StateDir: t.TempDir(), Agent: config.Agent{Provider: config.ProviderCodex}}
	id := uuid.NewString()
	root := filepath.Join(r.StateDir, "worktrees", id)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "exec sleep 60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = killGroup(cmd.Process.Pid)
			_ = cmd.Wait()
		}
	})
	stamp, err := processStart(t.Context(), cmd.Process.Pid)
	if err != nil || stamp == "" {
		t.Fatalf("identify fixture: %q %v", stamp, err)
	}
	o, err := newOwner(id)
	if err != nil {
		t.Fatal(err)
	}
	o.Version = 0
	o.Started = "previous coordinator"
	o.AgentPID = cmd.Process.Pid
	o.AgentStart = stamp
	if err := saveOwner(root, o); err != nil {
		t.Fatal(err)
	}
	cleaned, err := r.Cleanup(t.Context())
	if err != nil || len(cleaned) != 1 || cleaned[0] != id {
		t.Fatalf("legacy saved PID cleanup: %v %v", cleaned, err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("legacy owned agent was not terminated")
	}
	waited = true
}

func TestCleanupRejectsMalformedVersion2OwnershipBeforeLiveOwnerCheck(t *testing.T) {
	for _, mutate := range []func(*owner){
		func(o *owner) { o.Version = 3 },
		func(o *owner) { o.AgentPID = -1 },
		func(o *owner) { o.AgentPID = 1 },
		func(o *owner) { o.AgentStart = "unexpected" },
		func(o *owner) { o.Released = true },
		func(o *owner) { o.Starting = true },
	} {
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
		mutate(&o)
		if err := saveOwner(root, o); err != nil {
			t.Fatal(err)
		}
		cleaned, err := r.Cleanup(t.Context())
		if err == nil || len(cleaned) != 0 {
			t.Fatalf("malformed owner accepted: %+v, cleaned=%v err=%v", o, cleaned, err)
		}
		if _, err := os.Stat(ownerPath(root)); err != nil {
			t.Fatal("malformed ownership evidence removed:", err)
		}
	}
}

func TestCleanupRetriesPartiallyRemovedTemporaryArtifacts(t *testing.T) {
	recovery := t.TempDir()
	dryRunRoot := filepath.Join(recovery, "dry-runs")
	if err := os.MkdirAll(dryRunRoot, 0700); err != nil {
		t.Fatal(err)
	}
	artifacts, err := os.MkdirTemp(dryRunRoot, "run-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(artifacts) })
	id := uuid.NewString()
	diagnostics := filepath.Join(artifacts, "runs", id)
	if err := os.MkdirAll(filepath.Join(diagnostics, "scratch"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"agent-metadata.json": "metadata",
		"comparison.diff":     "diff",
	} {
		if err := os.WriteFile(filepath.Join(diagnostics, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate an interrupted RemoveAll: some entries are gone, diagnostics
	// remain, and only the persistent version-3 owner survives outside the tree.
	if err := os.Remove(filepath.Join(diagnostics, "comparison.diff")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(diagnostics, "scratch")); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(recovery, "worktrees", id)
	o, err := newOwner(id)
	if err != nil {
		t.Fatal(err)
	}
	o.Version = 3
	o.Started = "previous coordinator"
	o.ArtifactDir = artifacts
	if err := saveOwner(root, o); err != nil {
		t.Fatal(err)
	}
	cleaned, err := (Runner{StateDir: recovery}).Cleanup(t.Context())
	if err != nil || len(cleaned) != 1 || cleaned[0] != id {
		t.Fatalf("retry partial artifact cleanup: %v %v", cleaned, err)
	}
	for _, path := range []string{ownerPath(root), artifacts} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("retried artifact remains at %s: %v", path, err)
		}
	}
	if cleaned, err := (Runner{StateDir: recovery}).Cleanup(t.Context()); err != nil || len(cleaned) != 0 {
		t.Fatalf("completed retry was not idempotent: %v %v", cleaned, err)
	}
}

func TestCleanupRejectsSymlinkedTemporaryArtifactRoot(t *testing.T) {
	recovery := t.TempDir()
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(recovery, "dry-runs")); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	artifacts := filepath.Join(recovery, "dry-runs", "run-owned")
	if err := os.Mkdir(artifacts, 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(recovery, "worktrees", id)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	o, err := newOwner(id)
	if err != nil {
		t.Fatal(err)
	}
	o.Version = 3
	o.Started = "previous coordinator"
	o.ArtifactDir = artifacts
	if err := saveOwner(root, o); err != nil {
		t.Fatal(err)
	}
	cleaned, err := (Runner{StateDir: recovery}).Cleanup(t.Context())
	if err == nil || len(cleaned) != 0 {
		t.Fatalf("symlinked artifact root accepted: %v %v", cleaned, err)
	}
	for _, path := range []string{root, ownerPath(root), filepath.Join(external, "run-owned")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("failed validation removed %s: %v", path, err)
		}
	}
}

func TestCleanupFinalizesSharedTemporaryArtifactsAfterAllOwners(t *testing.T) {
	recovery := t.TempDir()
	dryRunRoot := filepath.Join(recovery, "dry-runs")
	if err := os.MkdirAll(dryRunRoot, 0700); err != nil {
		t.Fatal(err)
	}
	artifacts, err := os.MkdirTemp(dryRunRoot, "run-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(artifacts) })
	var ids []string
	for range 2 {
		id := uuid.NewString()
		ids = append(ids, id)
		root := filepath.Join(recovery, "worktrees", id)
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		o, err := newOwner(id)
		if err != nil {
			t.Fatal(err)
		}
		o.Version = 3
		o.Started = "previous coordinator"
		o.ArtifactDir = artifacts
		if err := saveOwner(root, o); err != nil {
			t.Fatal(err)
		}
	}
	badID := uuid.NewString()
	badOwner := ownerPath(filepath.Join(recovery, "worktrees", badID))
	if err := os.WriteFile(badOwner, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	cleaned, err := (Runner{StateDir: recovery}).Cleanup(t.Context())
	if err == nil || !strings.Contains(err.Error(), badID) || len(cleaned) != len(ids) {
		t.Fatalf("shared artifact cleanup: %v %v", cleaned, err)
	}
	if _, err := os.Stat(artifacts); !os.IsNotExist(err) {
		t.Fatalf("shared artifact root remains: %v", err)
	}
	for _, id := range ids {
		root := filepath.Join(recovery, "worktrees", id)
		for _, path := range []string{root, ownerPath(root)} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("shared recovery artifact remains at %s: %v", path, err)
			}
		}
	}
	if _, err := os.Stat(badOwner); err != nil {
		t.Fatal("unrelated invalid owner was not retained:", err)
	}
}

func TestReleaseArtifactOwnershipHandlesSharedRemovedRoot(t *testing.T) {
	recovery := t.TempDir()
	dryRunRoot := filepath.Join(recovery, "dry-runs")
	if err := os.MkdirAll(dryRunRoot, 0700); err != nil {
		t.Fatal(err)
	}
	artifacts, err := os.MkdirTemp(dryRunRoot, "run-")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for range 2 {
		id := uuid.NewString()
		ids = append(ids, id)
		o, err := newOwner(id)
		if err != nil {
			t.Fatal(err)
		}
		o.Version = 3
		o.ArtifactDir = artifacts
		root := filepath.Join(recovery, "worktrees", id)
		if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
			t.Fatal(err)
		}
		if err := saveOwner(root, o); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(artifacts); err != nil {
		t.Fatal(err)
	}
	r := Runner{StateDir: artifacts, RecoveryDir: recovery}
	for _, id := range ids {
		if err := r.ReleaseArtifactOwnership(id); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(ownerPath(filepath.Join(recovery, "worktrees", id))); !os.IsNotExist(err) {
			t.Fatalf("completed owner remains for %s: %v", id, err)
		}
	}
}

func TestReleaseArtifactOwnershipRejectsMalformedOwner(t *testing.T) {
	recovery := t.TempDir()
	dryRunRoot := filepath.Join(recovery, "dry-runs")
	if err := os.MkdirAll(dryRunRoot, 0700); err != nil {
		t.Fatal(err)
	}
	artifacts, err := os.MkdirTemp(dryRunRoot, "run-")
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	root := filepath.Join(recovery, "worktrees", id)
	o, err := newOwner(id)
	if err != nil {
		t.Fatal(err)
	}
	o.Version = 3
	o.Started = ""
	o.ArtifactDir = artifacts
	if err := saveOwner(root, o); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(artifacts); err != nil {
		t.Fatal(err)
	}
	r := Runner{StateDir: artifacts, RecoveryDir: recovery}
	if err := r.ReleaseArtifactOwnership(id); err == nil {
		t.Fatal("malformed completed owner was accepted")
	}
	if _, err := os.Stat(ownerPath(root)); err != nil {
		t.Fatal("malformed completed owner was removed:", err)
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
