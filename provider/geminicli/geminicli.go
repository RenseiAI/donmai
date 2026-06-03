package geminicli

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/RenseiAI/donmai/agent"
)

// ProviderName is the stable identifier for the gemini-cli provider.
// It is distinct from agent.ProviderGemini ("gemini") which identifies
// the API-direct Go provider.
const ProviderName agent.ProviderName = "gemini-cli"

// DefaultBinary is the executable name probed on $PATH at construction.
// Override via Options.Binary for tests or non-standard installs.
const DefaultBinary = "gemini"

// Options configure a Provider. The zero value is valid and probes the
// system $PATH for the `gemini` binary.
type Options struct {
	// Binary names the gemini CLI executable to invoke. When empty,
	// DefaultBinary is used. Tests inject a fake-CLI script path here
	// to drive deterministic JSONL fixtures.
	Binary string

	// LookPath overrides the binary-resolution function. Defaults to
	// exec.LookPath. Tests inject a fake to assert probe behavior
	// without touching the host's PATH.
	LookPath func(name string) (string, error)
}

// Provider is the agent.Provider implementation that shells out to the
// Google Gemini CLI.
//
// Constructed via New, which probes for the `gemini` binary on PATH
// and returns agent.ErrProviderUnavailable if missing. Once
// constructed, the Provider is safe for concurrent use; each Spawn
// returns an independent Handle backed by its own subprocess.
//
// AUTH MODE: local/host-session only. The `gemini` binary must be
// installed on the host; this provider is NOT suitable for cloud
// sandboxes that don't have the CLI in their image.
type Provider struct {
	binary string
}

// New constructs a Provider after probing for the `gemini` binary on
// $PATH (or at the configured Options.Binary path).
//
// Returns a non-nil error wrapping agent.ErrProviderUnavailable when
// the binary is not on PATH. The runner is expected to short-circuit
// and log WARN before any worktree provisioning runs.
//
// Per the FEASIBILITY.md fail-fast contract: the binary must be present
// at construction time; if absent the worker skips this provider without
// surfacing a hard error to the caller.
func New(opts Options) (*Provider, error) {
	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	lookup := opts.LookPath
	if lookup == nil {
		lookup = exec.LookPath
	}

	resolved, err := lookup(binary)
	if err != nil {
		return nil, fmt.Errorf(
			"%w: gemini CLI %q not on PATH (install: https://github.com/google-gemini/gemini-cli or `brew install gemini-cli`): %v",
			agent.ErrProviderUnavailable, binary, err,
		)
	}

	return &Provider{binary: resolved}, nil
}

// Name returns ProviderName ("gemini-cli"). Stable for the lifetime of the Provider.
func (*Provider) Name() agent.ProviderName { return ProviderName }

// Capabilities returns the gemini-cli capability matrix.
//
// Key decisions (per FEASIBILITY.md):
//
//   - SupportsMessageInjection=false: gemini CLI --resume uses session
//     indexes (not UUIDs), making between-turn injection impractical for
//     the headless subprocess model. Each task is a single run.
//   - AcceptsMcpServerSpec=true: MCP HTTP servers are wired at Spawn time
//     via a per-session .gemini/settings.json written into the worktree.
//     This is the headline advantage of the CLI over the API-direct
//     provider (which has AcceptsMcpServerSpec=false).
//   - SupportsToolPlugins=true: the CLI has first-class MCP client support.
//   - SupportsReasoningEffort=false: the CLI's --thinking-budget is not
//     exposed as a headless flag in v0.44.x.
//   - EmitsSubagentEvents=false: the gemini CLI does not emit Anthropic-
//     style subagent progress events.
func (*Provider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		SupportsMessageInjection:            false, // --resume uses index, not UUID
		SupportsSessionResume:               false, // same constraint
		SupportsToolPlugins:                 true,  // native MCP client
		NeedsBaseInstructions:               false,
		NeedsPermissionConfig:               false,
		SupportsCodeIntelligenceEnforcement: false,
		EmitsSubagentEvents:                 false,
		SupportsReasoningEffort:             false, // no headless thinking-budget flag in v0.44
		ToolPermissionFormat:                "claude",
		AcceptsAllowedToolsList:             false, // --allowed-tools is deprecated in v0.44; policy engine only
		AcceptsMcpServerSpec:                true,  // via .gemini/settings.json
		HumanLabel:                          "Gemini CLI",
	}
}

// Spawn starts a new Gemini CLI session.
//
// Steps:
//
//  1. Write Spec.MCPServers to .gemini/settings.json in the worktree cwd.
//  2. Build argv: gemini -p --output-format stream-json --yolo --skip-trust [--model <id>].
//  3. exec.CommandContext the resolved gemini binary. Stdin receives the
//     prompt; stdout is parsed as JSONL.
//  4. Return a Handle whose Events channel emits agent.Event values
//     mapped from each JSONL line.
//
// AUTH: GEMINI_API_KEY must be present in spec.Env or os.Environ().
// The provider does not inject or validate the key — it relies on the
// daemon credential socket (platform dispatch) or the standalone host-export
// path (per AGENTS.md "Credentials in standalone mode").
func (p *Provider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	return p.spawn(ctx, spec)
}

// Resume returns agent.ErrUnsupported. The gemini CLI's --resume uses
// session indexes rather than UUIDs, making programmatic between-session
// continuation impractical in the subprocess model. If resume support is
// added in a future CLI version, this method will dispatch to spawn with
// a --resume flag.
func (*Provider) Resume(_ context.Context, _ string, _ agent.Spec) (agent.Handle, error) {
	return nil, fmt.Errorf("provider/geminicli: Resume: %w (SupportsSessionResume=false; gemini CLI --resume uses indexes, not UUIDs)", agent.ErrUnsupported)
}

// Shutdown is a no-op. Each session is backed by its own subprocess that
// terminates with the Handle, so the provider holds no long-lived resources.
func (*Provider) Shutdown(_ context.Context) error { return nil }
