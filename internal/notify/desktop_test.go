package notify

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDesktopPrefersNotifierAndFallsBackWithLiteralArguments(t *testing.T) {
	message := "2 findings; owner/repo#1 \" $(touch /tmp/not-a-command)"
	for _, fallback := range []bool{false, true} {
		calls := []string{}
		d := Desktop{LookPath: func(name string) (string, error) { return "/tools/" + name, nil }, Exec: func(_ context.Context, path string, args []string) error {
			calls = append(calls, path)
			if strings.HasSuffix(path, "terminal-notifier") {
				if !reflect.DeepEqual(args, []string{"-title", "prqueue", "-message", message}) {
					t.Errorf("notifier arguments: %v", args)
				}
				if fallback {
					return errors.New("notifier failed")
				}
				return nil
			}
			if !reflect.DeepEqual(args, []string{"-e", appleScript, "--", message}) {
				t.Errorf("osascript arguments: %v", args)
			}
			if strings.Contains(args[1], message) {
				t.Error("PR text entered AppleScript source")
			}
			return nil
		}}
		if err := d.Send(t.Context(), message); err != nil {
			t.Fatal(err)
		}
		want := 1
		if fallback {
			want = 2
		}
		if len(calls) != want {
			t.Fatalf("sink calls: %v", calls)
		}
	}
}

func TestDesktopBothFailuresAndMissingOptionalTool(t *testing.T) {
	calls := 0
	d := Desktop{LookPath: func(name string) (string, error) {
		if name == "terminal-notifier" {
			return "", errors.New("not installed")
		}
		return name, nil
	}, Exec: func(context.Context, string, []string) error { calls++; return errors.New("no display session") }}
	if err := d.Send(t.Context(), "counts only"); err == nil || calls != 1 {
		t.Fatalf("fallback failure: %d %v", calls, err)
	}
}
