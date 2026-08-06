package afcli

// gateway_bind.go — the worker-local gateway producer.
//
// runs/2026-07-21-open-harness-strategy/08-design-gateway-host.md §5 (session
// binding) and §9 (M1 cut) reserve one seam that #212 (gateway module) and #216
// (runner Spec.Endpoint threading) deliberately left empty: something has to
// BIND a gateway session per worker and hand the harness the resulting
// EndpointBinding. Until this file, nothing did — a session whose resolved cell
// named the gateway serving host silently fell back to the harness's own
// default provider (the endpoint never travelled), which is the exact silent
// no-op runs/2026-07-25-coordination/05-sol-exec-endpoint-threading.md flagged
// as the remaining SMOKE-GAP.
//
// The binding is produced HERE, in the worker process, and never over a wire:
//
//   - The base URL is generated locally by the gateway's own loopback listener
//     (127.0.0.1:<ephemeral>). No payload supplies a URL, so no untrusted input
//     can point a harness anywhere — the deliberate narrowing recorded in `05`
//     ("do not accept an unvalidated arbitrary base URL merely to close the
//     current producer gap").
//   - The per-session bearer is minted in-process by gateway.Bind and reaches
//     the harness through agent.EndpointBinding.Env, which is json:"-" and
//     therefore cannot serialize into any poll/detail payload.
//   - The UPSTREAM credential stays in the worker: the gateway dials the
//     provider with it, while the harness child only ever sees the loopback
//     bearer. Provider keys are in runtime/env's AgentEnvBlocklist, so the
//     child cannot inherit one — the credential-isolation property that makes a
//     gateway cell worth having.
//
// M1 scope, matching the shipped module: the openai-chat inbound surface and an
// OpenAI-compatible upstream (direct OpenAI or any compat URL — OpenRouter,
// Vercel AI Gateway, LiteLLM/Bifrost, vLLM/Ollama /v1). Cross-protocol surfaces
// (M2), Class E (M3) and sanctioned Class S (M4) are unchanged by this file.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/gateway"
	"github.com/RenseiAI/donmai/gateway/costfeed"
	"github.com/RenseiAI/donmai/gateway/pool"
	"github.com/RenseiAI/donmai/gateway/token"
	"github.com/RenseiAI/donmai/gateway/upstream"
	"github.com/RenseiAI/donmai/internal/statepath"
	"github.com/RenseiAI/donmai/runner"
)

const (
	// EnvGatewayUpstreamBaseURL overrides the OpenAI-compatible upstream API
	// root the worker-local gateway dials. Empty uses the direct OpenAI root.
	// This is an OPERATOR knob (env, worker-side), never a dispatch payload
	// field — see the file header.
	EnvGatewayUpstreamBaseURL = "DONMAI_GATEWAY_UPSTREAM_BASE_URL"

	// EnvGatewayUpstreamAPIKey is the upstream credential the gateway dials
	// with. Falls back to OPENAI_API_KEY (the conventional name for the same
	// value) so an already-configured worker needs no new setting.
	EnvGatewayUpstreamAPIKey = "DONMAI_GATEWAY_UPSTREAM_API_KEY" //nolint:gosec // G101: env-var NAME, not a credential.

	// defaultGatewayUpstreamBaseURL is the direct OpenAI Chat Completions root.
	defaultGatewayUpstreamBaseURL = "https://api.openai.com/v1"

	// gatewayUpstreamTimeout bounds one upstream exchange. Generous enough for
	// a long reasoning turn, bounded so a hung provider cannot pin a session
	// open past its stage budget.
	gatewayUpstreamTimeout = 10 * time.Minute
)

// workerGateway owns the per-session gateway lifecycle for one `donmai agent
// run` process. The zero value and a nil pointer are both valid no-ops, so
// callers need no branch: a session whose cell is not gateway-served gets a nil
// workerGateway and Close is a no-op.
type workerGateway struct {
	gw  *gateway.Gateway
	tok token.Token
}

// isGatewayServed reports whether the resolved profile names the gateway as its
// serving host. ServingHost already rides the daemon's SessionResolvedProfile
// mirror (it is metadata, not a credential), so no wire change is needed to
// detect a gateway cell.
func isGatewayServed(d *daemon.SessionDetail) bool {
	if d == nil || d.ResolvedProfile == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(d.ResolvedProfile.ServingHost), string(agent.HostGateway))
}

// resolveGatewayUpstreamBaseURL validates the worker-local gateway's upstream
// route before the gateway starts. The route is never logged because any URL
// component may be operator-sensitive.
func resolveGatewayUpstreamBaseURL(raw string) (string, error) {
	baseURL := strings.TrimSpace(raw)
	if baseURL == "" {
		baseURL = defaultGatewayUpstreamBaseURL
	}

	parsed, parseErr := url.Parse(baseURL)
	if parseErr != nil || !parsed.IsAbs() || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(baseURL, "#") {
		return "", errors.New("gateway upstream base URL must be an absolute HTTP(S) URL with a non-empty host and no userinfo, query, or fragment")
	}

	return parsed.String(), nil
}

