package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

const lifecycleHelperEnv = "PRQ_PROCESS_LIFECYCLE_ROLE"

func TestProcessLifecycleHelper(t *testing.T) {
	switch os.Getenv(lifecycleHelperEnv) {
	case "agent":
		if _, err := os.NewFile(3, "gate").Stat(); err == nil {
			panic("gate descriptor remained open after exec")
		}
		mustWriteLifecycleFile(os.Getenv("PRQ_LIFECYCLE_FD_CLOSED"), "closed")
		mustWriteLifecycleFile(os.Getenv("PRQ_LIFECYCLE_AGENT_STARTED"), "started")
		time.Sleep(time.Minute)
	case "coordinator":
		runLifecycleCoordinatorHelper()
	}
}

func runLifecycleCoordinatorHelper() {
	root := os.Getenv("PRQ_LIFECYCLE_ROOT")
	o, err := newOwner(filepath.Base(root))
	if err != nil {
		panic(err)
	}
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		panic(err)
	}
	if err := reserveOwner(root, o); err != nil {
		panic(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		panic(err)
	}
	p, err := lifecycleProcess(context.Background(), os.Getenv("PRQ_LIFECYCLE_AGENT_STARTED"), os.Getenv("PRQ_LIFECYCLE_FD_CLOSED"))
	if err != nil {
		panic(err)
	}
	phase := os.Getenv("PRQ_LIFECYCLE_PHASE")
	block := func() error {
		mustWriteLifecycleFile(os.Getenv("PRQ_LIFECYCLE_GATE_PID"), strconv.Itoa(p.cmd.Process.Pid))
		if phase == "before_identity" {
			helper := exec.Command("/bin/sh", "-c", "sleep 60")
			if err := helper.Start(); err != nil {
				panic(err)
			}
			mustWriteLifecycleFile(os.Getenv("PRQ_LIFECYCLE_UNRELATED_PID"), strconv.Itoa(helper.Process.Pid))
		}
		if phase == "after_release" {
			waitLifecycleFile(os.Getenv("PRQ_LIFECYCLE_AGENT_STARTED"), 5*time.Second)
		}
		mustWriteLifecycleFile(os.Getenv("PRQ_LIFECYCLE_READY"), "ready")
		select {}
	}
	faults := &launchFaults{}
	switch phase {
	case "before_identity":
		faults.BeforeIdentitySave = block
	case "before_release":
		faults.BeforeRelease = block
	case "after_release":
		faults.AfterRelease = block
	default:
		panic("unknown lifecycle phase")
	}
	if err := p.start(root, &o, nil, faults); err != nil {
		panic(err)
	}
	panic("lifecycle coordinator unexpectedly returned")
}

func lifecycleProcess(ctx context.Context, started, fdClosed string) (*gatedProcess, error) {
	p, err := newGatedProcess(ctx, os.Args[0], []string{"-test.run=^TestProcessLifecycleHelper$"})
	if err != nil {
		return nil, err
	}
	p.cmd.Env = append(os.Environ(), lifecycleHelperEnv+"=agent", "PRQ_LIFECYCLE_AGENT_STARTED="+started, "PRQ_LIFECYCLE_FD_CLOSED="+fdClosed)
	return p, nil
}

