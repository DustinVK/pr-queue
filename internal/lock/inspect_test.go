package lock

import (
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

func TestContentionWithUnavailableHolder(t *testing.T) {
	path := RunPath(t.TempDir())
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", `{"pid":123,"key":`} {
		if err := f.Truncate(0); err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt([]byte(body), 0); err != nil {
			t.Fatal(err)
		}
		_, err := Acquire(path, "contender")
		var busy *BusyError
		if !errors.As(err, &busy) || !strings.Contains(err.Error(), "holder metadata unavailable") {
			t.Fatalf("contention diagnostic: %v", err)
		}
		holder, err := Inspect(path)
		if err != nil || holder == nil || *holder != (Holder{}) {
			t.Fatalf("unknown holder: %+v, %v", holder, err)
		}
	}
}

func TestAcquireReplacesLongerHolderRecord(t *testing.T) {
	path := RunPath(t.TempDir())
	for _, key := range []string{strings.Repeat("long", 100), "short"} {
		l, err := Acquire(path, key)
		if err != nil {
			t.Fatal(err)
		}
		h, err := Inspect(path)
		if err != nil || h == nil || h.Key != key || h.PID != os.Getpid() {
			l.Close()
			t.Fatalf("holder: %+v, %v", h, err)
		}
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInspectDoesNotContendWithAcquisition(t *testing.T) {
	path := RunPath(t.TempDir())
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var stop atomic.Bool
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for !stop.Load() {
				if _, err := Inspect(path); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	defer func() { stop.Store(true); wg.Wait() }()
	for range 10000 {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatalf("inspection caused acquisition contention: %v", err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatal(err)
		}
	}
}
