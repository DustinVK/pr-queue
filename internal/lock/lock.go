// Package lock provides the two nonblocking flock scopes used by prq.
// Never unlink lock files: their inode is the synchronization primitive.
package lock

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/DustinVK/pr-queue/internal/localfs"
)

type Holder struct {
	PID   int    `json:"pid"`
	Key   string `json:"key"`
	Since string `json:"since"`
}

type BusyError struct {
	Path   string
	Holder Holder
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("lock held: %s (pid %d, %s, since %s)", e.Path, e.Holder.PID, e.Holder.Key, e.Holder.Since)
}

type Lock struct{ file *os.File }

func RunPath(state string) string { return filepath.Join(state, "run.lock") }
func PRPath(state, repo string, number int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s#%d", strings.ToLower(repo), number)))
	return filepath.Join(state, "locks", hex.EncodeToString(sum[:])+".lock")
}

func Acquire(path, key string) (*Lock, error) {
	if err := localfs.PrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		h := readHolder(f)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, &BusyError{Path: path, Holder: h}
		}
		return nil, err
	}
	l := &Lock{file: f}
	data, err := json.Marshal(Holder{PID: os.Getpid(), Key: key, Since: time.Now().UTC().Format(time.RFC3339Nano)})
	if err == nil {
		err = f.Truncate(0)
	}
	if err == nil {
		_, err = f.WriteAt(data, 0)
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

func (l *Lock) Close() error {
	if l.file == nil {
		return nil
	}
	// Closing releases the lock. Metadata may remain and is only meaningful
	// while flock reports contention, never merely because a PID is present.
	err := l.file.Close()
	l.file = nil
	return err
}

func readHolder(f *os.File) Holder {
	var h Holder
	_ = json.NewDecoder(io.LimitReader(f, 4096)).Decode(&h)
	return h
}

// Inspect probes an existing lock without creating it or changing its metadata.
func Inspect(path string) (*Holder, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return nil, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		h := readHolder(f)
		return &h, nil
	}
	return nil, err
}
