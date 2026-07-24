package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/gateway/costfeed"
	"github.com/RenseiAI/donmai/gateway/ir"
	"github.com/RenseiAI/donmai/gateway/pool"
	"github.com/RenseiAI/donmai/gateway/surface"
	"github.com/RenseiAI/donmai/gateway/token"
	"github.com/RenseiAI/donmai/gateway/upstream"
)

// TokenEnvVar is the env-var NAME the gateway binding carries its per-session
// bearer under. A harness read site maps this onto whatever api-key knob its
// client uses (opencode's injected apiKey env, for example). NAME only — the
// value is a freshly-minted per-session token, never a long-lived secret.
const TokenEnvVar = "DONMAI_GW_TOKEN" //nolint:gosec // G101: env-var NAME, not a credential

// HostGateway mirrors agent.HostGateway for local readability. It is the
// ServingHost every gateway cell and binding carries.
const HostGateway = agent.HostGateway

// Gateway is the translating-gateway host: a loopback listener that presents
// inbound wire surfaces, binds each session to a route (upstream + credential
// scope) via a per-session bearer token, and meters every exchange. Safe for
// concurrent use.
type Gateway struct {
	sink costfeed.Sink
	now  func() time.Time

	mu       sync.RWMutex
	routes   map[token.Token]*route
	listener net.Listener
	httpd    *http.Server
	addr     string
	started  bool
}

// route is one bound session: the upstream to dial and the credential scope to
// dial it with, plus the attribution facts stamped onto every cost row.
type route struct {
	sessionID  string
	dispatchID string
	harness    string
	company    string
	authMode   string
	model      string
	upstream   upstream.Upstream
	pool       *pool.Pool
}

// Options configures a Gateway.
type Options struct {
	// Sink receives cost events. Nil uses an in-memory sink (a daemon with no
	// writable state home degrades to this rather than failing).
	Sink costfeed.Sink
	// Now is a clock seam for tests. Nil uses time.Now.
	Now func() time.Time
}

// New constructs a Gateway. Call Start to bind the loopback listener.
func New(opts Options) *Gateway {
	g := &Gateway{
		sink:   opts.Sink,
		now:    opts.Now,
		routes: map[token.Token]*route{},
	}
	if g.sink == nil {
		g.sink = &costfeed.MemorySink{}
	}
	if g.now == nil {
		g.now = time.Now
	}
	return g
}

// Start binds an ephemeral loopback listener (127.0.0.1:0) and serves every
// surface on it. The resolved address is published via Addr() and templated
// into each binding's BaseURL at Bind time. Idempotent-safe: a second Start
// returns an error rather than leaking a listener.
func (g *Gateway) Start(_ context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.started {
		return errors.New("gateway: already started")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("gateway: listen loopback: %w", err)
	}
	mux := http.NewServeMux()
	chat := surface.NewOpenAIChat(g)
	mux.Handle("/v1/chat/completions", chat)
	mux.HandleFunc("/v1/models", g.handleModels)

	g.listener = ln
	g.addr = ln.Addr().String()
	g.httpd = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	g.started = true
	go func() { _ = g.httpd.Serve(ln) }()
	return nil
}

// Stop shuts the listener down gracefully and releases every route. After Stop
// the Gateway may not be restarted (construct a new one).
func (g *Gateway) Stop(ctx context.Context) error {
	g.mu.Lock()
	httpd := g.httpd
	g.started = false
	g.routes = map[token.Token]*route{}
	g.mu.Unlock()
	if httpd == nil {
		return nil
	}
	return httpd.Shutdown(ctx)
}

// Addr returns the bound loopback address (host:port) after Start.
func (g *Gateway) Addr() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.addr
}

// Started reports whether the listener is serving.
func (g *Gateway) Started() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.started
}

// BindConfig describes a session's route. The daemon/resolver builds it from a
// resolved gateway cell (the chosen upstream + the single-key credential
// source) and hands the returned binding to the harness.
type BindConfig struct {
	SessionID  string
	DispatchID string
	Harness    string
	Company    agent.Company
	Model      string
	AuthMode   agent.AuthMode
	// Upstream is the outbound backend to dial for this session.
	Upstream upstream.Upstream
	// Source yields the credential(s). OSS: pool.SingleKey.
	Source pool.CredentialSource
	// PoolOpts tunes the rotation state machine (optional).
	PoolOpts pool.Options
	// Surface is the inbound wire protocol the harness will drive. M1 supports
	// agent.ProtoOpenAIChat only.
	Surface agent.WireProtocol
}

