package runner

import (
	"fmt"
	"os"

	"github.com/DustinVK/pr-queue/internal/findings"
)

// DecodeProviderOutput validates the provider's authoritative final file. It
// deliberately does not inspect or repair diagnostic event streams.
func DecodeProviderOutput(path string, input findings.Input) (findings.Document, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return findings.Document{}, fmt.Errorf("agent did not write findings.json: %w", err)
	}
	if !info.Mode().IsRegular() {
		return findings.Document{}, fmt.Errorf("findings output must be a regular file")
	}
	if err := os.Chmod(path, 0600); err != nil {
		return findings.Document{}, err
	}
	return findings.DecodeFile(path, input)
}
