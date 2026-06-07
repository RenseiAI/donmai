package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// ── fakes ────────────────────────────────────────────────────────────────────

type fakeHandle struct {
	events  chan Event
	stopped int
}

func (h *fakeHandle) SessionID() string                    { return "fake-session" }
func (h *fakeHandle) Events() <-chan Event                 { return h.events }
func (h *fakeHandle) Inject(context.Context, string) error { return nil }
func (h *fakeHandle) Stop(context.Context) error           { h.stopped++; return nil }

type fakeHarness struct {
	transport    TransportKind
	events       []Event
	spawnErr     error
	blockForever bool // when true, Spawn returns a handle whose Events() never closes
	lastSpec     Spec
	lastHandle   *fakeHandle
}

func (f *fakeHarness) Name() ProviderName         { return ProviderName("fake") }
func (f *fakeHarness) Capabilities() Capabilities { return Capabilities{} }
func (f *fakeHarness) Resume(context.Context, string, Spec) (Handle, error) {
	return nil, ErrUnsupported
}
func (f *fakeHarness) Shutdown(context.Context) error { return nil }

func (f *fakeHarness) Manifest() HarnessManifest {
	return HarnessManifest{Name: HarnessStub, Caps: HarnessCaps{Transport: f.transport}}
}

func (f *fakeHarness) Spawn(_ context.Context, spec Spec) (Handle, error) {
	f.lastSpec = spec
	if f.spawnErr != nil {
		return nil, f.spawnErr
	}
	ch := make(chan Event, len(f.events))
	for _, e := range f.events {
		ch <- e
	}
	if !f.blockForever {
		close(ch)
	}
	h := &fakeHandle{events: ch}
	f.lastHandle = h
	return h, nil
}

func ok(success bool, errs ...string) ResultEvent {
	return ResultEvent{Success: success, Errors: errs}
}

var verdictSchema = json.RawMessage(`{"type":"object","properties":{"verdict":{"type":"string"}},"required":["verdict"]}`)

// ── SpawnComplete ─────────────────────────────────────────────────────────────

