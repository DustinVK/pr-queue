// Package github confines GitHub transport to explicit gh API calls.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/findings"
)

type Response struct {
	Stdout []byte
	Stderr []byte
	Err    error
}
type Executor func(context.Context, []string, []byte) Response
type Client struct {
	Exec    Executor
	Timeout time.Duration
}

func New() *Client { return &Client{Exec: Execute, Timeout: 2 * time.Minute} }

type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buf.Len() {
		return 0, fmt.Errorf("GitHub response exceeds %d bytes", b.limit)
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }

func Execute(ctx context.Context, args []string, input []byte) Response {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = bytes.NewReader(input)
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "GH_DEBUG=") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
	var out = limitedBuffer{limit: 64 * 1024 * 1024}
	var errOut = limitedBuffer{limit: 1024 * 1024}
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	return Response{Stdout: out.Bytes(), Stderr: errOut.Bytes(), Err: err}
}

func (c *Client) get(ctx context.Context, endpoint string, paginate bool) ([]byte, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	args := []string{"api", "--hostname", "github.com", "--method", "GET", "--header", "Accept: application/vnd.github+json", "--header", "X-GitHub-Api-Version: 2026-03-10"}
	if paginate {
		args = append(args, "--paginate", "--slurp")
	}
	args = append(args, endpoint)
	execute := c.Exec
	if execute == nil {
		execute = Execute
	}
	r := execute(ctx, args, nil)
	if r.Err != nil {
		return nil, fmt.Errorf("GitHub GET %s: %w: %s", endpoint, r.Err, strings.TrimSpace(string(r.Stderr)))
	}
	return r.Stdout, nil
}