// Bind registers a session route, mints its bearer token, and returns the
// resolved EndpointBinding: BaseURL points at this gateway's surface, Env
// carries the token under TokenEnvVar. The harness presents the token as its
// api key / bearer; the gateway looks the session up by exact token match, so
// a request bearing session A's token can only ever reach session A's
// credential scope (the cross-session isolation guarantee, 08 §5).
func (g *Gateway) Bind(cfg BindConfig) (agent.EndpointBinding, error) {
	if cfg.Surface != agent.ProtoOpenAIChat {
		return agent.EndpointBinding{}, fmt.Errorf("gateway: unsupported inbound surface %q (M1 = openai-chat only)", cfg.Surface)
	}
	if cfg.Upstream == nil || cfg.Source == nil {
		return agent.EndpointBinding{}, errors.New("gateway: bind requires an upstream and a credential source")
	}
	tok, err := token.Mint()
	if err != nil {
		return agent.EndpointBinding{}, err
	}

	g.mu.Lock()
	if !g.started {
		g.mu.Unlock()
		return agent.EndpointBinding{}, errors.New("gateway: not started")
	}
	dispatchID := cfg.DispatchID
	if dispatchID == "" {
		dispatchID = cfg.SessionID
	}
	g.routes[tok] = &route{
		sessionID:  cfg.SessionID,
		dispatchID: dispatchID,
		harness:    cfg.Harness,
		company:    string(cfg.Company),
		authMode:   string(cfg.AuthMode),
		model:      cfg.Model,
		upstream:   cfg.Upstream,
		pool:       pool.New(cfg.Source, cfg.PoolOpts),
	}
	addr := g.addr
	g.mu.Unlock()

	return agent.EndpointBinding{
		Company:   cfg.Company,
		Model:     cfg.Model,
		BaseURL:   fmt.Sprintf("http://%s/v1", addr),
		Protocol:  agent.ProtoOpenAIChat,
		Host:      agent.HostGateway,
		Auth:      cfg.AuthMode,
		CostModel: agent.CostMeteredPerToken,
		Env:       map[string]string{TokenEnvVar: string(tok)},
	}, nil
}

// Unbind releases a session route (called at session teardown). Unknown tokens
// are a no-op.
func (g *Gateway) Unbind(tok token.Token) {
	g.mu.Lock()
	delete(g.routes, tok)
	g.mu.Unlock()
}

// Routes returns the number of live session routes (status/tests).
func (g *Gateway) Routes() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.routes)
}

// Dispatch implements surface.Dispatcher: resolve the token to its route,
// select a credential, invoke the upstream, and hand back the outcome plus a
// Meter closure the surface calls once the reply is fully written.
func (g *Gateway) Dispatch(ctx context.Context, tok token.Token, req ir.Request) (surface.Outcome, error) {
	g.mu.RLock()
	rt := g.routes[tok]
	g.mu.RUnlock()
	if rt == nil {
		return surface.Outcome{}, &surface.HTTPError{Status: http.StatusUnauthorized, Code: "invalid_token", Message: "unknown or expired session token"}
	}

	req.Metadata = ir.Metadata{SessionID: rt.sessionID, DispatchID: rt.dispatchID, Harness: rt.harness}

	cred, err := rt.pool.Acquire()
	if err != nil {
		return surface.Outcome{}, &surface.HTTPError{Status: http.StatusServiceUnavailable, Code: "no_credential", Message: "no upstream credential available"}
	}

	out, err := rt.upstream.Invoke(ctx, req, cred)
	rt.pool.Report(cred, out.Status)
	if err != nil {
		return surface.Outcome{}, &surface.HTTPError{Status: clientStatus(out.Status), Code: "upstream_error", Message: err.Error()}
	}

	model := rt.model
	if model == "" {
		model = req.Model
	}
	meter := func(usage ir.Usage, finish ir.FinishReason) {
		_ = finish // reserved: a future policy/eval hook keys on finish reason
		_ = g.sink.Record(costfeed.Event{
			DispatchID:      rt.dispatchID,
			SessionID:       rt.sessionID,
			ProviderID:      rt.company,
			Host:            costfeed.Host,
			Harness:         rt.harness,
			AuthMode:        rt.authMode,
			Model:           model,
			TokensIn:        usage.TokensIn,
			TokensOut:       usage.TokensOut,
			ReasoningTokens: usage.ReasoningTokens,
			RawCostUSD:      0, // M1: token-only ledger; a price table lands with the platform cost ingest
			EmittedAt:       g.now().UTC(),
		})
	}

	return surface.Outcome{Stream: out.Stream, Response: out.Response, Model: model, Meter: meter}, nil
}

// clientStatus maps an upstream HTTP status onto the status the gateway returns
// to its own client. Auth/rate/timeout pass through; a transport failure
// (status 0) or a 5xx becomes 502 Bad Gateway.
func clientStatus(upstreamStatus int) int {
	switch {
	case upstreamStatus == 0:
		return http.StatusBadGateway
	case upstreamStatus >= 500:
		return http.StatusBadGateway
	default:
		return upstreamStatus
	}
}

// handleModels serves GET /v1/models for the bound session: exactly the model
// the session's route pins (08 §5 — the gateway offers no catalog of its own
// in OSS; the menu is a platform/mesh concern).
func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	tok := token.FromBearer(r.Header.Get("Authorization"))
	g.mu.RLock()
	rt := g.routes[tok]
	g.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	if rt == nil {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "unknown session token"}})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   []map[string]any{{"id": rt.model, "object": "model", "owned_by": rt.company}},
	})
}

// SupportedSurfaces lists the inbound wire surfaces the gateway presents at
// this milestone (M1: openai-chat only). Exposed so a caller rendering a
// disabled-gateway status need not import the agent package.
func SupportedSurfaces() []string { return []string{string(agent.ProtoOpenAIChat)} }

// Status is the snapshot the daemon /api/daemon/gateway surface renders.
type Status struct {
	Enabled    bool     `json:"enabled"`
	Addr       string   `json:"addr,omitempty"`
	Routes     int      `json:"routes"`
	Surfaces   []string `json:"surfaces"`
	LedgerPath string   `json:"ledgerPath,omitempty"`
}

// Status returns the current gateway status. ledgerPath is passed in by the
// daemon (the gateway does not resolve the state home itself).
func (g *Gateway) Status(ledgerPath string) Status {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return Status{
		Enabled:    g.started,
		Addr:       g.addr,
		Routes:     len(g.routes),
		Surfaces:   SupportedSurfaces(),
		LedgerPath: ledgerPath,
	}
}
