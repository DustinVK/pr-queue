package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseDefaultsAndFilters(t *testing.T) {
	c, err := Parse([]byte(`github: {user: reviewer}
repos:
  - name: owner/repo
    filters:
      authors: [alice, 'dependabot[bot]']
      requested_reviewer: bob
      base_branches: [main, release/v1]
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent.Timeout != 15*time.Minute || c.Agent.Provider != ProviderClaude || c.Agent.Executable != "claude" || c.Agent.MaxParallelReviews != 1 {
		t.Fatalf("defaults: %+v", c.Agent)
	}
	if len(c.Repos) != 1 || *c.Repos[0].Filters.RequestedReviewer != "bob" {
		t.Fatalf("filters: %+v", c.Repos)
	}
	if _, err := Parse([]byte(DefaultYAML)); err != nil {
		t.Fatal(err)
	}
}

func TestRejectBadConfiguration(t *testing.T) {
	for _, input := range []string{
		"", "{}", "github: {user: 'not a login'}", "github: {user: alice, token: secret}",
		"github: {user: alice}\nagent: {timeout: 0s}",
		"github: {user: alice}\nagent: {timeout: tomorrow}",
		"github: {user: alice}\nagent: {max_parallel_reviews: 0}",
		"github: {user: alice}\nagent: {executable: ''}",
		"github: {user: alice}\nrepos: [{name: https://github.com/owner/repo}]",
		"github: {user: alice}\nrepos: [{name: owner/..}]",
		"github: {user: alice}\nrepos: [{name: owner/repo}, {name: OWNER/REPO}]",
		"github: {user: alice}\nrepos: [{name: owner/repo, filters: {authors: ['bad user']}}]",
		"github: {user: alice}\nrepos: [{name: owner/repo, filters: {requested_reviewer: ''}}]",
		"github: {user: alice}\nrepos: [{name: owner/repo, filters: {base_branches: ['bad..branch']}}]",
		"github: {user: alice}\n---\ngithub: {user: bob}",
		"github: {user: alice, user: bob}",
	} {
		t.Run(strings.ReplaceAll(input, "\n", "/"), func(t *testing.T) {
			if _, err := Parse([]byte(input)); err == nil {
				t.Fatalf("accepted %q", input)
			}
		})
	}
}

func TestCustomAgent(t *testing.T) {
	c, err := Parse([]byte("github: {user: alice}\nagent: {executable: '/path with spaces/claude', timeout: 2m, max_parallel_reviews: 3}"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent.Timeout != 2*time.Minute || c.Agent.MaxParallelReviews != 3 {
		t.Fatalf("agent: %+v", c.Agent)
	}
}

func TestProviderDefaultsAndExplicitExecutable(t *testing.T) {
	for _, test := range []struct {
		name       string
		yaml       string
		provider   string
		executable string
	}{
		{"legacy omitted agent", "github: {user: alice}", ProviderClaude, "claude"},
		{"codex default", "github: {user: alice}\nagent: {provider: codex}", ProviderCodex, "codex"},
		{"codex wrapper", "github: {user: alice}\nagent: {provider: codex, executable: '/path with spaces/reviewer'}", ProviderCodex, "/path with spaces/reviewer"},
		{"legacy wrapper", "github: {user: alice}\nagent: {executable: '/path with spaces/claude-wrapper'}", ProviderClaude, "/path with spaces/claude-wrapper"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, err := Parse([]byte(test.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if c.Agent.Provider != test.provider || c.Agent.Executable != test.executable {
				t.Fatalf("agent: %+v", c.Agent)
			}
		})
	}
}

func TestRejectExplicitEmptyProviderOrExecutable(t *testing.T) {
	for _, field := range []string{
		"provider: ''", "provider: null", "provider: ~",
		"executable: ''", "executable: null", "executable: ~",
	} {
		if _, err := Parse([]byte("github: {user: alice}\nagent: {" + field + "}")); err == nil {
			t.Fatalf("accepted %s", field)
		}
	}
}

func TestAgentNormalizedForDirectRunnerCallers(t *testing.T) {
	legacy, err := (Agent{Executable: "/custom/claude", Timeout: time.Minute}).Normalized()
	if err != nil || legacy.Provider != ProviderClaude || legacy.Executable != "/custom/claude" {
		t.Fatalf("legacy: %+v %v", legacy, err)
	}
	codex, err := (Agent{Provider: ProviderCodex, Timeout: time.Minute}).Normalized()
	if err != nil || codex.Executable != ProviderCodex {
		t.Fatalf("codex: %+v %v", codex, err)
	}
	for _, agent := range []Agent{
		{Provider: "other", Timeout: time.Minute},
		{Provider: ProviderClaude},
		{Provider: ProviderClaude, Executable: "\n", Timeout: time.Minute},
	} {
		if _, err := agent.Normalized(); err == nil {
			t.Fatalf("accepted %+v", agent)
		}
	}
}
