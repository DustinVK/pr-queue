package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/DustinVK/pr-queue/internal/localfs"
	"github.com/DustinVK/pr-queue/internal/notify"
)

type ReviewOutcome struct {
	PR     int    `json:"pr"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}
type RepoPass struct {
	Repo       string          `json:"repo"`
	StartedAt  string          `json:"started_at"`
	FinishedAt string          `json:"finished_at"`
	Problems   []Problem       `json:"problems"`
	Reviews    []ReviewOutcome `json:"reviews"`
}
type RunSummary struct {
	SchemaVersion     int                 `json:"schema_version"`
	Passes            map[string]RepoPass `json:"passes"`
	FailureSignatures map[string]string   `json:"failure_signatures"`
	NotificationError string              `json:"notification_error,omitempty"`
}

func SummaryPath(state string) string { return filepath.Join(state, "run-summary.json") }
func LoadRunSummary(state string) (RunSummary, error) {
	s := RunSummary{SchemaVersion: 1, Passes: map[string]RepoPass{}, FailureSignatures: map[string]string{}}
	data, err := os.ReadFile(SummaryPath(state))
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, err
	}
	if s.SchemaVersion != 1 || s.Passes == nil || s.FailureSignatures == nil {
		return s, fmt.Errorf("invalid run-summary format")
	}
	return s, nil
}

// FinishRun requires the global run lock. The five-table model stores PR runs;
// this small file also retains repository failures that have no PR row.
func FinishRun(ctx context.Context, state string, r RunResult, sink notify.Sink) error {
	if r.DryRun {
		return nil
	}
	s, err := LoadRunSummary(state)
	if err != nil {
		return err
	}
	newFailure := false
	refs := map[string]bool{}
	record := func(key, message string) {
		hash := sha256.Sum256([]byte(message))
		signature := hex.EncodeToString(hash[:])
		if s.FailureSignatures[key] != signature {
			newFailure = true
		}
		s.FailureSignatures[key] = signature
	}
	for _, repo := range r.Repos {
		key := strings.ToLower(repo.Repo)
		pass := RepoPass{Repo: repo.Repo, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, Problems: repo.Problems, Reviews: []ReviewOutcome{}}
		repoFailure := false
		for _, problem := range repo.Problems {
			if problem.PR == 0 {
				repoFailure = true
			}
		}
		if !repoFailure {
			delete(s.FailureSignatures, key+":repository")
		}
		for _, o := range repo.Observations {
			if o.Local == nil {
				continue
			} // A busy lock gives no new observation.
			prKey := fmt.Sprintf("%s#%d", key, o.Number)
			delete(s.FailureSignatures, prKey+":observation")
			if o.Local.State != "open" || o.Local.Draft {
				delete(s.FailureSignatures, prKey+":review")
			}
		}
		for _, problem := range repo.Problems {
			problemKey := key + ":repository"
			ref := repo.Repo
			if problem.PR > 0 {
				ref = fmt.Sprintf("%s#%d", repo.Repo, problem.PR)
				problemKey = fmt.Sprintf("%s#%d:observation", key, problem.PR)
			}
			record(problemKey, problem.Error)
			refs[ref] = true
		}
		for _, review := range repo.Reviews {
			pass.Reviews = append(pass.Reviews, ReviewOutcome{PR: review.PR, Status: review.Status, Error: review.Error})
			prKey := fmt.Sprintf("%s#%d:review", key, review.PR)
			if review.Error != "" {
				// Diagnostic filenames change every retry; the underlying failure doesn't.
				message := review.Error
				if review.ID != "" {
					message = strings.ReplaceAll(message, review.ID, "<run>")
				}
				message = review.Status + ":" + message
				record(prKey, message)
				refs[fmt.Sprintf("%s#%d", repo.Repo, review.PR)] = true
			} else {
				delete(s.FailureSignatures, prKey)
			}
			if review.Ingestion != nil && review.Ingestion.Changed > 0 {
				refs[fmt.Sprintf("%s#%d", repo.Repo, review.PR)] = true
			}
		}
		s.Passes[key] = pass
	}
	var notificationErr error
	if sink != nil && (r.Changed > 0 || newFailure) {
		ids := make([]string, 0, len(refs))
		for ref := range refs {
			ids = append(ids, ref)
		}
		sort.Strings(ids)
		more := ""
		if len(ids) > 5 {
			more = fmt.Sprintf(" (+%d more)", len(ids)-5)
			ids = ids[:5]
		}
		message := fmt.Sprintf("%d new or changed findings; %d failures. %s%s", r.Changed, r.Failed, strings.Join(ids, ", "), more)
		notificationErr = sink.Send(ctx, message)
		s.NotificationError = ""
		if notificationErr != nil {
			s.NotificationError = notificationErr.Error()
		}
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err == nil {
		err = localfs.Replace(SummaryPath(state), append(data, '\n'))
	}
	return errors.Join(notificationErr, err)
}
