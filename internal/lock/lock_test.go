package lock

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestCrossProcessContentionAndDeath(t *testing.T) {
	path := RunPath(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLockHelper$")
	cmd.Env = append(os.Environ(), "PRQ_LOCK_TEST_PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("child startup %q: %v", line, err)
	}
	_, err = Acquire(path, "parent")
	var busy *BusyError
	if !errors.As(err, &busy) || busy.Holder.PID != cmd.Process.Pid {
		t.Fatalf("contention: %v", err)
	}
	h, err := Inspect(path)
	if err != nil || h == nil || h.Key != "child" {
		t.Fatalf("inspect: %+v %v", h, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	h, err = Inspect(path)
	if err != nil || h != nil {
		t.Fatalf("dead process still holds lock: %+v %v", h, err)
	}
	l, err := Acquire(path, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("must not remove lock inode:", err)
	}
}

func TestLockHelper(t *testing.T) {
	path := os.Getenv("PRQ_LOCK_TEST_PATH")
	if path == "" {
		return
	}
	l, err := Acquire(path, "child")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fmt.Println("ready")
	time.Sleep(time.Minute)
}

func TestDifferentPRsRemainUsable(t *testing.T) {
	state := t.TempDir()
	path := PRPath(state, "owner/repo", 1)
	first, err := Acquire(path, "owner/repo#1")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	_, err = Acquire(path, "again")
	var busy *BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("wanted contention, got %v", err)
	}
	second, err := Acquire(PRPath(state, "owner/repo", 2), "owner/repo#2")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
}

func TestInspectDoesNotCreate(t *testing.T) {
	path := RunPath(t.TempDir())
	h, err := Inspect(path)
	if h != nil || err != nil {
		t.Fatalf("%v %v", h, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("inspect created file: %v", err)
	}
}

func TestPRPathsIgnoreRepositoryCase(t *testing.T) {
	state := t.TempDir()
	if PRPath(state, "Owner/Repo", 1) != PRPath(state, "owner/repo", 1) {
		t.Fatal("repository case must not create a separate lock")
	}
}
