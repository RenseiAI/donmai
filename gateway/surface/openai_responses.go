// Package surface holds the inbound wire surfaces the gateway presents to
// harnesses on loopback (the server side). Each surface DECODES a client
// request into the canonical IR, hands it to the Dispatcher (which selects a
// credential, invokes the chosen upstream, and meters), then ENCODES the reply
// back onto the surface's own wire — streaming incrementally, never buffering
// a whole streamed response.
//
// This file is the OpenAI Responses surface (POST /v1/responses, with SSE). A
// real Chat upstream serves Responses inbound through the canonical IR; the
// surface refuses request fields the hub cannot serve truthfully (see
// translate.DecodeResponsesRequest) rather than relabeling them.
package surface

import (
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

// OpenAIResponses is the http.Handler for the OpenAI Responses surface.
type OpenAIResponses struct {
	disp Dispatcher
	// Now is a clock seam for the response envelope timestamp; nil uses time.Now.
	Now func() time.Time
	// IDFn mints the response id; nil uses a time-based default.
	IDFn func() string
}

// NewOpenAIResponses builds the surface handler over disp.
func NewOpenAIResponses(disp Dispatcher) *OpenAIResponses { return &OpenAIResponses{disp: disp} }

func (h *OpenAIResponses) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *OpenAIResponses) newID() string {
	if h.IDFn != nil {
		return h.IDFn()
	}
	return fmt.Sprintf("resp-gw-%d", h.now().UnixNano())
}

// ServeHTTP handles POST /v1/responses.
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
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read_error", "could not read request body")
		return
	}
	req, err := translate.DecodeResponsesRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed responses request")
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

