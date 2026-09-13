package findings

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type Diff struct{ Files map[string]*FileDiff }
type FileDiff struct {
	Path  string
	Patch string
	Hunks []Hunk
	Error string
}
type Hunk struct{ Lines []DiffLine }
type DiffLine struct {
	Old  int
	New  int
	Kind byte
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(?:.*)$`)

// AddPatch accepts the filename and per-file patch supplied by GitHub's files API.
// An empty/binary/truncated patch is retained with a validation error.
func (d *Diff) AddPatch(path, patch string) {
	if d.Files == nil {
		d.Files = map[string]*FileDiff{}
	}
	f := &FileDiff{Path: path, Patch: patch}
	d.Files[path] = f
	f.Hunks, f.Error = parseHunks(patch)
}

func parseHunks(patch string) ([]Hunk, string) {
	lines := strings.Split(patch, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var hunks []Hunk
	old, newLine, oldLeft, newLeft := 0, 0, 0, 0
	lastOldEnd, lastNewEnd := 0, 0
	for _, line := range lines {
		if m := hunkHeader.FindStringSubmatch(line); m != nil {
			if oldLeft != 0 || newLeft != 0 {
				return nil, "truncated diff hunk"
			}
			var err error
			old, err = strconv.Atoi(m[1])
			if err != nil {
				return nil, "invalid hunk line number"
			}
			newLine, err = strconv.Atoi(m[3])
			if err != nil {
				return nil, "invalid hunk line number"
			}
			oldLeft, newLeft = 1, 1
			if m[2] != "" {
				oldLeft, err = strconv.Atoi(m[2])
				if err != nil {
					return nil, "invalid hunk count"
				}
			}
			if m[4] != "" {
				newLeft, err = strconv.Atoi(m[4])
				if err != nil {
					return nil, "invalid hunk count"
				}
			}
			if (oldLeft > 0 && old < 1) || (newLeft > 0 && newLine < 1) {
				return nil, "invalid hunk start"
			}
			effectiveOld, effectiveNew := old, newLine
			const maxInt = int(^uint(0) >> 1)
			if old == maxInt || newLine == maxInt {
				return nil, "hunk line number overflow"
			}
			if oldLeft == 0 {
				effectiveOld++
			}
			if newLeft == 0 {
				effectiveNew++
			}
			if oldLeft > maxInt-effectiveOld || newLeft > maxInt-effectiveNew {
				return nil, "hunk count overflow"
			}
			if len(hunks) > 0 && (effectiveOld < lastOldEnd || effectiveNew < lastNewEnd) {
				return nil, "overlapping or unordered diff hunks"
			}
			lastOldEnd, lastNewEnd = effectiveOld+oldLeft, effectiveNew+newLeft
			hunks = append(hunks, Hunk{})
			continue
		}
		if line == `\ No newline at end of file` {
			continue
		}
		if len(hunks) == 0 || line == "" {
			return nil, "missing or invalid diff hunk"
		}
		entry := DiffLine{Kind: line[0]}
		switch line[0] {
		case ' ':
			entry.Old, entry.New = old, newLine
			old++
			newLine++
			oldLeft--
			newLeft--
		case '-':
			entry.Old = old
			old++
			oldLeft--
		case '+':
			entry.New = newLine
			newLine++
			newLeft--
		default:
			return nil, "invalid diff line"
		}
		if oldLeft < 0 || newLeft < 0 {
			return nil, "diff exceeds declared hunk length"
		}
		i := len(hunks) - 1
		hunks[i].Lines = append(hunks[i].Lines, entry)
	}
	if oldLeft != 0 || newLeft != 0 {
		return nil, "truncated diff hunk"
	}
	if len(hunks) == 0 {
		return nil, "diff has no text hunks (binary, empty, or unavailable patch)"
	}
	return hunks, ""
}

func (d Diff) Validate(f Finding) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if f.Kind != "inline" {
		return nil
	}
	file := d.Files[*f.Path]
	if file == nil {
		return fmt.Errorf("path %q is not in the current diff", *f.Path)
	}
	if file.Error != "" {
		return fmt.Errorf("cannot validate %q: %s", *f.Path, file.Error)
	}
	if *f.Line < 1 || (f.StartLine != nil && *f.StartLine < 1) {
		return fmt.Errorf("anchor line must be positive")
	}
	for _, h := range file.Hunks {
		end := h.position(*f.Side, *f.Line)
		if end < 0 {
			continue
		}
		if f.StartLine == nil {
			return nil
		}
		start := h.position(*f.StartSide, *f.StartLine)
		if start < 0 {
			return fmt.Errorf("range start and end must be in the same diff hunk")
		}
		if start >= end {
			return fmt.Errorf("multi-line range must end after its start")
		}
		return nil
	}
	return fmt.Errorf("%s line %d does not land inside the diff", *f.Side, *f.Line)
}

func (h Hunk) position(side string, line int) int {
	for i, p := range h.Lines {
		// GitHub uses RIGHT for context, LEFT for deletions.
		if side == "LEFT" && p.Kind == '-' && p.Old == line {
			return i
		}
		if side == "RIGHT" && p.Kind != '-' && p.New == line {
			return i
		}
	}
	return -1
}

// ParseDiff accepts ordinary two-way git patch output. Combined diffs and
// malformed file sections fail closed rather than guessing an anchor.
func ParseDiff(text string) (Diff, error) {
	d := Diff{Files: map[string]*FileDiff{}}
	var path, oldPath string
	var patch []string
	inFile, inPatch := false, false
	flush := func() error {
		if !inFile {
			return nil
		}
		if path == "" {
			path = oldPath
		}
		if path == "" {
			return fmt.Errorf("diff file has no identifiable path")
		}
		if _, exists := d.Files[path]; exists {
			return fmt.Errorf("duplicate diff path %q", path)
		}
		d.AddPatch(path, strings.Join(patch, "\n"))
		return nil
	}
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			if err := flush(); err != nil {
				return d, err
			}
			path, oldPath = "", ""
			patch = nil
			inFile, inPatch = true, false
			// Extended headers below disambiguate spaces and renames. The
			// header supplies the path for binary and mode-only changes.
			var err error
			oldPath, path, err = diffPaths(strings.TrimPrefix(line, "diff --git "))
			if err != nil {
				return d, err
			}
			continue
		}
		if strings.HasPrefix(line, "diff --cc ") || strings.HasPrefix(line, "diff --combined ") {
			return d, fmt.Errorf("combined diffs are unsupported")
		}
		if !inFile {
			if strings.TrimSpace(line) == "" {
				continue
			}
			return d, fmt.Errorf("expected git diff file header")
		}
		if strings.HasPrefix(line, "@@ ") {
			inPatch = true
		}
		if inPatch {
			patch = append(patch, line)
			continue
		}
		var err error
		switch {
		case strings.HasPrefix(line, "--- "):
			oldPath, err = patchPath(strings.TrimPrefix(line, "--- "), "a/")
		case strings.HasPrefix(line, "+++ "):
			path, err = patchPath(strings.TrimPrefix(line, "+++ "), "b/")
		case strings.HasPrefix(line, "rename to "):
			path, err = unquotePath(strings.TrimPrefix(line, "rename to "))
		case strings.HasPrefix(line, "copy to "):
			path, err = unquotePath(strings.TrimPrefix(line, "copy to "))
		}
		if err != nil {
			return d, err
		}
	}
	return d, flush()
}

func unquotePath(s string) (string, error) {
	if strings.HasPrefix(s, `"`) {
		return strconv.Unquote(s)
	}
	return s, nil
}

