// Package notify provides the optional local notification sink.
package notify

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

type Sink interface {
	Send(context.Context, string) error
}
type Desktop struct {
	LookPath func(string) (string, error)
	Exec     func(context.Context, string, []string) error
}

const appleScript = "on run argv\n display notification (item 1 of argv) with title \"prqueue\"\nend run"

func (d Desktop) Send(ctx context.Context, message string) error {
	lookup := d.LookPath
	if lookup == nil {
		lookup = exec.LookPath
	}
	run := d.Exec
	if run == nil {
		run = func(ctx context.Context, path string, args []string) error {
			cmd := exec.CommandContext(ctx, path, args...)
			cmd.WaitDelay = time.Second
			return cmd.Run()
		}
	}
	var failures []error
	for _, candidate := range []struct {
		name string
		args []string
	}{
		{"terminal-notifier", []string{"-title", "prqueue", "-message", message}},
		{"osascript", []string{"-e", appleScript, "--", message}},
	} {
		path, err := lookup(candidate.name)
		if err == nil {
			attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
			err = run(attempt, path, candidate.args)
			cancel()
			if err == nil {
				return nil
			}
		}
		failures = append(failures, fmt.Errorf("%s: %w", candidate.name, err))
	}
	return errors.Join(failures...)
}
