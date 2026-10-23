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
		// Decode errors carry the codec refusal, which names a refused value
		// such as an unknown effort; they never carry upstream text.
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
	resp := out.Response
	if resp == nil || !resp.UsageReported {
		// Without reported usage there is nothing truthful to serve or meter.
		writeError(w, http.StatusBadGateway, "invalid_upstream", "upstream omitted response or usage")
		return
	}
	// The upstream finished and reported its usage, so the exchange is
	// metered exactly once whatever the client is served. A length or
	// content-filter finish is served as incomplete, never as completed.
	if out.Meter != nil {
		defer out.Meter(resp.Usage, resp.FinishReason)
	}
	if resp.Model == "" {
		resp.Model = req.Model
	}
	encoded, err := translate.EncodeResponsesResponse(*resp, h.newID())
	if err != nil {
		writeError(w, http.StatusBadGateway, "invalid_upstream", "upstream response cannot be represented")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

// chatSlot is one output item assembled from a Chat upstream stream: the
// assistant message or one function call, at the output index it was first
// announced with. Deltas, done events and the terminal snapshot all share
// that announcement order.
type chatSlot struct {
	item   ir.Item
	output int
	text   string // message text or function-call arguments
}

// chatFinished reports whether a Chat finish ends a turn the Responses
// surface can serve: completed (stop, tool_calls) or incomplete (length,
// content_filter). A stream that ends without one was truncated.
func chatFinished(finish ir.FinishReason) bool {
	switch finish {
	case ir.FinishStop, ir.FinishToolCalls, ir.FinishLength, ir.FinishContentFilter:
		return true
	default:
		return false
	}
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
	id := h.newID()
	failed := func() { _ = send(ir.ResponsesStreamEvent{Kind: "response.failed", ResponseID: id}) }
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
			failed() // the upstream has not finished: nothing is metered
			return
		case next, ok := <-out.Stream.Deltas:
			if !ok {
				break streamLoop
			}
			delta = next
		}
		// Once the stream has failed nothing more is forwarded, but the
		// upstream is still drained so its terminal usage is known.
		if delta.Responses != nil {
			event := *delta.Responses
			native = true
			switch {
			case event.Kind != "response.created" && (!created || event.ResponseID != "" && event.ResponseID != id):
				streamFailure = true
			case event.Kind == "response.created":
				if created {
					streamFailure = true
					continue
				}
				created = true
				id = event.ResponseID
				if !streamFailure && !send(event) {
					streamFailure = true
				}
			case event.Kind == "response.completed" || event.Kind == "response.incomplete":
				if completed != nil {
					streamFailure = true
					continue
				}
				terminal := event
				completed = &terminal
				usage = event.Usage // sent only after Stream.Err is known
				if event.Response != nil {
					finish = event.Response.FinishReason
				}
			default:
				if !streamFailure && !send(event) {
					streamFailure = true
				}
			}
			continue
		}
		if native || delta.ThinkingDelta != "" {
			streamFailure = true
		}
		if delta.Finish != "" {
			finish = delta.Finish
		}
		if delta.Usage != nil {
			usage = delta.Usage
		}
		if streamFailure {
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
					continue
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
				streamFailure = true // a drifted identity is never merged into the first call
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
	}
	upstreamFailed := out.Stream.Err != nil && out.Stream.Err() != nil
	finished := completed != nil
	if !native {
		finished = chatFinished(finish)
	}
	// An upstream that finished its turn and reported usage is metered
	// exactly once, whatever the client is served: on a clean stream from the
	// terminal it reported, and on a stream the reader refused from the usage
	// the upstream still billed. A truncated or cancelled stream that never
	// finished has no usage to meter.
	if out.Meter != nil {
		if !upstreamFailed && finished && usage != nil {
			defer out.Meter(*usage, finish)
		} else if upstreamFailed && out.Stream.RefusedUsage != nil {
			if billed := out.Stream.RefusedUsage(); billed != nil {
				defer out.Meter(*billed, "")
			}
		}
	}
	if streamFailure || upstreamFailed || ctx.Err() != nil || !finished || usage == nil || !created {
		failed()
		return
	}
	if completed == nil {
		snapshot := &ir.Response{Model: model, FinishReason: finish}
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
		kind := "response.completed"
		if finish == ir.FinishLength || finish == ir.FinishContentFilter {
			kind = "response.incomplete"
		}
		completed = &ir.ResponsesStreamEvent{Kind: kind, ResponseID: id, Usage: usage, Response: snapshot}
	}
	if !send(*completed) {
		failed()
	}
}