func patchPath(s, prefix string) (string, error) {
	// Git terminates unquoted ---/+++ paths containing spaces with a tab.
	// A tab inside the filename is quoted as \t, so retain that escaped byte.
	s, _, _ = strings.Cut(s, "\t")
	path, err := unquotePath(s)
	if err != nil {
		return "", err
	}
	if path == "/dev/null" {
		return "", nil
	}
	if !strings.HasPrefix(path, prefix) {
		return "", fmt.Errorf("invalid diff path %q", path)
	}
	return strings.TrimPrefix(path, prefix), nil
}

func diffPaths(s string) (string, string, error) {
	if strings.HasPrefix(s, `"`) {
		for i := 1; i < len(s); i++ {
			if s[i] == '\\' {
				i++
				continue
			}
			if s[i] == '"' && i+1 < len(s) && s[i+1] == ' ' {
				a, err := patchPath(s[:i+1], "a/")
				if err != nil {
					return "", "", err
				}
				b, err := patchPath(s[i+2:], "b/")
				return a, b, err
			}
		}
		return "", "", fmt.Errorf("invalid quoted diff header")
	}
	// Unquoted paths can themselves contain " b/". For unchanged names,
	// equal old/new halves identify the separator even without text headers
	// (binary and mode-only changes). Renames use their extended headers.
	if len(s) >= 5 && len(s)%2 == 1 && strings.HasPrefix(s, "a/") {
		sep := (len(s) - 1) / 2
		if s[sep:sep+3] == " b/" && s[2:sep] == s[sep+3:] {
			return s[2:sep], s[sep+3:], nil
		}
	}
	sep := strings.Index(s, " b/")
	if sep < 0 {
		sep = strings.Index(s, ` "b/`)
	}
	if sep < 0 {
		return "", "", fmt.Errorf("invalid diff header")
	}
	a, err := patchPath(s[:sep], "a/")
	if err != nil {
		return "", "", err
	}
	b, err := patchPath(s[sep+1:], "b/")
	return a, b, err
}
