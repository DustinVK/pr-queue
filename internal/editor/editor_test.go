package editor

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/DustinVK/pr-queue/internal/findings"
)

func TestCommandArgumentsDoNotExpandShellInput(t *testing.T) {
	args, err := commandArgs(`"/Applications/My Editor/editor" --wait 'two words' "$(touch nope)"`)
	want := []string{"/Applications/My Editor/editor", "--wait", "two words", "$(touch nope)"}
	if err != nil || !reflect.DeepEqual(args, want) {
		t.Fatalf("args: %v %v", args, err)
	}
	for _, bad := range []string{"'unfinished", `editor\`, `"" --wait`} {
		if _, err := commandArgs(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func editorFixture(t *testing.T, script string) (Editor, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "editor with spaces")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "output-path")
	t.Setenv("PRQ_EDITOR_MARKER", marker)
	return Editor{Command: strconv.Quote(path) + " --wait", In: strings.NewReader(""), Out: io.Discard}, marker
}

func TestEditorPrivateFileExactBytesAndCleanup(t *testing.T) {
	e, marker := editorFixture(t, `test "$1" = --wait || exit 40
printf '%s' "$2" > "$PRQ_EDITOR_MARKER"
printf '  Edited Markdown.\r\n' > "$2"
`)
	body, err := e.Edit(t.Context(), "original")
	if err != nil || body != "  Edited Markdown.\r\n" {
		t.Fatalf("editor: %q %v", body, err)
	}
	path, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(string(path))); !os.IsNotExist(err) {
		t.Fatal("editor directory remained")
	}
}

func TestEditorFailureAndOversizeRejectOutput(t *testing.T) {
	e, _ := editorFixture(t, "printf edited > \"$2\"\nexit 42\n")
	if _, err := e.Edit(t.Context(), "original"); err == nil {
		t.Fatal("failed editor accepted")
	}
	e, _ = editorFixture(t, "exit 0\n")
	if _, err := e.Edit(t.Context(), strings.Repeat("x", findings.MaxBodyBytes+1)); err == nil {
		t.Fatal("oversized editor result accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.Edit(ctx, "original"); err == nil {
		t.Fatal("cancelled editor accepted")
	}
}
