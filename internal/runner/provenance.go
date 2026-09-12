package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/findings"
	"github.com/DustinVK/pr-queue/internal/localfs"
)

const MetadataFilename = "agent-metadata.json"
const maxObservedIdentityBytes = 8 * 1024 * 1024

type ObservedIdentity struct {
	CLI     string `json:"cli,omitempty"`
	Model   string `json:"model,omitempty"`
	Session string `json:"session,omitempty"`
}

type AgentMetadata struct {
	Version              int                 `json:"version"`
	RunID                string              `json:"run_id"`
	Input                findings.Input      `json:"input"`
	Comparison           findings.Comparison `json:"comparison"`
	Provider             string              `json:"provider"`
	ConfiguredExecutable string              `json:"configured_executable"`
	ResolvedExecutable   string              `json:"resolved_executable"`
	Arguments            []string            `json:"arguments"`
	ModelSelection       string              `json:"model_selection"`
	State                string              `json:"state"`
	PreparedAt           time.Time           `json:"prepared_at"`
	ReleasedAt           *time.Time          `json:"released_at,omitempty"`
	FinishedAt           *time.Time          `json:"finished_at,omitempty"`
	Observed             *ObservedIdentity   `json:"observed,omitempty"`
}

func PreparedMetadata(runID string, input findings.Input, comparison findings.Comparison, provider, configuredExecutable, resolvedExecutable string, args []string) AgentMetadata {
	return AgentMetadata{Version: 1, RunID: runID, Input: input, Comparison: comparison, Provider: provider, ConfiguredExecutable: configuredExecutable, ResolvedExecutable: resolvedExecutable, Arguments: append([]string(nil), args...), ModelSelection: "inherited", State: "prepared", PreparedAt: time.Now().UTC()}
}

func (m *AgentMetadata) MarkReleased() {
	now := time.Now().UTC()
	m.State = "released"
	m.ReleasedAt = &now
}
func (m *AgentMetadata) MarkFinished(observed *ObservedIdentity) {
	now := time.Now().UTC()
	// A finished launch process does not by itself prove that its exec succeeded.
	m.State = "process_finished"
	m.FinishedAt = &now
	m.Observed = observed
}

// ObservedIdentityFromJSONL extracts only provider event fields whose meaning
// is explicit. Diagnostics that do not contain such an event prove nothing.
func ObservedIdentityFromJSONL(provider string, data []byte) *ObservedIdentity {
	return ObservedIdentityFromJSONLReader(provider, bytes.NewReader(data))
}

func ObservedIdentityFromJSONLReader(provider string, r io.Reader) *ObservedIdentity {
	decoder := json.NewDecoder(io.LimitReader(r, maxObservedIdentityBytes))
	for {
		var event struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			ThreadID  string `json:"thread_id"`
			SessionID string `json:"session_id"`
			Model     string `json:"model"`
		}
		if err := decoder.Decode(&event); err == io.EOF {
			return nil
		} else if err != nil {
			return nil
		}
		switch {
		case provider == config.ProviderCodex && event.Type == "thread.started" && event.ThreadID != "":
			return &ObservedIdentity{Session: event.ThreadID}
		case provider == config.ProviderClaude && event.Type == "system" && event.Subtype == "init" && event.SessionID != "":
			return &ObservedIdentity{Model: event.Model, Session: event.SessionID}
		}
	}
	return nil
}

func WriteAgentMetadata(path string, metadata AgentMetadata) error {
	return writeAgentMetadata(path, metadata, false)
}

func WritePreparedAgentMetadata(path string, metadata AgentMetadata) error {
	return writeAgentMetadata(path, metadata, true)
}

func writeAgentMetadata(path string, metadata AgentMetadata, exclusive bool) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("encode agent metadata: %w", err)
	}
	data = append(data, '\n')
	if exclusive {
		created, err := localfs.WriteNew(path, data)
		if err != nil {
			return fmt.Errorf("write agent metadata: %w", err)
		}
		if !created {
			return fmt.Errorf("agent metadata already exists: %s", path)
		}
		return nil
	}
	if err := localfs.Replace(path, data); err != nil {
		return fmt.Errorf("write agent metadata: %w", err)
	}
	return nil
}
