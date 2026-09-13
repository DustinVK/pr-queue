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

	"github.com/DustinVK/pr-queue/internal/localfs"
	"github.com/google/uuid"
)

type owner struct {
	Version     int    `json:"version,omitempty"`
	ID          string `json:"id"`
	PID         int    `json:"pid"`
	Started     string `json:"started"`
	AgentPID    int    `json:"agent_pid,omitempty"`
	AgentStart  string `json:"agent_start,omitempty"`
	Starting    bool   `json:"starting_agent,omitempty"`
	Released    bool   `json:"agent_released,omitempty"`
	ArtifactDir string `json:"artifact_dir,omitempty"`
}

func newOwner(id string) (owner, error) {
	o := owner{Version: 2, ID: id, PID: os.Getpid()}
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

func (r Runner) recoveryDir() string {
	if r.RecoveryDir != "" {
		return r.RecoveryDir
	}
	return r.StateDir
}

func (r Runner) separateRecoveryDir() bool {
	return filepath.Clean(r.recoveryDir()) != filepath.Clean(r.StateDir)
}

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
	base := filepath.Join(r.recoveryDir(), "worktrees")
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
		id, err := cleanupEntry(ctx, r.recoveryDir(), base, entry.Name())
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
func cleanupEntry(ctx context.Context, recoveryDir, base, name string) (string, error) {
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
	if err := validateOwner(recoveryDir, id, o); err != nil {
		return "", fmt.Errorf("invalid worktree ownership for %s: %w", name, err)
	}
	stamp, err := processStart(ctx, o.PID)
	if err != nil {
		return "", err
	}
	if stamp != "" && stamp == o.Started {
		return "", nil
	}
	if o.Version == 0 && o.Starting {
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
	} else if (o.Version == 2 || o.Version == 3) && o.AgentPID > 1 {
		if o.AgentStart == "" {
			return "", fmt.Errorf("invalid versioned agent ownership for %s", name)
		}
		stamp, err := processStart(ctx, o.AgentPID)
		if err != nil {
			return "", err
		}
		if stamp != "" && stamp == o.AgentStart {
			group, err := syscall.Getpgid(o.AgentPID)
			if err != nil && !errors.Is(err, syscall.ESRCH) {
				return "", err
			}
			if group == o.AgentPID {
				if err := killGroup(o.AgentPID); err != nil {
					return "", err
				}
			} else if err == nil {
				return "", fmt.Errorf("versioned agent process %d does not lead its process group", o.AgentPID)
			}
		}
	} else if o.Version == 0 && o.AgentPID > 1 {
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
	if err := removeWorktreeRoot(root); err != nil {
		return "", err
	}
	if o.Version == 3 {
		if err := os.RemoveAll(o.ArtifactDir); err != nil {
			return "", err
		}
	}
	if err := removeOwner(root); err != nil {
		return "", err
	}
	return o.ID, nil
}

func validateAgentOwnership(o owner) error {
	if o.Version != 2 && o.Version != 3 {
		return nil
	}
	if o.Starting {
		return fmt.Errorf("versioned owner contains legacy starting state")
	}
	if o.AgentPID == 0 {
		if o.AgentStart != "" || o.Released {
			return fmt.Errorf("agent identity is incomplete")
		}
		return nil
	}
	if o.AgentPID < 2 || o.AgentStart == "" {
		return fmt.Errorf("agent identity is incomplete")
	}
	return nil
}

func validateOwner(recoveryDir, id string, o owner) error {
	if o.ID != id || o.PID < 1 || o.Started == "" || (o.Version != 0 && o.Version != 2 && o.Version != 3) {
		return fmt.Errorf("owner identity is invalid")
	}
	if err := validateAgentOwnership(o); err != nil {
		return err
	}
	return validateArtifactOwnership(recoveryDir, o)
}

func validateArtifactOwnership(recoveryDir string, o owner) error {
	if o.Version != 3 {
		if o.ArtifactDir != "" {
			return fmt.Errorf("version %d owner contains an artifact directory", o.Version)
		}
		return nil
	}
	if err := validateArtifactDirPath(recoveryDir, o.ArtifactDir); err != nil {
		return err
	}
	info, err := os.Lstat(o.ArtifactDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := validateArtifactRoot(recoveryDir); err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("artifact path is not a directory")
	}
	if info.Mode().Perm() != 0700 {
		return fmt.Errorf("artifact directory is not private")
	}
	return nil
}

func validateArtifactDirPath(recoveryDir, dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return fmt.Errorf("artifact directory must be an absolute clean path")
	}
	root := filepath.Join(filepath.Clean(recoveryDir), "dry-runs")
	name := filepath.Base(dir)
	if filepath.Dir(dir) != root || !strings.HasPrefix(name, "run-") || name == "run-" {
		return fmt.Errorf("artifact directory is outside the recovery-owned dry-run root")
	}
	return nil
}

func validateArtifactRoot(recoveryDir string) error {
	root := filepath.Join(filepath.Clean(recoveryDir), "dry-runs")
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect dry-run artifact root: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("dry-run artifact root is not a directory")
	}
	if info.Mode().Perm() != 0700 {
		return fmt.Errorf("dry-run artifact root is not private")
	}
	return nil
}

// ReleaseArtifactOwnership removes a completed split-root review's persistent
// record only after its temporary artifact directory has been removed. If the
// worktree remains, recovery still owns it and the record is retained.
func (r Runner) ReleaseArtifactOwnership(id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return err
	}
	root := filepath.Join(r.recoveryDir(), "worktrees", id)
	if _, err := os.Lstat(root); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	data, err := os.ReadFile(ownerPath(root))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var o owner
	if err := json.Unmarshal(data, &o); err != nil {
		return err
	}
	if err := validateOwner(r.recoveryDir(), id, o); err != nil {
		return fmt.Errorf("invalid completed artifact ownership for %s: %w", id, err)
	}
	if o.Version != 3 || filepath.Clean(o.ArtifactDir) != filepath.Clean(r.StateDir) {
		return fmt.Errorf("invalid completed artifact ownership for %s", id)
	}
	if _, err := os.Lstat(o.ArtifactDir); !os.IsNotExist(err) {
		if err == nil {
			return fmt.Errorf("artifact directory still exists for %s", id)
		}
		return err
	}
	return removeOwner(root)
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

func removeWorktree(root string) error {
	// The bare repository, worktree registration, and checkout all belong to
	// this disposable root, including any incomplete setup artifacts.
	if err := removeWorktreeRoot(root); err != nil {
		return err
	}
	return removeOwner(root)
}

func removeWorktreeRoot(root string) error {
	return os.RemoveAll(root)
}

func removeOwner(root string) error {
	if err := os.Remove(ownerPath(root)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
