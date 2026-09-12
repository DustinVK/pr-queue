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

	"github.com/DustinVK/pr-queue/internal/localfs"
	"github.com/google/uuid"
)

type owner struct {
	ID         string `json:"id"`
	PID        int    `json:"pid"`
	Started    string `json:"started"`
	AgentPID   int    `json:"agent_pid,omitempty"`
	AgentStart string `json:"agent_start,omitempty"`
	Starting   bool   `json:"starting_agent,omitempty"`
}

func newOwner(id string) (owner, error) {
	o := owner{ID: id, PID: os.Getpid()}
	var err error
	o.Started, err = processStart(context.Background(), o.PID)
	if err == nil && o.Started == "" {
		err = fmt.Errorf("could not identify owning process")
	}
	return o, err
}

func saveOwner(root string, o owner) error {
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return localfs.Replace(ownerPath(root), data)
}

func ownerPath(root string) string { return root + ".owner.json" }

// Reserve ownership before creating anything that might need crash recovery.
func reserveOwner(root string, o owner) error {
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	created, err := localfs.WriteNew(ownerPath(root), data)
	if err == nil && !created {
		err = fmt.Errorf("worktree ownership already exists for %s", o.ID)
	}
	return err
}

func processStart(ctx context.Context, pid int) (string, error) {
	if pid < 1 {
		return "", nil
	}
	cmd := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "stat=,lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	data, err := cmd.Output()
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok && e.ExitCode() == 1 && len(data) == 0 {
			return "", nil
		}
		return "", err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 || strings.HasPrefix(fields[0], "Z") {
		return "", nil
	}
	return strings.Join(fields[1:], " "), nil
}

// Cleanup is called with the global run lock held. It never treats a leftover
// metadata file as evidence of a live process, and compares start times for PID reuse.
func (r Runner) Cleanup(ctx context.Context) ([]string, error) {
	base := filepath.Join(r.StateDir, "worktrees")
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cleaned []string
	var problems []error
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".owner.json") {
			continue
		}
		id, err := cleanupEntry(ctx, base, entry.Name())
		if err != nil {
			problems = append(problems, fmt.Errorf("recover %s: %w", entry.Name(), err))
			continue
		}
		if id != "" {
			cleaned = append(cleaned, id)
		}
	}
	return cleaned, errors.Join(problems...)
}

// Each ownership entry is independent; one failure must not leave other
// recoverable worktrees or agent groups behind. Errors still block a new run.
func cleanupEntry(ctx context.Context, base, name string) (string, error) {
	id := strings.TrimSuffix(name, ".owner.json")
	if _, err := uuid.Parse(id); err != nil {
		return "", fmt.Errorf("unexpected worktree ownership file %s", name)
	}
	root := filepath.Join(base, id)
	data, err := os.ReadFile(ownerPath(root))
	if err != nil {
		return "", err
	}
	var o owner
	if err := json.Unmarshal(data, &o); err != nil {
		return "", err
	}
	if o.ID != id || o.PID < 1 || o.Started == "" {
		return "", fmt.Errorf("invalid worktree ownership for %s", name)
	}
	stamp, err := processStart(ctx, o.PID)
	if err != nil {
		return "", err
	}
	if stamp != "" && stamp == o.Started {
		return "", nil
	}
	if o.Starting {
		// Cover a coordinator killed between Start and saving the child PID.
		groups, err := sessionGroups(ctx, o.ID)
		if err != nil {
			return "", err
		}
		for _, pid := range groups {
			if err := killGroup(pid); err != nil {
				return "", err
			}
		}
	} else if o.AgentPID > 1 {
		stamp, err := processStart(ctx, o.AgentPID)
		if err != nil {
			return "", err
		}
		// A group can outlive its leader, and stale metadata can survive a
		// reboot or PID reuse. Absence is not evidence that the group is ours.
		if stamp != "" && stamp == o.AgentStart {
			if err := killGroup(o.AgentPID); err != nil {
				return "", err
			}
		}
	}
	if err := removeWorktree(ctx, root); err != nil {
		return "", err
	}
	return o.ID, nil
}

func sessionGroups(ctx context.Context, id string) ([]int, error) {
	data, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,pgid=,args=").Output()
	if err != nil {
		return nil, err
	}
	var groups []int
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, _ := strconv.Atoi(fields[0])
		pgid, _ := strconv.Atoi(fields[1])
		if pid < 2 || pid != pgid {
			continue
		}
		for i := 2; i+1 < len(fields); i++ {
			if fields[i] == "--session-id" && fields[i+1] == id {
				groups = append(groups, pid)
				break
			}
		}
	}
	return groups, nil
}

func removeWorktree(ctx context.Context, root string) error {
	bare := filepath.Join(root, "repo.git")
	checkout := filepath.Join(root, "checkout")
	if _, err := os.Stat(checkout); err == nil {
		// Interrupted setup may leave an unregistered checkout. Both the bare
		// repository and checkout belong to this disposable root, so removing
		// that root also removes any incomplete internal Git metadata.
		_, _ = git(ctx, bare, "worktree", "remove", "--force", "--force", checkout)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	if err := os.Remove(ownerPath(root)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
