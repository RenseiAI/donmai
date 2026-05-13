package claude

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// DefaultBinary is the executable name probed on $PATH at construction.
// Override via Options.Binary for tests or non-standard installs.
const DefaultBinary = "claude"

// Options configure a Provider. The zero value is valid and probes the
// system $PATH for the `claude` binary.
type Options struct {
	// Binary names the claude CLI executable to invoke. When empty,
	// DefaultBinary is used. Tests inject a fake-CLI script path here
	// to drive deterministic JSONL fixtures.
	Binary string

	// LookPath overrides the binary-resolution function. Defaults to
	// exec.LookPath. Tests inject a fake to assert probe behavior
	// without touching the host's PATH.
	LookPath func(name string) (string, error)
}

// Provider is the v0.5.0 agent.Provider implementation that shells out
// to the Claude Code CLI.
//
// Constructed via New, which probes for the `claude` binary on PATH
// and returns agent.ErrProviderUnavailable if missing. Once
// constructed, the Provider is safe for concurrent use; each Spawn
// returns an independent Handle backed by its own subprocess.
type Provider struct {
	binary string
}

// New constructs a Provider after probing for the `claude` binary on
// $PATH (or at the configured Options.Binary path).
//
// Returns a non-nil error wrapping agent.ErrProviderUnavailable when
// the binary is not on PATH; the runner is expected to short-circuit
// and surface the failure before any worktree provisioning runs.
//
// Per F.1.1 §3.1: fail-fast at construction is the contract.
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
			"%w: claude CLI %q not on PATH (install: https://docs.claude.com/en/docs/agents/claude-code/cli or `npm i -g @anthropic-ai/claude-code`): %v",
			agent.ErrProviderUnavailable, binary, err,
		)
	}

	return &Provider{binary: resolved}, nil
}

// Name returns ProviderClaude. Stable for the lifetime of the Provider.
func (*Provider) Name() agent.ProviderName { return agent.ProviderClaude }

// Capabilities returns the v0.5.0 capability matrix.
//
// Per F.1.1 §3.1 and the F.2.3-cap-flip coordinator decision (REN-1455):
//
//   - SupportsMessageInjection=true. Implemented by spawning a fresh
//     `claude --resume <session-id> -p <text>` subprocess between turns
//     and forwarding its JSONL stream to the parent Handle's events
//     channel. This is between-turn injection — same semantic level as
//     the legacy TS Agent SDK and the future Go-native option C upgrade
//     in REN-1451 (which replaces the subprocess shell-out with the
//     Anthropic Go SDK + a Go-native agent loop for true mid-turn
//     injection without subprocess overhead).
//   - SupportsSessionResume=false. The Provider.Resume entrypoint is
//     wired but not exercised by the v0.5.0 runner; flips to true when
//     the resume code path lands (also tracked under REN-1451).
//
// All other flags follow the legacy claude-provider.ts capability
// table verbatim, except SupportsCodeIntelligenceEnforcement which is
// gated on the canUseTool callback the CLI does not yet expose.
func (*Provider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		SupportsMessageInjection:            true,  // between-turn injection via --resume
		SupportsSessionResume:               false, // v0.5.0 runner limitation
		SupportsToolPlugins:                 true,
		NeedsBaseInstructions:               false,
		NeedsPermissionConfig:               false,
		SupportsCodeIntelligenceEnforcement: false, // v0.5.0; flips in F.5 wrapper
		EmitsSubagentEvents:                 true,
		SupportsReasoningEffort:             true,
		ToolPermissionFormat:                "claude",
		// Tool-use surface (002 v2): both wired through the CLI.
		// Spec.AllowedTools → --allowedTools; Spec.MCPServers →
		// --mcp-config <tmpfile>. See cli_args.go and mcp.go.
		AcceptsAllowedToolsList: true,
		AcceptsMcpServerSpec:    true,
		HumanLabel:              "Claude",
	}
}

// Spawn starts a new Claude session.
//
// Steps (per F.1.1 §3.1):
//
//  1. Write Spec.MCPServers to a per-session JSON tmpfile.
//  2. Translate Spec into CLI args via buildArgs.
//  3. exec.CommandContext the resolved claude binary with stream-json
//     output. Stdin receives the prompt; stdout is parsed as JSONL.
//  4. Return a Handle whose Events channel emits agent.Event values
//     mapped from each JSONL line.
//
// On any pre-spawn failure (tmpfile write, exec start) the provider
// returns an error wrapping agent.ErrSpawnFailed and cleans up any
// half-allocated resources.
func (p *Provider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	return p.spawn(ctx, spec, "")
}

// SpawnBinary is the exported entry point for providers that share the
// Claude Code JSONL execution machinery but supply a different binary
// and their own pre-built argv.
//
// Amp uses this: amp's --stream-json mode emits Claude Code-compatible
// JSONL, so the same Handle + JSONL mapper work with the `amp` binary;
// only the argv differs (amp uses -x --stream-json instead of
// -p --output-format stream-json --verbose).
//
// Parameters:
//   - ctx: cancellation context for the subprocess lifetime.
//   - binary: absolute path to the CLI executable (pre-validated by the
//     caller via exec.LookPath).
//   - argv: the pre-built argument slice (NOT including the binary
//     itself). The caller is responsible for all flag translation.
//   - stdinPrompt: text written to the subprocess's stdin before close.
//   - mcpConfigPath: absolute path to an MCP config tmpfile written by
//     the caller, or "" if no MCP servers are configured. The Handle's
//     Stop() method removes this file.
//   - cwd: working directory for the subprocess; sets cmd.Dir when
//     non-empty.
//   - env: spec.Env passed to composeEnv (merged onto os.Environ()).
//   - onProcessSpawned: optional PID callback fired after cmd.Start().
//
// On any pre-spawn failure (pipe creation, exec start) the function
// returns an error wrapping agent.ErrSpawnFailed.
func SpawnBinary(
	ctx context.Context,
	binary string,
	argv []string,
	stdinPrompt string,
	mcpConfigPath string,
	cwd string,
	env map[string]string,
	onProcessSpawned func(pid int),
) (*Handle, error) {
	return spawnRaw(ctx, binary, argv, stdinPrompt, mcpConfigPath, cwd, env, onProcessSpawned)
}

// Resume returns agent.ErrUnsupported on v0.5.0 per the locked
// capability matrix. When the runner gains resume support this method
// will dispatch to spawn(ctx, spec, sessionID).
func (*Provider) Resume(_ context.Context, _ string, _ agent.Spec) (agent.Handle, error) {
	return nil, fmt.Errorf("provider/claude: Resume: %w (SupportsSessionResume=false in v0.5.0; tracked in REN-1451)", agent.ErrUnsupported)
}

// Shutdown is a no-op for the CLI shell-out provider. Each session is
// backed by its own subprocess that terminates with the Handle, so
// the provider holds no long-lived resources to release.
func (*Provider) Shutdown(_ context.Context) error { return nil }
