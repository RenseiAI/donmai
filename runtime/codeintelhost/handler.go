package codeintelhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	mcpserver "github.com/RenseiAI/donmai/runtime/mcp/server"
)

// defaultMaxBodyBytes bounds the /v1/tools/call request body. A tool-call
// arguments payload is small (identifiers, search queries); 1 MiB is
// generous headroom while still bounding a malicious/misbehaving caller.
const defaultMaxBodyBytes = 1 << 20

// HandlerConfig configures a Handler.
type HandlerConfig struct {
	// Verifier authenticates every request's bearer token. Required.
	Verifier *Verifier
	// Pool acquires the leased workarea for each request's binding. Required.
	Pool *Pool
	// MaxConcurrentCalls bounds the number of admitted in-flight requests via
	// a non-blocking global semaphore. Must be positive.
	MaxConcurrentCalls int
	// RequestTimeout bounds each request's acquisition wait plus tool
	// dispatch. Zero means no per-request timeout beyond the client's own
	// context.
	RequestTimeout time.Duration
	// MaxBodyBytes overrides defaultMaxBodyBytes when positive.
	MaxBodyBytes int64
	// Logf receives diagnostic log lines; nil discards them.
	Logf func(format string, args ...any)
}

// Handler serves the fixed wire contract: POST /v1/tools/call, GET
// /healthz, GET /readyz.
type Handler struct {
	verifier     *Verifier
	pool         *Pool
	sem          chan struct{}
	reqTimeout   time.Duration
	maxBodyBytes int64
	logf         func(format string, args ...any)
}

// NewHandler validates cfg and constructs a Handler.
func NewHandler(cfg HandlerConfig) (*Handler, error) {
	if cfg.Verifier == nil {
		return nil, errors.New("code intel host: handler requires a verifier")
	}
	if cfg.Pool == nil {
		return nil, errors.New("code intel host: handler requires a pool")
	}
	if cfg.MaxConcurrentCalls <= 0 {
		return nil, errors.New("code intel host: handler requires a positive max concurrent calls")
	}
	maxBody := cfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBodyBytes
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Handler{
		verifier:     cfg.Verifier,
		pool:         cfg.Pool,
		sem:          make(chan struct{}, cfg.MaxConcurrentCalls),
		reqTimeout:   cfg.RequestTimeout,
		maxBodyBytes: maxBody,
		logf:         logf,
	}, nil
}

// Routes returns the http.Handler serving the fixed wire contract, ready to
// mount on an *http.Server.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tools/call", h.handleToolsCall)
	mux.HandleFunc("/healthz", h.handleHealthz)
	mux.HandleFunc("/readyz", h.handleReadyz)
	return mux
}

func (h *Handler) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if h.pool.Closed() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("draining"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// callRequest is the fixed /v1/tools/call request body shape.
type callRequest struct {
	Tool         string          `json:"tool"`
	Arguments    json.RawMessage `json:"arguments"`
	InvocationID string          `json:"invocationId"`
	Binding      Binding         `json:"binding"`
}

func (h *Handler) handleToolsCall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.writeResult(w, http.StatusMethodNotAllowed, errResult(errors.New("method not allowed")))
		return
	}

	token, err := bearerToken(r.Header.Get("Authorization"))
	if err != nil {
		h.writeResult(w, http.StatusUnauthorized, errResult(err))
		return
	}

	req, err := h.decodeRequest(w, r)
	if err != nil {
		h.writeResult(w, http.StatusBadRequest, errResult(err))
		return
	}
	if err := req.Binding.Validate(); err != nil {
		h.writeResult(w, http.StatusBadRequest, errResult(fmt.Errorf("binding: %w", err)))
		return
	}
	if req.Tool == "" {
		h.writeResult(w, http.StatusBadRequest, errResult(errors.New("tool is required")))
		return
	}
	if req.InvocationID == "" {
		h.writeResult(w, http.StatusBadRequest, errResult(errors.New("invocationId is required")))
		return
	}

	claims, err := h.verifier.Verify(token)
	if err != nil {
		h.writeResult(w, http.StatusUnauthorized, errResult(err))
		return
	}
	if err := claims.MatchesRequest(req.InvocationID, req.Binding); err != nil {
		h.writeResult(w, http.StatusForbidden, errResult(err))
		return
	}

	select {
	case h.sem <- struct{}{}:
	default:
		h.writeResult(w, http.StatusOK, busyResult())
		return
	}

	ctx := r.Context()
	if h.reqTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.reqTimeout)
		defer cancel()
	}

	lease, err := h.pool.Acquire(ctx, req.Binding)
	if err != nil {
		<-h.sem
		h.writeResult(w, http.StatusOK, toolErrorResultFor(err))
		return
	}

	// Held-binding equality recheck: the lease's workarea must have been
	// warmed for exactly the authenticated/requested binding. This should
	// always hold by construction (Pool keys entries by Binding.Key()) —
	// the check is a defense-in-depth backstop, not the primary guarantee.
	if !lease.Binding().Equal(req.Binding) {
		lease.Release()
		<-h.sem
		h.writeResult(w, http.StatusForbidden,
			errResult(fmt.Errorf("%w: leased workarea binding does not match request", ErrBindingMismatch)))
		return
	}

	// dispatchToolCall takes ownership of releasing both lease and h.sem
	// exactly once, on every path — including a soft timeout, where they are
	// deliberately NOT released here (see its doc).
	h.writeResult(w, http.StatusOK, h.dispatchToolCall(ctx, lease, req.Tool, req.Arguments))
}

