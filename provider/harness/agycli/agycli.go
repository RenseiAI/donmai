package agycli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// ProviderName is the stable identifier for the agy-cli provider.
// It is distinct from agent.ProviderGemini ("gemini", the API-direct provider);
// agy-cli wraps the Antigravity `agy` CLI (pty, single-shot).
const ProviderName agent.ProviderName = "agy-cli"

// DefaultBinary is the executable name probed on $PATH at construction.
// Override via Options.Binary for tests or non-standard installs.
const DefaultBinary = "agy"

const agyHelpTimeout = 5 * time.Second

// Options configure a Provider. The zero value is valid: it probes $PATH
// for `agy`, injects the result envelope, and enables transcript enrichment.
type Options struct {
	// Binary names the agy CLI executable to invoke. When empty,
	// DefaultBinary is used. Tests inject a fake-CLI script path here.
	Binary string

	// LookPath overrides the binary-resolution function. Defaults to
	// exec.LookPath. Tests inject a fake to assert probe behavior.
	LookPath func(name string) (string, error)

	// VersionProbe overrides the "<binary> --version" probe used for the
	// API-key-mode compatibility check. Defaults to defaultVersionProbe.
	// Tests inject a fake to assert enforcement without a real binary.
	VersionProbe versionProbeFunc

	// HelpProbe overrides the "<binary> --help" probe used for the
	// --add-dir and API-key-mode compatibility checks. Defaults to
	// runAgyHelp. Tests inject a fake to assert enforcement without a
	// real binary.
	HelpProbe agyHelpRunner

	// InjectResultEnvelope, when true (the default for the zero value via
	// New), appends a delimited-JSON result-envelope instruction to the
	// prompt so the plain-text stdout carries a machine-parseable final
	// result. Set DisableResultEnvelope to turn it off.
	DisableResultEnvelope bool

	// DisableTranscriptEnrichment turns off the best-effort reader of
	// agy's on-disk transcript.jsonl. When true the provider relies solely
	// on the stdout spine (no ToolUse/ToolResult events).
	DisableTranscriptEnrichment bool

	// TrustWorkspace, when true, adds spec.Cwd to agy's GLOBAL
	// trustedWorkspaces list before spawn (best-effort). It defaults OFF:
	// the probe showed `-p` runs (including tool calls) succeed in an UNtrusted cwd
	// under --dangerously-skip-permissions, so writing shared settings is an
	// unnecessary side-effect (and accumulates ephemeral worktree paths).
	// On a keyed spawn the provider writes trust into the session's own
	// isolated HOME instead (keymode.go), so this flag only ever affects
	// the host-session path. Enable only if a future agy version gates
	// tool/edit ops on workspace trust even with
	// --dangerously-skip-permissions.
	TrustWorkspace bool

	// StateHome overrides the agy state directory root (default
	// ~/.gemini). Tests point this at a fixture tree; production leaves it
	// empty to resolve the real host path. The host-session transcript
	// reader and trust writer resolve their paths under
	// StateHome/antigravity-cli. Keyed spawns never read or write here —
	// they run under a per-session HOME (keymode.go).
	StateHome string
}

// Provider is the agent.Provider implementation that shells out to the
// Antigravity `agy` CLI under a pty.
//
// Constructed via New, which probes for the `agy` binary on PATH and returns
// agent.ErrProviderUnavailable if missing. Once constructed, the Provider is
// safe for concurrent use; each Spawn returns an independent Handle backed by
// its own pty-attached subprocess.
//
// AUTH MODES (selected per spawn, never mixed):
//
//   - Keyed (API-key mode): when spec.Env carries GEMINI_API_KEY, the child
//     runs under a per-session HOME holding settings.json
//     {"modelProvider":"gemini"} (keymode.go), so agy bills the session key
//     and nothing is written to the operator's real ~/.gemini.
//   - Host-session: otherwise the `agy` binary uses its own host login and
//     the provider injects no key. NOT suitable for cloud sandboxes.
type Provider struct {
	binary           string
	injectEnvelope   bool
	enrichTranscript bool
	trustWorkspace   bool
	stateHome        string
	versionProbe     versionProbeFunc
	helpProbe        agyHelpRunner
}

// New constructs a Provider after probing for the `agy` binary on $PATH (or
// at Options.Binary). Returns an error wrapping agent.ErrProviderUnavailable
// when the binary is not on PATH so the registry warns-and-skips.
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
			"%w: agy CLI %q not on PATH (install Antigravity: https://antigravity.google ; this provider needs an agy binary that supports API-key mode): %v",
			agent.ErrProviderUnavailable, binary, err,
		)
	}
	versionProbe := opts.VersionProbe
	if versionProbe == nil {
		versionProbe = defaultVersionProbe
	}
	helpProbe := opts.HelpProbe
	if helpProbe == nil {
		helpProbe = runAgyHelp
	}
	if err := verifyAddDirSupport(resolved, helpProbe); err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrProviderUnavailable, err)
	}
	if err := verifyAPIKeyModeSupport(resolved, versionProbe, helpProbe); err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrProviderUnavailable, err)
	}

	return &Provider{
		binary:           resolved,
		injectEnvelope:   !opts.DisableResultEnvelope,
		enrichTranscript: !opts.DisableTranscriptEnrichment,
		trustWorkspace:   opts.TrustWorkspace,
		stateHome:        opts.StateHome,
		versionProbe:     versionProbe,
		helpProbe:        helpProbe,
	}, nil
}

