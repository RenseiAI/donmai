package amp

import (
	"context"
	"fmt"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// EnvAPIKey is the environment variable NAME probed at construction.
// Amp's hosted endpoint authenticates with a personal access token; any
// future runner implementation would read it from this variable so the
// probe path stays compatible.
const EnvAPIKey = "AMP_API_KEY" //nolint:gosec // G101: env-var name, not a credential

// Options configures Provider construction. The zero value is valid
// and reads the API key from the environment.
type Options struct {
	// APIKey is the Amp personal access token. When empty the
	// constructor falls back to os.Getenv(EnvAPIKey). Tests inject
	// a fixed value here to bypass the environment.
	APIKey string

	// Getenv overrides the environment lookup. Defaults to
	// os.Getenv. Tests inject a fake to drive the probe-failure
	// branch without touching the real process environment.
	Getenv func(string) string
}

// Provider is the registration-only agent.Provider implementation for
// Sourcegraph's Amp. Spawn always returns an error wrapping
// agent.ErrSpawnFailed — see the package doc for the rationale.
type Provider struct {
	apiKey string
}

// New constructs a Provider after probing for the AMP_API_KEY env var.
//
// Returns an error wrapping agent.ErrProviderUnavailable when no key is
// set; the daemon's `af agent run` startup logs WARN and skips
// registration in that case (identical to claude / codex / gemini).
//
// When a key IS set the constructor succeeds — the runner-not-shipped
// failure is deferred to Spawn so operators see "amp runner pending"
// rather than "no provider" when they explicitly select amp.
func New(opts Options) (*Provider, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = defaultGetenv
	}
	key := opts.APIKey
	if key == "" {
		key = getenv(EnvAPIKey)
	}
	if key == "" {
		return nil, fmt.Errorf(
			"%w: amp provider needs %s in env (Amp personal access token)",
			agent.ErrProviderUnavailable, EnvAPIKey,
		)
	}
	return &Provider{apiKey: key}, nil
}

// Name returns ProviderAmp. Stable for the lifetime of the Provider.
func (*Provider) Name() agent.ProviderName { return agent.ProviderAmp }

// Capabilities returns the conservative all-off matrix for the
// registration-only runner. HumanLabel surfaces "Amp" in TUI/dashboard
// strings.
func (*Provider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		SupportsMessageInjection:            false,
		SupportsSessionResume:               false,
		SupportsToolPlugins:                 false,
		NeedsBaseInstructions:               false,
		NeedsPermissionConfig:               false,
		SupportsCodeIntelligenceEnforcement: false,
		EmitsSubagentEvents:                 false,
		SupportsReasoningEffort:             false,
		ToolPermissionFormat:                "claude",
		HumanLabel:                          "Amp",
	}
}

// Spawn always fails with a wrapped agent.ErrSpawnFailed because the
// real runner has not yet shipped. The error message names the
// follow-up Linear issue so operators can find the work.
func (*Provider) Spawn(_ context.Context, _ agent.Spec) (agent.Handle, error) {
	return nil, fmt.Errorf(
		"%w: amp runner not yet implemented — use the BYOK A2A path or wait for REN-1499",
		agent.ErrSpawnFailed,
	)
}

// Resume always fails with agent.ErrUnsupported. SupportsSessionResume
// is false so the runner should never call this in practice.
func (*Provider) Resume(_ context.Context, _ string, _ agent.Spec) (agent.Handle, error) {
	return nil, fmt.Errorf("provider/amp: Resume: %w (registration-only runner)", agent.ErrUnsupported)
}

// Shutdown is a no-op. There are no long-lived resources.
func (*Provider) Shutdown(_ context.Context) error { return nil }
