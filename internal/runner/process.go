package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const gateScript = `IFS= read -r token <&3 || exit 125
exec 3<&-
[ "$token" = prqueue-release-v1 ] || exit 125
umask 077
exec "$@"`

type launchFaults struct {
	BeforeIdentitySave func() error
	BeforeRelease      func() error
	AfterRelease       func() error
}

type gatedProcess struct {
	cmd      *exec.Cmd
	reader   *os.File
	writer   *os.File
	released bool
}

type launchCleanupError struct{ err error }

func (e *launchCleanupError) Error() string { return "clean up failed agent launch: " + e.err.Error() }
func (e *launchCleanupError) Unwrap() error { return e.err }

func newGatedProcess(ctx context.Context, executable string, args []string) (*gatedProcess, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmdArgs := append([]string{"-c", gateScript, "prq-agent-gate", executable}, args...)
	cmd := exec.CommandContext(ctx, "/bin/sh", cmdArgs...)
	cmd.ExtraFiles = []*os.File{reader}
	return &gatedProcess{cmd: cmd, reader: reader, writer: writer}, nil
}

func (p *gatedProcess) start(root string, o *owner, afterRelease func() error, faults *launchFaults) (err error) {
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p.cmd.Cancel = func() error { return killGroup(p.cmd.Process.Pid) }
	p.cmd.WaitDelay = 2 * time.Second
	if err := p.cmd.Start(); err != nil {
		p.closeGate()
		return err
	}
	_ = p.reader.Close()
	p.reader = nil
	defer func() {
		if err != nil {
			p.closeGate()
			cleanupErr := errors.Join(killGroup(p.cmd.Process.Pid), waitBounded(p.cmd, 2*time.Second))
			if cleanupErr != nil {
				err = errors.Join(err, &launchCleanupError{err: cleanupErr})
			}
		}
	}()
	if faults != nil && faults.BeforeIdentitySave != nil {
		if err := faults.BeforeIdentitySave(); err != nil {
			return err
		}
	}
	identityCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	o.AgentPID = p.cmd.Process.Pid
	o.AgentStart, err = processStart(identityCtx, o.AgentPID)
	if err != nil {
		return err
	}
	if o.AgentStart == "" {
		return fmt.Errorf("could not identify agent process")
	}
	group, err := syscall.Getpgid(o.AgentPID)
	if err != nil {
		return fmt.Errorf("identify agent process group: %w", err)
	}
	if group != o.AgentPID {
		return fmt.Errorf("agent process %d does not lead process group %d", o.AgentPID, group)
	}
	if err := saveOwner(root, *o); err != nil {
		return err
	}
	if faults != nil && faults.BeforeRelease != nil {
		if err := faults.BeforeRelease(); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(p.writer, "prqueue-release-v1\n"); err != nil {
		return fmt.Errorf("release agent launch: %w", err)
	}
	p.released = true
	if err := p.writer.Close(); err != nil {
		return fmt.Errorf("close agent launch gate: %w", err)
	}
	p.writer = nil
	o.Released = true
	if err := saveOwner(root, *o); err != nil {
		return err
	}
	if afterRelease != nil {
		if err := afterRelease(); err != nil {
			return err
		}
	}
	if faults != nil && faults.AfterRelease != nil {
		if err := faults.AfterRelease(); err != nil {
			return err
		}
	}
	return nil
}

func (p *gatedProcess) closeGate() {
	if p.reader != nil {
		_ = p.reader.Close()
		p.reader = nil
	}
	if p.writer != nil {
		_ = p.writer.Close()
		p.writer = nil
	}
}

func waitBounded(cmd *exec.Cmd, limit time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil
		}
		return err
	case <-time.After(limit):
		return errors.New("timed out waiting for agent process cleanup")
	}
}
