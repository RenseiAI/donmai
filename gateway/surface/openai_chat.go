// Package surface holds the inbound wire surfaces the gateway presents to
// harnesses on loopback (the server side). Each surface DECODES a client
// request into the canonical IR, hands it to the Dispatcher (which selects a
// credential, invokes the chosen upstream, and meters), then ENCODES the reply
// back onto the surface's own wire — streaming incrementally, never buffering a
// whole streamed response.
//
// M1 ships the OpenAI Chat Completions surface (POST /v1/chat/completions,
// with SSE). The Anthropic Messages and Gemini surfaces land in later
// milestones behind this same Dispatcher seam.
package surface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/RenseiAI/donmai/gateway/ir"
	"github.com/RenseiAI/donmai/gateway/token"
	"github.com/RenseiAI/donmai/gateway/translate"
)

// Dispatcher resolves a per-session token to its route, invokes the upstream,
// and returns the Outcome. It is implemented by *gateway.Gateway. Keeping the
// surface behind this narrow interface keeps the HTTP/codec concern out of the
// gateway core and lets each surface be tested against a fake dispatcher.
type Dispatcher interface {
	Dispatch(ctx context.Context, tok token.Token, req ir.Request) (Outcome, error)
}

// Outcome is a resolved dispatch. Exactly one of Stream / Response is set on
// success (mirroring req.Stream). Meter is invoked by the surface once, after
// the reply is fully written, with the final usage and finish reason — so a
// streamed exchange is metered from the assembled terminal delta.
type Outcome struct {
	Stream   *ir.Stream
	Response *ir.Response
	Model    string
	Meter    func(usage ir.Usage, finish ir.FinishReason)
}

// HTTPError is a dispatch failure carrying the client-facing HTTP status. The
// gateway returns it; the surface renders it as an OpenAI-shaped error body.
type HTTPError struct {
	Status  int
	Code    string
	Message string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("gateway: %d %s", e.Status, e.Message) }

// OpenAIChat is the http.Handler for the OpenAI Chat Completions surface.
type OpenAIChat struct {
	disp Dispatcher
	// Now is a clock seam for the response envelope timestamp; nil uses time.Now.
	Now func() time.Time
	// IDFn mints the response id; nil uses a time-based default.
	IDFn func() string
}

// NewOpenAIChat builds the surface handler over disp.
func NewOpenAIChat(disp Dispatcher) *OpenAIChat { return &OpenAIChat{disp: disp} }

func (h *OpenAIChat) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *OpenAIChat) newID() string {
	if h.IDFn != nil {
		return h.IDFn()
	}
	return fmt.Sprintf("chatcmpl-gw-%d", h.now().UnixNano())
}

// ServeHTTP handles POST /v1/chat/completions.
func (h *OpenAIChat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	tok := token.FromBearer(r.Header.Get("Authorization"))
	if tok == "" {
		writeError(w, http.StatusUnauthorized, "missing_token", "missing session bearer token")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_error", "could not read request body")
		return
	}
	req, err := translate.DecodeRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed chat completions request")
		return
	}

	outcome, err := h.disp.Dispatch(r.Context(), tok, req)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) {
			writeError(w, he.Status, he.Code, he.Message)
			return
		}
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	if req.Stream {
		h.streamSSE(w, req, outcome)
		return
	}
	h.writeJSON(w, req, outcome)
}

// writeJSON renders a non-streaming response and meters it.
func (h *OpenAIChat) writeJSON(w http.ResponseWriter, req ir.Request, o Outcome) {
	if o.Response == nil {
		writeError(w, http.StatusBadGateway, "empty_response", "upstream returned no response")
		return
	}
	if o.Response.Model == "" {
		o.Response.Model = req.Model
	}
	out, err := translate.EncodeResponse(*o.Response, h.newID(), h.now().Unix())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode_error", "could not encode response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	if o.Meter != nil {
		o.Meter(o.Response.Usage, o.Response.FinishReason)
	}
}

// streamSSE relays the upstream IR stream as OpenAI chat.completion.chunk SSE,
// accumulating usage/finish for a single terminal Meter call.
func (h *OpenAIChat) streamSSE(w http.ResponseWriter, req ir.Request, o Outcome) {
	if o.Stream == nil {
		writeError(w, http.StatusBadGateway, "empty_stream", "upstream returned no stream")
		return
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	id := h.newID()
	created := h.now().Unix()
	model := o.Model
	if model == "" {
		model = req.Model
	}

	var (
		usage  ir.Usage
		finish = ir.FinishStop
	)
	for delta := range o.Stream.Deltas {
		if delta.Usage != nil {
			usage = *delta.Usage
		}
		if delta.Finish != "" {
			finish = delta.Finish
		}
		chunk, err := translate.EncodeStreamChunk(delta, id, model, created)
		if err != nil {
			continue // skip an unencodable chunk rather than tear down the stream
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		if flusher != nil {
			flusher.Flush()
		}
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	if o.Meter != nil {
		o.Meter(usage, finish)
	}
}

// ─── OpenAI-shaped error body ────────────────────────────────────────────────

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: errorBody{Message: msg, Type: "gateway_error", Code: code}})
}
