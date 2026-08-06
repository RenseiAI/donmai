package agycli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// ProviderName is the stable identifier for the agy-cli provider.
// It is distinct from agent.ProviderGemini ("gemini", the API-direct provider);
// agy-cli wraps the Antigravity `agy` CLI (OAuth/local, pty, no key).
const ProviderName agent.ProviderName = "agy-cli"

// DefaultBinary is the executable name probed on $PATH at construction.
// Override via Options.Binary for tests or non-standard installs.
const DefaultBinary = "agy"

// Options configure a Provider. The zero value is valid: it probes $PATH
// for `agy`, injects the result envelope, and enables transcript enrichment.
type Options struct {
	// Binary names the agy CLI executable to invoke. When empty,
	// DefaultBinary is used. Tests inject a fake-CLI script path here.
	Binary string

	// LookPath overrides the binary-resolution function. Defaults to
	// exec.LookPath. Tests inject a fake to assert probe behavior.
	LookPath func(name string) (string, error)

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
	// trustedWorkspaces list before spawn (best-effort). It defaults OFF: the
	// probe showed `-p` runs (including tool calls) succeed in an UNtrusted cwd
	// under --dangerously-skip-permissions, so writing the host's shared
	// settings.json is an unnecessary side-effect (and accumulates ephemeral
	// worktree paths). Enable only if a future agy version gates tool/edit ops
	// on workspace trust even with --dangerously-skip-permissions.
	TrustWorkspace bool

	// StateHome overrides the agy state directory root (default
	// ~/.gemini). Tests point this at a fixture tree; production leaves it
	// empty to resolve the real host path. The transcript reader and trust
	// writer resolve their paths under StateHome/antigravity-cli.
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
// AUTH MODE: local / host-session / OAuth only. The `agy` binary must be
// installed AND logged in on the host. NOT suitable for cloud sandboxes.
type Provider struct {
	binary           string
	injectEnvelope   bool
	enrichTranscript bool
	trustWorkspace   bool
	stateHome        string
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
			"%w: agy CLI %q not on PATH (install Antigravity: https://antigravity.google ; this provider needs an OAuth-logged-in agy): %v",
			agent.ErrProviderUnavailable, binary, err,
		)
	}

	return &Provider{
		binary:           resolved,
		injectEnvelope:   !opts.DisableResultEnvelope,
		enrichTranscript: !opts.DisableTranscriptEnrichment,
		trustWorkspace:   opts.TrustWorkspace,
		stateHome:        opts.StateHome,
	}, nil
}

// Name returns ProviderName ("agy-cli").
func (*Provider) Name() agent.ProviderName { return ProviderName }

// Capabilities returns the agy-cli capability matrix (CONTRACT.md §5).
//
// Everything is conservative: single-shot, no MCP, no model flag, no usage.
// The provider's value is running the user's OWN OAuth-authed agy locally with
// no API key — not feature parity with the structured gemini provider.
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
// AUTH: none injected. The `agy` binary uses its own host OAuth. spec.Env is
// merged onto the environment (after AGENT_ENV_BLOCKLIST filtering by the
// runner) but no *_API_KEY is required.
func (p *Provider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	if p.injectEnvelope {
		plan := agent.EnsurePromptPlan(spec)
		if !planContainsResultEnvelope(plan) {
			plan.UserAmendments = append(plan.UserAmendments, agent.UserPromptAmendment{
				ID: "agy-result-envelope", Position: agent.UserPromptAppend, Order: 1000,
				Content: agent.PromptContent{ID: "agy-result-envelope-content", Text: resultEnvelopeInstruction, Required: true},
			})
		}
		spec.PromptPlan = &plan
	}
	var err error
	spec, err = agent.PrepareHarness(spec, p.Manifest())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	return p.spawn(ctx, spec)
}

func planContainsResultEnvelope(plan agent.PromptPlan) bool {
	if strings.Contains(plan.UserPrompt.Text, resultEnvelopeBegin) {
		return true
	}
	for _, amendment := range plan.UserAmendments {
		if strings.Contains(amendment.Content.Text, resultEnvelopeBegin) {
			return true
		}
	}
	return false
}

// Resume returns agent.ErrUnsupported. `agy --conversation <id>` resumes by id
// (better than the gemini CLI's indexes), but between-session continuation is
// deferred to a later wave; SupportsSessionResume=false.
func (*Provider) Resume(_ context.Context, _ string, _ agent.Spec) (agent.Handle, error) {
	return nil, fmt.Errorf("provider/agycli: Resume: %w (SupportsSessionResume=false; --conversation resume deferred)", agent.ErrUnsupported)
}

// Shutdown is a no-op. Each session is its own pty-attached subprocess that
// terminates with the Handle; the provider holds no long-lived resources.
func (*Provider) Shutdown(_ context.Context) error { return nil }
