package config

import (
	"strings"
	"testing"
)

func TestParseMalformedTrailingDocumentDiagnostic(t *testing.T) {
	_, err := Parse([]byte("github: {user: alice}\n---\nextra: [\n"))
	if err == nil || !strings.Contains(err.Error(), "decode config: yaml:") {
		t.Fatalf("missing YAML parser diagnostic: %v", err)
	}
}
