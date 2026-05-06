package ollama

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// DefaultEndpoint is the address Ollama binds to by default when an
// operator runs `ollama serve`. Override via Options.Endpoint for tests
// or non-standard installs (e.g. a remote box on the same LAN).
const DefaultEndpoint = "http://localhost:11434"

// DefaultModel is the model name used when a Spec does not supply one.
// Empty string is intentional: we do not invent a default; Spawn will
// reject specs without an explicit model so misconfiguration surfaces
// loudly rather than silently routing to whatever is installed first.
const DefaultModel = ""

// defaultProbeTimeout caps how long New waits for GET /api/tags. The
// probe is local-first and tiny; 5s is generous and still snappy enough
// for daemon-startup logging to land before user-visible spinners stall.
const defaultProbeTimeout = 5 * time.Second

// Options configure a Provider. The zero value is valid and probes
// DefaultEndpoint with the default HTTP client.
type Options struct {
	// Endpoint is the base URL of a reachable Ollama server. When empty,
	// DefaultEndpoint is used. Trailing slashes are trimmed.
	Endpoint string

	// HTTPClient is the *http.Client used for both the construction
	// probe and per-Spawn streaming requests. When nil, a fresh
	// http.Client with no timeout is used (per-request ctx provides the
	// deadline; a client-wide timeout would prematurely kill streaming
	// responses for long agent turns).
	HTTPClient *http.Client

	// ProbeTimeout caps the GET /api/tags probe in New. Zero falls back
	// to defaultProbeTimeout. Negative disables the probe entirely
	// (useful for tests that wire a fake transport without a server).
	ProbeTimeout time.Duration
}

// Provider is the agent.Provider implementation against a local-or-LAN
// Ollama HTTP endpoint.
//
// Constructed via New, which probes for /api/tags and returns
// agent.ErrProviderUnavailable when the endpoint is unreachable. Once
// constructed the Provider is safe for concurrent use; each Spawn
// returns an independent Handle backed by its own POST /api/chat
// streaming request.
type Provider struct {
	endpoint string
	client   *http.Client
}

// New constructs a Provider after probing GET /api/tags on the
// configured endpoint.
//
// Returns a non-nil error wrapping agent.ErrProviderUnavailable when
// the endpoint cannot be reached or the probe response is non-2xx; the
// runner short-circuits and surfaces a "ollama serve not running"
// remediation hint without ever attempting a Spawn.
//
// Per F.1.1 §3.1 / 002-provider-base-contract.md: fail-fast at
// construction is the contract. The probe is read-only and idempotent;
// it does not allocate any server-side state.
func New(opts Options) (*Provider, error) {
	endpoint := strings.TrimRight(opts.Endpoint, "/")
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	client := opts.HTTPClient
	if client == nil {
		// No client-wide timeout: streaming chat responses run as long
		// as the model takes to produce them. Per-request deadlines
		// come from the spawn ctx the caller provides.
		client = &http.Client{}
	}

	if opts.ProbeTimeout >= 0 {
		probeTimeout := opts.ProbeTimeout
		if probeTimeout == 0 {
			probeTimeout = defaultProbeTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		if err := probe(ctx, client, endpoint); err != nil {
			return nil, fmt.Errorf(
				"%w: ollama endpoint %q unreachable (start it with `ollama serve` or set Options.Endpoint to a reachable host): %v",
				agent.ErrProviderUnavailable, endpoint, err,
			)
		}
	}

	return &Provider{endpoint: endpoint, client: client}, nil
}

// Name returns ProviderOllama. Stable for the lifetime of the Provider.
func (*Provider) Name() agent.ProviderName { return agent.ProviderOllama }

// Capabilities returns the v0.1 capability matrix for the Ollama
// provider. See package documentation for the rationale behind each
// flag — the short version is "the smallest viable agent runtime: text
// in, text out, no tools, no resume".
//
// Conservative declaration is deliberate per the 002 base contract:
// providers MUST NOT advertise capabilities they cannot deliver.
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
		// Tool-use surface (002 v2): /api/chat does not expose `tools`
		// or MCP shape today. Some Ollama models (llama3.1, gemma3)
		// support OpenAI-compat function calling, but this provider
		// targets the bare /api/chat surface — declare false until
		// wired.
		AcceptsAllowedToolsList: false,
		AcceptsMcpServerSpec:    false,
		HumanLabel:              "Ollama",
	}
}

// Spawn starts a new Ollama session by issuing a streaming POST /api/chat
// request. The request body is built from the Spec via buildChatRequest.
//
// Spawn returns immediately once the HTTP response headers arrive; the
// handle's events channel is fed by a goroutine that streams the NDJSON
// body. Pre-spawn validation failures (missing model, build error) wrap
// agent.ErrSpawnFailed so the runner can distinguish them from
// in-session errors.
//
// On any HTTP / transport failure the returned error wraps
// agent.ErrSpawnFailed.
func (p *Provider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	model := spec.Model
	if model == "" {
		model = DefaultModel
	}
	if model == "" {
		return nil, fmt.Errorf(
			"%w: ollama Spec.Model required (no default; pass e.g. \"llama3.3\")",
			agent.ErrSpawnFailed,
		)
	}
	body, err := buildChatRequest(model, spec)
	if err != nil {
		return nil, fmt.Errorf("%w: build chat request: %w", agent.ErrSpawnFailed, err)
	}
	return p.startStream(ctx, body, spec)
}

// Resume returns agent.ErrUnsupported. Ollama has no server-side
// conversation state; there is no resume concept to honor. Per the
// capability matrix, the runner gates on SupportsSessionResume before
// calling Resume, so this branch only fires when a caller bypasses the
// gate.
func (*Provider) Resume(_ context.Context, _ string, _ agent.Spec) (agent.Handle, error) {
	return nil, fmt.Errorf("provider/ollama: Resume: %w (Ollama is stateless; SupportsSessionResume=false)", agent.ErrUnsupported)
}

// Shutdown is a no-op. Each session owns one in-flight HTTP request
// that terminates when its Handle is stopped or when the spawn ctx is
// canceled — there is no provider-level resource (long-lived
// subprocess, pooled connection state, etc.) to release.
func (*Provider) Shutdown(_ context.Context) error { return nil }
