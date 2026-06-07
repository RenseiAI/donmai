package codeintel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// archFakeHarness is a minimal HarnessProvider that replays a fixed event
// sequence, driving the real one-shot lane (agent.Complete → SpawnComplete) that
// LaneAdapter.AssessChange depends on.
type archFakeHarness struct {
	events   []agent.Event
	spawnErr error
}

func (archFakeHarness) Name() agent.ProviderName         { return agent.ProviderStub }
func (archFakeHarness) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (archFakeHarness) Shutdown(context.Context) error   { return nil }
func (archFakeHarness) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}

func (archFakeHarness) Manifest() agent.HarnessManifest {
	return agent.HarnessManifest{Name: agent.HarnessStub}
}

func (h archFakeHarness) Spawn(_ context.Context, _ agent.Spec) (agent.Handle, error) {
	if h.spawnErr != nil {
		return nil, h.spawnErr
	}
	ch := make(chan agent.Event, len(h.events))
	for _, e := range h.events {
		ch <- e
	}
	close(ch)
	return &archFakeHandle{events: ch}, nil
}

type archFakeHandle struct{ events chan agent.Event }

func (h *archFakeHandle) SessionID() string                    { return "arch-fake" }
func (h *archFakeHandle) Events() <-chan agent.Event           { return h.events }
func (h *archFakeHandle) Inject(context.Context, string) error { return agent.ErrUnsupported }
func (h *archFakeHandle) Stop(context.Context) error           { return nil }

func emitJSON(text string) []agent.Event {
	return []agent.Event{
		agent.AssistantTextEvent{Text: text},
		agent.ResultEvent{Success: true},
	}
}

func sampleReq() AssessChangeRequest {
	return AssessChangeRequest{
		Change:       "owner/repo#123",
		Observations: []DiffObservation{{Kind: "pattern", Payload: map[string]any{"name": "global state"}, Confidence: 0.9, Scope: "project"}},
		Conventions:  []string{"no global mutable state"},
	}
}

func TestLaneAdapter_AssessChange_CriticalDeviation(t *testing.T) {
	h := archFakeHarness{events: emitJSON(
		`{"deviations":[{"observation":"global state","severity":"critical","rationale":"violates no-globals"}]}`,
	)}
	got, err := LaneAdapter{Harness: h}.AssessChange(context.Background(), sampleReq())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.SchemaOK {
		t.Errorf("SchemaOK = false, want true")
	}
	if len(got.Deviations) != 1 || got.Deviations[0].Severity != SeverityCritical {
		t.Fatalf("deviations = %+v", got.Deviations)
	}
	if !got.HasCriticalDrift {
		t.Errorf("HasCriticalDrift = false, want true")
	}
}

func TestLaneAdapter_AssessChange_NoDeviations(t *testing.T) {
	h := archFakeHarness{events: emitJSON(`{"deviations":[]}`)}
	got, err := LaneAdapter{Harness: h}.AssessChange(context.Background(), sampleReq())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.SchemaOK {
		t.Errorf("SchemaOK = false, want true")
	}
	if len(got.Deviations) != 0 || got.HasCriticalDrift {
		t.Errorf("expected no deviations + no critical drift, got %+v / %v", got.Deviations, got.HasCriticalDrift)
	}
}

func TestLaneAdapter_AssessChange_WarningOnly(t *testing.T) {
	h := archFakeHarness{events: emitJSON(
		`{"deviations":[{"observation":"x","severity":"warning","rationale":"minor"}]}`,
	)}
	got, err := LaneAdapter{Harness: h}.AssessChange(context.Background(), sampleReq())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.HasCriticalDrift {
		t.Errorf("HasCriticalDrift = true, want false for warning-only")
	}
	if len(got.Deviations) != 1 {
		t.Errorf("want 1 deviation, got %d", len(got.Deviations))
	}
}

func TestLaneAdapter_AssessChange_SoftMiss(t *testing.T) {
	// Model emits prose, not schema-valid JSON → SchemaOK false, no error.
	h := archFakeHarness{events: emitJSON("I could not determine any deviations, sorry.")}
	got, err := LaneAdapter{Harness: h}.AssessChange(context.Background(), sampleReq())
	if err != nil {
		t.Fatalf("soft miss must not be an error: %v", err)
	}
	if got.SchemaOK {
		t.Errorf("SchemaOK = true, want false on a non-JSON response")
	}
	if len(got.Deviations) != 0 || got.HasCriticalDrift {
		t.Errorf("soft miss must yield no deviations, got %+v", got)
	}
}

func TestLaneAdapter_AssessChange_SchemaMismatchIsSoftMiss(t *testing.T) {
	// Valid JSON but wrong shape (missing required severity) → schema-invalid → soft miss.
	h := archFakeHarness{events: emitJSON(`{"deviations":[{"observation":"x"}]}`)}
	got, err := LaneAdapter{Harness: h}.AssessChange(context.Background(), sampleReq())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.SchemaOK {
		t.Errorf("SchemaOK = true, want false on schema mismatch")
	}
}

func TestLaneAdapter_AssessChange_NilHarness(t *testing.T) {
	_, err := LaneAdapter{}.AssessChange(context.Background(), sampleReq())
	if err == nil {
		t.Fatal("expected error for nil Harness")
	}
}

func TestLaneAdapter_AssessChange_ProviderError(t *testing.T) {
	sentinel := errors.New("spawn boom")
	h := archFakeHarness{spawnErr: sentinel}
	_, err := LaneAdapter{Harness: h}.AssessChange(context.Background(), sampleReq())
	if err == nil {
		t.Fatal("expected provider error to surface")
	}
}

// LaneAdapter must satisfy ModelAdapter — the mandated seam.
var _ ModelAdapter = LaneAdapter{}

func TestRenderAssessPrompt(t *testing.T) {
	p := renderAssessPrompt(sampleReq())
	for _, want := range []string{"owner/repo#123", "Observations", "global state", "no global mutable state"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	// No conventions → explicit "(none provided)".
	empty := renderAssessPrompt(AssessChangeRequest{Observations: []DiffObservation{}})
	if !strings.Contains(empty, "(none provided)") {
		t.Errorf("empty conventions should render '(none provided)': %s", empty)
	}
}