// dispatchToolCall runs lease.Call and takes ownership of releasing lease
// and the global admission slot (h.sem) exactly once, regardless of which
// path returns first.
//
// The native engine has no context-aware mid-call abort (see
// mcpserver.Server.Call): once a tool call is running, a caller's context
// deadline/cancellation cannot stop it. So when ctx fires before the call
// finishes, dispatchToolCall returns a stable, non-sensitive timeout result
// promptly — but a background goroutine keeps the lease and semaphore slot
// held until the real Call actually returns. This is required for
// correctness, not just tidiness: releasing the lease/slot early would let
// the pool evict the workarea (or admit a conflicting concurrent warm)
// while the abandoned call is still reading/writing it, and would let
// h.sem admit more concurrent work than MaxConcurrentCalls actually bounds.
// ctx is threaded into Call so a future context-aware engine method can
// still abort promptly; today it is advisory only.
func (h *Handler) dispatchToolCall(ctx context.Context, lease *Lease, tool string, args json.RawMessage) mcpserver.ToolResult {
	type outcome struct {
		result mcpserver.ToolResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := lease.Call(ctx, tool, args)
		done <- outcome{result, err}
	}()

	select {
	case out := <-done:
		lease.Release()
		<-h.sem
		if out.err != nil {
			return toolErrorResultFor(out.err)
		}
		return out.result
	case <-ctx.Done():
		go func() {
			<-done
			lease.Release()
			<-h.sem
		}()
		return timeoutResult()
	}
}

// decodeRequest strictly decodes r's JSON body into a callRequest: unknown
// top-level fields, trailing content, and a body over maxBodyBytes are all
// rejected rather than silently accepted.
func (h *Handler) decodeRequest(w http.ResponseWriter, r *http.Request) (callRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req callRequest
	if err := dec.Decode(&req); err != nil {
		return callRequest{}, fmt.Errorf("decode request body: %w", err)
	}
	if dec.More() {
		return callRequest{}, errors.New("request body must contain exactly one JSON object")
	}
	return req, nil
}

// bearerToken extracts the token from a "Bearer <token>" Authorization
// header value.
func bearerToken(header string) (string, error) {
	if header == "" {
		return "", fmt.Errorf("%w: missing authorization header", ErrUnauthorized)
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", fmt.Errorf("%w: authorization header must use the Bearer scheme", ErrUnauthorized)
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if token == "" {
		return "", fmt.Errorf("%w: empty bearer token", ErrUnauthorized)
	}
	return token, nil
}

func (h *Handler) writeResult(w http.ResponseWriter, status int, result mcpserver.ToolResult) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(result); err != nil {
		h.logf("encode tool result: %v", err)
	}
}

// errResult wraps a Go error as a frozen isError ToolResult. Authentication
// and malformed-protocol refusals still return this exact body shape, only
// under a non-2xx status code.
func errResult(err error) mcpserver.ToolResult {
	return errorResult(err.Error())
}

func errorResult(text string) mcpserver.ToolResult {
	return mcpserver.ToolResult{
		IsError: true,
		Content: []mcpserver.ContentItem{{Type: "text", Text: text}},
	}
}

// busyResult is the stable operation-error text for the active-call bound
// (design E.1): a non-blocking semaphore refusal, always HTTP 200 with
// isError true so the platform's synchronous caller sees a known,
// non-retryable-without-backoff signal rather than a queued request.
func busyResult() mcpserver.ToolResult {
	return errorResult("code_intel_host_busy")
}

// timeoutResult is the stable, non-sensitive operation-error text returned
// when the request context's deadline/cancellation fires while a tool call
// is still in flight (dispatchToolCall): always HTTP 200 with isError true,
// mirroring busyResult's non-blocking-refusal shape. It deliberately never
// echoes ctx.Err()'s own text.
func timeoutResult() mcpserver.ToolResult {
	return errorResult("code_intel_host_call_timeout")
}

// toolErrorResultFor maps a pool/dispatch error to its stable semantic
// operation-error ToolResult (always HTTP 200, isError true) per the
// design's "unknown tools, unavailable repositories/revisions, acquisition
// failures, timeouts, cancellation, and capacity exhaustion" list.
func toolErrorResultFor(err error) mcpserver.ToolResult {
	if errors.Is(err, ErrAtCapacity) {
		return busyResult()
	}
	return errResult(err)
}
