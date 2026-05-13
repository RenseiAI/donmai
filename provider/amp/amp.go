package amp

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/provider/claude"
)

// DefaultBinary is the executable name probed on $PATH at construction.
const DefaultBinary = "amp"

// EnvAPIKey is the environment variable NAME probed at construction.
// Amp's hosted endpoint authenticates with a personal access token; the
// probe path checks for this variable so operators who have not yet
// logged in see a clear error rather than a mysterious exec failure.
const EnvAPIKey = "AMP_API_KEY" //nolint:gosec // G101: env-var name, not a credential

// Options configures Provider construction. The zero value is valid
// and probes both the AMP_API_KEY environment variable and the `amp`
// binary on $PATH.
type Options struct {
	// Binary names the amp CLI executable to invoke. When empty,
	// DefaultBinary is used. Tests inject a fake-CLI script path here
	// to drive deterministic JSONL fixtures.
	Binary string

	// APIKey is the Amp personal access token. When empty the
	// constructor falls back to os.Getenv(EnvAPIKey). Tests inject
	// a fixed value here to bypass the environment.
	APIKey string

	// Getenv overrides the environment lookup. Defaults to
	// os.Getenv. Tests inject a fake to drive the probe-failure
	// branch without touching the real process environment.
	Getenv func(string) string

	// LookPath overrides the binary-resolution function. Defaults to
	// exec.LookPath. Tests inject a fake to assert probe behavior
	// without touching the host's PATH.
	LookPath func(name string) (string, error)
}

// Provider is the agent.Provider implementation for Sourcegraph's Amp.
// Spawn execs the `amp` binary in headless execute mode (amp -x) with
// --stream-json, which emits Claude Code-compatible JSONL that the
// claude package's Handle machinery reads without modification.
type Provider struct {
	binary string
	apiKey string
}

// New constructs a Provider after probing for the AMP_API_KEY env var
// AND the `amp` binary on PATH.
//
// Returns an error wrapping agent.ErrProviderUnavailable when:
//   - AMP_API_KEY is not set (operator has not authenticated with Amp)
//   - the `amp` binary is not found on PATH
//
// Per F.1.1 §3.1: fail-fast at construction is the contract.
func New(opts Options) (*Provider, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = defaultGetenv
	}
	lookup := opts.LookPath
	if lookup == nil {
		lookup = exec.LookPath
	}

	key := opts.APIKey
	if key == "" {
		key = getenv(EnvAPIKey)
	}
	if key == "" {
		return nil, fmt.Errorf(
			"%w: amp provider needs %s in env (Amp personal access token; run `amp login`)",
			agent.ErrProviderUnavailable, EnvAPIKey,
		)
	}

	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	resolved, err := lookup(binary)
	if err != nil {
		return nil, fmt.Errorf(
			"%w: amp CLI %q not on PATH (install: https://ampcode.com/download or `npm i -g @sourcegraph/amp`): %v",
			agent.ErrProviderUnavailable, binary, err,
		)
	}

	return &Provider{binary: resolved, apiKey: key}, nil
}

// Name returns ProviderAmp. Stable for the lifetime of the Provider.
func (*Provider) Name() agent.ProviderName { return agent.ProviderAmp }

// Capabilities returns the v1.0.0 capability matrix for the Amp runner.
//
// Amp emits Claude Code-compatible JSONL via --stream-json so the same
// Handle and event decoder work unchanged.
// SupportsMessageInjection is false in this release — resume would
// require `amp threads continue <threadId> -x --stream-json`, which
// needs the session's thread-id captured from the system.init event
// and is deferred to a follow-up.
func (*Provider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		SupportsMessageInjection:            false, // future: amp threads continue <threadId> -x --stream-json
		SupportsSessionResume:               false, // future: same mechanism
		SupportsToolPlugins:                 false, // amp manages its own tools via settings.json
		NeedsBaseInstructions:               false,
		NeedsPermissionConfig:               false,
		SupportsCodeIntelligenceEnforcement: false,
		EmitsSubagentEvents:                 false, // amp does not emit claude Task-tool subagent events
		SupportsReasoningEffort:             false, // amp uses --mode (deep/smart/rush/free) not a numeric effort
		ToolPermissionFormat:                "claude",
		// AcceptsAllowedToolsList: amp does not expose --allowedTools;
		// permission control is via --dangerously-allow-all and the
		// amp settings.json amp.permissions array.
		AcceptsAllowedToolsList: false,
		// AcceptsMcpServerSpec: amp accepts --mcp-config with the same
		// JSON shape as claude, so we wire Spec.MCPServers through.
		AcceptsMcpServerSpec: true,
		HumanLabel:           "Amp",
	}
}

