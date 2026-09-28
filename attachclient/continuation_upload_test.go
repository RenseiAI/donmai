package attachclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	attachwirev2 "github.com/RenseiAI/donmai/attachwire/v2"
	"github.com/coder/websocket"
)

func uploadFixture(t *testing.T, size int) *checkpointSourceFixture {
	t.Helper()
	session := newFakeSession(3)
	session.PushOutput([]byte("seed"))
	picture, seq, err := session.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return &checkpointSourceFixture{fakeSession: session, checkpoint: attachwire.ContinuationCheckpoint{Schema: attachwire.ContinuationSchema, Epoch: 3, AtSeq: seq, Picture: picture, State: bytes.Repeat([]byte("s"), size)}}
}

func TestContinuationUploadChunksAndBoundedRetry(t *testing.T) {
	source := uploadFixture(t, attachwire.ContinuationChunkBytes+7)
	encoded, err := source.checkpoint.Encode(attachwire.ContinuationSchema)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var chunks []attachwire.ContinuationChunk
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/rooms/room/continuation/request_1" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("upload origin/path/authority changed")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, attachwire.ContinuationChunkJSONLimit+1))
		if err != nil {
			t.Error(err)
			return
		}
		chunk, err := attachwire.DecodeContinuationChunk(body)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		attempts++
		attempt := attempts
		if attempt > 1 {
			chunks = append(chunks, chunk)
		}
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		next := chunk.Offset + uint32(uint64(len(chunk.Data))&0xffffffff)
		_ = json.NewEncoder(w).Encode(attachwire.ContinuationChunkAck{NextOffset: next, Complete: next == chunk.Total})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := uploadContinuation(ctx, server.Client(), server.URL+"/v2/rooms/room", "fixture-token", attachwire.ContinuationRequest{Schema: attachwire.ContinuationSchema, RequestID: "request_1"}, 3, encoded); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != len(chunks)+1 || len(chunks) < 2 {
		t.Fatal("chunk/retry path was not exercised")
	}
	var got []byte
	for _, chunk := range chunks {
		got = append(got, chunk.Data...)
		if chunk.HostEpoch != 3 {
			t.Fatal("upload epoch changed")
		}
	}
	if !bytes.Equal(got, encoded) {
		t.Fatal("upload changed complete checkpoint bytes")
	}
}

func TestContinuationUploadRejectsRedirectAndWrongAck(t *testing.T) {
	source := uploadFixture(t, 8)
	encoded, err := source.checkpoint.Encode(attachwire.ContinuationSchema)
	if err != nil {
		t.Fatal(err)
	}
	var redirected atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer other.Close()
	for _, redirect := range []bool{false, true} {
		t.Run(map[bool]string{false: "wrong-ack", true: "redirect"}[redirect], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if redirect {
					http.Redirect(w, r, other.URL, http.StatusFound)
					return
				}
				_, _ = io.WriteString(w, `{"nextOffset":0,"complete":false}`)
			}))
			defer server.Close()
			err := uploadContinuation(context.Background(), server.Client(), server.URL+"/v1/rooms/room", "fixture-token", attachwire.ContinuationRequest{Schema: attachwire.ContinuationSchema, RequestID: "request"}, 3, encoded)
			if err == nil {
				t.Fatal("unsafe upload response accepted")
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("checkpoint or bearer followed a redirect")
	}
}

