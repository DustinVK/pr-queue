package main

import (
	"bytes"
	"testing"
)

func TestQueueReadErrorsDoNotRenderZeroValues(t *testing.T) {
	a, _, out := runFixture(t)
	var diagnostics bytes.Buffer
	a.errOut = &diagnostics
	for _, args := range [][]string{
		{"list", "--status", "invalid"},
		{"show", "owner/repo#999"},
		{"diff", "not-a-finding"},
	} {
		out.Reset()
		diagnostics.Reset()
		if code := a.run(t.Context(), args); code != 1 {
			t.Fatalf("failed read %v returned %d", args, code)
		}
		if out.Len() != 0 {
			t.Fatalf("failed read %v rendered zero-value output: %q", args, out.String())
		}
		if diagnostics.Len() == 0 {
			t.Fatalf("failed read %v omitted its diagnostic", args)
		}
	}
}