// agyHelpRunner is deliberately narrow so the compatibility check is easy to
// test without weakening the production constructor's fail-closed behavior.
type agyHelpRunner func(context.Context, string) ([]byte, error)

func runAgyHelp(ctx context.Context, binary string) ([]byte, error) {
	//nolint:gosec // binary was resolved with exec.LookPath before this probe.
	return exec.CommandContext(ctx, binary, "--help").CombinedOutput()
}

// verifyAddDirSupport rejects CLI builds that cannot bind a run to its
// requested worktree. cmd.Dir alone is insufficient because agy may create a
// scratch project and ignore the process directory for workspace tool calls.
func verifyAddDirSupport(binary string, help agyHelpRunner) error {
	ctx, cancel := context.WithTimeout(context.Background(), agyHelpTimeout)
	defer cancel()
	out, err := help(ctx, binary)
	if err != nil {
		return fmt.Errorf("agy CLI %q cannot verify required --add-dir support: %w", binary, err)
	}
	if !strings.Contains(string(out), "--add-dir") {
		return fmt.Errorf("agy CLI %q does not advertise required --add-dir worktree binding", binary)
	}
	return nil
}

// Name returns ProviderName ("agy-cli").
func (*Provider) Name() agent.ProviderName { return ProviderName }

// Capabilities returns the agy-cli capability matrix.
//
// Everything is conservative: single-shot, no MCP, no model flag, no usage.
// The provider's value is running the Antigravity `agy` CLI headless —
// keyed (API-key mode, billed to the session key) when spec.Env carries
// GEMINI_API_KEY, otherwise under the host's own agy login.
func (*Provider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		SupportsMessageInjection:            false, // single-shot -p
		SupportsSessionResume:               false, // --conversation resume deferred
		SupportsToolPlugins:                 false, // MCP wiring deferred (global-only mcpServers)
		NeedsBaseInstructions:               false,
		NeedsPermissionConfig:               false, // --dangerously-skip-permissions blanket-approves
		SupportsCodeIntelligenceEnforcement: false,
		EmitsSubagentEvents:                 false,
		SupportsReasoningEffort:             false, // no flag
		ToolPermissionFormat:                "",
		AcceptsAllowedToolsList:             false,
		AcceptsMcpServerSpec:                false, // see doc.go; deferred
		HumanLabel:                          "Antigravity (agy)",
	}
}

// Spawn starts a new `agy` session under a pty.
//
// AUTH: keyed when spec.Env carries GEMINI_API_KEY (the child runs under a
// per-session HOME holding the API-key-mode settings.json, keymode.go);
// host-session otherwise (the `agy` binary uses its own host login).
// spec.Env is merged onto the child environment (after AGENT_ENV_BLOCKLIST
// filtering by the runner); the session key rides the environment, never
// argv.
func (p *Provider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	if p.injectEnvelope {
		plan := agent.EnsurePromptPlan(spec)
		// The result envelope is provider-owned protocol, not a caller opt-out.
		// In particular, untrusted user task/amendment text can contain the
		// markers verbatim and must never suppress the provider's instruction.
		// Keep the stable typed ID: a caller attempting to supply that ID creates
		// a duplicate and is rejected by PromptPlan validation before spawn.
		plan.UserAmendments = append(plan.UserAmendments, agent.UserPromptAmendment{
			ID: "agy-result-envelope", Position: agent.UserPromptAppend, Order: 1000,
			Content: agent.PromptContent{ID: "agy-result-envelope-content", Text: resultEnvelopeInstruction, Required: true},
		})
		spec.PromptPlan = &plan
	}
	var err error
	spec, err = agent.PrepareHarness(spec, p.Manifest())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	return p.spawn(ctx, spec)
}

// Resume returns agent.ErrUnsupported. `agy --conversation <id>` resumes by id
// (better than the gemini CLI's indexes), but between-session continuation is
// deferred to a later wave; SupportsSessionResume=false.
func (*Provider) Resume(_ context.Context, _ string, _ agent.Spec) (agent.Handle, error) {
	return nil, fmt.Errorf("provider/agycli: Resume: %w (SupportsSessionResume=false; --conversation resume deferred)", agent.ErrUnsupported)
}

// Shutdown is a no-op. Each session is its own pty-attached subprocess that
// terminates with the Handle; the provider holds no long-lived resources.
// Per-session HOME trees are owned by their Handle's cleanup, not the
// provider.
func (*Provider) Shutdown(_ context.Context) error { return nil }
