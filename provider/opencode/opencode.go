package opencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// DefaultEndpoint is the URL probed at construction when no explicit
// endpoint or env var is supplied. Mirrors the OpenCode CLI's default
// server bind address.
const DefaultEndpoint = "http://localhost:7700"

// EnvEndpoint overrides DefaultEndpoint when set.
const EnvEndpoint = "OPENCODE_ENDPOINT"

// EnvAPIKey is the optional bearer-token env var NAME. Forwarded for
// future hosted variants; not required for the default localhost
// server.
const EnvAPIKey = "OPENCODE_API_KEY" //nolint:gosec // G101: env-var name, not a credential

// DefaultProbeTimeout caps the probe HTTP GET at construction.
const DefaultProbeTimeout = 2 * time.Second

// Options configures Provider construction.
type Options struct {
	// Endpoint overrides the OpenCode server URL. Empty falls back to
	// $OPENCODE_ENDPOINT then DefaultEndpoint.
	Endpoint string

	// APIKey is an optional bearer token. Empty falls back to
	// $OPENCODE_API_KEY (which may also be empty).
	APIKey string

	// HTTPClient is used for the probe call. Defaults to a client
	// with DefaultProbeTimeout. Tests inject httptest fakes.
	HTTPClient *http.Client

	// Getenv overrides the environment lookup. Defaults to os.Getenv.
	Getenv func(string) string

	// SkipProbe disables the construction-time liveness check.
	// Tests use this when the goal is to assert capability / Spawn
	// behavior without standing up a server.
	SkipProbe bool
}

// Provider is the registration-only agent.Provider implementation for
// OpenCode. Constructor probes liveness; Spawn always fails with a
// wrapped agent.ErrSpawnFailed until the runner lands.
type Provider struct {
	endpoint string
	apiKey   string
}

// New constructs a Provider after probing the OpenCode HTTP server.
//
// Probe contract: a GET to <endpoint>/ that returns ANY response with
// status < 500 is treated as live. The OpenCode server publishes a
// pre-1.0 surface; rather than couple to a specific health endpoint
// we accept any non-5xx as signal-of-life. Connection refused or
// 5xx → wrapped agent.ErrProviderUnavailable, identical to the
// claude / codex probe-failure path.
func New(opts Options) (*Provider, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = defaultGetenv
	}

	endpoint := strings.TrimRight(opts.Endpoint, "/")
	if endpoint == "" {
		endpoint = strings.TrimRight(getenv(EnvEndpoint), "/")
	}
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}

	apiKey := opts.APIKey
	if apiKey == "" {
		apiKey = getenv(EnvAPIKey)
	}

	if !opts.SkipProbe {
		client := opts.HTTPClient
		if client == nil {
			client = &http.Client{Timeout: DefaultProbeTimeout}
		}
		if err := probeLive(client, endpoint, apiKey); err != nil {
			return nil, fmt.Errorf(
				"%w: opencode server at %s unreachable (start with `opencode serve` or set %s): %v",
				agent.ErrProviderUnavailable, endpoint, EnvEndpoint, err,
			)
		}
	}

	return &Provider{endpoint: endpoint, apiKey: apiKey}, nil
}

// probeLive issues a GET against the server's root and accepts any
// non-5xx response as a successful liveness check. ConnectionRefused
// surfaces as a transport-error from client.Do.
func probeLive(client *http.Client, endpoint, apiKey string) error {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4*1024))
	if resp.StatusCode >= 500 {
		return errors.New("HTTP " + resp.Status)
	}
	return nil
}

// Name returns ProviderOpenCode. Stable for the lifetime of the
// Provider.
func (*Provider) Name() agent.ProviderName { return agent.ProviderOpenCode }

// Capabilities returns the conservative all-off matrix for the
// registration-only runner. HumanLabel surfaces "OpenCode" in TUI
// surfaces.
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
		HumanLabel:                          "OpenCode",
	}
}

// Spawn always fails with a wrapped agent.ErrSpawnFailed because the
// real runner has not yet shipped. The error message names the
// follow-up Linear issue so operators can find the work.
func (p *Provider) Spawn(_ context.Context, _ agent.Spec) (agent.Handle, error) {
	return nil, fmt.Errorf(
		"%w: opencode runner not yet implemented — endpoint=%s reachable but spec translation pending (REN-1501)",
		agent.ErrSpawnFailed, p.endpoint,
	)
}

// Resume always fails with agent.ErrUnsupported.
func (*Provider) Resume(_ context.Context, _ string, _ agent.Spec) (agent.Handle, error) {
	return nil, fmt.Errorf("provider/opencode: Resume: %w (registration-only runner)", agent.ErrUnsupported)
}

// Shutdown is a no-op. There are no long-lived resources.
func (*Provider) Shutdown(_ context.Context) error { return nil }
