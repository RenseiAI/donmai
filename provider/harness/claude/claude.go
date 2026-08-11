package claude

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/clijsonl"
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
// Per F.1.1 §3.1 and the F.2.3-cap-flip coordinator decision:
//
//   - SupportsMessageInjection=true. Implemented by spawning a fresh
//     `claude --resume <session-id> -p <text>` subprocess between turns
//     and forwarding its JSONL stream to the parent Handle's events
//     channel. This is between-turn injection — same semantic level as
//     the legacy TS Agent SDK and the future Go-native option C upgrade
//     (which replaces the subprocess shell-out with the
//     Anthropic Go SDK + a Go-native agent loop for true mid-turn
//     injection without subprocess overhead).
//   - SupportsSessionResume=false. The Provider.Resume entrypoint is
//     wired but not exercised by the v0.5.0 runner; flips to true when
//     the resume code path lands.
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
	var err error
	spec, err = agent.PrepareHarness(spec, p.Manifest())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	// Project the resolved model-endpoint binding (when set) onto the spec
	// BEFORE the interactive/headless split below, so both spawn modes see
	// the identical routing knobs: serving-host env vars (direct/bedrock/
	// vertex per endpoint.go), binding credentials, and Endpoint.Model
	// overriding Spec.Model. This used to run only inside the headless
	// spawn() path (see below) — AFTER the interactive branch had already
	// forked off with the raw, unprojected spec — so a resolved endpoint
	// binding was silently dropped on the interactive PTY floor. Interactive
	// endpoint projection, sibling of the model-projection fix (#323): #323
	// taught the interactive REPL to read Spec.Model, but the endpoint's own
	// model override (and every other binding-derived knob) never reached it
	// because applyEndpoint itself never ran ahead of spawnInteractive.
	spec, err = applyEndpoint(spec)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	// Interactive spawn mode (W4): capability-gated on the live manifest, not
	// a static branch — a future edit that flips SupportsInteractivePTY back
	// to false makes this a silent no-op fallthrough to the headless path
	// rather than a crash. See interactive.go.
	if spec.Interactive != nil && p.Manifest().Caps.SupportsInteractivePTY {
		return p.spawnInteractive(ctx, spec)
	}
	return p.spawn(ctx, spec, "")
}

// spawn is the internal Provider.Spawn implementation shared with
// (the future) Resume. It writes the MCP config tmpfile, builds the
// CLI args, and starts the subprocess via the shared clijsonl driver,
// returning a fully-wired Handle.
//
// The endpoint-binding projection (applyEndpoint) already ran in Spawn,
// before the interactive/headless split — spec arrives here with it applied.
//
// On any failure prior to the subprocess being started, the driver
// cleans up the MCP tmpfile and returns an error wrapping
// agent.ErrSpawnFailed.
func (p *Provider) spawn(ctx context.Context, spec agent.Spec, resumeSessionID string) (agent.Handle, error) {
	mcpPath, err := clijsonl.WriteMCPConfig(spec.MCPServers)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}

	argv, stdinPrompt := buildArgs(spec, mcpPath, resumeSessionID)
	return clijsonl.SpawnBinary(ctx, p.binary, argv, stdinPrompt, mcpPath, spec.Cwd, spec.Env, spec.OnProcessSpawned)
}

// Resume returns agent.ErrUnsupported on v0.5.0 per the locked
// capability matrix. When the runner gains resume support this method
// will dispatch to spawn(ctx, spec, sessionID).
func (*Provider) Resume(_ context.Context, _ string, _ agent.Spec) (agent.Handle, error) {
	return nil, fmt.Errorf("provider/claude: Resume: %w (SupportsSessionResume=false in v0.5.0)", agent.ErrUnsupported)
}

// Shutdown is a no-op for the CLI shell-out provider. Each session is
// backed by its own subprocess that terminates with the Handle, so
// the provider holds no long-lived resources to release.
func (*Provider) Shutdown(_ context.Context) error { return nil }
