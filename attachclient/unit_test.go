package attachclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestDegradedEndpoints(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, sse, post string
		wantErr       bool
	}{
		{in: "wss://host/v1/rooms/r1", sse: "https://host/v1/rooms/r1/host/sse", post: "https://host/v1/rooms/r1/host/output"},
		{in: "ws://127.0.0.1:8080/v1/rooms/r1", sse: "http://127.0.0.1:8080/v1/rooms/r1/host/sse", post: "http://127.0.0.1:8080/v1/rooms/r1/host/output"},
		{in: "wss://host/v1/rooms/r1/", sse: "https://host/v1/rooms/r1/host/sse", post: "https://host/v1/rooms/r1/host/output"},
		{in: "http://host/v1/rooms/r1", wantErr: true},
		{in: "://bad", wantErr: true},
	}
	for _, c := range cases {
		sse, post, err := degradedEndpoints(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("degradedEndpoints(%q): want error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("degradedEndpoints(%q): %v", c.in, err)
			continue
		}
		if sse != c.sse || post != c.post {
			t.Errorf("degradedEndpoints(%q) = %q,%q; want %q,%q", c.in, sse, post, c.sse, c.post)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()
	if d := parseRetryAfter("2"); d != 2*time.Second {
		t.Errorf("parseRetryAfter(2) = %v, want 2s", d)
	}
	if d := parseRetryAfter(""); d != 500*time.Millisecond {
		t.Errorf("parseRetryAfter(empty) = %v, want 500ms", d)
	}
	if d := parseRetryAfter("garbage"); d != 500*time.Millisecond {
		t.Errorf("parseRetryAfter(garbage) = %v, want 500ms", d)
	}
	future := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	if d := parseRetryAfter(future); d <= 0 || d > 4*time.Second {
		t.Errorf("parseRetryAfter(http-date) = %v, want ~3s", d)
	}
}

func TestBackoffProgressionAndReset(t *testing.T) {
	t.Parallel()
	b := newBackoff(10*time.Millisecond, 80*time.Millisecond)
	for i := 0; i < 6; i++ {
		d := b.next()
		if d <= 0 {
			t.Fatalf("backoff.next returned non-positive %v", d)
		}
		if d > 80*time.Millisecond {
			t.Errorf("backoff.next %v exceeds max 80ms", d)
		}
	}
	b.reset()
	if d := b.next(); d > 10*time.Millisecond {
		t.Errorf("after reset, first delay %v should be <= min (10ms)", d)
	}
}

func TestSleepCtxCancels(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); err == nil {
		t.Error("sleepCtx with cancelled ctx should return the ctx error")
	}
	if err := sleepCtx(context.Background(), 0); err != nil {
		t.Errorf("sleepCtx(0) = %v, want nil", err)
	}
}

func TestDedupSetEviction(t *testing.T) {
	t.Parallel()
	d := newDedupSet(2)
	k1 := dedupKey{userID: "u", inputSeq: 1}
	k2 := dedupKey{userID: "u", inputSeq: 2}
	k3 := dedupKey{userID: "u", inputSeq: 3}
	d.add(k1)
	d.add(k2)
	if !d.has(k1) || !d.has(k2) {
		t.Fatal("k1/k2 should be present")
	}
	d.add(k3) // evicts k1 (oldest)
	if d.has(k1) {
		t.Error("k1 should have been evicted")
	}
	if !d.has(k2) || !d.has(k3) {
		t.Error("k2/k3 should be present")
	}
	d.add(k2) // re-adding an existing key is a no-op (no double eviction)
	if !d.has(k3) {
		t.Error("k3 should still be present after re-adding k2")
	}
}

func TestPostHostBatchTaxonomy(t *testing.T) {
	t.Parallel()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch n.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests) // 429 → retry
		case 2:
			w.WriteHeader(http.StatusUnauthorized) // 401 → re-mint, retry
		case 3:
			w.WriteHeader(http.StatusServiceUnavailable) // 5xx → retry
		default:
			writeAccepted(w)
		}
	}))
	defer srv.Close()

	var reminted atomic.Int64
	tokH := &tokenHolder{cur: "tok-1", src: func(context.Context) (string, error) {
		reminted.Add(1)
		return "tok-2", nil
	}}
	h := &host{cfg: HostConfig{}, log: discardLogger()}
	ack, outcome, err := h.postHostBatch(context.Background(), srv.URL, tokH,
		attachwire.HostFrameBatch{BatchID: "b1", FirstSeq: 1, LastSeq: 3})
	if err != nil {
		t.Fatalf("postHostBatch: %v", err)
	}
	if outcome != postOK || ack != 3 {
		t.Errorf("outcome=%v ack=%d, want postOK ack=3", outcome, ack)
	}
	if reminted.Load() != 1 {
		t.Errorf("re-mint count = %d, want 1 (401 handling)", reminted.Load())
	}
	if tokH.current() != "tok-2" {
		t.Errorf("token after 401 = %q, want tok-2", tokH.current())
	}
}

