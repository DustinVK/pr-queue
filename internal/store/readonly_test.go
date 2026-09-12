package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestReadOnlyWithoutLiveWriter(t *testing.T) {
	for _, abrupt := range []bool{false, true} {
		name := "clean close"
		if abrupt {
			name = "abrupt exit with committed WAL"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "queue.db")
			if abrupt {
				cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestReadOnlyWriterHelper$")
				cmd.Env = append(os.Environ(), "PRQ_STORE_WRITER_TEST_PATH="+path)
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("writer: %v\n%s", err, output)
				}
				if info, err := os.Stat(path + "-wal"); err != nil || info.Size() == 0 {
					t.Fatalf("writer left no WAL: %v, %v", info, err)
				}
			} else {
				s, err := Create(t.Context(), path)
				if err != nil {
					t.Fatal(err)
				}
				seedPR(t, s)
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			ro, err := OpenReadOnly(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer ro.Close()
			var head string
			if err := ro.DB.QueryRow("SELECT head_sha FROM pull_requests WHERE id=1").Scan(&head); err != nil || head != "head" {
				t.Fatalf("committed row: %q, %v", head, err)
			}
			if _, err := ro.DB.Exec("DELETE FROM pull_requests"); err == nil {
				t.Fatal("read-only handle accepted a write")
			}
		})
	}
}

func TestReadOnlyWriterHelper(t *testing.T) {
	path := os.Getenv("PRQ_STORE_WRITER_TEST_PATH")
	if path == "" {
		return
	}
	s, err := Create(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	seedPR(t, s)
	// Exit without closing SQLite, preserving the committed WAL like a killed
	// writer. The parent verifies the durable state through a read-only handle.
	os.Exit(0)
}
