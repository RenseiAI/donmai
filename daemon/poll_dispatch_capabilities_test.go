package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/runner"
)

// TestPollDispatchTargetCapabilities threads the dispatch-target span-ingest
// flag from the raw poll item onto the built SessionDetail: the coordinator
// stamps "capabilities":{"llm-span-ingest":true} on every poll item, and the
// runner gates its additive per-call span pipeline on that key. Older
// coordinators that stamp nothing, and worker-local flags, behave exactly as
// before.
func TestPollDispatchTargetCapabilities(t *testing.T) {
	spanKey := runner.CapabilitySpanIngest

	t.Run("AC-1 raw poll JSON with the stamped key builds a detail carrying it", func(t *testing.T) {
		raw := `{"sessionId":"sess-span-1","capabilities":{"llm-span-ingest":true}}`
		var item PollWorkItem
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			t.Fatalf("decode poll item: %v", err)
		}
		if !item.Capabilities[spanKey] {
			t.Fatalf("decoded item Capabilities[%q] = false; want true", spanKey)
		}
		detail := PollItemToSessionDetail(item, nil, "", "", "")
		if !detail.Capabilities[spanKey] {
			t.Errorf("detail Capabilities[%q] = false; want true", spanKey)
		}
	})

	t.Run("AC-2 no field and no options keeps Capabilities nil", func(t *testing.T) {
		detail := PollItemToSessionDetail(PollWorkItem{SessionID: "sess-span-2"}, nil, "", "", "")
		if detail.Capabilities != nil {
			t.Errorf("Capabilities = %v; want nil", detail.Capabilities)
		}
	})

	t.Run("AC-3 worker capabilities and the item key coexist", func(t *testing.T) {
		item := PollWorkItem{
			SessionID:    "sess-span-3",
			Capabilities: map[string]bool{spanKey: true},
		}
		detail := PollItemToSessionDetail(item, nil, "", "", "",
			WithWorkerCapabilities(map[string]bool{"x": true}))
		if !detail.Capabilities["x"] {
			t.Errorf(`Capabilities["x"] = false; want true (worker option survives the item merge)`)
		}
		if !detail.Capabilities[spanKey] {
			t.Errorf("Capabilities[%q] = false; want true (item key merges after the options loop)", spanKey)
		}
	})

	t.Run("AC-4 unlisted item keys are never threaded", func(t *testing.T) {
		item := PollWorkItem{
			SessionID: "sess-span-4",
			Capabilities: map[string]bool{
				"merge-queue":            true,
				"execution-event-ingest": true,
				"foo":                    true,
			},
		}
		detail := PollItemToSessionDetail(item, nil, "", "", "", WithMergeQueueLanding(nil))
		for _, key := range []string{"merge-queue", "execution-event-ingest", "foo"} {
			if detail.Capabilities[key] {
				t.Errorf("Capabilities[%q] = true; want absent (only %q is threaded)", key, spanKey)
			}
		}
		if detail.Capabilities != nil {
			t.Errorf("Capabilities = %v; want nil (no listed key stamped true)", detail.Capabilities)
		}
	})

	t.Run("AC-5 a false item key never clears an option-set flag", func(t *testing.T) {
		item := PollWorkItem{
			SessionID:    "sess-span-5",
			Capabilities: map[string]bool{spanKey: false},
		}
		detail := PollItemToSessionDetail(item, nil, "", "", "",
			WithWorkerCapabilities(map[string]bool{spanKey: true}))
		if !detail.Capabilities[spanKey] {
			t.Errorf("Capabilities[%q] = false; want true (item false must not clear the option)", spanKey)
		}
	})

	t.Run("AC-6 served detail JSON round-trips the key into runner QueuedWork", func(t *testing.T) {
		item := PollWorkItem{
			SessionID:    "sess-span-6",
			Capabilities: map[string]bool{spanKey: true},
		}
		detail := PollItemToSessionDetail(item, nil, "", "", "")
		// The session route serves the detail through writeJSON (the
		// any-typed encoder the credentialed read path uses), so
		// serialize through that same serializer: the bytes under test
		// are the bytes the route writes, and the runner decodes them
		// into QueuedWork on the same shape.
		rec := httptest.NewRecorder()
		writeJSON(rec, http.StatusOK, detail)
		served := rec.Body.Bytes()
		if want := `"capabilities":{"llm-span-ingest":true}`; !strings.Contains(string(served), want) {
			t.Errorf("served detail lacks %s; got %s", want, served)
		}
		var qw runner.QueuedWork
		if err := json.Unmarshal(served, &qw); err != nil {
			t.Fatalf("decode served detail into QueuedWork: %v", err)
		}
		if !qw.Capabilities[spanKey] {
			t.Errorf("QueuedWork Capabilities[%q] = false; want true", spanKey)
		}
	})
}
