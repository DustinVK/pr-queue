package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConsentFileRequiresExactAcknowledgment(t *testing.T) {
	for _, tc := range []struct {
		name string
		body *string
		want bool
		bad  bool
	}{
		{"missing", nil, false, false},
		{"exact", new(ConsentText), true, false},
		{"empty", new(""), false, true},
		{"wrong", new("yes\n"), false, true},
		{"truncated", new("trusted-local-agent"), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := ForHome(t.TempDir())
			if tc.body != nil {
				if err := os.MkdirAll(filepath.Dir(p.Consent), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p.Consent, []byte(*tc.body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := p.Consented()
			if got != tc.want || (err != nil) != tc.bad {
				t.Fatalf("consent = %v, %v", got, err)
			}
		})
	}
}

func TestConsentReadFailure(t *testing.T) {
	p := ForHome(t.TempDir())
	if err := os.MkdirAll(p.Consent, 0700); err != nil {
		t.Fatal(err)
	}
	if consented, err := p.Consented(); consented || err == nil {
		t.Fatalf("directory accepted as consent: %v, %v", consented, err)
	}
}
