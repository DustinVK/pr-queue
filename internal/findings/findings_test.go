package findings

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func sampleInput() Input { return Input{Repo: "owner/repo", PR: 42, HeadSHA: strings.Repeat("a", 40)} }
func sampleDocument() Document {
	i := sampleInput()
	return Document{SchemaVersion: 1, Repo: i.Repo, PR: i.PR, HeadSHA: i.HeadSHA, Summary: "Summary", Verdict: "comment", Findings: []Finding{
		{Kind: "inline", Anchor: Anchor{Path: Ptr("file.go"), Side: Ptr("RIGHT"), Line: Ptr(2)}, Severity: "major", Category: "correctness", Title: "A bug", Body: "Exact **body**.\r\nNext line.", Rationale: Ptr("Private reasoning")},
		{Kind: "general", Severity: "minor", Category: "test-coverage", Title: "Test it", Body: "A general finding."},
	}}
}
func encoded(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecodeAndIndependentSummary(t *testing.T) {
	d, err := Decode(bytes.NewReader(encoded(t, sampleDocument())), sampleInput())
	if err != nil {
		t.Fatal(err)
	}
	items := d.QueueItems()
	if len(items) != 3 || items[0].Kind != "summary" || items[0].Body != "Summary" {
		t.Fatalf("queue: %+v", items)
	}
	if items[1].Body != sampleDocument().Findings[0].Body {
		t.Fatal("body bytes changed")
	}
	if !items[0].Anchor.Empty() || items[0].Rationale != nil {
		t.Fatal("summary inherited private metadata")
	}
}

func TestContractFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"wrong repo", func(m map[string]any) { m["repo"] = "another/repo" }},
		{"wrong case", func(m map[string]any) { m["repo"] = "Owner/Repo" }},
		{"wrong PR", func(m map[string]any) { m["pr"] = 43 }},
		{"wrong head", func(m map[string]any) { m["head_sha"] = strings.Repeat("b", 40) }},
		{"schema bump", func(m map[string]any) { m["schema_version"] = 2 }},
		{"missing summary", func(m map[string]any) { delete(m, "summary") }},
		{"null summary", func(m map[string]any) { m["summary"] = nil }},
		{"null findings", func(m map[string]any) { m["findings"] = nil }},
		{"unknown top field", func(m map[string]any) { m["suggestion"] = "bad" }},
		{"verdict", func(m map[string]any) { m["verdict"] = "ship-it" }},
		{"severity", func(m map[string]any) { first(m)["severity"] = "high" }},
		{"category", func(m map[string]any) { first(m)["category"] = "quality" }},
		{"kind", func(m map[string]any) { first(m)["kind"] = "file" }},
		{"agent summary kind", func(m map[string]any) {
			f := first(m)
			f["kind"] = "summary"
			delete(f, "path")
			delete(f, "side")
			delete(f, "line")
		}},
		{"missing body", func(m map[string]any) { delete(first(m), "body") }},
		{"null body", func(m map[string]any) { first(m)["body"] = nil }},
		{"case alias", func(m map[string]any) { first(m)["Body"] = "surprise" }},
		{"suggestion", func(m map[string]any) { first(m)["suggestion"] = "unapproved" }},
		{"null rationale", func(m map[string]any) { first(m)["rationale"] = nil }},
		{"missing inline line", func(m map[string]any) { delete(first(m), "line") }},
		{"string line", func(m map[string]any) { first(m)["line"] = "2" }},
		{"null line", func(m map[string]any) { first(m)["line"] = nil }},
		{"invalid side", func(m map[string]any) { first(m)["side"] = "right" }},
		{"unpaired range", func(m map[string]any) { first(m)["start_line"] = 1 }},
		{"general anchor", func(m map[string]any) { first(m)["kind"] = "general" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal(encoded(t, sampleDocument()), &m); err != nil {
				t.Fatal(err)
			}
			tc.mutate(m)
			if _, err := Decode(bytes.NewReader(encoded(t, m)), sampleInput()); err == nil {
				t.Fatal("invalid output accepted")
			}
		})
	}
}
func first(m map[string]any) map[string]any { return m["findings"].([]any)[0].(map[string]any) }

func TestJSONAmbiguityAndTruncation(t *testing.T) {
	base := string(encoded(t, sampleDocument()))
	for _, bad := range []string{
		base[:len(base)-1], base + base, "```json\n" + base + "\n```",
		strings.Replace(base, `"repo":"owner/repo"`, `"repo":"bad/repo","repo":"owner/repo"`, 1),
		strings.Replace(base, `"line":2`, `"line":9,"line":2`, 1),
		strings.Replace(base, "Summary", string([]byte{0xff}), 1),
	} {
		if _, err := Decode(strings.NewReader(bad), sampleInput()); err == nil {
			t.Fatalf("accepted ambiguous JSON %q", bad)
		}
	}
}