func TestOpenHostSSEPersistentUnauthorizedIsBounded(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name       string
		current    string
		resolved   string
		wantCalls  int64
		wantErrIs  error
		wantErrSub string
	}{
		{
			name:      "unchanged rejected token stops immediately",
			current:   "stale",
			resolved:  "stale",
			wantCalls: 1,
			wantErrIs: errRejectedTokenUnchanged,
		},
		{
			name:       "replacement is retried only once",
			current:    "stale",
			resolved:   "replacement",
			wantCalls:  2,
			wantErrSub: "remained unauthorized",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := requests.Load()
			tokH := &tokenHolder{cur: tc.current, src: func(context.Context) (string, error) {
				return tc.resolved, nil
			}}
			h := &host{cfg: HostConfig{HTTPClient: srv.Client()}, log: discardLogger()}
			resp, err := h.openHostSSE(context.Background(), srv.URL, tokH, hostClaims{Epoch: 1})
			if resp != nil {
				_ = resp.Body.Close()
				t.Fatal("openHostSSE unexpectedly returned a response")
			}
			if err == nil {
				t.Fatal("openHostSSE unexpectedly succeeded against persistent 401")
			}
			if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("error=%v; want errors.Is(%v)", err, tc.wantErrIs)
			}
			if tc.wantErrSub != "" && !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Fatalf("error=%v; want substring %q", err, tc.wantErrSub)
			}
			if got := requests.Load() - before; got != tc.wantCalls {
				t.Fatalf("request count=%d; want %d", got, tc.wantCalls)
			}
		})
	}
}

func TestPostHostBatchPersistentUnauthorizedIsBoundedAndRecovers(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	var allowed atomic.Value
	allowed.Store("never")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+allowed.Load().(string) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeAccepted(w)
	}))
	defer srv.Close()

	var resolved atomic.Value
	resolved.Store("stale")
	tokH := &tokenHolder{cur: "stale", src: func(context.Context) (string, error) {
		return resolved.Load().(string), nil
	}}
	h := &host{cfg: HostConfig{HTTPClient: srv.Client()}, log: discardLogger()}
	batch := attachwire.HostFrameBatch{BatchID: "persistent-401", FirstSeq: 1, LastSeq: 3}

	_, outcome, err := h.postHostBatch(context.Background(), srv.URL, tokH, batch)
	if outcome != postFatal || !errors.Is(err, errRejectedTokenUnchanged) {
		t.Fatalf("first POST outcome=%v err=%v; want postFatal + unchanged-token error", outcome, err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("request count=%d after unchanged token; want 1", got)
	}

	resolved.Store("replacement")
	_, outcome, err = h.postHostBatch(context.Background(), srv.URL, tokH, batch)
	if outcome != postFatal || err == nil || !strings.Contains(err.Error(), "remained unauthorized") {
		t.Fatalf("second POST outcome=%v err=%v; want bounded persistent-401 failure", outcome, err)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("request count=%d after one replacement retry; want 3", got)
	}

	allowed.Store("replacement")
	ack, outcome, err := h.postHostBatch(context.Background(), srv.URL, tokH, batch)
	if err != nil || outcome != postOK || ack != 3 {
		t.Fatalf("recovery POST outcome=%v ack=%d err=%v; want postOK ack=3", outcome, ack, err)
	}
	if got := requests.Load(); got != 4 {
		t.Fatalf("request count=%d after recovery; want 4", got)
	}
}

func TestPostHostBatchRewind409(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSONStatus(w, http.StatusConflict, attachwire.HostBatchRejected{BatchID: "b1", AckSeq: 4})
	}))
	defer srv.Close()
	tokH := &tokenHolder{cur: "t", src: func(context.Context) (string, error) { return "t", nil }}
	h := &host{cfg: HostConfig{}, log: discardLogger()}
	ack, outcome, err := h.postHostBatch(context.Background(), srv.URL, tokH,
		attachwire.HostFrameBatch{BatchID: "b1", FirstSeq: 9, LastSeq: 10})
	if err != nil {
		t.Fatalf("postHostBatch: %v", err)
	}
	if outcome != postRewind || ack != 4 {
		t.Errorf("outcome=%v ack=%d, want postRewind ack=4", outcome, ack)
	}
}

func TestPostHostBatchNetworkErrorFatal(t *testing.T) {
	t.Parallel()
	tokH := &tokenHolder{cur: "t", src: func(context.Context) (string, error) { return "t", nil }}
	h := &host{cfg: HostConfig{}, log: discardLogger()}
	// A closed server address → Do errors → exhausts retries → postFatal.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, outcome, err := h.postHostBatch(ctx, "http://127.0.0.1:1/nope", tokH,
		attachwire.HostFrameBatch{BatchID: "b1", FirstSeq: 1, LastSeq: 1})
	if outcome != postFatal || err == nil {
		t.Errorf("outcome=%v err=%v, want postFatal + error", outcome, err)
	}
}

func writeAccepted(w http.ResponseWriter) {
	writeJSONStatus(w, http.StatusOK, attachwire.HostBatchAccepted{BatchID: "b1", AckSeq: 3})
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