func TestContinuationWSSPositiveAdvertisement(t *testing.T) {
	for _, supported := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "supported"}[supported], func(t *testing.T) {
			seen := make(chan attachwire.Subscribe, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{attachwire.SubprotocolVersion}})
				if err != nil {
					return
				}
				defer func() { _ = conn.CloseNow() }()
				_, b, err := conn.Read(r.Context())
				if err != nil {
					return
				}
				frame, err := attachwire.DecodeFrame(b)
				if err != nil {
					t.Error(err)
					return
				}
				control, err := attachwire.DecodeControlPayload(frame.Payload)
				if err != nil {
					t.Error(err)
					return
				}
				message, err := attachwire.DecodeControl(control)
				if err != nil {
					t.Error(err)
					return
				}
				subscribe, ok := message.(attachwire.Subscribe)
				if !ok {
					t.Error("first host frame was not subscribe")
					return
				}
				seen <- subscribe
				<-r.Context().Done()
			}))
			defer server.Close()
			source := uploadFixture(t, 8)
			var session Session = source.fakeSession
			if supported {
				session = source
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan error, 1)
			cfg := HostConfig{AttachURL: "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/rooms/room", TokenSource: func(context.Context) (string, error) { return mkHostToken(testSessionID, 3, "continuation", true), nil }, Session: session, DisableDegraded: true}
			go func() { finished <- RunHost(ctx, cfg) }()
			select {
			case subscribe := <-seen:
				if supported && (len(subscribe.ContinuationSchemas) != 1 || subscribe.ContinuationSchemas[0] != attachwire.ContinuationSchema) {
					t.Fatal("supported host omitted capability")
				}
				if !supported && len(subscribe.ContinuationSchemas) != 0 {
					t.Fatal("legacy host advertised support")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("host did not subscribe")
			}
			cancel()
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Fatal("host did not release connection")
			}
		})
	}
}

