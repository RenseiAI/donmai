package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestPollResponse_DecodesBatchWork verifies the batchWork[] array decodes into
// typed BatchWorkItem envelopes with the discriminants surfaced and Raw
// preserved, while leaving the agent WorkItems lane untouched.
func TestPollResponse_DecodesBatchWork(t *testing.T) {
	body := `{
		"work_items": [{"id":"wi_1","type":"session.start","payload":{"k":"v"}}],
		"batchWork": [{
			"batchJobId":"batch:due_checkpoint:42",
			"workType":"code-survival-scan",
			"contractVersion":1,
			"orgId":"org-1",
			"mergeSha":"abc",
			"resultEndpoint":"https://p/api/factory/code-survival/results"
		}]
	}`
	var resp PollResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.WorkItems) != 1 || resp.WorkItems[0].ID != "wi_1" {
		t.Fatalf("agent lane wrong: %+v", resp.WorkItems)
	}
	if len(resp.BatchWork) != 1 {
		t.Fatalf("len(BatchWork) = %d, want 1", len(resp.BatchWork))
	}
	bw := resp.BatchWork[0]
	if bw.BatchJobID != "batch:due_checkpoint:42" || bw.WorkType != "code-survival-scan" {
		t.Errorf("discriminants = %q / %q", bw.BatchJobID, bw.WorkType)
	}
	// Raw must carry the full item so the router can decode the work-type
	// specific fields.
	var full map[string]any
	if err := json.Unmarshal(bw.Raw, &full); err != nil {
		t.Fatalf("Raw not valid json: %v", err)
	}
	if full["mergeSha"] != "abc" || full["resultEndpoint"] == nil {
		t.Errorf("Raw missing work-type fields: %v", full)
	}
}

