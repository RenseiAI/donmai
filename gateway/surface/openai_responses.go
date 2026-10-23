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
		// Decode errors name the refused field or value (never upstream text).
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
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
		model := out.Model
		if model == "" {
			model = req.Model
		}
		h.stream(r.Context(), w, out, model)
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

// chatSlot is one output item assembled from a Chat upstream stream: the
// assistant message or one function call, at the output index it was first
// announced with. Deltas, done events and the completed snapshot all share
// that announcement order.
type chatSlot struct {
	item   ir.Item
	output int
	text   string // message text or function-call arguments
}

func (h *OpenAIResponses) stream(ctx context.Context, w http.ResponseWriter, out Outcome, model string) {
	if out.Stream == nil {
		writeError(w, http.StatusBadGateway, "empty_stream", "upstream returned no stream")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	var sequence int
	send := func(event ir.ResponsesStreamEvent) bool {
		if event.SequenceNumber == nil {
			n := sequence
			event.SequenceNumber = &n
		}
		sequence++
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
	var order []*chatSlot
	var message *chatSlot
	tools := map[int]*chatSlot{}
	var streamFailure bool
	var native bool
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
			if !send(ir.ResponsesStreamEvent{Kind: "response.created", ResponseID: id, Response: &ir.Response{Model: model, Details: &ir.ResponseDetails{Object: "response", Status: "in_progress"}}}) {
				streamFailure = true
				continue
			}
			created = true
		}
		if delta.TextDelta != "" {
			if message == nil {
				message = &chatSlot{item: ir.Item{Kind: "message", ID: id + "_message", Role: ir.RoleAssistant}, output: len(order)}
				order = append(order, message)
				added := message.item
				added.Status = "in_progress"
				if !send(ir.ResponsesStreamEvent{Kind: "response.output_item.added", OutputIndex: message.output, Item: &added}) ||
					!send(ir.ResponsesStreamEvent{Kind: "response.content_part.added", OutputIndex: message.output, ItemID: message.item.ID, Part: &ir.Part{Kind: ir.PartText}}) {
					streamFailure = true
				}
			}
			message.text += delta.TextDelta
			if !send(ir.ResponsesStreamEvent{Kind: "response.output_text.delta", OutputIndex: message.output, ItemID: message.item.ID, Delta: delta.TextDelta}) {
				streamFailure = true
				continue
			}
		}
		if fragment := delta.ToolCallDelta; fragment != nil {
			call := tools[fragment.Index]
			if call == nil {
				// The first fragment of a Chat tool call names it; a call that
				// cannot be announced with its identity is not representable.
				if fragment.ID == "" || fragment.Name == "" {
					streamFailure = true
					continue
				}
				call = &chatSlot{item: ir.Item{Kind: "function_call", ID: fmt.Sprintf("%s_function_%d", id, fragment.Index), CallID: fragment.ID, Name: fragment.Name}, output: len(order)}
				tools[fragment.Index] = call
				order = append(order, call)
				added := call.item
				added.Status = "in_progress"
				if !send(ir.ResponsesStreamEvent{Kind: "response.output_item.added", OutputIndex: call.output, Item: &added}) {
					streamFailure = true
					continue
				}
			} else if fragment.ID != "" && fragment.ID != call.item.CallID || fragment.Name != "" && fragment.Name != call.item.Name {
				streamFailure = true
				continue
			}
			if fragment.ArgsDelta != "" {
				call.text += fragment.ArgsDelta
				if !send(ir.ResponsesStreamEvent{Kind: "response.function_call_arguments.delta", OutputIndex: call.output, ItemID: call.item.ID, Delta: fragment.ArgsDelta}) {
					streamFailure = true
					continue
				}
			}
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
		snapshot := &ir.Response{Model: model}
		for _, slot := range order {
			done := slot.item
			done.Status = "completed"
			var sent bool
			if done.Kind == "message" {
				done.Parts = []ir.Part{{Kind: ir.PartText, Text: slot.text}}
				sent = send(ir.ResponsesStreamEvent{Kind: "response.output_text.done", OutputIndex: slot.output, ItemID: done.ID, Delta: slot.text}) &&
					send(ir.ResponsesStreamEvent{Kind: "response.content_part.done", OutputIndex: slot.output, ItemID: done.ID, Part: &ir.Part{Kind: ir.PartText, Text: slot.text}})
			} else {
				done.Arguments = slot.text
				sent = send(ir.ResponsesStreamEvent{Kind: "response.function_call_arguments.done", OutputIndex: slot.output, ItemID: done.ID, Delta: slot.text})
			}
			if !sent || !send(ir.ResponsesStreamEvent{Kind: "response.output_item.done", OutputIndex: slot.output, Item: &done}) {
				failed()
				return
			}
			snapshot.Items = append(snapshot.Items, done)
		}
		completed = &ir.ResponsesStreamEvent{Kind: "response.completed", ResponseID: id, Usage: usage, Response: snapshot}
	}
	if usage == nil || !created || !send(*completed) {
		failed()
		return
	}
	if out.Meter != nil {
		out.Meter(*usage, finish)
	}
}
