package span

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func sampleLlmSpan(n int) agent.LlmCallSpan {
	return agent.LlmCallSpan{
		SpanCore: agent.SpanCore{
			TraceID:           "00112233445566778899aabbccddeeff",
			SpanID:            fmt.Sprintf("%016x", n),
			ParentSpanID:      "8899aabbccddeeff",
			Kind:              agent.SpanKindLLM,
			Name:              "chat openai/gpt-5-codex",
			StartTimeUnixNano: "1700000000000000000",
			EndTimeUnixNano:   "1700000000100000000",
			Status:            agent.SpanStatus{Code: agent.StatusOK},
			Donmai: agent.DonmaiSpanExtensions{
				OrgID:       "org-1",
				WorkspaceID: "workspace-1",
				SessionID:   "session-1",
			},
		},
		GenAI: agent.GenAIAttributes{
			System:            "openai",
			RequestModel:      "gpt-5-codex",
			UsageInputTokens:  10,
			UsageOutputTokens: 5,
		},
	}
}

func TestPoster_FlushesAtBatchThreshold(t *testing.T) {
	t.Parallel()
	bodies := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != DefaultEndpointPath {
			t.Errorf("path = %q, want %q", r.URL.Path, DefaultEndpointPath)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-1" {
			t.Errorf("Authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		bodies <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	p, err := NewPoster(PosterConfig{
		BaseURL:       srv.URL,
		AuthToken:     "token-1",
		HTTPClient:    srv.Client(),
		FlushSpans:    2,
		FlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !p.Send(sampleLlmSpan(1)) || !p.Send(sampleLlmSpan(2)) {
		t.Fatal("Send rejected threshold batch")
	}
	select {
	case body := <-bodies:
		var got []json.RawMessage
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode batch: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("batch spans = %d, want 2", len(got))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("batch threshold did not flush")
	}
	_ = p.Stop()
	if p.Posted() != 2 || p.Batches() != 1 || p.Dropped() != 0 {
		t.Fatalf("stats = posted:%d batches:%d dropped:%d", p.Posted(), p.Batches(), p.Dropped())
	}
}

func TestPoster_TimerAndStopFlush(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		interval time.Duration
		stopNow  bool
	}{
		{name: "timer", interval: 5 * time.Millisecond},
		{name: "stop drain", interval: time.Hour, stopNow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			posted := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				posted <- struct{}{}
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)
			p, err := NewPoster(PosterConfig{BaseURL: srv.URL, HTTPClient: srv.Client(), FlushInterval: tc.interval})
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !p.Send(sampleLlmSpan(1)) {
				t.Fatal("Send rejected")
			}
			if tc.stopNow {
				_ = p.Stop()
			}
			select {
			case <-posted:
			case <-time.After(2 * time.Second):
				t.Fatal("partial batch did not flush")
			}
			_ = p.Stop()
		})
	}
}

