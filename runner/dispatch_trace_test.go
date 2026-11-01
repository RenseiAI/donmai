package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachclient"
	"github.com/RenseiAI/donmai/attachclient/attachtest"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/internal/interview"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/runtime/activity"
	"github.com/RenseiAI/donmai/runtime/heartbeat"
	spanruntime "github.com/RenseiAI/donmai/runtime/span"
	"github.com/RenseiAI/donmai/runtime/statusbeacon"
	"github.com/RenseiAI/donmai/runtime/stepheartbeat"
	"github.com/RenseiAI/donmai/runtime/tokendelta"
)

type traceInterviewProvider struct {
	agent.HarnessProvider
	handle agent.Handle
}

func (p *traceInterviewProvider) Spawn(context.Context, agent.Spec) (agent.Handle, error) {
	return p.handle, nil
}

func TestRunDispatchTraceHeaders(t *testing.T) {
	for _, mode := range []string{"headless", interview.InterviewRunMode} {
		for _, traced := range []bool{false, true} {
			name := mode + "/absent"
			if traced {
				name = mode + "/present"
			}
			t.Run(name, func(t *testing.T) {
				h := newRunnerHarness(t)
				qw := h.queuedWork("trace-headers")
				if traced {
					qw.Traceparent = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
					qw.Tracestate = "vendor=value"
				}
				var activities, deltas atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					activity := strings.HasSuffix(req.URL.Path, "/activity")
					delta := strings.HasSuffix(req.URL.Path, "/token-delta")
					if activity || delta {
						for header, want := range map[string]string{"traceparent": qw.Traceparent, "tracestate": qw.Tracestate} {
							values, present := req.Header[http.CanonicalHeaderKey(header)]
							if want == "" {
								if present {
									t.Errorf("%s: %s unexpectedly present: %q", req.URL.Path, header, values)
								}
							} else if len(values) != 1 || values[0] != want {
								t.Errorf("%s: %s = %q; want [%q]", req.URL.Path, header, values, want)
							}
						}
						if activity {
							activities.Add(1)
						} else {
							deltas.Add(1)
						}
						w.WriteHeader(http.StatusOK)
						return
					}
					h.server.Config.Handler.ServeHTTP(w, req)
				}))
				t.Cleanup(srv.Close)
				qw.PlatformURL = srv.URL
				if mode == interview.InterviewRunMode {
					qw.Mode = mode
					handle := newScriptedHandle()
					handle.pushTurn(
						agent.InitEvent{SessionID: "trace-provider"},
						agent.AssistantTextEvent{Text: "Complete.\n" + interview.InterviewCompleteSentinel},
						agent.ResultEvent{Success: true},
					)
					provider, err := stub.New()
					if err != nil {
						t.Fatal(err)
					}
					if err := h.runner.registry.Register(&traceInterviewProvider{HarnessProvider: provider.(agent.HarnessProvider), handle: handle}); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				res, err := h.runner.Run(ctx, qw)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				if res.Status != "completed" {
					t.Fatalf("status = %q; want completed (%s)", res.Status, res.Error)
				}
				if activities.Load() == 0 {
					t.Error("Run posted no activity")
				}
				if mode == interview.InterviewRunMode && deltas.Load() == 0 {
					t.Error("interview Run posted no token delta")
				}
			})
		}
	}
}

// dispatchTraceRecorder is a platform double for trace-propagation tests: it
// accepts every session endpoint, records the trace headers observed on each
// request per path suffix, and retains raw span-ingest batch bodies.
type dispatchTraceRecorder struct {
	mu      sync.Mutex
	headers map[string][]http.Header
	batches [][]byte
}

func newDispatchTraceRecorder() *dispatchTraceRecorder {
	return &dispatchTraceRecorder{headers: map[string][]http.Header{}}
}

