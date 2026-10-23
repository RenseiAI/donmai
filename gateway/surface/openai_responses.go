package surface

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/gateway/ir"
	"github.com/RenseiAI/donmai/gateway/token"
	"github.com/RenseiAI/donmai/gateway/translate"
)

// ResponsesDispatcher uses the gateway's exact surface-bound token route.
type ResponsesDispatcher interface {
	DispatchProtocol(context.Context, token.Token, agent.WireProtocol, ir.Request) (Outcome, error)
}

// OpenAIResponses serves the stateless Responses HTTP/SSE surface.
type OpenAIResponses struct {
	disp ResponsesDispatcher
	IDFn func() string
}

// NewOpenAIResponses binds the surface to the gateway dispatcher.
func NewOpenAIResponses(disp ResponsesDispatcher) *OpenAIResponses {
	return &OpenAIResponses{disp: disp}
}

func (h *OpenAIResponses) newID() string {
	if h.IDFn != nil {
		return h.IDFn()
	}
	return fmt.Sprintf("resp_gw_%d", time.Now().UnixNano())
}

// ServeHTTP strictly decodes and dispatches one surface-bound exchange.
func (h *OpenAIResponses) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	tok := token.FromBearer(r.Header.Get("Authorization"))
	if tok == "" {
		writeError(w, http.StatusUnauthorized, "missing_token", "missing session bearer token")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, (32<<20)+1))
	if err != nil || len(body) > 32<<20 {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid responses body")
		return
	}
	req, err := translate.DecodeResponsesRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "unsupported responses request")
		return
	}
	out, err := h.disp.DispatchProtocol(r.Context(), tok, agent.ProtoOpenAIResponses, req)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) {
			writeError(w, he.Status, he.Code, he.Message)
		} else {
			writeError(w, http.StatusBadGateway, "upstream_error", "upstream request failed")
		}
		return
	}
	if req.Stream {
		h.stream(r.Context(), w, out)
		return
	}
	if out.Response == nil || !out.Response.UsageReported || (out.Response.FinishReason != "" && out.Response.FinishReason != ir.FinishStop && out.Response.FinishReason != ir.FinishToolCalls) {
		writeError(w, http.StatusBadGateway, "invalid_upstream", "upstream omitted response or usage")
		return
	}
	if out.Response.Model == "" {
		out.Response.Model = req.Model
	}
	encoded, err := translate.EncodeResponsesResponse(*out.Response, h.newID())
	if err != nil {
		writeError(w, http.StatusBadGateway, "invalid_upstream", "upstream response cannot be represented")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err = w.Write(encoded); err != nil || r.Context().Err() != nil {
		return
	}
	if out.Meter != nil {
		out.Meter(out.Response.Usage, out.Response.FinishReason)
	}
}

