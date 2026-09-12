package findings

import (
	"bytes"
	"strings"
	"testing"
)

const patchFixture = `@@ -1,4 +1,5 @@
 alpha
-old
+new
+newer
 omega
 tail
@@ -10 +11 @@
-x
+y
`

func TestDiffAnchors(t *testing.T) {
	d := Diff{}
	d.AddPatch("file.go", patchFixture)
	for _, tc := range []struct {
		name, side string
		line       int
		startSide  string
		startLine  int
		valid      bool
	}{
		{"added", "RIGHT", 2, "", 0, true},
		{"deleted", "LEFT", 2, "", 0, true},
		{"context", "RIGHT", 4, "", 0, true},
		{"context left", "LEFT", 1, "", 0, false},
		{"outside", "RIGHT", 6, "", 0, false},
		{"zero", "RIGHT", 0, "", 0, false},
		{"negative", "RIGHT", -1, "", 0, false},
		{"range", "RIGHT", 3, "RIGHT", 2, true},
		{"mixed range", "RIGHT", 3, "LEFT", 2, true},
		{"reversed", "RIGHT", 2, "RIGHT", 3, false},
		{"same line", "RIGHT", 2, "RIGHT", 2, false},
		{"cross hunk", "RIGHT", 11, "RIGHT", 2, false},
		{"range gap", "RIGHT", 3, "RIGHT", 99, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := sampleDocument().Findings[0]
			f.Side = Ptr(tc.side)
			f.Line = Ptr(tc.line)
			if tc.startSide != "" {
				f.StartSide = Ptr(tc.startSide)
				f.StartLine = Ptr(tc.startLine)
			}
			if err := d.Validate(f); (err == nil) != tc.valid {
				t.Fatalf("valid=%t: %v", tc.valid, err)
			}
		})
	}
}

func TestInvalidAnchorDoesNotRejectDocument(t *testing.T) {
	doc := sampleDocument()
	doc.Findings[0].Line = Ptr(10000)
	decoded, err := Decode(bytes.NewReader(encoded(t, doc)), sampleInput())
	if err != nil {
		t.Fatal(err)
	}
	diff := Diff{}
	diff.AddPatch("file.go", patchFixture)
	if len(decoded.Findings) != 2 {
		t.Fatal("finding was dropped")
	}
	if err := diff.Validate(decoded.Findings[0]); err == nil {
		t.Fatal("invalid anchor must have a block reason")
	}
	if err := diff.Validate(decoded.Findings[1]); err != nil {
		t.Fatal("general finding unexpectedly blocked", err)
	}
}

func TestUnverifiableDiffs(t *testing.T) {
	for _, patch := range []string{"", "Binary files differ", "@@ -1,4 +1,4 @@\n one\n", "@@ -1 +1 @@\n a\n+extra\n", "@@ -0 +1 @@\n-no\n+yes\n", "@@@ -1 -1 +1 @@@\n text\n"} {
		d := Diff{}
		d.AddPatch("file.go", patch)
		if d.Files["file.go"].Error == "" {
			t.Fatalf("accepted malformed patch %q", patch)
		}
		if err := d.Validate(sampleDocument().Findings[0]); err == nil {
			t.Fatal("unverifiable anchor accepted")
		}
	}
}

func TestParseGitDiffFilesAndRenames(t *testing.T) {
	text := `diff --git a/old name.go b/new name.go
similarity index 50%
rename from old name.go
rename to new name.go
--- a/old name.go
+++ b/new name.go
@@ -1 +1 @@
-old
+new
diff --git a/gone.go b/gone.go
deleted file mode 100644
--- a/gone.go
+++ /dev/null
@@ -1 +0,0 @@
-gone
\ No newline at end of file
diff --git a/added.go b/added.go
new file mode 100644
--- /dev/null
+++ b/added.go
@@ -0,0 +1 @@
+added
diff --git a/image.bin b/image.bin
Binary files a/image.bin and b/image.bin differ
diff --git "a/caf\303\251.go" "b/caf\303\251.go"
--- "a/caf\303\251.go"
+++ "b/caf\303\251.go"
@@ -1 +1 @@
-old
+new
`
	d, err := ParseDiff(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != 5 {
		t.Fatalf("files: %+v", d.Files)
	}
	for _, path := range []string{"new name.go", "gone.go", "added.go", "café.go"} {
		f := sampleDocument().Findings[0]
		f.Path = Ptr(path)
		f.Line = Ptr(1)
		if path == "gone.go" {
			f.Side = Ptr("LEFT")
		}
		if err := d.Validate(f); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	if d.Files["image.bin"].Error == "" {
		t.Fatal("binary patch considered anchorable")
	}
	if _, ok := d.Files["old name.go"]; ok {
		t.Fatal("rename uses old path")
	}
	for _, bad := range []string{"diff --cc file.go\n", "unexpected prose\n", text + text} {
		if _, err := ParseDiff(bad); err == nil {
			t.Fatal("invalid full diff accepted")
		}
	}
}

func TestTruncatedLaterHunkBlocksEarlierAnchor(t *testing.T) {
	d := Diff{}
	d.AddPatch("file.go", strings.TrimSuffix(patchFixture, "+y\n"))
	if err := d.Validate(sampleDocument().Findings[0]); err == nil {
		t.Fatal("partial patch cannot validate even an earlier hunk")
	}
}

func TestOverlappingHunksCannotValidateAmbiguousLines(t *testing.T) {
	d := Diff{}
	d.AddPatch("file.go", patchFixture+"@@ -1 +1 @@\n-a\n+b\n")
	if err := d.Validate(sampleDocument().Findings[0]); err == nil {
		t.Fatal("overlapping hunks considered valid")
	}
}
