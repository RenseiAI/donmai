package gemini

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/mcp"
)

// DefaultEndpoint is the public Gemini API base URL. Override via
// Options.Endpoint to point at a regional mirror or an httptest fake.
const DefaultEndpoint = "https://generativelanguage.googleapis.com"

// DefaultModel is the model identifier used when Spec.Model is empty.
// gemini-3.5-flash is the GA flagship agentic/coding model (1M context,
// native function-calling, thinking_level knob). It is the donmai-wide
// default per the Gemini-first-class program.
const DefaultModel = "gemini-3.5-flash"

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
	// dialMCP connects one Spec.MCPServers entry for the in-box MCP
	// bridge. Defaults to runtime/mcp.Dial; tests inject fakes.
	dialMCP mcpDialer
}

// New constructs a Provider after probing for an API key.
//
// Returns an error wrapping agent.ErrProviderUnavailable when no key
// is set. The daemon `donmai agent run` registry build logs WARN and skips
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
		dialMCP:      mcp.Dial,
	}, nil
}

// Name returns ProviderGemini. Stable for the lifetime of the Provider.
func (*Provider) Name() agent.ProviderName { return agent.ProviderGemini }

// Capabilities returns the agentic matrix for the Gemini native runner.
// The runner drives a multi-turn generateContent conversation with
// function-calling, executes the model's functionCalls via a
// session-local executor (native Bash/Read/Edit/Write), maps
// reasoning-effort onto thinkingConfig, and supports post-completion
// steering by appending a user turn and re-driving the loop.
func (*Provider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		// Message injection appends a user turn to the maintained
		// contents history and re-drives the conversation loop (no
		// subprocess, no resume). This also unblocks runner steering
		// so Gemini sessions are no longer skipped.
		SupportsMessageInjection: true,
		// Resume folds prior contents into a fresh Spawn; the REST
		// endpoint is stateless so resume is best-effort prompt-fold.
		SupportsSessionResume: false,
		// SupportsToolPlugins covers both tool surfaces: the session-local
		// executor runs Bash/Read/Edit/Write functionCalls in-box, and the
		// in-box MCP bridge routes mcp__* functionCalls to the declared
		// MCP servers (see AcceptsMcpServerSpec below).
		SupportsToolPlugins:                 true,
		NeedsBaseInstructions:               false,
		NeedsPermissionConfig:               false,
		SupportsCodeIntelligenceEnforcement: false,
		EmitsSubagentEvents:                 false,
		// Reasoning effort maps to thinkingConfig: thinking_level for
		// 3.x model IDs, thinkingBudget for 2.5 model IDs.
		SupportsReasoningEffort: true,
		// Gemini has no Claude permission grammar; tool gating happens
		// via toolConfig.functionCallingConfig.mode, not a pattern list.
		ToolPermissionFormat: ToolPermissionFormatGemini,
		// AcceptsAllowedToolsList: Spec.AllowedTools is honored
		// end-to-end — each pattern's verb becomes a functionDeclaration
		// AND the session-local executor runs the resulting native
		// functionCall (Bash/Read/Edit/Write), so the field shape is not
		// silently dropped.
		AcceptsAllowedToolsList: true,
		// AcceptsMcpServerSpec: Spec.MCPServers is honored end-to-end via
		// the in-box MCP bridge (mcp.go + runtime/mcp): Spawn dials every
		// declared server (stdio or Streamable HTTP), discovers its tools,
		// declares them to the model as mcp__<server>__<tool> function
		// declarations, and the session-local executor routes the
		// resulting mcp__* functionCalls to the live server. A server
		// that fails to connect degrades to a structured tool error — the
		// shape is consumed either way, satisfying the v2 contract.
		AcceptsMcpServerSpec: true,
		HumanLabel:           "Gemini",
	}
}

// Spawn drives a multi-turn generateContent conversation and returns a
// Handle whose Events channel emits exactly one InitEvent, zero or more
// AssistantText / ToolUse events, and exactly one terminal ResultEvent
// (or ErrorEvent on transport failure).
//
// The conversation history (contents array) is maintained inside the
// Handle across turns because the REST endpoint is stateless: a
// functionCall from the model surfaces a ToolUse event, the provider
// runs the tool via the session-local executor, surfaces a ToolResult
// event, and folds the functionResponse back into the loop as a
// user-role turn (the live API rejects role "function"). Post-completion
// steering arrives via Handle.Inject, which appends a user turn and
// re-drives.
func (p *Provider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}

	model := spec.Model
	if model == "" {
		model = p.defaultModel
	}

	plan, err := buildSpawnPlan(spec, model)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %w", agent.ErrSpawnFailed, err)
	}

	// Per-Spawn credential resolution (per-session BYOK + rotation):
	// Spec.Env[GEMINI_API_KEY] → Spec.Env[GOOGLE_API_KEY] → the
	// construction-time fallback captured in New().
	apiKey := resolveSpawnKey(spec, p.apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf(
			"%w: gemini provider has no API key (Spec.Env[%s|%s] empty and no construction fallback)",
			agent.ErrSpawnFailed, EnvAPIKeyPrimary, EnvAPIKeyFallback,
		)
	}

	// Resolve the MaxTurns cap. 0 = uncapped (no MaxTurns set or
	// explicitly zero). MaxTurns > 0 is a hard agentic round-trip ceiling
	// enforced inside the driver loop (distinct from MaxOutputTokens which
	// caps per-response token count).
	maxTurns := 0
	if spec.MaxTurns != nil && *spec.MaxTurns > 0 {
		maxTurns = *spec.MaxTurns
	}

	// In-box MCP bridge: dial the declared servers and discover their tool
	// surfaces so the model sees real mcp__<server>__<tool> declarations
	// (replacing the catch-all per-server placeholders in the plan). Best
	// effort with a hard time budget — a failed server degrades to
	// structured tool errors, never a failed Spawn.
	var bridge *mcpBridge
	if len(spec.MCPServers) > 0 {
		dialCtx, cancel := context.WithTimeout(ctx, mcpConnectTimeout)
		bridge = newMCPBridge(dialCtx, spec.MCPServers, p.dialMCP)
		cancel()
		bridge.amendPlan(&plan)
	}

	return startSession(ctx, sessionParams{
		apiKey:    apiKey,
		endpoint:  p.endpoint,
		model:     model,
		plan:      plan,
		client:    p.httpClient,
		sessionID: p.sessionIDFn(),
		// cwd / env drive the session-local tool executor: native
		// functionCalls (Bash/Read/Edit/Write) run in the session's
		// working directory with the session env; mcp routes mcp__*
		// calls to the bridged servers.
		cwd:      spec.Cwd,
		env:      spec.Env,
		mcp:      bridge,
		maxTurns: maxTurns,
	})
}

// resolveSpawnKey resolves the API key for a single Spawn. The
// precedence is Spec.Env[GEMINI_API_KEY] → Spec.Env[GOOGLE_API_KEY] →
// the construction-time fallback. This supports per-session BYOK and
// key rotation without reconstructing the Provider.
func resolveSpawnKey(spec agent.Spec, fallback string) string {
	if spec.Env != nil {
		if k := strings.TrimSpace(spec.Env[EnvAPIKeyPrimary]); k != "" {
			return k
		}
		if k := strings.TrimSpace(spec.Env[EnvAPIKeyFallback]); k != "" {
			return k
		}
	}
	return fallback
}

// Resume folds prior conversation history into a fresh Spawn. Gemini's
// REST endpoint is stateless, so true server-side resume is impossible;
// SupportsSessionResume is false and the runner gates on it. The method
// returns ErrUnsupported to keep the contract explicit.
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
