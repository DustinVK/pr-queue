package runner

import (
	"fmt"

	"github.com/DustinVK/pr-queue/internal/config"
	"github.com/DustinVK/pr-queue/internal/findings"
)

type InvocationPaths struct {
	Output  string
	Diff    string
	Scratch string
	Schema  string
}

type ProviderInvocation struct {
	Args       []string
	Prompt     string
	SchemaPath string
}

func BuildInvocation(provider, runID string, input findings.Input, comparison findings.Comparison, paths InvocationPaths) (ProviderInvocation, error) {
	prompt := ProviderPrompt(provider, input, comparison.BaseSHA, paths.Output, paths.Diff, paths.Scratch)
	switch provider {
	case config.ProviderClaude:
		return ProviderInvocation{Args: []string{"-p", "--verbose", "--output-format", "stream-json", "--no-session-persistence", "--session-id", runID, "--dangerously-skip-permissions"}, Prompt: prompt}, nil
	case config.ProviderCodex:
		if paths.Output == "" || paths.Diff == "" || paths.Scratch == "" || paths.Schema == "" {
			return ProviderInvocation{}, fmt.Errorf("codex invocation requires output, diff, scratch, and schema paths")
		}
		return ProviderInvocation{
			Args:   []string{"--ask-for-approval", "never", "exec", "--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=false", "-c", "sandbox_workspace_write.writable_roots=[]", "--add-dir", paths.Scratch, "--ephemeral", "--color", "never", "--json", "--output-schema", paths.Schema, "--output-last-message", paths.Output, "-"},
			Prompt: prompt, SchemaPath: paths.Schema,
		}, nil
	default:
		return ProviderInvocation{}, fmt.Errorf("unsupported agent provider %q", provider)
	}
}