// bindWorkerGateway starts a worker-local gateway, binds this session to it,
// and stamps the resulting EndpointBinding onto qw.ResolvedProfile.Endpoint so
// runner.translateSpec forwards it into agent.Spec (donmai#216) and the harness
// applies it (pi/opencode endpoint read sites). harnessID is the canonical
// loop-driver identity supplied by admission; this function must not infer it
// again from provider/model detail because that could split execution and cost
// attribution.
//
// Returns (nil, nil) when the session is not gateway-served — the overwhelming
// majority of dispatches — leaving qw untouched. Returns an error only when the
// cell IS gateway-served and the worker cannot honor it; the caller surfaces
// that as a preflight failure rather than silently running the session on some
// other endpoint, which is the whole point of this file.
func bindWorkerGateway(ctx context.Context, logger *slog.Logger, d *daemon.SessionDetail, qw *runner.QueuedWork, harnessID string, sinkOverride ...costfeed.Sink) (*workerGateway, error) {
	if !isGatewayServed(d) || qw == nil {
		return nil, nil
	}
	// A binding that already arrived (a future platform/daemon producer) wins:
	// this file fills a gap, it does not override a resolved endpoint.
	if ep := qw.ResolvedProfile.Endpoint; ep != nil && strings.TrimSpace(ep.BaseURL) != "" {
		return nil, nil
	}

	model := strings.TrimSpace(qw.ResolvedProfile.Model)
	if model == "" {
		return nil, errors.New("gateway-served cell carries no model: the gateway serves exactly the bound model, so an unpinned cell cannot be bound")
	}
	if harnessID == "" || harnessID != strings.TrimSpace(harnessID) {
		return nil, errors.New("gateway-served cell carries no valid admitted harness identity")
	}

	key := strings.TrimSpace(os.Getenv(EnvGatewayUpstreamAPIKey))
	if key == "" {
		key = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	}
	if key == "" {
		return nil, fmt.Errorf(
			"gateway-served cell needs an upstream credential in the worker env: set %s (or OPENAI_API_KEY). "+
				"The key stays in this process — the harness child receives only the gateway's per-session bearer",
			EnvGatewayUpstreamAPIKey)
	}

	upstreamBase, err := resolveGatewayUpstreamBaseURL(os.Getenv(EnvGatewayUpstreamBaseURL))
	if err != nil {
		return nil, err
	}

	var sink costfeed.Sink
	if len(sinkOverride) > 0 && sinkOverride[0] != nil {
		sink = sinkOverride[0]
	} else {
		sink = workerGatewaySink(logger)
	}
	g := gateway.New(gateway.Options{Sink: sink})
	if err := g.Start(ctx); err != nil {
		return nil, fmt.Errorf("start worker-local gateway: %w", err)
	}

	binding, err := g.Bind(gateway.BindConfig{
		SessionID:  qw.SessionID,
		DispatchID: qw.SessionID,
		Harness:    harnessID,
		Company:    agent.CompanyOpenAI,
		Model:      model,
		AuthMode:   agent.AuthBYOK,
		Surface:    agent.ProtoOpenAIChat,
		Upstream: &upstream.OpenAICompat{
			Company:    string(agent.CompanyOpenAI),
			BaseURL:    upstreamBase,
			HTTPClient: &http.Client{Timeout: gatewayUpstreamTimeout},
		},
		Source: pool.SingleKey{Key: pool.Credential{ID: "worker-env", Secret: key}},
	})
	if err != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = g.Stop(stopCtx)
		return nil, fmt.Errorf("bind gateway session: %w", err)
	}

	qw.ResolvedProfile.Endpoint = &binding
	if logger != nil {
		// The bearer and upstream route are never logged; only the gateway's own
		// loopback address and the bound model are safe diagnostics.
		logger.Info("agent run: bound worker-local gateway session",
			"sessionId", qw.SessionID,
			"addr", g.Addr(),
			"model", model,
		)
	}
	return &workerGateway{gw: g, tok: token.Token(binding.Env[gateway.TokenEnvVar])}, nil
}

// workerGatewaySink resolves the local JSONL cost ledger, mirroring the
// daemon's startGateway posture: a ledger that cannot be opened degrades to an
// in-memory sink rather than failing the session (metering is observability,
// not a gate at M1).
func workerGatewaySink(logger *slog.Logger) costfeed.Sink {
	ledgerPath := statepath.Resolve("gateway/cost-events.jsonl", "")
	if ledgerPath == "" {
		return &costfeed.MemorySink{}
	}
	ledger, err := costfeed.NewJSONLLedger(ledgerPath)
	if err != nil {
		if logger != nil {
			logger.Warn("agent run: gateway cost ledger unavailable; using in-memory sink", "err", err.Error())
		}
		return &costfeed.MemorySink{}
	}
	return ledger
}

// Close releases the session route and stops the listener. Safe on a nil
// receiver so the caller can defer it unconditionally.
func (w *workerGateway) Close(logger *slog.Logger) {
	if w == nil || w.gw == nil {
		return
	}
	w.gw.Unbind(w.tok)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.gw.Stop(ctx); err != nil && logger != nil {
		logger.Warn("agent run: worker-local gateway stop returned an error", "err", err.Error())
	}
}