func (r *dispatchTraceRecorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(req.URL.Path, "/execution-preflight-registration") {
			_, _ = w.Write([]byte(`{"contractVersion":"execution-security-report/v1","decision":"accepted"}`))
			return
		}
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		for _, door := range []string{"/activity", "/status", "/lock-refresh", "/step-heartbeat", "/token-delta", "/traces", "/progress"} {
			if strings.HasSuffix(req.URL.Path, door) {
				r.headers[door] = append(r.headers[door], req.Header.Clone())
				if door == "/traces" {
					r.batches = append(r.batches, body)
				}
			}
		}
		r.mu.Unlock()
		_, _ = w.Write([]byte(`{"refreshed":true,"ok":true}`))
	})
}

func (r *dispatchTraceRecorder) headersFor(door string) []http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]http.Header(nil), r.headers[door]...)
}

func (r *dispatchTraceRecorder) spanBatches() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.batches...)
}

// requireDispatchHeaders asserts every request observed on door carried
// exactly the want context: present headers match, absent names stay absent.
func requireDispatchHeaders(t *testing.T, rec *dispatchTraceRecorder, door string, want map[string]string) {
	t.Helper()
	got := rec.headersFor(door)
	if len(got) == 0 {
		t.Fatalf("no request observed on %s", door)
	}
	for i, hdr := range got {
		for name, wantValue := range want {
			values := hdr.Values(name)
			if wantValue == "" {
				if len(values) != 0 {
					t.Errorf("%s[%d]: %s unexpectedly present: %q", door, i, name, values)
				}
			} else if len(values) != 1 || values[0] != wantValue {
				t.Errorf("%s[%d]: %s = %q; want [%q]", door, i, name, values, wantValue)
			}
		}
	}
}

