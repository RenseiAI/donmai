package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeServer is an httptest server that speaks the subset of the opencode v2
// /api/ surface clientV1 uses, with hooks for asserting request bodies.
type fakeServer struct {
	t          *testing.T
	createBody json.RawMessage
	promptBody json.RawMessage
	replyBody  json.RawMessage
	aborted    string
	pending    []permissionRequest
	sseFrames  []string // raw "data: {json}" payloads to stream on /api/event
}

func (f *fakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true})
	})
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.createBody = json.RawMessage(body)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ses_created"}})
	})
	mux.HandleFunc("/api/session/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/prompt"):
			body, _ := io.ReadAll(r.Body)
			f.promptBody = json.RawMessage(body)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"admittedSeq": 1}})
		case strings.HasSuffix(r.URL.Path, "/interrupt"):
			f.aborted = r.URL.Path
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/reply"):
			body, _ := io.ReadAll(r.Body)
			f.replyBody = json.RawMessage(body)
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/message"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data":   []map[string]any{{"id": "msg1", "type": "user"}},
				"cursor": map[string]any{"next": "cur2"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("/api/permission/request", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": f.pending})
	})
	mux.HandleFunc("/api/event", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, frame := range f.sseFrames {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
			if flusher != nil {
				flusher.Flush()
			}
		}
		// Block until the client disconnects so the stream stays "live".
		<-r.Context().Done()
	})
	return mux
}

func newFakeServer(t *testing.T) (*fakeServer, *clientV1) {
	t.Helper()
	f := &fakeServer{t: t}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return f, newClientV1(srv.URL, "", srv.Client())
}

func TestClientV1_Health(t *testing.T) {
	t.Parallel()
	_, c := newFakeServer(t)
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestClientV1_CreateSession(t *testing.T) {
	t.Parallel()
	f, c := newFakeServer(t)
	id, err := c.CreateSession(context.Background(), createSessionReq{
		Model:    modelRef{ProviderID: "donmai", ID: "m", Variant: "high"},
		Location: locationRef{Directory: "/w"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if id != "ses_created" {
		t.Errorf("session id = %q, want ses_created", id)
	}
	// The request body carried the model ref + location.
	var got createSessionReq
	if err := json.Unmarshal(f.createBody, &got); err != nil {
		t.Fatalf("decode create body: %v", err)
	}
	if got.Model.ProviderID != "donmai" || got.Model.ID != "m" || got.Location.Directory != "/w" {
		t.Errorf("create body = %+v, want provider=donmai model=m dir=/w", got)
	}
}

func TestClientV1_Prompt_DefaultsSteer(t *testing.T) {
	t.Parallel()
	f, c := newFakeServer(t)
	if err := c.Prompt(context.Background(), "ses1", promptReq{Prompt: promptInput{Text: "hi"}}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	var got promptReq
	_ = json.Unmarshal(f.promptBody, &got)
	if got.Delivery != "steer" {
		t.Errorf("delivery = %q, want steer (default)", got.Delivery)
	}
	if got.Prompt.Text != "hi" {
		t.Errorf("prompt text = %q, want hi", got.Prompt.Text)
	}
}

func TestClientV1_Abort(t *testing.T) {
	t.Parallel()
	f, c := newFakeServer(t)
	if err := c.Abort(context.Background(), "ses9"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if !strings.Contains(f.aborted, "ses9/interrupt") {
		t.Errorf("abort path = %q, want .../ses9/interrupt", f.aborted)
	}
}

func TestClientV1_PendingPermissions_FiltersSession(t *testing.T) {
	t.Parallel()
	f, c := newFakeServer(t)
	f.pending = []permissionRequest{
		{ID: "a", SessionID: "mine", Action: "bash"},
		{ID: "b", SessionID: "other", Action: "bash"},
	}
	got, err := c.PendingPermissions(context.Background(), "mine")
	if err != nil {
		t.Fatalf("PendingPermissions: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Errorf("pending = %+v, want only session 'mine' request 'a'", got)
	}
}

func TestClientV1_RespondPermission(t *testing.T) {
	t.Parallel()
	f, c := newFakeServer(t)
	err := c.RespondPermission(context.Background(), "ses1", "req7", permissionResponse{Reply: replyReject, Message: "blocked"})
	if err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}
	var got permissionResponse
	_ = json.Unmarshal(f.replyBody, &got)
	if got.Reply != replyReject || got.Message != "blocked" {
		t.Errorf("reply body = %+v, want reject/blocked", got)
	}
}

func TestClientV1_Messages(t *testing.T) {
	t.Parallel()
	_, c := newFakeServer(t)
	msgs, err := c.Messages(context.Background(), "ses1", "")
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != "msg1" {
		t.Errorf("messages = %+v, want one message 'msg1'", msgs)
	}
}

func TestClientV1_Events_StreamsFrames(t *testing.T) {
	t.Parallel()
	f, c := newFakeServer(t)
	// "data" is the real wire key (verified live against opencode 1.17.18) —
	// see serverEvent's doc comment. This fixture previously (incorrectly)
	// used "properties", which never appears on the wire and would decode
	// Properties as empty on every real frame.
	f.sseFrames = []string{
		`{"id":"e1","type":"session.created","data":{"sessionID":"ses1"}}`,
		`{"id":"e2","type":"session.next.text.ended","data":{"sessionID":"ses1","text":"hi"}}`,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, stop, err := c.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	defer func() { _ = stop() }()

	var got []serverEvent
	for len(got) < 2 {
		select {
		case ev := <-ch:
			got = append(got, ev)
		case <-ctx.Done():
			t.Fatalf("timed out after %d frames", len(got))
		}
	}
	if got[0].Type != "session.created" || got[1].Type != "session.next.text.ended" {
		t.Errorf("frames = %q,%q; want session.created, text.ended", got[0].Type, got[1].Type)
	}
	// Properties must actually decode from the wire key ("data") — this is
	// the assertion that would have failed before the fix: with the wrong
	// json tag, Properties is always empty and eventSessionID always
	// returns "".
	if sid := eventSessionID(got[0]); sid != "ses1" {
		t.Errorf("eventSessionID(session.created frame) = %q; want %q (Properties must decode from the real \"data\" wire key)", sid, "ses1")
	}
	var textProps struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(got[1].Properties, &textProps); err != nil {
		t.Fatalf("decode got[1].Properties: %v", err)
	}
	if textProps.Text != "hi" {
		t.Errorf("got[1].Properties text = %q; want \"hi\"", textProps.Text)
	}
}

func TestClientV1_Health_ErrorOnDeadServer(t *testing.T) {
	t.Parallel()
	c := newClientV1("http://127.0.0.1:1", "", &http.Client{Timeout: time.Second})
	if err := c.Health(context.Background()); err == nil {
		t.Fatal("Health against dead server: want error, got nil")
	}
}