// TestPollLoopWithBatch_LaneIsolation is the core isolation guarantee: a batch
// item is routed to the batch handler and NEVER to the agent handler, and an
// agent item is routed to the agent handler and NEVER to the batch handler.
// It also verifies that a landingWork item flows through batchHandler (same mux).
func TestPollLoopWithBatch_LaneIsolation(t *testing.T) {
	var served atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Serve the work once, then empty so the loop spins without piling up.
		if served.Swap(true) {
			_ = json.NewEncoder(w).Encode(PollResponse{})
			return
		}
		resp := PollResponse{
			WorkItems: []WorkItem{{ID: "wi_agent", Type: "session.start", CreatedAt: time.Now()}},
			BatchWork: []BatchWorkItem{{
				BatchJobID: "batch:due_checkpoint:1",
				WorkType:   "code-survival-scan",
				Raw:        json.RawMessage(`{"batchJobId":"batch:due_checkpoint:1","workType":"code-survival-scan"}`),
			}},
			LandingWork: []BatchWorkItem{{
				BatchJobID: "landing:claim:99",
				WorkType:   "landing-claim",
				Raw:        json.RawMessage(`{"batchJobId":"landing:claim:99","workType":"landing-claim"}`),
			}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	c := newRegisteredClient(srv)

	var agentItems, batchItems []string
	var agentSawBatch, batchSawAgent atomic.Bool

	agentHandler := func(item WorkItem) error {
		agentItems = append(agentItems, item.ID)
		// An agent handler must NEVER receive a batch job id.
		if item.ID == "batch:due_checkpoint:1" || item.ID == "landing:claim:99" {
			agentSawBatch.Store(true)
		}
		return nil
	}
	batchHandler := func(_ context.Context, item BatchWorkItem) error {
		batchItems = append(batchItems, item.BatchJobID)
		// A batch handler must NEVER receive an agent work item.
		if item.BatchJobID == "wi_agent" {
			batchSawAgent.Store(true)
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_ = c.PollLoopWithBatch(ctx, 10*time.Millisecond, agentHandler, batchHandler)

	if len(agentItems) == 0 || agentItems[0] != "wi_agent" {
		t.Errorf("agent lane = %v, want [wi_agent]", agentItems)
	}
	if len(batchItems) < 2 {
		t.Fatalf("batch lane = %v, want at least 2 items (batchWork + landingWork)", batchItems)
	}
	var sawBatchWork, sawLandingWork bool
	for _, id := range batchItems {
		switch id {
		case "batch:due_checkpoint:1":
			sawBatchWork = true
		case "landing:claim:99":
			sawLandingWork = true
		}
	}
	if !sawBatchWork {
		t.Errorf("batch lane missing batchWork item: %v", batchItems)
	}
	if !sawLandingWork {
		t.Errorf("batch lane missing landingWork item: %v", batchItems)
	}
	if agentSawBatch.Load() {
		t.Error("ISOLATION VIOLATION: agent handler received a batch item")
	}
	if batchSawAgent.Load() {
		t.Error("ISOLATION VIOLATION: batch handler received an agent item")
	}
}

// TestPollLoopWithBatch_LandingWorkDispatched verifies that landingWork[] items
// are routed through batchHandler exactly like batchWork[] and kgExtractWork[]
// items — sharing the same workType-mux, never touching the agent lane.
func TestPollLoopWithBatch_LandingWorkDispatched(t *testing.T) {
	var served atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if served.Swap(true) {
			_ = json.NewEncoder(w).Encode(PollResponse{})
			return
		}
		resp := PollResponse{
			LandingWork: []BatchWorkItem{{
				BatchJobID: "landing:claim:42",
				WorkType:   "landing-claim",
				Raw:        json.RawMessage(`{"batchJobId":"landing:claim:42","workType":"landing-claim","orgId":"org-x"}`),
			}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	c := newRegisteredClient(srv)

	var batchItems []string
	var agentCalls atomic.Int32

	agentHandler := func(_ WorkItem) error {
		agentCalls.Add(1)
		return nil
	}
	batchHandler := func(_ context.Context, item BatchWorkItem) error {
		batchItems = append(batchItems, item.BatchJobID)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_ = c.PollLoopWithBatch(ctx, 10*time.Millisecond, agentHandler, batchHandler)

	if len(batchItems) == 0 || batchItems[0] != "landing:claim:42" {
		t.Errorf("landing lane = %v, want [landing:claim:42]", batchItems)
	}
	if agentCalls.Load() != 0 {
		t.Errorf("ISOLATION VIOLATION: agent handler called %d times, want 0", agentCalls.Load())
	}
}

// TestPollLoopWithBatch_NilBatchHandler asserts graceful degradation: a worker
// with no batch handler skips batch items (logs) without crashing the loop or
// affecting the agent lane.
func TestPollLoopWithBatch_NilBatchHandler(t *testing.T) {
	var served atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if served.Swap(true) {
			_ = json.NewEncoder(w).Encode(PollResponse{})
			return
		}
		_ = json.NewEncoder(w).Encode(PollResponse{
			WorkItems: []WorkItem{{ID: "wi_agent", Type: "session.start", CreatedAt: time.Now()}},
			BatchWork: []BatchWorkItem{{BatchJobID: "b1", WorkType: "code-survival-scan", Raw: json.RawMessage(`{}`)}},
		})
	}))
	t.Cleanup(srv.Close)

	c := newRegisteredClient(srv)
	var agentItems atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	// nil batch handler — must not panic.
	err := c.PollLoopWithBatch(ctx, 10*time.Millisecond, func(WorkItem) error {
		agentItems.Add(1)
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("loop returned error: %v", err)
	}
	if agentItems.Load() == 0 {
		t.Error("agent lane should still process items when batch handler is nil")
	}
}

// TestPollLoop_LegacyUnchanged asserts the legacy PollLoop (no batch handler)
// still drives only the agent lane, byte-for-byte behaviour preserved.
func TestPollLoop_LegacyUnchanged(t *testing.T) {
	var served atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if served.Swap(true) {
			_ = json.NewEncoder(w).Encode(PollResponse{})
			return
		}
		_ = json.NewEncoder(w).Encode(PollResponse{
			WorkItems: []WorkItem{{ID: "wi_1", Type: "session.start", CreatedAt: time.Now()}},
		})
	}))
	t.Cleanup(srv.Close)

	c := newRegisteredClient(srv)
	var got atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = c.PollLoop(ctx, 10*time.Millisecond, func(WorkItem) error {
		got.Add(1)
		return nil
	})
	if got.Load() == 0 {
		t.Error("legacy PollLoop should still process agent items")
	}
}