// Spawn starts a new Amp session.
//
// Amp is invoked in headless execute mode:
//
//	amp -x --stream-json --no-ide --no-notifications
//	    [--dangerously-allow-all]   (when Autonomous)
//	    [--mcp-config <tmpfile>]    (when MCPServers non-empty)
//	    [--model <id>]              (when Model non-empty)
//
// The prompt is delivered via stdin. Amp's --stream-json mode emits
// Claude Code-compatible JSONL (same system.init / assistant / result
// event shape), so the claude package's Handle and JSONL mapper are
// reused via claude.SpawnBinary — no parsing code is duplicated.
//
// On any pre-spawn failure (tmpfile write, exec start) the provider
// returns an error wrapping agent.ErrSpawnFailed.
func (p *Provider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	// Write the per-session MCP config tmpfile (same format as claude).
	mcpPath, err := claude.WriteMCPConfig(spec.MCPServers)
	if err != nil {
		return nil, fmt.Errorf("%w: amp: write MCP config: %v", agent.ErrSpawnFailed, err)
	}

	argv := buildAmpArgs(spec, mcpPath)
	h, err := claude.SpawnBinary(
		ctx,
		p.binary,
		argv,
		spec.Prompt, // stdinPrompt
		mcpPath,
		spec.Cwd,
		spec.Env,
		spec.OnProcessSpawned,
	)
	if err != nil {
		return nil, fmt.Errorf("provider/amp: %w", err)
	}
	return h, nil
}

// buildAmpArgs translates an agent.Spec into the argv array passed to
// the amp CLI in headless execute mode.
//
// Amp execute mode:
//
//	amp -x --stream-json --no-ide --no-notifications
//	    [--dangerously-allow-all]
//	    [--mcp-config <file>]
//	    [--model <id>]
//
// Amp flag mapping vs claude:
//
//	claude: -p                    → amp: -x (execute; reads prompt from stdin)
//	claude: --output-format …     → amp: --stream-json
//	claude: --verbose             → (not needed; --stream-json implies it)
//	claude: --dangerously-skip-permissions → amp: --dangerously-allow-all
//	claude: --mcp-config          → amp: --mcp-config (same JSON format)
//	claude: --model               → amp: --model (same)
//	claude: --add-dir             → (not available in amp)
//	claude: --allowedTools        → (not available; amp uses settings.json)
//	claude: --disallowedTools     → (not available)
//	claude: --max-turns           → (not available)
//	claude: --effort              → (not available; amp uses --mode)
//	claude: --append-system-prompt→ (not available)
//
// Prompt is delivered via stdin (same as claude -p reads from stdin).
// The -x flag without an inline message argument causes amp to read
// the prompt from stdin, matching the claude pattern.
func buildAmpArgs(spec agent.Spec, mcpConfigPath string) []string {
	argv := []string{
		"-x",
		"--stream-json",
		"--no-ide",
		"--no-notifications",
	}

	if spec.Autonomous {
		argv = append(argv, "--dangerously-allow-all")
	}

	if mcpConfigPath != "" {
		argv = append(argv, "--mcp-config", mcpConfigPath)
	}

	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}

	return argv
}

// Resume always fails with agent.ErrUnsupported.
// SupportsSessionResume is false — future work will wire
// `amp threads continue <threadId> -x --stream-json`.
func (*Provider) Resume(_ context.Context, _ string, _ agent.Spec) (agent.Handle, error) {
	return nil, fmt.Errorf("provider/amp: Resume: %w (SupportsSessionResume=false; future: amp threads continue)", agent.ErrUnsupported)
}

// Shutdown is a no-op. There are no long-lived resources.
func (*Provider) Shutdown(_ context.Context) error { return nil }