func TestContinuationDegradedAdvertisementAndLegacySnapshot(t *testing.T) {
	source := uploadFixture(t, 8)
	seen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.Query().Get("continuation_schema")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	h := &host{cfg: HostConfig{Session: source}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	response, err := h.openHostSSE(context.Background(), server.URL, &tokenHolder{cur: "fixture-token"}, hostClaims{Epoch: 3})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if got := <-seen; got != attachwire.ContinuationSchema {
		t.Fatalf("degraded capability=%q", got)
	}
	// New metadata leaves the ordinary picture response unchanged even when the
	// caller has no OOB leg context (unsupported receiver fails closed).
	if _, err := h.handleControl(context.Background(), attachwire.SnapshotRequest{Reason: attachwire.ReasonJoin, Continuation: &attachwire.ContinuationRequest{Schema: attachwire.ContinuationSchema, RequestID: "request"}}); err != nil {
		t.Fatal(err)
	}
	source.mu.Lock()
	frame := source.ring[len(source.ring)-1]
	source.mu.Unlock()
	envelope, err := attachwire.DecodeSnapshotEnvelope(frame.Payload)
	if err != nil || envelope.SnapFormat != attachwire.SnapFormatScreen {
		t.Fatal("new request replaced canonical legacy picture")
	}
}

func TestContinuationControlStartsOOBAndKeepsCanonicalPicture(t *testing.T) {
	source := uploadFixture(t, 8)
	uploaded := make(chan attachwire.ContinuationChunk, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, attachwire.ContinuationChunkJSONLimit+1))
		if err != nil {
			t.Error(err)
			return
		}
		chunk, err := attachwire.DecodeContinuationChunk(body)
		if err != nil {
			t.Error(err)
			return
		}
		if r.Header.Get("Authorization") != "Bearer active-leg" {
			t.Error("active bearer was not reused")
		}
		uploaded <- chunk
		_ = json.NewEncoder(w).Encode(attachwire.ContinuationChunkAck{NextOffset: chunk.Total, Complete: true})
	}))
	defer server.Close()
	h := &host{cfg: HostConfig{Session: source, AttachURL: server.URL + "/v1/rooms/room", HTTPClient: server.Client()}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = h.continuationContext(ctx, func() string { return "active-leg" })
	request := attachwire.SnapshotRequest{Reason: attachwire.ReasonJoin, Continuation: &attachwire.ContinuationRequest{Schema: attachwire.ContinuationSchema, RequestID: "capture"}}
	if _, err := h.handleControl(ctx, request); err != nil {
		t.Fatal(err)
	}
	select {
	case chunk := <-uploaded:
		if chunk.RequestID != "capture" || chunk.HostEpoch != 3 {
			t.Fatal("request/epoch binding changed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("optional metadata did not reach OOB upload")
	}
	source.mu.Lock()
	frame := source.ring[len(source.ring)-1]
	source.mu.Unlock()
	envelope, err := attachwire.DecodeSnapshotEnvelope(frame.Payload)
	if err != nil || envelope.SnapFormat != attachwire.SnapFormatScreen {
		t.Fatal("OOB request replaced canonical0x01")
	}
}

func TestContinuationV2SubscribeAdvertisesActualSource(t *testing.T) {
	seen := make(chan attachwire.Subscribe, 1)
	ready := make(chan struct{})
	uploaded := make(chan struct{}, 1)
	token := v2TestToken(t, nil)
	var legacyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if r.Header.Get("Authorization") != "Bearer "+token || r.URL.Path != "/v3/rooms/session-v2/continuation/v2_capture" {
				t.Error("v2 upload lost active authority or endpoint")
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, attachwire.ContinuationChunkJSONLimit+1))
			if err != nil {
				t.Error(err)
				return
			}
			chunk, err := attachwire.DecodeContinuationChunk(body)
			if err != nil {
				t.Error(err)
				return
			}
			_ = json.NewEncoder(w).Encode(attachwire.ContinuationChunkAck{NextOffset: chunk.Total, Complete: true})
			uploaded <- struct{}{}
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{attachwirev2.SubprotocolVersion}})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_, raw, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		frame, err := attachwire.DecodeFrame(raw)
		if err != nil {
			t.Error(err)
			return
		}
		payload, err := attachwire.DecodeControlPayload(frame.Payload)
		if err != nil {
			t.Error(err)
			return
		}
		message, err := attachwirev2.DecodeControl(payload)
		if err != nil {
			t.Error(err)
			return
		}
		subscribe, ok := message.(attachwire.Subscribe)
		if !ok {
			t.Error("v2 subscribe missing")
			return
		}
		seen <- subscribe
		select {
		case <-ready:
		case <-r.Context().Done():
			return
		}
		request, err := attachwirev2.BuildControlFrame(attachwire.SnapshotRequest{Reason: attachwire.ReasonJoin, Continuation: &attachwire.ContinuationRequest{Schema: attachwire.ContinuationSchema, RequestID: "v2_capture"}})
		if err != nil {
			t.Error(err)
			return
		}
		if err := conn.Write(r.Context(), websocket.MessageBinary, request.Encode()); err != nil {
			return
		}
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	source := uploadFixture(t, 8)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	candidate, err := DialV2HostCandidate(ctx, V2HostConfig{AttachURL: "ws" + strings.TrimPrefix(server.URL, "http") + "/v2/rooms/session-v2", TokenSource: func(context.Context) (string, error) { return token, nil }, ContinuationSource: source, OnSnapshotRequest: func(context.Context, attachwire.SnapshotRequest) error {
		legacyCalls.Add(1)
		_, _, err := source.EmitSnapshot()
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = candidate.Close() })
	select {
	case subscribe := <-seen:
		if len(subscribe.ContinuationSchemas) != 1 || subscribe.ContinuationSchemas[0] != attachwire.ContinuationSchema {
			t.Fatal("actual v2 source was not advertised")
		}
	case <-time.After(time.Second):
		t.Fatal("v2 capability advertisement missing")
	}

	// Activation behavior is independently exercised by the carrier suite. This
	// fixture enters its committed state to drive the real post-activation reader.
	candidate.mu.Lock()
	candidate.active = true
	candidate.mu.Unlock()
	close(ready)
	select {
	case <-uploaded:
	case <-time.After(2 * time.Second):
		t.Fatal("v2 optional request did not reach OOB upload")
	}
	if legacyCalls.Load() != 1 {
		t.Fatal("v2 upload bypassed the legacy snapshot callback")
	}
	source.mu.Lock()
	legacy := source.ring[len(source.ring)-1]
	source.mu.Unlock()
	envelope, err := attachwire.DecodeSnapshotEnvelope(legacy.Payload)
	if err != nil || envelope.SnapFormat != attachwire.SnapFormatScreen {
		t.Fatal("v2 OOB request changed canonical0x01")
	}
}
