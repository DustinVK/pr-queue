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
	if e.Holder.PID <= 0 || e.Holder.Key == "" || e.Holder.Since == "" {
		return fmt.Sprintf("lock held: %s (holder metadata unavailable)", e.Path)
	}
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
	// Publish the new record before trimming an older, longer one. Readers may
	// still race initial creation; holder metadata is a best-effort diagnostic.
	if err == nil {
		_, err = f.WriteAt(data, 0)
	}
	if err == nil {
		err = f.Truncate(int64(len(data)))
	}
	// No Sync: the kernel lock cannot survive a crash, so its diagnostic record
	// needs visibility to other readers, not storage durability.
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
	if err := json.NewDecoder(io.LimitReader(f, 4096)).Decode(&h); err != nil {
		return Holder{}
	}
	return h
}

// Inspect queries an existing lock without acquiring it or changing its metadata.
// On macOS, F_GETLK detects flock locks without competing with real acquisitions.
func Inspect(path string) (*Holder, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	query := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &query); err != nil {
		return nil, err
	}
	if query.Type == syscall.F_UNLCK {
		return nil, nil
	}
	h := readHolder(f)
	return &h, nil
}
