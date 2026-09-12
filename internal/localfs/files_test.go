package localfs

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestWriteNewConcurrentPreservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "config")
	var wg sync.WaitGroup
	var created atomic.Int32
	for i := range 8 {
		wg.Go(func() {
			body := strings.Repeat(string(rune('a'+i)), 4096)
			ok, err := WriteNew(path, []byte(body))
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				created.Add(1)
			}
		})
	}
	wg.Wait()
	if created.Load() != 1 {
		t.Fatalf("created %d times", created.Load())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 4096 || string(data) != strings.Repeat(string(data[0]), 4096) {
		t.Fatal("partial or mixed file")
	}
	for p, mode := range map[string]os.FileMode{path: 0600, filepath.Dir(path): 0700} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("%s: mode %v", p, info.Mode())
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files left: %v, %v", entries, err)
	}
}
