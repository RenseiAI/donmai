package gemini

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// DefaultEndpoint is the public Gemini API base URL. Override via
// Options.Endpoint to point at a regional mirror or an httptest fake.
const DefaultEndpoint = "https://generativelanguage.googleapis.com"

// DefaultModel is the model identifier used when Spec.Model is empty.
// 2.0-flash is the cheapest GA model that supports streaming today.
const DefaultModel = "gemini-2.0-flash"

// DefaultRequestTimeout caps a single Spawn HTTP request. Streaming
// responses can run for many minutes, so this is generous; the runner
// imposes its own per-session deadlines.
const DefaultRequestTimeout = 30 * time.Minute

// EnvAPIKeyPrimary is the primary environment variable NAME probed at
// construction. Aligns with Google's published env-var convention.
const EnvAPIKeyPrimary = "GEMINI_API_KEY" //nolint:gosec // G101: env-var name, not a credential

// EnvAPIKeyFallback is the fallback environment variable NAME. Many
// existing tools (gcloud, Vertex SDK) standardize on GOOGLE_API_KEY;
// honouring both keeps day-1 onboarding painless.
const EnvAPIKeyFallback = "GOOGLE_API_KEY" //nolint:gosec // G101: env-var name, not a credential

// Options configures Provider construction. The zero value reads the
// API key from the environment and targets the public Gemini endpoint.
type Options struct {
	// APIKey is the Google AI Studio / Gemini API key. When empty the
	// constructor falls back to env (GEMINI_API_KEY then
	// GOOGLE_API_KEY).
	APIKey string

	// Endpoint overrides the API base URL. Empty → DefaultEndpoint.
	// Tests inject httptest.NewServer URLs here.
	Endpoint string

	// HTTPClient overrides the http.Client used for streaming.
	// Defaults to a client with DefaultRequestTimeout. Tests inject
	// fakes; production callers leave this nil.
	HTTPClient *http.Client

	// Getenv overrides the environment lookup. Defaults to
	// os.Getenv. Tests inject a fake to drive probe-failure paths
	// without touching the real process environment.
	Getenv func(string) string

	// SessionIDFn overrides the synthetic session-id generator.
	// Defaults to "gemini-session-<8 hex bytes>". Tests inject a
	// deterministic generator.
	SessionIDFn func() string
}

// Provider is the agent.Provider implementation backed by direct
// HTTPS calls to generativelanguage.googleapis.com.
//
// Provider holds no per-session state — every Spawn opens its own
// streaming POST. Safe for concurrent use across goroutines.
type Provider struct {
	apiKey       string
	endpoint     string
	httpClient   *http.Client
	sessionIDFn  func() string
	defaultModel string
}

// New constructs a Provider after probing for an API key.
//
// Returns an error wrapping agent.ErrProviderUnavailable when no key
// is set. The daemon `af agent run` registry build logs WARN and skips
// registration in that case, identical to claude / codex.
func New(opts Options) (*Provider, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = defaultGetenv
	}
	key := opts.APIKey
	if key == "" {
		key = getenv(EnvAPIKeyPrimary)
	}
	if key == "" {
		key = getenv(EnvAPIKeyFallback)
	}
	if key == "" {
		return nil, fmt.Errorf(
			"%w: gemini provider needs %s (or %s) in env (https://aistudio.google.com/app/apikey)",
			agent.ErrProviderUnavailable, EnvAPIKeyPrimary, EnvAPIKeyFallback,
		)
	}

	endpoint := strings.TrimRight(opts.Endpoint, "/")
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: DefaultRequestTimeout}
	}

	sidFn := opts.SessionIDFn
	if sidFn == nil {
		sidFn = defaultSessionID
	}

	return &Provider{
		apiKey:       key,
		endpoint:     endpoint,
		httpClient:   client,
		sessionIDFn:  sidFn,
		defaultModel: DefaultModel,
	}, nil
}

// Name returns ProviderGemini. Stable for the lifetime of the Provider.
func (*Provider) Name() agent.ProviderName { return agent.ProviderGemini }

// Capabilities returns the conservative v0.1 matrix. Tool use and
// reasoning effort flip on as we wire each round-trip.
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
		HumanLabel:                          "Gemini",
	}
}

// Spawn opens a streaming generateContent call and returns a Handle
// whose Events channel emits exactly one InitEvent, zero or more
// AssistantTextEvents, and exactly one terminal ResultEvent (or
// ErrorEvent on transport failure).
func (p *Provider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}

	model := spec.Model
	if model == "" {
		model = p.defaultModel
	}

	body, err := buildRequestBody(spec)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %w", agent.ErrSpawnFailed, err)
	}

	url := fmt.Sprintf(
		"%s/v1beta/models/%s:streamGenerateContent?alt=sse",
		p.endpoint, model,
	)

	return startSession(ctx, sessionParams{
		apiKey:    p.apiKey,
		url:       url,
		body:      body,
		client:    p.httpClient,
		sessionID: p.sessionIDFn(),
	})
}

// Resume always returns ErrUnsupported. Gemini's REST endpoint is
// stateless; the runner is expected to gate on Capabilities.
func (*Provider) Resume(_ context.Context, _ string, _ agent.Spec) (agent.Handle, error) {
	return nil, fmt.Errorf("provider/gemini: Resume: %w (stateless REST endpoint)", agent.ErrUnsupported)
}

// Shutdown is a no-op. There are no long-lived child processes or
// shared HTTP connections that need explicit teardown.
func (*Provider) Shutdown(_ context.Context) error { return nil }

// defaultSessionID returns "gemini-session-<8 hex bytes>". The Gemini
// REST endpoint does not return a server-side session id of its own,
// so we synthesise one for InitEvent.
func defaultSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "gemini-session-fallback"
	}
	return "gemini-session-" + hex.EncodeToString(b[:])
}