func (h *OpenAIResponses) stream(ctx context.Context, w http.ResponseWriter, out Outcome) {
	if out.Stream == nil {
		writeError(w, http.StatusBadGateway, "empty_stream", "upstream returned no stream")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	send := func(event ir.ResponsesStreamEvent) bool {
		body, err := translate.EncodeResponsesStreamEvent(event)
		if err != nil {
			return false
		}
		if _, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Kind, body); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	failed := func() {
		_, _ = io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"gateway_stream_failed\",\"message\":\"upstream stream failed\"}}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	id := h.newID()
	created := false
	var completed *ir.ResponsesStreamEvent
	var usage *ir.Usage
	var finish ir.FinishReason
	var text string
	tools := map[int]*ir.ToolCall{}
	var streamFailure bool
	var native bool
	var textStarted bool
	var sequence int
	originalSend := send
	send = func(event ir.ResponsesStreamEvent) bool {
		if event.SequenceNumber == nil {
			n := sequence
			event.SequenceNumber = &n
		}
		sequence++
		return originalSend(event)
	}
streamLoop:
	for {
		var delta ir.StreamDelta
		select {
		case <-ctx.Done():
			failed()
			return
		case next, ok := <-out.Stream.Deltas:
			if !ok {
				break streamLoop
			}
			delta = next
		}
		if delta.Responses != nil {
			event := *delta.Responses
			native = true
			if event.Kind != "response.created" && (!created || event.ResponseID != "" && event.ResponseID != id) {
				streamFailure = true
				continue
			}
			if event.Kind == "response.created" {
				if created {
					streamFailure = true
					continue
				}
				created = true
				id = event.ResponseID
			}
			if event.Kind == "response.completed" {
				if completed != nil {
					streamFailure = true
					continue
				}
				terminal := event
				completed = &terminal
				usage = event.Usage
				continue // terminal success is sent only after Stream.Err is known
			}
			if !send(event) {
				streamFailure = true
				continue
			}
			continue
		}
		if native {
			streamFailure = true
			continue
		}
		if delta.ThinkingDelta != "" {
			streamFailure = true
			continue
		}
		// A Chat upstream can serve only a representable text/function subset.
		if !created {
			if !send(ir.ResponsesStreamEvent{Kind: "response.created", ResponseID: id}) {
				streamFailure = true
				continue
			}
			created = true
		}
		if delta.TextDelta != "" {
			if !textStarted {
				textStarted = true
				if !send(ir.ResponsesStreamEvent{Kind: "response.output_item.added", Item: &ir.Item{Kind: "message", ID: id + "_message", Status: "in_progress", Role: ir.RoleAssistant}}) || !send(ir.ResponsesStreamEvent{Kind: "response.content_part.added", ItemID: id + "_message", Part: &ir.Part{Kind: ir.PartText}}) {
					streamFailure = true
				}
			}
			text += delta.TextDelta
			if !send(ir.ResponsesStreamEvent{Kind: "response.output_text.delta", ItemID: id + "_message", Delta: delta.TextDelta}) {
				streamFailure = true
				continue
			}
		}
		if fragment := delta.ToolCallDelta; fragment != nil {
			call := tools[fragment.Index]
			if call == nil {
				call = &ir.ToolCall{}
				tools[fragment.Index] = call
			}
			if fragment.ID != "" {
				call.ID = fragment.ID
			}
			if fragment.Name != "" {
				call.Name = fragment.Name
			}
			call.Arguments += fragment.ArgsDelta
		}
		if delta.Finish != "" {
			finish = delta.Finish
		}
		if delta.Usage != nil {
			usage = delta.Usage
		}
	}
	if streamFailure || out.Stream.Err != nil && out.Stream.Err() != nil || ctx.Err() != nil {
		failed()
		return
	}
	if completed == nil {
		if native || !created || (finish != ir.FinishStop && finish != ir.FinishToolCalls) || usage == nil {
			failed()
			return
		}
		if text != "" {
			if !send(ir.ResponsesStreamEvent{Kind: "response.output_text.done", ItemID: id + "_message", Delta: text}) || !send(ir.ResponsesStreamEvent{Kind: "response.content_part.done", ItemID: id + "_message", Part: &ir.Part{Kind: ir.PartText, Text: text}}) {
				failed()
				return
			}
			if !send(ir.ResponsesStreamEvent{Kind: "response.output_item.done", Item: &ir.Item{Kind: "message", ID: id + "_message", Status: "completed", Role: ir.RoleAssistant, Parts: []ir.Part{{Kind: ir.PartText, Text: text}}}}) {
				failed()
				return
			}
		}
		for index := 0; index < len(tools); index++ {
			call := tools[index]
			if call == nil || call.ID == "" || call.Name == "" {
				failed()
				return
			}
			item := &ir.Item{Kind: "function_call", ID: fmt.Sprintf("%s_function_%d", id, index), Status: "in_progress", CallID: call.ID, Name: call.Name}
			if !send(ir.ResponsesStreamEvent{Kind: "response.output_item.added", OutputIndex: index + 1, Item: item}) || !send(ir.ResponsesStreamEvent{Kind: "response.function_call_arguments.delta", OutputIndex: index + 1, ItemID: item.ID, Delta: call.Arguments}) || !send(ir.ResponsesStreamEvent{Kind: "response.function_call_arguments.done", OutputIndex: index + 1, ItemID: item.ID, Delta: call.Arguments}) {
				failed()
				return
			}
			item.Status = "completed"
			item.Arguments = call.Arguments
			if !send(ir.ResponsesStreamEvent{Kind: "response.output_item.done", OutputIndex: index + 1, Item: item}) {
				failed()
				return
			}
		}
		completed = &ir.ResponsesStreamEvent{Kind: "response.completed", ResponseID: id, Usage: usage}
	}
	if usage == nil || !created || !send(*completed) {
		failed()
		return
	}
	if out.Meter != nil {
		out.Meter(*usage, finish)
	}
}
