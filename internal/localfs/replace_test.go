package localfs

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestReplaceConcurrentReadersAndPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 8192)), 0644); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var reads atomic.Int64
	var reader sync.WaitGroup
	reader.Go(func() {
		for !stop.Load() {
			data, err := os.ReadFile(path)
			if err != nil || len(data) != 8192 || string(data) != strings.Repeat(string(data[0]), 8192) {
				t.Errorf("incomplete replacement: %d bytes, %v", len(data), err)
				return
			}
			reads.Add(1)
		}
	})
	defer func() { stop.Store(true); reader.Wait() }()
	var writers sync.WaitGroup
	for i := range 4 {
		writers.Go(func() {
			for range 8 {
				if err := Replace(path, []byte(strings.Repeat(string(rune('b'+i)), 8192))); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	writers.Wait()
	if reads.Load() == 0 {
		t.Fatal("reader never sampled replacements")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("replacement mode: %v, %v", info, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("left temporary files: %v, %v", entries, err)
	}
}

func TestReplaceFailurePreservesDestinationAndRemovesTemp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "destination")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Replace(path, []byte("new state")); err == nil {
		t.Fatal("replaced a directory with a file")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("changed destination: %v, %v", info, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("left temporary files: %v, %v", entries, err)
	}
}