func lifecycleRoot(t *testing.T) (string, owner) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "worktrees", uuid.NewString())
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		t.Fatal(err)
	}
	o, err := newOwner(filepath.Base(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := reserveOwner(root, o); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root, o
}

func TestGatedProcessFaultBoundariesAndClosedAgentFD(t *testing.T) {
	sentinel := errors.New("injected lifecycle failure")
	for _, phase := range []string{"before_identity", "before_release", "after_release"} {
		t.Run(phase, func(t *testing.T) {
			root, o := lifecycleRoot(t)
			started := filepath.Join(t.TempDir(), "started")
			fdClosed := filepath.Join(t.TempDir(), "fd-closed")
			p, err := lifecycleProcess(t.Context(), started, fdClosed)
			if err != nil {
				t.Fatal(err)
			}
			faults := &launchFaults{}
			switch phase {
			case "before_identity":
				faults.BeforeIdentitySave = func() error { return sentinel }
			case "before_release":
				faults.BeforeRelease = func() error { return sentinel }
			case "after_release":
				faults.AfterRelease = func() error {
					waitLifecycleFile(started, 5*time.Second)
					return sentinel
				}
			}
			err = p.start(root, &o, nil, faults)
			if !errors.Is(err, sentinel) {
				t.Fatalf("start error = %v", err)
			}
			assertLifecycleProcessGone(t, p.cmd.Process.Pid)
			_, startErr := os.Stat(started)
			if phase == "after_release" {
				if startErr != nil {
					t.Fatalf("released agent did not execute: %v", startErr)
				}
				if _, err := os.Stat(fdClosed); err != nil {
					t.Fatalf("agent did not verify fd 3 closure: %v", err)
				}
			} else if !os.IsNotExist(startErr) {
				t.Fatalf("agent executed before release: %v", startErr)
			}
			if phase == "before_release" && (o.AgentPID == 0 || o.AgentStart == "" || o.Released) {
				t.Fatalf("identity was not persisted before release: %+v", o)
			}
			if phase == "before_release" {
				stored := readLifecycleOwner(t, root)
				if stored.Version != 2 || stored.AgentPID != o.AgentPID || stored.AgentStart == "" || stored.Released {
					t.Fatalf("stored pre-release identity: %+v", stored)
				}
			}
		})
	}
}

func TestGatedProcessRealSaveReleaseAndIdentityFailures(t *testing.T) {
	t.Run("owner save", func(t *testing.T) {
		root, o := lifecycleRoot(t)
		started := filepath.Join(t.TempDir(), "started")
		p, err := lifecycleProcess(t.Context(), started, filepath.Join(t.TempDir(), "fd"))
		if err != nil {
			t.Fatal(err)
		}
		faults := &launchFaults{BeforeIdentitySave: func() error {
			if err := os.Remove(ownerPath(root)); err != nil {
				return err
			}
			return os.Mkdir(ownerPath(root), 0700)
		}}
		err = p.start(root, &o, nil, faults)
		if err == nil || !strings.Contains(err.Error(), "owner") && !strings.Contains(err.Error(), "directory") {
			t.Fatalf("real owner save failure = %v", err)
		}
		assertLifecycleProcessGone(t, p.cmd.Process.Pid)
		if _, err := os.Stat(started); !os.IsNotExist(err) {
			t.Fatalf("agent executed after owner save failure: %v", err)
		}
	})

	t.Run("release write", func(t *testing.T) {
		root, o := lifecycleRoot(t)
		started := filepath.Join(t.TempDir(), "started")
		p, err := lifecycleProcess(t.Context(), started, filepath.Join(t.TempDir(), "fd"))
		if err != nil {
			t.Fatal(err)
		}
		faults := &launchFaults{BeforeRelease: func() error { return p.writer.Close() }}
		err = p.start(root, &o, nil, faults)
		if err == nil || !strings.Contains(err.Error(), "release agent launch") {
			t.Fatalf("real release failure = %v", err)
		}
		assertLifecycleProcessGone(t, p.cmd.Process.Pid)
		if _, err := os.Stat(started); !os.IsNotExist(err) {
			t.Fatalf("agent executed after release failure: %v", err)
		}
	})

	t.Run("empty identity", func(t *testing.T) {
		root, o := lifecycleRoot(t)
		bin := t.TempDir()
		ps := filepath.Join(bin, "ps")
		if err := os.WriteFile(ps, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin)
		started := filepath.Join(t.TempDir(), "started")
		p, err := lifecycleProcess(t.Context(), started, filepath.Join(t.TempDir(), "fd"))
		if err != nil {
			t.Fatal(err)
		}
		err = p.start(root, &o, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "could not identify agent process") {
			t.Fatalf("empty identity failure = %v", err)
		}
		assertLifecycleProcessGone(t, p.cmd.Process.Pid)
	})
}

func TestCoordinatorDeathAcrossGatePhases(t *testing.T) {
	for _, phase := range []string{"before_identity", "before_release", "after_release"} {
		t.Run(phase, func(t *testing.T) {
			state := t.TempDir()
			root := filepath.Join(state, "worktrees", uuid.NewString())
			artifacts := t.TempDir()
			started := filepath.Join(artifacts, "started")
			fdClosed := filepath.Join(artifacts, "fd")
			ready := filepath.Join(artifacts, "ready")
			gatePIDPath := filepath.Join(artifacts, "gate-pid")
			unrelatedPIDPath := filepath.Join(artifacts, "unrelated-pid")
			cmd := exec.Command(os.Args[0], "-test.run=^TestProcessLifecycleHelper$")
			cmd.Env = append(os.Environ(), lifecycleHelperEnv+"=coordinator", "PRQ_LIFECYCLE_PHASE="+phase, "PRQ_LIFECYCLE_ROOT="+root, "PRQ_LIFECYCLE_AGENT_STARTED="+started, "PRQ_LIFECYCLE_FD_CLOSED="+fdClosed, "PRQ_LIFECYCLE_READY="+ready, "PRQ_LIFECYCLE_GATE_PID="+gatePIDPath, "PRQ_LIFECYCLE_UNRELATED_PID="+unrelatedPIDPath)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
			waitLifecycleFile(ready, 5*time.Second)
			stored := readLifecycleOwner(t, root)
			switch phase {
			case "before_identity":
				if stored.Version != 2 || stored.AgentPID != 0 || stored.AgentStart != "" || stored.Released {
					t.Fatalf("pre-identity owner: %+v", stored)
				}
			case "before_release":
				if stored.Version != 2 || stored.AgentPID == 0 || stored.AgentStart == "" || stored.Released {
					t.Fatalf("pre-release owner: %+v", stored)
				}
			case "after_release":
				if stored.Version != 2 || stored.AgentPID == 0 || stored.AgentStart == "" || !stored.Released {
					t.Fatalf("released owner: %+v", stored)
				}
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_, _ = cmd.Process.Wait()

			gatePID := readLifecyclePID(t, gatePIDPath)
			if phase == "after_release" {
				cleaned, err := (Runner{StateDir: state}).Cleanup(t.Context())
				if err != nil || len(cleaned) != 1 {
					t.Fatalf("recover released agent: %v %v", cleaned, err)
				}
			} else {
				assertLifecycleProcessGone(t, gatePID)
				if _, err := os.Stat(started); !os.IsNotExist(err) {
					t.Fatalf("unreleased agent executed: %v", err)
				}
				cleaned, err := (Runner{StateDir: state}).Cleanup(t.Context())
				if err != nil || len(cleaned) != 1 {
					t.Fatalf("recover unreleased gate: %v %v", cleaned, err)
				}
			}
			assertLifecycleProcessGone(t, gatePID)
			if phase == "after_release" {
				if _, err := os.Stat(started); err != nil {
					t.Fatalf("released agent never started: %v", err)
				}
				if _, err := os.Stat(fdClosed); err != nil {
					t.Fatalf("released agent inherited fd 3: %v", err)
				}
			}
			if phase == "before_identity" {
				helperPID := readLifecyclePID(t, unrelatedPIDPath)
				t.Cleanup(func() { _ = syscall.Kill(helperPID, syscall.SIGKILL) })
				stamp, err := processStart(t.Context(), helperPID)
				if err != nil || stamp == "" {
					t.Fatalf("unrelated helper did not remain alive: %q %v", stamp, err)
				}
				_ = syscall.Kill(helperPID, syscall.SIGKILL)
			}
		})
	}
}

func mustWriteLifecycleFile(path, value string) {
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		panic(err)
	}
}

func waitLifecycleFile(path string, limit time.Duration) {
	deadline := time.Now().Add(limit)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			panic(fmt.Sprintf("timed out waiting for %s", path))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readLifecyclePID(t *testing.T, path string) int {
	t.Helper()
	waitLifecycleFile(path, 5*time.Second)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func assertLifecycleProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		stamp, err := processStart(t.Context(), pid)
		if err == nil && stamp == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d survived: %q %v", pid, stamp, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readLifecycleOwner(t *testing.T, root string) owner {
	t.Helper()
	data, err := os.ReadFile(ownerPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var o owner
	if err := json.Unmarshal(data, &o); err != nil {
		t.Fatal(err)
	}
	return o
}
