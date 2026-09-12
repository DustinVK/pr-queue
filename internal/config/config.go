// Package config loads the local, non-secret configuration.
package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	GitHub GitHub `yaml:"github" json:"github"`
	Agent  Agent  `yaml:"agent" json:"agent"`
	Repos  []Repo `yaml:"repos" json:"repos"`
}

type GitHub struct {
	User string `yaml:"user" json:"user"`
}
type Agent struct {
	Executable         string        `yaml:"executable" json:"executable"`
	Timeout            time.Duration `yaml:"timeout" json:"timeout"`
	MaxParallelReviews int           `yaml:"max_parallel_reviews" json:"max_parallel_reviews"`
}
type Repo struct {
	Name    string  `yaml:"name" json:"name"`
	Filters Filters `yaml:"filters" json:"filters"`
}
type Filters struct {
	Authors           []string `yaml:"authors" json:"authors"`
	RequestedReviewer *string  `yaml:"requested_reviewer" json:"requested_reviewer"`
	BaseBranches      []string `yaml:"base_branches" json:"base_branches"`
}

const DefaultYAML = `github:
  user: your-login
agent:
  executable: claude
  timeout: 15m
  max_parallel_reviews: 1
# Add repositories, for example: [{name: owner/name}]
repos: []
`

func Defaults() Config {
	return Config{Agent: Agent{Executable: "claude", Timeout: 15 * time.Minute, MaxParallelReviews: 1}, Repos: []Repo{}}
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config (run prq init first): %w", err)
	}
	return Parse(data)
}

func Parse(data []byte) (Config, error) {
	c := Defaults()
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("config must contain exactly one YAML document")
	}
	return c, c.Validate()
}

var loginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func ValidLogin(s string) bool { return loginPattern.MatchString(s) && !strings.Contains(s, "--") }
func validActor(s string) bool { return ValidLogin(strings.TrimSuffix(s, "[bot]")) }

func ValidateRepo(s string) error {
	parts := strings.Split(s, "/")
	if len(parts) != 2 || !ValidLogin(parts[0]) || !repoPattern.MatchString(parts[1]) || parts[1] == "." || parts[1] == ".." {
		return fmt.Errorf("invalid repository %q: use owner/name", s)
	}
	return nil
}

func validBranch(s string) bool {
	if s == "" || s == "@" || strings.HasPrefix(s, "-") || strings.HasSuffix(s, ".") {
		return false
	}
	for _, bad := range []string{"..", "//", "@{", " ", "~", "^", ":", "?", "*", "[", "\\"} {
		if strings.Contains(s, bad) {
			return false
		}
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	for _, r := range s {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}

func (c Config) Validate() error {
	if !ValidLogin(c.GitHub.User) {
		return fmt.Errorf("github.user must be a GitHub login")
	}
	if strings.TrimSpace(c.Agent.Executable) == "" || strings.ContainsAny(c.Agent.Executable, "\x00\r\n") {
		return fmt.Errorf("agent.executable must name an executable")
	}
	if c.Agent.Timeout <= 0 {
		return fmt.Errorf("agent.timeout must be positive")
	}
	if c.Agent.MaxParallelReviews < 1 {
		return fmt.Errorf("agent.max_parallel_reviews must be positive")
	}
	seen := map[string]bool{}
	for _, repo := range c.Repos {
		if err := ValidateRepo(repo.Name); err != nil {
			return err
		}
		key := strings.ToLower(repo.Name)
		if seen[key] {
			return fmt.Errorf("duplicate repository %q", repo.Name)
		}
		seen[key] = true
		for _, a := range repo.Filters.Authors {
			if !validActor(a) {
				return fmt.Errorf("invalid author %q", a)
			}
		}
		if r := repo.Filters.RequestedReviewer; r != nil && !validActor(*r) {
			return fmt.Errorf("invalid requested reviewer %q", *r)
		}
		for _, b := range repo.Filters.BaseBranches {
			if !validBranch(b) {
				return fmt.Errorf("invalid base branch %q", b)
			}
		}
	}
	return nil
}

type Paths struct {
	Config   string `json:"config"`
	State    string `json:"state"`
	Database string `json:"database"`
	Consent  string `json:"consent"`
}

func ForHome(home string) Paths {
	state := filepath.Join(home, ".local", "state", "prqueue")
	return Paths{Config: filepath.Join(home, ".config", "prqueue", "config.yaml"), State: state, Database: filepath.Join(state, "queue.db"), Consent: filepath.Join(state, "agent-consent-v1")}
}

func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	return ForHome(home), nil
}

const ConsentText = "trusted-local-agent-v1\n"

func (p Paths) Consented() (bool, error) {
	data, err := os.ReadFile(p.Consent)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if string(data) != ConsentText {
		return false, fmt.Errorf("invalid agent acknowledgment file: %s", p.Consent)
	}
	return true, nil
}
