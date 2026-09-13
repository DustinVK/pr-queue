// Package editor runs the user's chosen local editor on a private body file.
package editor

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/DustinVK/pr-queue/internal/findings"
)

type Editor struct {
	Command string
	In      io.Reader
	Out     io.Writer
}

func (e Editor) Edit(ctx context.Context, body string) (string, error) {
	command := e.Command
	if strings.TrimSpace(command) == "" {
		command = "vi"
	}
	args, err := commandArgs(command)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "prqueue-edit-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "body.md")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, args[0], append(args[1:], path)...)
	cmd.Stdin = e.In
	cmd.Stdout = e.Out
	cmd.Stderr = e.Out
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("editor cancelled or failed; finding unchanged: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, findings.MaxBodyBytes+1))
	if err != nil {
		return "", err
	}
	if err := findings.ValidateBody(string(data)); err != nil {
		return "", err
	}
	return string(data), nil
}

// Accept executable arguments and quotes without evaluating shell substitutions.
func commandArgs(command string) ([]string, error) {
	var args []string
	var word strings.Builder
	var quote rune
	escaped, started := false, false
	flush := func() {
		if started {
			args = append(args, word.String())
			word.Reset()
			started = false
		}
	}
	for _, r := range command {
		switch {
		case escaped:
			word.WriteRune(r)
			escaped = false
			started = true
		case r == '\\' && quote != '\'':
			escaped = true
			started = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			started = true
		case unicode.IsSpace(r):
			flush()
		default:
			word.WriteRune(r)
			started = true
		}
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("EDITOR has an unfinished quote or escape")
	}
	flush()
	if len(args) == 0 || args[0] == "" {
		return nil, fmt.Errorf("EDITOR must name an executable")
	}
	return args, nil
}