func decode(data []byte, dest any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(dest); err != nil {
		return fmt.Errorf("decode GitHub response: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("unexpected trailing GitHub response")
	}
	return nil
}

func (c *Client) Identity(ctx context.Context, expected string) (string, error) {
	data, err := c.get(ctx, "user", false)
	if err != nil {
		return "", err
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := decode(data, &user); err != nil {
		return "", err
	}
	if !config.ValidLogin(user.Login) {
		return "", fmt.Errorf("GitHub returned an invalid authenticated user")
	}
	if !strings.EqualFold(user.Login, expected) {
		return "", fmt.Errorf("authenticated GitHub user %q does not match configured user %q", user.Login, expected)
	}
	return user.Login, nil
}

type PR struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	findings.Comparison
	Author             string   `json:"author"`
	BaseBranch         string   `json:"base_branch"`
	RequestedReviewers []string `json:"requested_reviewers"`
	ChangedFiles       *int     `json:"changed_files,omitempty"`
}

type apiPR struct {
	Number   int     `json:"number"`
	State    string  `json:"state"`
	Draft    *bool   `json:"draft"`
	Merged   bool    `json:"merged"`
	MergedAt *string `json:"merged_at"`
	Head     struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"base"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	RequestedReviewers []struct {
		Login string `json:"login"`
	} `json:"requested_reviewers"`
	ChangedFiles *int `json:"changed_files"`
}

func (p apiPR) value(repo string) (PR, error) {
	r := PR{Repo: repo, Number: p.Number, Comparison: findings.Comparison{HeadSHA: p.Head.SHA, BaseSHA: p.Base.SHA, State: p.State}, Author: p.User.Login, BaseBranch: p.Base.Ref, ChangedFiles: p.ChangedFiles, RequestedReviewers: []string{}}
	if p.Draft == nil || p.Number < 1 || r.Author == "" || r.BaseBranch == "" {
		return r, fmt.Errorf("incomplete GitHub PR observation")
	}
	r.Draft = *p.Draft
	if p.Merged || p.MergedAt != nil {
		r.State = "merged"
	}
	if err := r.Comparison.Validate(); err != nil {
		return r, err
	}
	for _, u := range p.RequestedReviewers {
		r.RequestedReviewers = append(r.RequestedReviewers, u.Login)
	}
	return r, nil
}

func (c *Client) ListOpen(ctx context.Context, repo string) ([]PR, error) {
	if err := config.ValidateRepo(repo); err != nil {
		return nil, err
	}
	data, err := c.get(ctx, "repos/"+repo+"/pulls?state=open&per_page=100", true)
	if err != nil {
		return nil, err
	}
	var pages [][]apiPR
	if err := decode(data, &pages); err != nil {
		return nil, err
	}
	if pages == nil || len(pages) == 0 {
		return nil, fmt.Errorf("missing paginated PR response")
	}
	result := []PR{}
	seen := map[int]bool{}
	for _, page := range pages {
		if page == nil {
			return nil, fmt.Errorf("invalid PR page")
		}
		for _, raw := range page {
			p, err := raw.value(repo)
			if err != nil {
				return nil, err
			}
			if seen[p.Number] {
				return nil, fmt.Errorf("duplicate PR %d across pages; retry observation", p.Number)
			}
			seen[p.Number] = true
			result = append(result, p)
		}
	}
	return result, nil
}

func (c *Client) FetchPR(ctx context.Context, repo string, number int) (PR, error) {
	if err := config.ValidateRepo(repo); err != nil {
		return PR{}, err
	}
	if number < 1 {
		return PR{}, fmt.Errorf("PR number must be positive")
	}
	data, err := c.get(ctx, fmt.Sprintf("repos/%s/pulls/%d", repo, number), false)
	if err != nil {
		return PR{}, err
	}
	var raw apiPR
	if err := decode(data, &raw); err != nil {
		return PR{}, err
	}
	p, err := raw.value(repo)
	if err == nil && p.Number != number {
		err = fmt.Errorf("GitHub returned PR %d instead of %d", p.Number, number)
	}
	return p, err
}

func (c *Client) FetchDiff(ctx context.Context, p PR) (findings.Diff, error) {
	d := findings.Diff{}
	if err := config.ValidateRepo(p.Repo); err != nil {
		return d, err
	}
	if p.Number < 1 {
		return d, fmt.Errorf("PR number must be positive")
	}
	if p.ChangedFiles == nil || *p.ChangedFiles < 0 {
		return d, fmt.Errorf("fetch full PR metadata before reading its diff")
	}
	data, err := c.get(ctx, fmt.Sprintf("repos/%s/pulls/%d/files?per_page=100", p.Repo, p.Number), true)
	if err != nil {
		return d, err
	}
	var pages [][]struct {
		Filename  string  `json:"filename"`
		Patch     *string `json:"patch"`
		Additions *int    `json:"additions"`
		Deletions *int    `json:"deletions"`
	}
	if err := decode(data, &pages); err != nil {
		return d, err
	}
	if len(pages) == 0 {
		return d, fmt.Errorf("missing paginated files response")
	}
	seen := map[string]bool{}
	for _, page := range pages {
		if page == nil {
			return d, fmt.Errorf("invalid files page")
		}
		for _, file := range page {
			if file.Filename == "" || seen[file.Filename] {
				return d, fmt.Errorf("missing or duplicate diff path")
			}
			seen[file.Filename] = true
			patch := ""
			if file.Patch != nil {
				patch = *file.Patch
			}
			d.AddPatch(file.Filename, patch)
			parsed := d.Files[file.Filename]
			if parsed.Error == "" {
				added, deleted := 0, 0
				for _, hunk := range parsed.Hunks {
					for _, line := range hunk.Lines {
						if line.Kind == '+' {
							added++
						}
						if line.Kind == '-' {
							deleted++
						}
					}
				}
				if file.Additions == nil || file.Deletions == nil || *file.Additions != added || *file.Deletions != deleted {
					parsed.Error = "patch change counts do not match complete file metadata"
				}
			}
		}
	}
	if len(seen) != *p.ChangedFiles {
		return d, fmt.Errorf("incomplete PR files: received %d of %d", len(seen), *p.ChangedFiles)
	}
	return d, nil
}