func TestLimitsAreBytesAndNeverTruncate(t *testing.T) {
	d := sampleDocument()
	d.Findings[0].Body = strings.Repeat("é", MaxBodyBytes/2)
	d.Summary = strings.Repeat("s", MaxBodyBytes)
	if _, err := Decode(bytes.NewReader(encoded(t, d)), sampleInput()); err != nil {
		t.Fatal(err)
	}
	d.Findings[0].Body += "é"
	if _, err := Decode(bytes.NewReader(encoded(t, d)), sampleInput()); err == nil {
		t.Fatal("oversized UTF-8 body accepted")
	}
	d = sampleDocument()
	d.Summary = strings.Repeat("s", MaxBodyBytes+1)
	if _, err := Decode(bytes.NewReader(encoded(t, d)), sampleInput()); err == nil {
		t.Fatal("oversized summary accepted")
	}
	d = sampleDocument()
	f := d.Findings[1]
	d.Findings = nil
	for range MaxFindings {
		d.Findings = append(d.Findings, f)
	}
	if got, err := Decode(bytes.NewReader(encoded(t, d)), sampleInput()); err != nil || len(got.QueueItems()) != MaxFindings+1 {
		t.Fatalf("100 plus summary: %v", err)
	}
	d.Findings = append(d.Findings, f)
	if _, err := Decode(bytes.NewReader(encoded(t, d)), sampleInput()); err == nil {
		t.Fatal("101 findings accepted")
	}
	base := encoded(t, sampleDocument())
	output := append(base, bytes.Repeat([]byte(" "), MaxOutputBytes-len(base))...)
	if _, err := Decode(bytes.NewReader(output), sampleInput()); err != nil {
		t.Fatal(err)
	}
	output = append(output, ' ')
	if _, err := Decode(bytes.NewReader(output), sampleInput()); err == nil {
		t.Fatal("oversized file accepted")
	}
}

func TestFingerprintCanonicalInputs(t *testing.T) {
	f := sampleDocument().Findings[0]
	f.Body = "A\r\nB  \n"
	want := sha256.Sum256([]byte(`["inline","file.go","RIGHT","A\nB  \n"]`))
	if f.Fingerprint() != hex.EncodeToString(want[:]) {
		t.Fatalf("fingerprint %s", f.Fingerprint())
	}
	moved := f
	moved.Line = Ptr(55)
	moved.StartLine = Ptr(53)
	moved.StartSide = Ptr("RIGHT")
	moved.Title = "Changed title"
	moved.Body = "A\nB  \n"
	if moved.Fingerprint() != f.Fingerprint() {
		t.Fatal("line numbers, title, or CRLF changed fingerprint")
	}
	moved.Body = "A\nB\n"
	if moved.Fingerprint() == f.Fingerprint() {
		t.Fatal("Markdown whitespace was normalized")
	}
	general := Finding{Kind: "general", Body: "<body>"}
	want = sha256.Sum256([]byte(`["general",null,null,"<body>"]`))
	if general.Fingerprint() != hex.EncodeToString(want[:]) {
		t.Fatal("absent anchor fields or HTML escaping differ")
	}
}

func TestComparisonAndFullAnchor(t *testing.T) {
	c := Comparison{HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), Draft: false, State: "open"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	want := `["` + c.HeadSHA + `","` + c.BaseSHA + `",false,"open"]`
	if c.Key() != want {
		t.Fatalf("key %s", c.Key())
	}
	for _, changed := range []Comparison{
		{HeadSHA: c.HeadSHA, BaseSHA: strings.Repeat("c", 40), State: "open"},
		{HeadSHA: c.HeadSHA, BaseSHA: c.BaseSHA, Draft: true, State: "open"},
		{HeadSHA: c.HeadSHA, BaseSHA: c.BaseSHA, State: "closed"},
	} {
		if changed.Key() == c.Key() {
			t.Fatal("comparison transition omitted")
		}
	}
	c.HeadSHA = "abcdef"
	if c.Validate() == nil {
		t.Fatal("abbreviated SHA accepted")
	}
	a := sampleDocument().Findings[0].Anchor
	b := a
	b.Line = Ptr(*a.Line + 1)
	if a.Equal(b) {
		t.Fatal("changed complete anchor considered equal")
	}
}

func TestUnicodeEscapesMustNotSilentlyChangeBodies(t *testing.T) {
	base := string(encoded(t, sampleDocument()))
	for _, escaped := range []string{`\ud800`, `\udc00`, `\ud800\u0041`} {
		data := strings.Replace(base, `"Summary"`, `"`+escaped+`"`, 1)
		if _, err := Decode(strings.NewReader(data), sampleInput()); err == nil {
			t.Fatalf("accepted unpaired surrogate %s", escaped)
		}
	}
	for _, escaped := range []string{`\ud83d\ude00`, `\\ud800`} {
		data := strings.Replace(base, `"Summary"`, `"`+escaped+`"`, 1)
		if _, err := Decode(strings.NewReader(data), sampleInput()); err != nil {
			t.Fatalf("valid escape %s: %v", escaped, err)
		}
	}
}
