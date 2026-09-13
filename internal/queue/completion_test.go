package queue

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/DustinVK/pr-queue/internal/store"
	"github.com/google/uuid"
)

type captureSink struct {
	messages []string
	err      error
}

func (s *captureSink) Send(_ context.Context, message string) error {
	s.messages = append(s.messages, message)
	return s.err
}

func TestCompletionQuietOnRepeatedFailuresAndNotifiesWhenChanged(t *testing.T) {
	state := t.TempDir()
	sink := &captureSink{}
	repo := RepoResult{Repo: "owner/repo", ObservationBatch: ObservationBatch{Problems: []Problem{{Repo: "owner/repo", Error: "list failed"}}}}
	r := RunResult{StartedAt: "start", FinishedAt: "finish", Failed: 1, Repos: []RepoResult{repo}}
	for range 2 {
		if err := FinishRun(t.Context(), state, r, sink); err != nil {
			t.Fatal(err)
		}
	}
	if len(sink.messages) != 1 {
		t.Fatalf("repeated repository failure notified %d times", len(sink.messages))
	}
	r.Repos[0].Problems[0].Error = "different list failure"
	if err := FinishRun(t.Context(), state, r, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.messages) != 2 {
		t.Fatal("changed failure stayed quiet")
	}
	r.Repos[0].Problems = nil
	r.Failed = 0
	if err := FinishRun(t.Context(), state, r, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.messages) != 2 {
		t.Fatal("clean pass notified")
	}
	r.Repos[0].Problems = []Problem{{Repo: "owner/repo", Error: "different list failure"}}
	r.Failed = 1
	if err := FinishRun(t.Context(), state, r, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.messages) != 3 {
		t.Fatal("returning failure was not new")
	}
	summary, err := LoadRunSummary(state)
	if err != nil || len(summary.Passes) != 1 || len(summary.Passes["owner/repo"].Problems) != 1 {
		t.Fatalf("repository failure absent from summary: %+v %v", summary, err)
	}
}

func TestCompletionIgnoresRunIDsAndPreservesFailuresAcrossLockSkips(t *testing.T) {
	state := t.TempDir()
	sink := &captureSink{}
	r := RunResult{Failed: 1, Repos: []RepoResult{{Repo: "owner/repo"}}}
	for range 2 {
		id := uuid.NewString()
		r.Repos[0].Reviews = []ReviewResult{{ID: id, PR: 1, Status: "failed", Error: "agent failed (see /state/runs/" + id + "/agent.jsonl): exit status 1"}}
		if err := FinishRun(t.Context(), state, r, sink); err != nil {
			t.Fatal(err)
		}
	}
	if len(sink.messages) != 1 {
		t.Fatal("diagnostic filename made an identical error appear new")
	}
	skipped := RunResult{Repos: []RepoResult{{Repo: "owner/repo", ObservationBatch: ObservationBatch{Observations: []Observation{{Number: 1, Reason: "lock held"}}}}}}
	if err := FinishRun(t.Context(), state, skipped, sink); err != nil {
		t.Fatal(err)
	}
	if err := FinishRun(t.Context(), state, r, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.messages) != 1 {
		t.Fatal("busy observation reset failure baseline")
	}
	r.Repos[0].Reviews[0].Error = "timeout"
	r.Repos[0].Reviews[0].Status = "timed_out"
	if err := FinishRun(t.Context(), state, r, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.messages) != 2 {
		t.Fatal("different PR failure stayed quiet")
	}
}

func TestCompletionCountsOnlyNotificationErrorsAndDryRunIsolation(t *testing.T) {
	state := t.TempDir()
	sink := &captureSink{err: errors.New("both desktop sinks failed")}
	ingested := &store.Ingestion{Changed: 1, Findings: []store.Finding{{}}}
	review := ReviewResult{ID: uuid.NewString(), PR: 1, Status: "succeeded", Ingestion: ingested}
	r := RunResult{Changed: 1, Repos: []RepoResult{{Repo: "owner/repo", Reviews: []ReviewResult{review}}}}
	// Use a body that must never be put in a notification.
	r.Repos[0].Reviews[0].Ingestion.Findings[0].Finding.Body = "PRIVATE FINDING BODY"
	if err := FinishRun(t.Context(), state, r, sink); err == nil {
		t.Fatal("notification failure not reported")
	} else {
		var summaryErr *RunSummaryError
		if errors.As(err, &summaryErr) {
			t.Fatal("notification failure classified as run-summary failure")
		}
	}
	if len(sink.messages) != 1 || strings.Contains(sink.messages[0], "PRIVATE") || !strings.Contains(sink.messages[0], "owner/repo#1") {
		t.Fatalf("notification leaked content: %v", sink.messages)
	}
	summary, err := LoadRunSummary(state)
	if err != nil || summary.NotificationError == "" {
		t.Fatal("notification failure missing from status data")
	}
	before, err := os.ReadFile(SummaryPath(state))
	if err != nil {
		t.Fatal(err)
	}
	r.DryRun = true
	if err := FinishRun(t.Context(), state, r, sink); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(SummaryPath(state))
	if err != nil || string(after) != string(before) || len(sink.messages) != 1 {
		t.Fatal("dry run updated summary or notified")
	}
}

func TestCompletionClassifiesRunSummaryFailure(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(SummaryPath(state), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	err := FinishRun(t.Context(), state, RunResult{}, nil)
	var summaryErr *RunSummaryError
	if !errors.As(err, &summaryErr) {
		t.Fatalf("run-summary failure was not classified: %v", err)
	}
}