// TestRunDispatchTraceEndToEnd decodes a platform-shaped work item through
// the production consumer (the wire JSON tags on QueuedWork), runs a stubbed
// agent turn with span emission enabled, and pins the emitted spans: a valid
// dispatch trace id/parent is adopted as the span root with the join keys
// preserved exactly, while absent or malformed context keeps execution and
// the local-root fallback without authorizing anything new.
func TestRunDispatchTraceEndToEnd(t *testing.T) {
	const (
		traceID  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		parentID = "bbbbbbbbbbbbbbbb"
	)
	validParent := "00-" + traceID + "-" + parentID + "-01"
	for _, tc := range []struct {
		name       string
		trace      map[string]string
		wantParent string
		wantTrace  string
	}{
		{
			name:       "valid",
			trace:      map[string]string{"traceparent": validParent, "tracestate": "vendor=value", "sessionStorageId": "sto-e2e", "sessionPublicId": "pub-e2e", "trackerSessionId": "trk-e2e"},
			wantParent: validParent,
			wantTrace:  traceID,
		},
		{
			name:       "malformed",
			trace:      map[string]string{"traceparent": "not-a-traceparent", "tracestate": "bad\nstate"},
			wantParent: "",
			wantTrace:  "",
		},
		{name: "absent", trace: nil, wantParent: "", wantTrace: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRunnerHarness(t)
			rec := newDispatchTraceRecorder()
			srv := httptest.NewServer(rec.handler())
			t.Cleanup(srv.Close)

			// Platform-shaped wire item: camelCase keys decoded by the
			// production consumer (encoding/json over QueuedWork).
			wire := map[string]any{
				"sessionId": "test-session-trace-e2e-" + tc.name, "issueId": "issue-uuid-trace-e2e",
				"issueIdentifier": "TRACE-E2E", "projectName": "TestProject",
				"organizationId": "org_test", "repository": h.bareRepo,
				"workType": "development", "body": "trace e2e body", "title": "trace e2e",
			}
			for k, v := range tc.trace {
				wire[k] = v
			}
			raw, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			var qw QueuedWork
			if err := json.Unmarshal(raw, &qw); err != nil {
				t.Fatalf("decode platform-shaped work item: %v", err)
			}
			qw.WorkerID = "worker-1"
			qw.AuthToken = "tok"
			qw.PlatformURL = srv.URL
			qw.ResolvedProfile = ResolvedProfile{Provider: agent.ProviderStub}
			qw.Capabilities = map[string]bool{CapabilitySpanIngest: true}

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			res, err := h.runner.Run(ctx, qw)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Status != "completed" {
				t.Fatalf("status = %q; want completed (%s)", res.Status, res.Error)
			}

			wantHeaders := map[string]string{"traceparent": "", "tracestate": ""}
			if tc.name == "valid" {
				wantHeaders = map[string]string{"traceparent": validParent, "tracestate": "vendor=value"}
			}
			for _, door := range []string{"/activity", "/lock-refresh", "/step-heartbeat", "/progress", "/traces"} {
				requireDispatchHeaders(t, rec, door, wantHeaders)
			}

			batches := rec.spanBatches()
			if len(batches) == 0 {
				t.Fatal("span ingest received no batch")
			}
			var root map[string]any
			for _, body := range batches {
				var spans []map[string]any
				if err := json.Unmarshal(body, &spans); err != nil {
					t.Fatalf("decode span batch: %v", err)
				}
				for _, s := range spans {
					if s["kind"] != string(agent.SpanKindAgent) {
						continue
					}
					root = s
				}
			}
			if root == nil {
				t.Fatal("no session root span in ingest batches")
			}
			if tc.name == "valid" {
				if root["traceId"] != traceID {
					t.Errorf("root traceId = %v; want %s (dispatch trace not adopted)", root["traceId"], traceID)
				}
				if root["parentSpanId"] != parentID {
					t.Errorf("root parentSpanId = %v; want %s (dispatch parent not adopted)", root["parentSpanId"], parentID)
				}
				donmai, _ := root["donmai"].(map[string]any)
				for k, want := range map[string]string{"sessionStorageId": "sto-e2e", "sessionPublicId": "pub-e2e", "trackerSessionId": "trk-e2e"} {
					if donmai[k] != want {
						t.Errorf("donmai.%s = %v; want %q (join identity not preserved)", k, donmai[k], want)
					}
				}
			} else {
				if parent, _ := root["parentSpanId"].(string); parent != "" {
					t.Errorf("root parentSpanId = %q; want empty local root", parent)
				}
				if trace, _ := root["traceId"].(string); len(trace) != 32 {
					t.Errorf("root traceId = %q; want fresh 32-hex local root", trace)
				}
			}
			// The trace context is correlation-only: authoritative org and
			// session identity always come from the dispatched work.
			donmai, _ := root["donmai"].(map[string]any)
			if donmai["orgId"] != "org_test" {
				t.Errorf("donmai.orgId = %v; want org_test (context must not overwrite authority)", donmai["orgId"])
			}
			if donmai["sessionId"] != qw.SessionID {
				t.Errorf("donmai.sessionId = %v; want %s (context must not overwrite authority)", donmai["sessionId"], qw.SessionID)
			}
		})
	}
}

// traceFakeSubscription is a minimal attachclient.Subscription: an open frame
// feed the test never writes to, closed once per carrier leg.
type traceFakeSubscription struct {
	frames chan attachwire.Frame
	once   sync.Once
}

func (s *traceFakeSubscription) Frames() <-chan attachwire.Frame { return s.frames }
func (s *traceFakeSubscription) Close() error {
	s.once.Do(func() { close(s.frames) })
	return nil
}

// traceFakeSession is a minimal attachclient.Session pinned at PTY epoch 1.
// It never produces frames or exits; the host leg runs until the test
// cancels it.
type traceFakeSession struct {
	done chan struct{}
}

func (s *traceFakeSession) WriteInput(p []byte) (int, error) { return len(p), nil }
func (s *traceFakeSession) Resize(_, _, _, _ uint32) error   { return nil }
func (s *traceFakeSession) Snapshot() (attachwire.Screen, attachwire.HostSeq, error) {
	return attachwire.Screen{Epoch: 1}, 0, nil
}