func TestPoster_BackpressureDropsWithoutBlocking(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	p, err := NewPoster(PosterConfig{
		BaseURL:       srv.URL,
		HTTPClient:    srv.Client(),
		FlushSpans:    1,
		QueueSize:     1,
		FlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !p.Send(sampleLlmSpan(1)) {
		t.Fatal("first Send rejected")
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not enter blocked request")
	}
	if !p.Send(sampleLlmSpan(2)) {
		t.Fatal("second Send should fill queue")
	}
	start := time.Now()
	if p.Send(sampleLlmSpan(3)) {
		t.Fatal("third Send should be dropped on full queue")
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("backpressure path blocked the caller")
	}
	close(release)
	_ = p.Stop()
	if p.Dropped() != 1 {
		t.Fatalf("Dropped = %d, want 1", p.Dropped())
	}
}

func TestPoster_SendRacingStopIsSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	p, err := NewPoster(PosterConfig{BaseURL: srv.URL, HTTPClient: srv.Client(), FlushSpans: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				p.Send(sampleLlmSpan(i*100 + j + 1))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = p.Stop()
	}()
	wg.Wait()
}

func TestPoster_GoldenFixtureElementCompatibility(t *testing.T) {
	t.Parallel()
	golden, err := os.ReadFile(filepath.Join("..", "..", "agent", "testdata", "llm_call_span.golden.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	decoded, err := agent.UnmarshalSpan(golden)
	if err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	bodies := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	p, err := NewPoster(PosterConfig{BaseURL: srv.URL, HTTPClient: srv.Client(), FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !p.Send(decoded) {
		t.Fatal("Send rejected golden span")
	}
	_ = p.Stop()
	body := <-bodies
	var batch []json.RawMessage
	if err := json.Unmarshal(body, &batch); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(batch) != 1 {
		t.Fatalf("batch size = %d, want 1", len(batch))
	}
	var compactGolden bytes.Buffer
	if err := json.Compact(&compactGolden, golden); err != nil {
		t.Fatalf("compact golden: %v", err)
	}
	if !bytes.Equal(batch[0], compactGolden.Bytes()) {
		t.Fatalf("poster changed golden element\nwant=%s\ngot =%s", compactGolden.Bytes(), batch[0])
	}
}

func TestPoster_PermanentFailureCountsDroppedBatch(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not installed", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	p, err := NewPoster(PosterConfig{BaseURL: srv.URL, HTTPClient: srv.Client(), FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.Send(sampleLlmSpan(1))
	_ = p.Stop()
	if p.Dropped() != 1 || p.Posted() != 0 {
		t.Fatalf("stats = dropped:%d posted:%d, want 1/0", p.Dropped(), p.Posted())
	}
}

func TestPoster_CancelledRunContextStillDrainsOnStop(t *testing.T) {
	t.Parallel()
	posted := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posted <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	p, err := NewPoster(PosterConfig{BaseURL: srv.URL, HTTPClient: srv.Client(), FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	if !p.Send(sampleLlmSpan(1)) {
		t.Fatal("Send rejected after caller cancellation but before Stop")
	}
	_ = p.Stop()
	select {
	case <-posted:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not drain after caller context cancellation")
	}
}

func TestPosterDispatchTraceHeaders(t *testing.T) {
	t.Parallel()
	const parent = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	for _, tc := range []struct {
		name, parent, state string
	}{
		{name: "both", parent: parent, state: "vendor=value"},
		{name: "parent only", parent: parent},
		{name: "state only", state: "vendor=value"},
		{name: "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var attempts atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for name, want := range map[string]string{"traceparent": tc.parent, "tracestate": tc.state} {
					values, present := r.Header[http.CanonicalHeaderKey(name)]
					if want == "" {
						if present {
							t.Errorf("%s unexpectedly present: %q", name, values)
						}
					} else if len(values) != 1 || values[0] != want {
						t.Errorf("%s = %q; want [%q]", name, values, want)
					}
				}
				attempts.Add(1)
				w.WriteHeader(http.StatusAccepted)
			}))
			t.Cleanup(srv.Close)
			p, err := NewPoster(PosterConfig{
				BaseURL: srv.URL, HTTPClient: srv.Client(),
				Traceparent: tc.parent, Tracestate: tc.state,
				FlushSpans: 2, FlushInterval: time.Hour,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !p.Send(sampleLlmSpan(1)) {
				t.Fatal("Send rejected")
			}
			_ = p.Stop()
			if got := attempts.Load(); got != 1 {
				t.Fatalf("post attempts = %d; want 1", got)
			}
		})
	}
}

// TestPosterHangingTransportCancelled pins the cancellation bound on the
// trace ingest path: a request against an endpoint that never settles must
// return once the caller cancels, rather than holding the delivery
// goroutine (and Stop's drain) for the full client timeout.
func TestPosterHangingTransportCancelled(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
			w.WriteHeader(http.StatusAccepted)
		case <-r.Context().Done():
		}
	}))
	// LIFO cleanups: release first so the hanging handler can return,
	// then the server close. A never-settling handler does not observe
	// client cancellation promptly, so closing the server first would
	// wait on the handler while the handler waited on release.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	p, err := NewPoster(PosterConfig{BaseURL: srv.URL, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := agent.MarshalSpan(sampleLlmSpan(1))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal([]json.RawMessage{encoded})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.post(ctx, body) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("hanging post returned %v; want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hanging post did not return within 5s of cancellation")
	}
}
