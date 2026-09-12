// Package findings implements the versioned agent contract and diff validation.
// It has no dependency on other prqueue packages.
package findings

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	MaxOutputBytes = 4 * 1024 * 1024
	MaxFindings    = 100
	MaxBodyBytes   = 60000
)

type Input struct {
	Repo    string `json:"repo"`
	PR      int    `json:"pr"`
	HeadSHA string `json:"head_sha"`
}

type Comparison struct {
	HeadSHA string `json:"head_sha"`
	BaseSHA string `json:"base_sha"`
	Draft   bool   `json:"draft"`
	State   string `json:"state"`
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (c Comparison) Validate() error {
	if !fullSHA.MatchString(c.HeadSHA) || !fullSHA.MatchString(c.BaseSHA) {
		return fmt.Errorf("comparison requires full lowercase commit SHAs")
	}
	if !slices.Contains([]string{"open", "closed", "merged"}, c.State) {
		return fmt.Errorf("invalid PR state %q", c.State)
	}
	return nil
}

func (c Comparison) Key() string { return compactJSON([]any{c.HeadSHA, c.BaseSHA, c.Draft, c.State}) }

type Anchor struct {
	Path      *string `json:"path,omitempty"`
	Side      *string `json:"side,omitempty"`
	StartLine *int    `json:"start_line,omitempty"`
	StartSide *string `json:"start_side,omitempty"`
	Line      *int    `json:"line,omitempty"`
}

func (a Anchor) Empty() bool {
	return a.Path == nil && a.Side == nil && a.Line == nil && a.StartLine == nil && a.StartSide == nil
}
func (a Anchor) Equal(b Anchor) bool {
	return equal(a.Path, b.Path) && equal(a.Side, b.Side) && equal(a.Line, b.Line) && equal(a.StartLine, b.StartLine) && equal(a.StartSide, b.StartSide)
}
func equal[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
func Ptr[T any](v T) *T { return &v }

type Finding struct {
	Kind string `json:"kind"`
	Anchor
	Severity  string  `json:"severity,omitempty"`
	Category  string  `json:"category,omitempty"`
	Title     string  `json:"title,omitempty"`
	Body      string  `json:"body"`
	Rationale *string `json:"rationale,omitempty"`
}

func (f Finding) Fingerprint() string {
	sum := sha256.Sum256([]byte(compactJSON([]any{f.Kind, f.Path, f.Side, strings.ReplaceAll(f.Body, "\r\n", "\n")})))
	return hex.EncodeToString(sum[:])
}

func compactJSON(value any) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(value); err != nil {
		panic(err)
	} // Only fixed, JSON-safe shapes above.
	return strings.TrimSuffix(b.String(), "\n")
}

type Document struct {
	SchemaVersion int       `json:"schema_version"`
	Repo          string    `json:"repo"`
	PR            int       `json:"pr"`
	HeadSHA       string    `json:"head_sha"`
	Summary       string    `json:"summary"`
	Verdict       string    `json:"verdict"`
	Findings      []Finding `json:"findings"`
}

func (d Document) QueueItems() []Finding {
	items := make([]Finding, 0, len(d.Findings)+1)
	items = append(items, Finding{Kind: "summary", Body: d.Summary})
	return append(items, d.Findings...)
}

func DecodeFile(path string, input Input) (Document, error) {
	f, err := os.Open(path)
	if err != nil {
		return Document{}, err
	}
	defer f.Close()
	return Decode(f, input)
}

func Decode(r io.Reader, input Input) (Document, error) {
	var d Document
	data, err := io.ReadAll(io.LimitReader(r, MaxOutputBytes+1))
	if err != nil {
		return d, err
	}
	if len(data) > MaxOutputBytes {
		return d, fmt.Errorf("findings output exceeds %d bytes", MaxOutputBytes)
	}
	if !utf8.Valid(data) {
		return d, fmt.Errorf("findings output is not valid UTF-8")
	}
	if err := checkJSON(data); err != nil {
		return d, err
	}
	top, err := object(data, []string{"schema_version", "repo", "pr", "head_sha", "summary", "verdict", "findings"}, nil)
	if err != nil {
		return d, err
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(top["findings"], &rawItems); err != nil {
		return d, fmt.Errorf("findings must be an array: %w", err)
	}
	if len(rawItems) > MaxFindings {
		return d, fmt.Errorf("output exceeds %d findings", MaxFindings)
	}
	for i, raw := range rawItems {
		if _, err := object(raw, []string{"kind", "severity", "category", "title", "body"}, []string{"path", "side", "line", "start_line", "start_side", "rationale"}); err != nil {
			return d, fmt.Errorf("finding %d: %w", i+1, err)
		}
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return d, fmt.Errorf("decode findings: %w", err)
	}
	if d.SchemaVersion != 1 {
		return d, fmt.Errorf("unsupported schema_version %d", d.SchemaVersion)
	}
	if input.PR <= 0 || !fullSHA.MatchString(input.HeadSHA) {
		return d, fmt.Errorf("runner supplied invalid PR input")
	}
	if d.Repo != input.Repo || d.PR != input.PR || d.HeadSHA != input.HeadSHA {
		return d, fmt.Errorf("output repo/pr/head_sha does not exactly match runner input")
	}
	if !slices.Contains([]string{"comment", "approve", "request_changes"}, d.Verdict) {
		return d, fmt.Errorf("invalid verdict %q", d.Verdict)
	}
	if err := ValidateBody(d.Summary); err != nil {
		return d, fmt.Errorf("summary: %w", err)
	}
	for i, f := range d.Findings {
		if f.Kind != "inline" && f.Kind != "general" {
			return d, fmt.Errorf("finding %d: agent findings must be inline or general", i+1)
		}
		if err := f.Validate(); err != nil {
			return d, fmt.Errorf("finding %d: %w", i+1, err)
		}
	}
	return d, nil
}

func ValidateBody(body string) error {
	if !utf8.ValidString(body) {
		return fmt.Errorf("body is not valid UTF-8")
	}
	if len(body) > MaxBodyBytes {
		return fmt.Errorf("body exceeds %d bytes", MaxBodyBytes)
	}
	return nil
}

// Validate checks shape, independent of whether the anchor lands in a diff.
func (f Finding) Validate() error {
	if err := ValidateBody(f.Body); err != nil {
		return err
	}
	if f.Kind == "summary" {
		if !f.Anchor.Empty() {
			return fmt.Errorf("summary cannot have an anchor")
		}
		return nil
	}
	if f.Kind != "inline" && f.Kind != "general" {
		return fmt.Errorf("invalid kind %q", f.Kind)
	}
	if !slices.Contains([]string{"nit", "minor", "major", "critical"}, f.Severity) {
		return fmt.Errorf("invalid severity %q", f.Severity)
	}
	if !slices.Contains([]string{"correctness", "security", "performance", "style", "test-coverage", "design"}, f.Category) {
		return fmt.Errorf("invalid category %q", f.Category)
	}
	if f.Kind == "general" {
		if !f.Anchor.Empty() {
			return fmt.Errorf("general finding cannot have an anchor")
		}
		return nil
	}
	if f.Path == nil || *f.Path == "" || f.Side == nil || f.Line == nil {
		return fmt.Errorf("inline finding requires path, side, and line")
	}
	if !validSide(*f.Side) {
		return fmt.Errorf("invalid side %q", *f.Side)
	}
	if (f.StartLine == nil) != (f.StartSide == nil) {
		return fmt.Errorf("start_line and start_side must appear together")
	}
	if f.StartSide != nil && !validSide(*f.StartSide) {
		return fmt.Errorf("invalid start_side %q", *f.StartSide)
	}
	return nil
}

func validSide(side string) bool { return side == "LEFT" || side == "RIGHT" }

func object(data []byte, required, optional []string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("expected JSON object")
	}
	for _, key := range required {
		if _, ok := obj[key]; !ok {
			return nil, fmt.Errorf("missing field %q", key)
		}
	}
	for key, value := range obj {
		if !slices.Contains(required, key) && !slices.Contains(optional, key) {
			return nil, fmt.Errorf("unknown field %q", key)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("field %q cannot be null", key)
		}
	}
	return obj, nil
}

// encoding/json otherwise accepts duplicate keys and silently uses the last one.
func checkJSON(data []byte) error {
	if err := checkUnicodeEscapes(data); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := jsonValue(d, 0); err != nil {
		return fmt.Errorf("invalid findings JSON: %w", err)
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("findings must contain exactly one JSON document")
	}
	return nil
}

// Go's JSON decoder replaces unpaired UTF-16 surrogates with U+FFFD. Reject
// them so the text accepted into the approval queue is never silently repaired.
func checkUnicodeEscapes(data []byte) error {
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) || data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return fmt.Errorf("incomplete Unicode escape")
		}
		v, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			return fmt.Errorf("invalid Unicode escape")
		}
		i += 4
		if v >= 0xd800 && v <= 0xdbff {
			if i+6 >= len(data) || string(data[i+1:i+3]) != `\u` {
				return fmt.Errorf("unpaired Unicode surrogate")
			}
			low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return fmt.Errorf("unpaired Unicode surrogate")
			}
			i += 6
		} else if v >= 0xdc00 && v <= 0xdfff {
			return fmt.Errorf("unpaired Unicode surrogate")
		}
	}
	return nil
}

func jsonValue(d *json.Decoder, depth int) error {
	if depth > 32 {
		return fmt.Errorf("JSON nesting too deep")
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return fmt.Errorf("invalid object key")
			}
			if keys[name] {
				return fmt.Errorf("duplicate key %q", name)
			}
			keys[name] = true
			if err := jsonValue(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := jsonValue(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected delimiter %q", delim)
	}
	_, err = d.Token()
	return err
}