func (s *traceFakeSession) EmitSnapshot() (attachwire.Frame, bool, error) {
	return attachwire.Frame{}, false, nil
}
func (s *traceFakeSession) EmitMarker(string) error { return nil }
func (s *traceFakeSession) Subscribe(attachwire.HostSeq) (attachclient.Subscription, error) {
	return &traceFakeSubscription{frames: make(chan attachwire.Frame)}, nil
}
func (s *traceFakeSession) Done() <-chan struct{} { return s.done }
func (s *traceFakeSession) Exit() (attachwire.ExitPayload, bool) {
	return attachwire.ExitPayload{}, false
}

// traceFakeJWT mints an unsigned bearer with the host claims the client and
// the test relay read. Signatures are the relay's job; both sides parse the
// payload leniently, exactly like the attachclient fakes.
func traceFakeJWT(t *testing.T, sessionID string, epoch int64) string {
	t.Helper()
	claims := map[string]any{
		"sessionId": sessionID, "roomId": sessionID, "role": "host",
		"orgId": "org-1", "aud": "relay", "jti": "jti-trace-1",
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "epoch": epoch,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT"}`))
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("sig"))
}

// TestDispatchTracePropagationDoors drives every real dispatch-trace door
// against doubles and asserts the same context arrives on each: the span
// ingest, activity (+ its status transition), lock-refresh, step-heartbeat,
// token-delta and progress posters, plus the host-relay attach handshake on
// the initial attach and on a reconnect after a relay restart.
func TestDispatchTracePropagationDoors(t *testing.T) {
	const (
		traceID    = "cccccccccccccccccccccccccccccccc"
		parentID   = "dddddddddddddddd"
		traceState = "vendor=value"
	)
	traceparent := "00-" + traceID + "-" + parentID + "-01"
	want := map[string]string{"traceparent": traceparent, "tracestate": traceState}

	rec := newDispatchTraceRecorder()
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)

	t.Run("trace ingest", func(t *testing.T) {
		p, err := spanruntime.NewPoster(spanruntime.PosterConfig{
			BaseURL: srv.URL, EndpointPath: "/traces", HTTPClient: srv.Client(),
			Traceparent: traceparent, Tracestate: traceState,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		p.Send(agent.LlmCallSpan{
			SpanCore: agent.SpanCore{
				TraceID: "00112233445566778899aabbccddeeff", SpanID: "0011223344556677",
				Kind: agent.SpanKindLLM, Name: "chat stub/stub",
				Donmai: agent.DonmaiSpanExtensions{OrgID: "org-1", WorkspaceID: "org-1", SessionID: "sess-doors"},
			},
			GenAI: agent.GenAIAttributes{System: "stub", RequestModel: "stub"},
		})
		_ = p.Stop()
		requireDispatchHeaders(t, rec, "/traces", want)
	})

	t.Run("activity", func(t *testing.T) {
		p, err := activity.New(activity.Config{
			SessionID: "sess-doors", BaseURL: srv.URL, HTTPClient: srv.Client(),
			Traceparent: traceparent, Tracestate: traceState,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		p.Send(context.Background(), agent.AssistantTextEvent{Text: "hello"})
		_ = p.Stop()
		requireDispatchHeaders(t, rec, "/activity", want)
		requireDispatchHeaders(t, rec, "/status", want)
	})

	t.Run("lock-refresh", func(t *testing.T) {
		p, err := heartbeat.New(heartbeat.Config{
			SessionID: "sess-doors", WorkerID: "worker", BaseURL: srv.URL,
			Traceparent: traceparent, Tracestate: traceState,
			Interval: time.Hour, HTTPClient: srv.Client(),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Stop() })
		if err := p.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		requireDispatchHeaders(t, rec, "/lock-refresh", want)
	})

	t.Run("step-heartbeat", func(t *testing.T) {
		e, err := stepheartbeat.New(stepheartbeat.Config{
			SessionID: "sess-doors", WorkerID: "worker", BaseURL: srv.URL,
			Traceparent: traceparent, Tracestate: traceState,
			Interval: time.Hour, HTTPClient: srv.Client(),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Stop() })
		if err := e.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		requireDispatchHeaders(t, rec, "/step-heartbeat", want)
	})

	t.Run("token-delta", func(t *testing.T) {
		p, err := tokendelta.New(tokendelta.Config{
			SessionID: "sess-doors", BaseURL: srv.URL, HTTPClient: srv.Client(),
			Traceparent: traceparent, Tracestate: traceState,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		p.Send(tokendelta.Frame{Index: 0, Text: "hi", Done: true})
		_ = p.Stop()
		requireDispatchHeaders(t, rec, "/token-delta", want)
	})

	t.Run("progress", func(t *testing.T) {
		b, err := statusbeacon.New(statusbeacon.Config{
			SessionID: "sess-doors", WorkerID: "worker", BaseURL: srv.URL,
			Traceparent: traceparent, Tracestate: traceState, HTTPClient: srv.Client(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !b.Report(context.Background(), "streaming") {
			t.Fatal("beacon report returned false")
		}
		requireDispatchHeaders(t, rec, "/progress", want)
	})

	t.Run("attach initial and reconnect", func(t *testing.T) {
		relay := attachtest.New(attachtest.Config{RoomID: "room-trace"})
		if err := relay.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = relay.Close() })
		token := traceFakeJWT(t, "sess-trace", 1)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		done := make(chan error, 1)
		go func() {
			done <- attachclient.RunHost(ctx, attachclient.HostConfig{
				AttachURL:   relay.BaseWSURL(),
				TokenSource: func(context.Context) (string, error) { return token, nil },
				Session:     &traceFakeSession{done: make(chan struct{})},
				Traceparent: traceparent,
				Tracestate:  traceState,
				BackoffMin:  5 * time.Millisecond,
				BackoffMax:  50 * time.Millisecond,
				DialTimeout: 5 * time.Second,
				// Keep the background upgrade probe parked: the WSS lane
				// carries the test, and a racing probe dial would add
				// handshake noise to the assertion below.
				UpgradeProbeInterval: time.Hour,
			})
		}()
		waitFor := func(cond func() bool, what string) {
			t.Helper()
			deadline := time.Now().Add(10 * time.Second)
			for !cond() && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if !cond() {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
		waitFor(relay.HostBound, "initial host bind")
		waitFor(func() bool { return len(relay.DialHeaders()) >= 1 }, "initial handshake")
		for i, hdr := range relay.DialHeaders() {
			if got := hdr.Get("traceparent"); got != traceparent {
				t.Errorf("handshake %d traceparent = %q; want %q", i, got, traceparent)
			}
			if got := hdr.Get("tracestate"); got != traceState {
				t.Errorf("handshake %d tracestate = %q; want %q", i, got, traceState)
			}
		}
		// A relay restart wipes ring state: the host resets and re-attaches
		// from scratch, and the fresh handshake must carry the same context.
		relay.SimulateRestart()
		waitFor(func() bool { return len(relay.DialHeaders()) >= 2 }, "reconnect handshake")
		waitFor(relay.HostBound, "host rebind")
		second := relay.DialHeaders()[1]
		if got := second.Get("traceparent"); got != traceparent {
			t.Errorf("reconnect traceparent = %q; want %q", got, traceparent)
		}
		if got := second.Get("tracestate"); got != traceState {
			t.Errorf("reconnect tracestate = %q; want %q", got, traceState)
		}
		cancel()
		select {
		case err := <-done:
			if err != context.Canceled {
				t.Fatalf("RunHost = %v; want context.Canceled", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("RunHost did not return after cancel")
		}
	})

	t.Run("attach degraded lane", func(t *testing.T) {
		relay := attachtest.New(attachtest.Config{RoomID: "room-trace-degraded", RefuseWSS: true})
		if err := relay.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = relay.Close() })
		token := traceFakeJWT(t, "sess-trace-degraded", 1)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		done := make(chan error, 1)
		go func() {
			done <- attachclient.RunHost(ctx, attachclient.HostConfig{
				AttachURL:   relay.BaseWSURL(),
				TokenSource: func(context.Context) (string, error) { return token, nil },
				Session:     &traceFakeSession{done: make(chan struct{})},
				Traceparent: traceparent,
				Tracestate:  traceState,
				BackoffMin:  5 * time.Millisecond,
				BackoffMax:  50 * time.Millisecond,
				DialTimeout: 5 * time.Second,
				// Fast probe so at least one upgrade attempt dials while
				// the relay still refuses WSS: its handshake headers pin
				// the probe path independently of the initial WSS dial.
				UpgradeProbeInterval: 20 * time.Millisecond,
				FallbackAfterN:       1,
			})
		}()
		waitFor := func(cond func() bool, what string) {
			t.Helper()
			deadline := time.Now().Add(10 * time.Second)
			for !cond() && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if !cond() {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
		// The refused WSS upgrade falls back to the degraded lane: the
		// SSE-down GET binds the leg and the first POST-up carries the
		// subscribe control even with no session frames.
		waitFor(relay.HostBound, "degraded host bind")
		waitFor(func() bool { return len(relay.SSEHeaders()) >= 1 }, "degraded SSE request")
		waitFor(func() bool { return len(relay.BatchHeaders()) >= 1 }, "degraded batch request")
		// At least one upgrade-probe dial must have run while refused so
		// the probe header path is observed, not just the initial WSS dial.
		waitFor(func() bool { return len(relay.DialHeaders()) >= 2 }, "probe dial while refused")
		for i, hdr := range relay.SSEHeaders() {
			if got := hdr.Get("traceparent"); got != traceparent {
				t.Errorf("degraded SSE %d traceparent = %q; want %q", i, got, traceparent)
			}
			if got := hdr.Get("tracestate"); got != traceState {
				t.Errorf("degraded SSE %d tracestate = %q; want %q", i, got, traceState)
			}
		}
		for i, hdr := range relay.BatchHeaders() {
			if got := hdr.Get("traceparent"); got != traceparent {
				t.Errorf("degraded batch %d traceparent = %q; want %q", i, got, traceparent)
			}
			if got := hdr.Get("tracestate"); got != traceState {
				t.Errorf("degraded batch %d tracestate = %q; want %q", i, got, traceState)
			}
		}
		// Every WSS handshake observed so far is either the initial dial
		// or an upgrade-probe dial: both must carry the same context, so
		// deleting either header site turns this assertion red.
		for i, hdr := range relay.DialHeaders() {
			if got := hdr.Get("traceparent"); got != traceparent {
				t.Errorf("degraded-phase handshake %d traceparent = %q; want %q", i, got, traceparent)
			}
			if got := hdr.Get("tracestate"); got != traceState {
				t.Errorf("degraded-phase handshake %d tracestate = %q; want %q", i, got, traceState)
			}
		}
		// Re-admit WSS: the probe upgrades the live degraded leg and the
		// fresh WSS handshake must keep the same context.
		relay.SetRefuseWSS(false)
		binds := relay.HostBinds()
		waitFor(func() bool { return relay.HostBinds() > binds }, "upgrade rebind")
		waitFor(func() bool { return len(relay.DialHeaders()) >= 3 }, "post-upgrade handshake")
		for i, hdr := range relay.DialHeaders() {
			if got := hdr.Get("traceparent"); got != traceparent {
				t.Errorf("handshake %d traceparent = %q; want %q", i, got, traceparent)
			}
			if got := hdr.Get("tracestate"); got != traceState {
				t.Errorf("handshake %d tracestate = %q; want %q", i, got, traceState)
			}
		}
		cancel()
		select {
		case err := <-done:
			if err != context.Canceled {
				t.Fatalf("RunHost = %v; want context.Canceled", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("RunHost did not return after cancel")
		}
	})
}

// interactiveTraceSession adapts the trace fake to the runner's PTY surface
// (agent.InteractiveSession) at epoch 1. Subscribe returns an
// agent.InteractiveSubscription so sessAdapter can hand it to the attach
// client; the session stays alive until the test closes done.
type interactiveTraceSession struct {
	done chan struct{}
}

func (s *interactiveTraceSession) WriteInput(p []byte) (int, error) { return len(p), nil }
func (s *interactiveTraceSession) Resize(_, _, _, _ uint32) error   { return nil }
func (s *interactiveTraceSession) Snapshot() (attachwire.Screen, attachwire.HostSeq, error) {
	return attachwire.Screen{Epoch: 1}, 0, nil
}
func (s *interactiveTraceSession) EmitSnapshot() (attachwire.Frame, bool, error) {
	return attachwire.Frame{}, false, nil
}
func (s *interactiveTraceSession) EmitMarker(string) error { return nil }
func (s *interactiveTraceSession) Subscribe(attachwire.HostSeq) (agent.InteractiveSubscription, error) {
	return &traceFakeSubscription{frames: make(chan attachwire.Frame)}, nil
}
func (s *interactiveTraceSession) Done() <-chan struct{} { return s.done }
func (s *interactiveTraceSession) Exit() (attachwire.ExitPayload, bool) {
	return attachwire.NewNormalExit(0), true
}

// TestDispatchInteractiveTraceCarriesToRelay drives the production
// interactive lane (dispatchInteractive) against a stub relay and asserts
// the runner-built attach handshake carries the dispatch trace context.
// Deleting the HostConfig trace wiring in dispatchInteractive leaves the
// direct-construction Doors subtests green, so this runner-level pin is the
// only guard for the interactive lane.
func TestDispatchInteractiveTraceCarriesToRelay(t *testing.T) {
	const (
		traceID    = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		parentID   = "ffffffffffffffff"
		traceState = "vendor=value"
	)
	traceparent := "00-" + traceID + "-" + parentID + "-01"

	relay := attachtest.New(attachtest.Config{RoomID: "room-trace-interactive"})
	if err := relay.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	token := traceFakeJWT(t, "sess-trace-interactive", 1)
	t.Setenv(envAttachURL, relay.BaseWSURL())
	t.Setenv(envAttachToken, token)
	t.Setenv(envAttachTokenFile, "")

	session := &interactiveTraceSession{done: make(chan struct{})}
	handle := &testInteractiveHandle{Handle: &fakeHandle{events: make(chan agent.Event)}, session: session}
	r := minimalRunner(t)
	qw := QueuedWork{}
	qw.SessionID = "sess-trace-interactive"
	qw.Mode = interactiveRunMode
	qw.Traceparent = traceparent
	qw.Tracestate = traceState

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type outcome struct {
		res *Result
		err error
	}
	resultCh := make(chan outcome, 1)
	go func() {
		res := &Result{SessionID: qw.SessionID}
		res.ProviderName = agent.ProviderStub
		out, err := r.dispatchInteractive(ctx, handle, t.TempDir(), qw, res, noopSink{}, nil, nil, agent.NoticeDeliveryPTYNotice)
		resultCh <- outcome{res: out, err: err}
	}()

	deadline := time.Now().Add(10 * time.Second)
	for len(relay.DialHeaders()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(relay.DialHeaders()) == 0 {
		close(session.done)
		t.Fatal("timed out waiting for interactive attach handshake")
	}
	for i, hdr := range relay.DialHeaders() {
		if got := hdr.Get("traceparent"); got != traceparent {
			t.Errorf("interactive handshake %d traceparent = %q; want %q", i, got, traceparent)
		}
		if got := hdr.Get("tracestate"); got != traceState {
			t.Errorf("interactive handshake %d tracestate = %q; want %q", i, got, traceState)
		}
	}

	close(session.done)
	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("dispatchInteractive: %v", got.err)
		}
		if got.res.Status != "completed" {
			t.Fatalf("status = %q; want completed (%s)", got.res.Status, got.res.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("dispatchInteractive did not return after session exit")
	}
}