// writeJSON renders a non-streaming response object and meters it.
func (h *OpenAIResponses) writeJSON(w http.ResponseWriter, req ir.Request, o Outcome) {
	if o.Response == nil {
		writeError(w, http.StatusBadGateway, "empty_response", "upstream returned no response")
		return
	}
	if o.Response.Model == "" {
		o.Response.Model = req.Model
	}
	ids := translate.ResponsesIDs{Response: h.newID(), CreatedAt: h.now().Unix()}
	out, err := json.Marshal(translate.EncodeResponsesResponse(*o.Response, ids))
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

// responsesStreamState tracks the streaming assembly: which output items were
// announced (with stable item ids, in first-seen order), the assembled text,
// and the assembled per-tool arguments — so the terminal done events and the
// completed snapshot carry the full output in exactly the announced order,
// even though every delta streamed incrementally.
type responsesStreamState struct {
	ids     translate.ResponsesIDs
	model   string
	finish  ir.FinishReason
	usage   ir.Usage
	failed  bool
	msg     *responsesMessageSlot
	tools   []*responsesToolSlot
	byIndex map[int]*responsesToolSlot
	// order records announcement order: each entry is either *responsesMessageSlot
	// or *responsesToolSlot, so done events and the snapshot share one order.
	order []any
}

type responsesMessageSlot struct {
	itemID string
	text   string
}

type responsesToolSlot struct {
	index     int
	itemID    string
	name      string
	args      string
	announced bool
}

func (h *OpenAIResponses) streamSSE(w http.ResponseWriter, req ir.Request, o Outcome) {
	if o.Stream == nil {
		writeError(w, http.StatusBadGateway, "empty_stream", "upstream returned no stream")
		return
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	model := o.Model
	if model == "" {
		model = req.Model
	}
	st := &responsesStreamState{
		ids:     translate.ResponsesIDs{Response: h.newID(), CreatedAt: h.now().Unix()},
		model:   model,
		finish:  ir.FinishStop,
		byIndex: map[int]*responsesToolSlot{},
	}
	emit := func(ev translate.ResponsesStreamEvent) {
		raw, err := json.Marshal(ev)
		if err != nil {
			return // skip an unencodable event rather than tear down the stream
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
		if flusher != nil {
			flusher.Flush()
		}
	}

	emit(translate.ResponsesCreated(st.ids, model))
	for delta := range o.Stream.Deltas {
		if delta.Usage != nil {
			st.usage = *delta.Usage
		}
		if delta.Finish != "" {
			st.finish = delta.Finish
		}
		if delta.TextDelta != "" {
			if st.msg == nil {
				st.msg = &responsesMessageSlot{itemID: st.ids.Response + "-msg-0"}
				st.order = append(st.order, st.msg)
				emit(translate.ResponsesMessageAdded(len(st.order)-1, st.msg.itemID))
			}
			st.msg.text += delta.TextDelta
			emit(translate.ResponsesTextDelta(outputPosition(st, st.msg), st.msg.itemID, delta.TextDelta))
		}
		if tc := delta.ToolCallDelta; tc != nil {
			slot, ok := st.byIndex[tc.Index]
			if !ok {
				slot = &responsesToolSlot{index: tc.Index, itemID: fmt.Sprintf("%s-fc-%d", st.ids.Response, tc.Index)}
				if tc.ID != "" {
					slot.itemID = tc.ID
				}
				slot.name = tc.Name
				st.byIndex[tc.Index] = slot
				st.tools = append(st.tools, slot)
			}
			if slot.name == "" {
				slot.name = tc.Name
			}
			if !slot.announced {
				st.order = append(st.order, slot)
				emit(translate.ResponsesFunctionCallAdded(len(st.order)-1, slot.itemID, slot.name))
				slot.announced = true
			}
			if tc.ArgsDelta != "" {
				slot.args += tc.ArgsDelta
				emit(translate.ResponsesArgsDelta(outputPosition(st, slot), slot.itemID, tc.ArgsDelta))
			}
		}
	}
	if err := o.Stream.Err(); err != nil {
		st.failed = true
	}
	emitTerminal(st, emit)
	if flusher != nil {
		flusher.Flush()
	}
	if o.Meter != nil {
		o.Meter(st.usage, st.finish)
	}
}

// outputPosition returns the stable output index of an announced slot: its
// position in announcement order, shared by deltas, done events, and the
// terminal snapshot.
func outputPosition(st *responsesStreamState, target any) int {
	for i, s := range st.order {
		if s == target {
			return i
		}
	}
	return 0
}

// emitTerminal writes the per-item done events in announcement order and then
// the final snapshot. A mid-stream transport failure surfaces as
// response.failed with the assembled output so far — a partial success is
// never relabeled complete.
func emitTerminal(st *responsesStreamState, emit func(translate.ResponsesStreamEvent)) {
	finish := st.finish
	if st.failed {
		finish = ir.FinishError
	}
	obj := translate.EncodeResponsesOrderedResponse(st.model, st.usage, finish, st.ids, st.orderedItems())
	for pos, item := range st.order {
		switch s := item.(type) {
		case *responsesMessageSlot:
			emit(translate.ResponsesTextDone(pos, s.itemID, s.text))
		case *responsesToolSlot:
			emit(translate.ResponsesFunctionCallDone(pos, s.itemID, s.args))
		}
	}
	emit(translate.ResponsesTerminal(obj))
}

// orderedItems renders the announced slots as snapshot output items in
// announcement order.
func (st *responsesStreamState) orderedItems() []translate.ResponsesOutputItem {
	var out []translate.ResponsesOutputItem
	for _, item := range st.order {
		switch s := item.(type) {
		case *responsesMessageSlot:
			out = append(out, translate.ResponsesOutputItem{
				Type:    "message",
				ID:      s.itemID,
				Status:  "completed",
				Role:    "assistant",
				Content: []translate.ResponsesOutputContent{{Type: "output_text", Text: s.text}},
			})
		case *responsesToolSlot:
			out = append(out, translate.ResponsesOutputItem{
				Type:      "function_call",
				ID:        s.itemID,
				CallID:    s.itemID,
				Status:    "completed",
				Name:      s.name,
				Arguments: s.args,
			})
		}
	}
	if out == nil {
		out = []translate.ResponsesOutputItem{}
	}
	return out
}
