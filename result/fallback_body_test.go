package result_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestPrepareWorkerExitedWithoutResultBody pins the fallback record body: a
// failed status naming the dead worker with the worker_exited_without_result
// failure mode. It must be deterministic in its inputs so the first-writer
// comparison is stable, and it must validate its identities.
func TestPrepareWorkerExitedWithoutResultBody(t *testing.T) {
	t.Parallel()
	p := newPoster(t, "http://127.0.0.1:1", 0)
	first, err := p.PrepareWorkerExitedWithoutResultBody("sess-fallback", "worker-9", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.PrepareWorkerExitedWithoutResultBody("sess-fallback", "worker-9", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("fallback body is not deterministic in its inputs")
	}
	var body map[string]any
	if err := json.Unmarshal(first, &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "failed" {
		t.Fatalf("status=%v, want failed", body["status"])
	}
	if body["workerId"] != "worker-9" {
		t.Fatalf("workerId=%v, want worker-9", body["workerId"])
	}
	// The wire string is asserted literally, not through the production
	// constant: the platform routes this exact value, so a rename or typo
	// in the constant must fail this test rather than move with it.
	if body["failureMode"] != "worker_exited_without_result" {
		t.Fatalf("failureMode=%v, want %q", body["failureMode"], "worker_exited_without_result")
	}
	if body["summary"] == nil || body["summary"] == "" {
		t.Fatal("fallback body carries no human summary")
	}
	for _, tc := range []struct{ name, session, worker string }{
		{"empty session", "", "worker-9"},
		{"empty worker", "sess-fallback", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := p.PrepareWorkerExitedWithoutResultBody(tc.session, tc.worker, nil); err == nil {
				t.Fatal("missing identity was accepted")
			}
		})
	}
}

// TestFallbackBodySendsThroughPreparedOutcome pins that the fallback body
// travels the exact-bytes send path: what Prepare produces is what the
// receiver observes, with no reconstruction in between.
func TestFallbackBodySendsThroughPreparedOutcome(t *testing.T) {
	t.Parallel()
	srv, state := captureServer(t,
		func(_ int) (int, string) { return 200, `{"ok":true}` },
		func(_ int) (int, string) { return 200, `{"ok":true}` },
	)
	p := newPoster(t, srv.URL, 0)
	fallback, err := p.PrepareWorkerExitedWithoutResultBody("sess-fallback", "worker-9", nil)
	if err != nil {
		t.Fatal(err)
	}
	outcome := p.PostPreparedOutcome(context.Background(), "sess-fallback", agent.Result{Status: "failed"}, fallback)
	if err := outcome.Err(); err != nil {
		t.Fatalf("PostPreparedOutcome: %v", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	var statusSeen string
	for _, raw := range state.bodies {
		var body map[string]any
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		if _, ok := body["status"]; ok {
			if statusSeen != "" {
				t.Fatalf("two status sends: %q", state.bodies)
			}
			statusSeen = raw
		}
	}
	if statusSeen == "" {
		t.Fatalf("no status send observed: %q", state.bodies)
	}
	if statusSeen != string(fallback) {
		t.Fatalf("sent bytes differ from prepared bytes:\nsent=%s\nprepared=%s", statusSeen, fallback)
	}
}
