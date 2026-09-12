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
	if c.Agent.Timeout != 15*time.Minute || c.Agent.Executable != "claude" || c.Agent.MaxParallelReviews != 1 {
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