func TestSpawnComplete_SoftJSON_HappyPath(t *testing.T) {
	h := &fakeHarness{
		transport: TransportKind("cli-injection"),
		events: []Event{
			AssistantTextEvent{Text: `{"verdict":"pass"}`},
			ResultEvent{Success: true, Cost: &CostData{TotalCostUsd: 0.42}},
		},
	}
	res, err := SpawnComplete(context.Background(), h, OneShotRequest{
		Messages:       []Message{{Role: "user", Content: "judge it"}},
		ResponseSchema: verdictSchema,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.SchemaOK {
		t.Errorf("SchemaOK = false, want true (structured=%q)", res.Structured)
	}
	if string(res.Structured) != `{"verdict":"pass"}` {
		t.Errorf("Structured = %q", res.Structured)
	}
	if res.Cost == nil || res.Cost.TotalCostUsd != 0.42 {
		t.Errorf("Cost not passed through: %+v", res.Cost)
	}
	if res.TransportUsed != TransportKind("cli-injection") {
		t.Errorf("TransportUsed = %q, want cli-injection", res.TransportUsed)
	}
	if h.lastHandle == nil || h.lastHandle.stopped == 0 {
		t.Errorf("session was not Stop()'d (cleanup leak)")
	}
}

func TestSpawnComplete_ExtractsJSONFromProseAndFences(t *testing.T) {
	h := &fakeHarness{
		events: []Event{
			AssistantTextEvent{Text: "Sure, here's the result:\n```json\n{\"verdict\":\"fail\"}\n```\nHope that helps!"},
			ok(true),
		},
	}
	res, err := SpawnComplete(context.Background(), h, OneShotRequest{
		Messages:       []Message{{Content: "x"}},
		ResponseSchema: verdictSchema,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.SchemaOK || string(res.Structured) != `{"verdict":"fail"}` {
		t.Errorf("did not extract JSON from prose/fences: ok=%v structured=%q", res.SchemaOK, res.Structured)
	}
}

func TestSpawnComplete_SchemaMismatch_SoftDrop(t *testing.T) {
	h := &fakeHarness{
		events: []Event{
			AssistantTextEvent{Text: `{"unrelated":"shape"}`}, // valid JSON, missing required "verdict"
			ok(true),
		},
	}
	res, err := SpawnComplete(context.Background(), h, OneShotRequest{
		Messages:       []Message{{Content: "x"}},
		ResponseSchema: verdictSchema,
	})
	if err != nil {
		t.Fatalf("schema mismatch must NOT be an error (validate-repair-drop): %v", err)
	}
	if res.SchemaOK {
		t.Errorf("SchemaOK = true, want false on schema mismatch")
	}
	if res.Structured != nil {
		t.Errorf("Structured = %q, want nil on !SchemaOK", res.Structured)
	}
	if res.Text != `{"unrelated":"shape"}` {
		t.Errorf("Text should still carry raw output, got %q", res.Text)
	}
}

func TestSpawnComplete_NoJSON_SoftDrop(t *testing.T) {
	h := &fakeHarness{events: []Event{AssistantTextEvent{Text: "I cannot do that."}, ok(true)}}
	res, err := SpawnComplete(context.Background(), h, OneShotRequest{
		Messages:       []Message{{Content: "x"}},
		ResponseSchema: verdictSchema,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.SchemaOK {
		t.Errorf("SchemaOK = true, want false when no JSON present")
	}
}

func TestSpawnComplete_NoSchema_FreeText(t *testing.T) {
	h := &fakeHarness{events: []Event{AssistantTextEvent{Text: "free text answer"}, ok(true)}}
	res, err := SpawnComplete(context.Background(), h, OneShotRequest{
		Messages: []Message{{Content: "x"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "free text answer" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.SchemaOK || res.Structured != nil {
		t.Errorf("no schema => SchemaOK false + Structured nil; got ok=%v structured=%q", res.SchemaOK, res.Structured)
	}
}

func TestSpawnComplete_AccumulatesMultipleTextChunks(t *testing.T) {
	h := &fakeHarness{events: []Event{
		AssistantTextEvent{Text: `{"verd`},
		AssistantTextEvent{Text: `ict":"pass"}`},
		ok(true),
	}}
	res, err := SpawnComplete(context.Background(), h, OneShotRequest{
		Messages:       []Message{{Content: "x"}},
		ResponseSchema: verdictSchema,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.SchemaOK {
		t.Errorf("did not accumulate split chunks: text=%q", res.Text)
	}
}

func TestSpawnComplete_SessionFailure_IsError(t *testing.T) {
	h := &fakeHarness{events: []Event{
		AssistantTextEvent{Text: `{"verdict":"pass"}`},
		ResultEvent{Success: false, Errors: []string{"boom", "kaboom"}},
	}}
	_, err := SpawnComplete(context.Background(), h, OneShotRequest{Messages: []Message{{Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected failure error mentioning boom, got %v", err)
	}
}

func TestSpawnComplete_ErrorEvent_IsError(t *testing.T) {
	h := &fakeHarness{events: []Event{ErrorEvent{Message: "rate_limited"}}}
	_, err := SpawnComplete(context.Background(), h, OneShotRequest{Messages: []Message{{Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "rate_limited") {
		t.Fatalf("expected error event surfaced, got %v", err)
	}
}

func TestSpawnComplete_NoTerminalEvent_IsError(t *testing.T) {
	h := &fakeHarness{events: []Event{AssistantTextEvent{Text: "partial"}}} // channel closes, no ResultEvent
	_, err := SpawnComplete(context.Background(), h, OneShotRequest{Messages: []Message{{Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "terminal result") {
		t.Fatalf("expected no-terminal-result error, got %v", err)
	}
}

func TestSpawnComplete_SpawnError_Wrapped(t *testing.T) {
	sentinel := errors.New("spawn boom")
	h := &fakeHarness{spawnErr: sentinel}
	_, err := SpawnComplete(context.Background(), h, OneShotRequest{Messages: []Message{{Content: "x"}}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped spawn error, got %v", err)
	}
}

func TestSpawnComplete_ErrorThenSuccess_ErrorWins(t *testing.T) {
	// An ErrorEvent followed by a success ResultEvent must still be an error:
	// failErr is set first and a later success cannot clear it.
	h := &fakeHarness{events: []Event{
		ErrorEvent{Message: "transport_died"},
		ResultEvent{Success: true},
	}}
	_, err := SpawnComplete(context.Background(), h, OneShotRequest{Messages: []Message{{Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "transport_died") {
		t.Fatalf("error must win over a trailing success result, got %v", err)
	}
}

func TestSpawnComplete_ContextCancel_AbortsAndStops(t *testing.T) {
	// Events() never closes; a hung harness must not pin Complete forever — ctx
	// cancellation aborts the drive and the session is still Stop()'d.
	h := &fakeHarness{blockForever: true}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := SpawnComplete(ctx, h, OneShotRequest{Messages: []Message{{Content: "x"}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if h.lastHandle == nil || h.lastHandle.stopped == 0 {
		t.Errorf("session not Stop()'d on ctx cancel (cleanup leak)")
	}
}

// ── specFromOneShot ───────────────────────────────────────────────────────────

func TestSpecFromOneShot(t *testing.T) {
	ep := &EndpointBinding{Company: Company("anthropic"), Model: "claude-haiku"}
	spec := specFromOneShot(OneShotRequest{
		System:         "you are a judge",
		Messages:       []Message{{Role: "user", Content: "the task"}},
		ResponseSchema: verdictSchema,
		Effort:         EffortLow,
		Endpoint:       ep,
	})
	if spec.SystemPromptAppend != "you are a judge" {
		t.Errorf("System not mapped to SystemPromptAppend: %q", spec.SystemPromptAppend)
	}
	if !strings.Contains(spec.Prompt, "the task") {
		t.Errorf("prompt missing message content: %q", spec.Prompt)
	}
	if !strings.Contains(spec.Prompt, "JSON Schema") || !strings.Contains(spec.Prompt, `"verdict"`) {
		t.Errorf("prompt missing schema instruction: %q", spec.Prompt)
	}
	if spec.MaxTurns == nil || *spec.MaxTurns != 1 {
		t.Errorf("MaxTurns not pinned to 1: %v", spec.MaxTurns)
	}
	if !spec.Autonomous {
		t.Errorf("Autonomous should be true")
	}
	if spec.Model != "claude-haiku" || spec.Endpoint != ep {
		t.Errorf("endpoint not threaded: model=%q endpoint=%v", spec.Model, spec.Endpoint)
	}
	if spec.Effort != EffortLow {
		t.Errorf("Effort = %q", spec.Effort)
	}
}

func TestFlattenMessages(t *testing.T) {
	if got := flattenMessages([]Message{{Content: "solo"}}); got != "solo" {
		t.Errorf("single message = %q, want verbatim", got)
	}
	got := flattenMessages([]Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "yo"}, {Content: "again"}})
	want := "user: hi\n\nassistant: yo\n\nuser: again"
	if got != want {
		t.Errorf("multi = %q, want %q", got, want)
	}
}

// ── extractJSONValue ──────────────────────────────────────────────────────────

func TestExtractJSONValue(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"bare object", `{"a":1}`, `{"a":1}`, true},
		{"prose wrapped", `prefix {"a":{"b":2}} suffix`, `{"a":{"b":2}}`, true},
		{"array", `[1,2,3]`, `[1,2,3]`, true},
		{"brace inside string", `{"s":"has } brace"}`, `{"s":"has } brace"}`, true},
		{"escaped quote then brace", `{"s":"esc \" and } "}`, `{"s":"esc \" and } "}`, true},
		{"escaped backslash then quote", `{"k":"a\\"}`, `{"k":"a\\"}`, true},
		{"first of two", `a {"x":1} b {"y":2}`, `{"x":1}`, true},
		{"opener quoted in prose recovers", `He said "use {" then {"a":1}`, `{"a":1}`, true},
		{"bare bracket in prose recovers", `list [ then {"a":1}`, `{"a":1}`, true},
		{"balanced but invalid recovers to next", `{,} then {"a":1}`, `{"a":1}`, true},
		{"no json", `no json here`, "", false},
		{"unbalanced", `{"a":1`, "", false},
		{"fenced", "```json\n{\"k\":true}\n```", `{"k":true}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractJSONValue(tt.in)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v (got %q)", ok, tt.ok, got)
			}
			if ok && string(got) != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateAgainstSchema(t *testing.T) {
	if !validateAgainstSchema(json.RawMessage(`{"verdict":"x"}`), verdictSchema) {
		t.Errorf("valid instance rejected")
	}
	if validateAgainstSchema(json.RawMessage(`{"nope":"x"}`), verdictSchema) {
		t.Errorf("missing-required instance accepted")
	}
	if validateAgainstSchema(json.RawMessage(`{"verdict":"x"}`), json.RawMessage(`{not valid schema`)) {
		t.Errorf("malformed schema must fail-closed (return false)")
	}
	if validateAgainstSchema(json.RawMessage(`not json`), verdictSchema) {
		t.Errorf("malformed instance accepted")
	}
	if !validateAgainstSchema(json.RawMessage(`{"anything":1}`), json.RawMessage(`{}`)) {
		t.Errorf("empty schema {} should match anything")
	}
	if !validateAgainstSchema(json.RawMessage(`{"x":1}`), json.RawMessage(`true`)) {
		t.Errorf("boolean schema true should match anything")
	}
	if validateAgainstSchema(json.RawMessage(`{"x":1}`), json.RawMessage(`false`)) {
		t.Errorf("boolean schema false should match nothing")
	}
}
