package afcli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/gateway"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/runner"
)

// gatewayDetail builds a session detail whose resolved profile names the given
// serving host.
func gatewayDetail(servingHost, provider string) *daemon.SessionDetail {
	return &daemon.SessionDetail{
		SessionID: "sess-gw",
		ResolvedProfile: &daemon.SessionResolvedProfile{
			Provider:    provider,
			Model:       "gpt-5.5",
			ServingHost: servingHost,
		},
	}
}

func gatewayQW() *runner.QueuedWork {
	return &runner.QueuedWork{
		QueuedWork: prompt.QueuedWork{SessionID: "sess-gw"},
		ResolvedProfile: runner.ResolvedProfile{
			Provider: agent.ProviderName("pi"),
			Model:    "gpt-5.5",
		},
	}
}

// TestIsGatewayServed covers the detection seam, including the mixed-case and
// absent-profile defaults (a legacy dispatch must never be treated as gateway).
func TestIsGatewayServed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		detail *daemon.SessionDetail
		want   bool
	}{
		{"nil detail", nil, false},
		{"nil resolved profile", &daemon.SessionDetail{SessionID: "s"}, false},
		{"empty serving host", gatewayDetail("", "pi"), false},
		{"direct host", gatewayDetail("direct", "pi"), false},
		{"gateway host", gatewayDetail("gateway", "pi"), true},
		{"gateway host mixed case", gatewayDetail("Gateway", "pi"), true},
		{"gateway host padded", gatewayDetail("  gateway  ", "pi"), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isGatewayServed(tc.detail); got != tc.want {
				t.Errorf("isGatewayServed = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBindWorkerGateway_NonGatewayCellIsNoOp proves the overwhelmingly common
// path is untouched: no listener, no endpoint stamped, no error.
func TestResolveGatewayUpstreamBaseURL(t *testing.T) {
	httptestUpstream := httptest.NewServer(http.NotFoundHandler())
	defer httptestUpstream.Close()

	tests := []struct {
		name     string
		raw      string
		wantBase string
		wantErr  bool
	}{
		{
			name:     "default direct OpenAI route",
			wantBase: defaultGatewayUpstreamBaseURL,
		},
		{
			name:     "local httptest HTTP route",
			raw:      httptestUpstream.URL + "/v1",
			wantBase: httptestUpstream.URL + "/v1",
		},
		{
			name:    "relative route",
			raw:     "/v1",
			wantErr: true,
		},
		{
			name:    "unsupported scheme",
			raw:     "ftp://upstream.example/v1",
			wantErr: true,
		},
		{
			name:    "missing host",
			raw:     "https:///v1",
			wantErr: true,
		},
		{
			name:    "userinfo is rejected",
			raw:     "https://secret@upstream.example/v1",
			wantErr: true,
		},
		{
			name:    "query is rejected",
			raw:     "https://upstream.example/v1?api_key=secret",
			wantErr: true,
		},
		{
			name:    "empty query is rejected",
			raw:     "https://upstream.example/v1?",
			wantErr: true,
		},
		{
			name:    "fragment is rejected",
			raw:     "https://upstream.example/v1#secret",
			wantErr: true,
		},
		{
			name:    "empty fragment is rejected",
			raw:     "https://upstream.example/v1#",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotBase, err := resolveGatewayUpstreamBaseURL(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveGatewayUpstreamBaseURL case %q succeeded; want an error", tc.name)
				}
				if strings.Contains(err.Error(), "secret") {
					t.Errorf("validation error leaked part of the supplied route: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveGatewayUpstreamBaseURL case %q: %v", tc.name, err)
			}
			if gotBase != tc.wantBase {
				t.Errorf("resolveGatewayUpstreamBaseURL case %q = %q, want %q", tc.name, gotBase, tc.wantBase)
			}
		})
	}
}

func TestBindWorkerGateway_DoesNotLogUpstreamRoute(t *testing.T) {
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	t.Setenv(EnvGatewayUpstreamBaseURL, "https://secret-upstream.example/private/route")
	t.Setenv(EnvGatewayUpstreamAPIKey, "sk-test")
	t.Setenv("DONMAI_STATE_HOME", t.TempDir())

	gw, err := bindWorkerGateway(context.Background(), logger, gatewayDetail("gateway", "pi"), gatewayQW())
	if err != nil {
		t.Fatalf("bindWorkerGateway: %v", err)
	}
	defer gw.Close(logger)

	output := logs.String()
	if strings.Contains(output, "secret-upstream.example") || strings.Contains(output, "/private/route") {
		t.Errorf("gateway bind log leaked the upstream route: %s", output)
	}
}

func TestBindWorkerGateway_NonGatewayCellIsNoOp(t *testing.T) {
	t.Setenv(EnvGatewayUpstreamAPIKey, "sk-test")
	qw := gatewayQW()
	gw, err := bindWorkerGateway(context.Background(), slog.Default(), gatewayDetail("direct", "pi"), qw)
	if err != nil {
		t.Fatalf("bindWorkerGateway: %v", err)
	}
	if gw != nil {
		t.Errorf("non-gateway cell returned a gateway session: %+v", gw)
	}
	if qw.ResolvedProfile.Endpoint != nil {
		t.Errorf("non-gateway cell stamped an endpoint: %+v", qw.ResolvedProfile.Endpoint)
	}
	// Close on a nil receiver is a safe no-op (the caller defers unconditionally).
	gw.Close(slog.Default())
}

// TestBindWorkerGateway_MissingUpstreamCredentialFailsClosed proves a
// gateway-served cell with no worker-side upstream credential fails LOUDLY at
// preflight rather than silently running the session on the harness's own
// default endpoint (the silent-fallback failure this producer exists to remove).
func TestBindWorkerGateway_MissingUpstreamCredentialFailsClosed(t *testing.T) {
	t.Setenv(EnvGatewayUpstreamAPIKey, "")
	t.Setenv("OPENAI_API_KEY", "")
	qw := gatewayQW()
	gw, err := bindWorkerGateway(context.Background(), slog.Default(), gatewayDetail("gateway", "pi"), qw)
	if err == nil {
		gw.Close(slog.Default())
		t.Fatal("want an error when a gateway-served cell has no upstream credential; got nil")
	}
	if !strings.Contains(err.Error(), EnvGatewayUpstreamAPIKey) {
		t.Errorf("error must name the operator knob to set; got %v", err)
	}
	if qw.ResolvedProfile.Endpoint != nil {
		t.Errorf("failed bind must leave the resolved profile unstamped; got %+v", qw.ResolvedProfile.Endpoint)
	}
}

// TestBindWorkerGateway_UnpinnedModelFailsClosed: the gateway serves exactly the
// bound model, so a cell with no model cannot be bound.
func TestBindWorkerGateway_UnpinnedModelFailsClosed(t *testing.T) {
	t.Setenv(EnvGatewayUpstreamAPIKey, "sk-test")
	qw := gatewayQW()
	qw.ResolvedProfile.Model = "  "
	if _, err := bindWorkerGateway(context.Background(), slog.Default(), gatewayDetail("gateway", "pi"), qw); err == nil {
		t.Fatal("want an error when a gateway-served cell carries no model; got nil")
	}
}

// TestBindWorkerGateway_ExistingBindingWins proves this producer fills a gap
// rather than overriding a binding a future platform/daemon producer supplies.
func TestBindWorkerGateway_ExistingBindingWins(t *testing.T) {
	t.Setenv(EnvGatewayUpstreamAPIKey, "sk-test")
	qw := gatewayQW()
	existing := &agent.EndpointBinding{BaseURL: "http://127.0.0.1:9/v1", Host: agent.HostGateway}
	qw.ResolvedProfile.Endpoint = existing
	gw, err := bindWorkerGateway(context.Background(), slog.Default(), gatewayDetail("gateway", "pi"), qw)
	if err != nil {
		t.Fatalf("bindWorkerGateway: %v", err)
	}
	if gw != nil {
		t.Errorf("an already-bound session must not start a second gateway; got %+v", gw)
	}
	if qw.ResolvedProfile.Endpoint != existing {
		t.Errorf("existing binding was replaced: %+v", qw.ResolvedProfile.Endpoint)
	}
}

// TestBindWorkerGateway_BindsLoopbackAndForwardsToUpstream is the load-bearing
// unit test: a gateway-served cell yields a loopback binding carrying a
// per-session bearer, an OpenAI-chat request to that binding reaches the
// configured upstream WITH the worker's credential (never the bearer), and Close
// releases the listener.
func TestBindWorkerGateway_BindsLoopbackAndForwardsToUpstream(t *testing.T) {
	var gotAuth, gotBody string
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		gotAuth = r.Header.Get("Authorization")
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-1",
			"object":  "chat.completion",
			"model":   "gpt-5.5",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "pong"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4},
		})
	}))
	defer upstreamSrv.Close()

	t.Setenv(EnvGatewayUpstreamBaseURL, upstreamSrv.URL+"/v1")
	t.Setenv(EnvGatewayUpstreamAPIKey, "sk-worker-only")
	t.Setenv("DONMAI_STATE_HOME", t.TempDir())

	qw := gatewayQW()
	gw, err := bindWorkerGateway(context.Background(), slog.Default(), gatewayDetail("gateway", "pi"), qw)
	if err != nil {
		t.Fatalf("bindWorkerGateway: %v", err)
	}
	if gw == nil {
		t.Fatal("gateway-served cell returned no gateway session")
	}

	ep := qw.ResolvedProfile.Endpoint
	if ep == nil {
		t.Fatal("gateway-served cell stamped no endpoint binding")
	}
	if ep.Host != agent.HostGateway || ep.Protocol != agent.ProtoOpenAIChat {
		t.Errorf("binding host/protocol = %q/%q; want gateway/openai-chat", ep.Host, ep.Protocol)
	}
	if !strings.HasPrefix(ep.BaseURL, "http://127.0.0.1:") || !strings.HasSuffix(ep.BaseURL, "/v1") {
		t.Errorf("binding BaseURL = %q; want a loopback http://127.0.0.1:<port>/v1", ep.BaseURL)
	}
	bearer := ep.Env[gateway.TokenEnvVar]
	if bearer == "" {
		t.Fatalf("binding carries no %s bearer", gateway.TokenEnvVar)
	}
	if bearer == "sk-worker-only" {
		t.Fatal("the harness bearer must never be the upstream credential")
	}

	// Drive the binding the way the harness would.
	reqBody := `{"model":"gpt-5.5","messages":[{"role":"user","content":"ping"}]}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		ep.BaseURL+"/chat/completions", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("dial gateway binding: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("gateway returned %d: %s", resp.StatusCode, msg)
	}
	var decoded struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode gateway response: %v", err)
	}
	if len(decoded.Choices) == 0 || decoded.Choices[0].Message.Content != "pong" {
		t.Errorf("gateway did not relay the upstream completion; got %+v", decoded)
	}

	// Credential isolation: the upstream saw the worker key; the harness only
	// ever holds the loopback bearer.
	if gotAuth != "Bearer sk-worker-only" {
		t.Errorf("upstream Authorization = %q; want the worker-held credential", gotAuth)
	}
	if strings.Contains(gotBody, bearer) {
		t.Error("the session bearer leaked into the upstream request body")
	}

	// A foreign token cannot use this session's route.
	badReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		ep.BaseURL+"/chat/completions", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	badReq.Header.Set("Authorization", "Bearer not-this-session")
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatalf("dial gateway binding with a foreign token: %v", err)
	}
	_ = badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("foreign token got %d; want 401", badResp.StatusCode)
	}

	// Teardown releases the listener.
	gw.Close(slog.Default())
	if _, err := http.Get(ep.BaseURL + "/models"); err == nil { //nolint:noctx // post-teardown liveness probe; failure IS the assertion.
		t.Error("gateway listener still accepting connections after Close")
	}
}
