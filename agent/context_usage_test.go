package agent_test

import (
	"encoding/json"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func floatPtr(f float64) *float64 { return &f }
func intPtr(n int64) *int64       { return &n }
func boolPtr(b bool) *bool        { return &b }

// Fixture: a statusline sidecar JSON document in the shape the Claude
// harness reports — context-window observation beside session token
// totals.
const sidecarFixture = `{
  "used_percentage": 62.5,
  "total_input_tokens": 125000,
  "total_output_tokens": 8000,
  "context_window_size": 200000
}`

func TestParseClaudeSidecarNotice_FixtureDocument(t *testing.T) {
	t.Parallel()
	notice, err := agent.ParseClaudeSidecarNotice([]byte(sidecarFixture), "claude-code")
	if err != nil {
		t.Fatalf("ParseClaudeSidecarNotice: %v", err)
	}
	if notice == nil {
		t.Fatal("no notice for the fixture sidecar")
	}
	if notice.Harness != "claude-code" {
		t.Errorf("harness = %q, want claude-code", notice.Harness)
	}
	if notice.Source != agent.ContextUsageSourceStatuslineSidecar {
		t.Errorf("source = %q, want statusline-sidecar", notice.Source)
	}
	if notice.NoticeKind != agent.ContextUsageThreshold {
		t.Errorf("kind = %q, want threshold", notice.NoticeKind)
	}
	if notice.UsedPercent == nil || *notice.UsedPercent != 62.5 {
		t.Errorf("usedPercent = %v, want 62.5", notice.UsedPercent)
	}
	if notice.InputTokens == nil || *notice.InputTokens != 125000 {
		t.Errorf("inputTokens = %v, want 125000", notice.InputTokens)
	}
	if notice.OutputTokens == nil || *notice.OutputTokens != 8000 {
		t.Errorf("outputTokens = %v, want 8000", notice.OutputTokens)
	}
	if notice.ContextWindowSize == nil || *notice.ContextWindowSize != 200000 {
		t.Errorf("contextWindowSize = %v, want 200000", notice.ContextWindowSize)
	}
}

func TestParseClaudeSidecarNotice_PartialStaysPartial(t *testing.T) {
	t.Parallel()
	// A sidecar carrying only a percentage must not fabricate token
	// counts: absent stays nil, never zero.
	notice, err := agent.ParseClaudeSidecarNotice([]byte(`{"used_percentage": 10}`), "claude-code")
	if err != nil {
		t.Fatalf("ParseClaudeSidecarNotice: %v", err)
	}
	if notice == nil {
		t.Fatal("no notice for a percentage-only sidecar")
	}
	if notice.InputTokens != nil || notice.OutputTokens != nil || notice.ContextWindowSize != nil {
		t.Fatalf("partial sidecar fabricated counts: %+v", notice)
	}
	if notice.UsedPercent == nil || *notice.UsedPercent != 10 {
		t.Errorf("usedPercent = %v, want 10", notice.UsedPercent)
	}
	// Round-trip: unknown stays absent on the wire, never zero.
	raw, err := agent.MarshalEvent(*notice)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode wire: %v", err)
	}
	for _, key := range []string{"inputTokens", "outputTokens", "contextWindowSize"} {
		if _, present := wire[key]; present {
			t.Errorf("wire carries %q for an unknown measurement; want absent", key)
		}
	}
}

func TestParseClaudeSidecarNotice_CompactionMarker(t *testing.T) {
	t.Parallel()
	notice, err := agent.ParseClaudeSidecarNotice([]byte(`{"compacted": true}`), "claude-code")
	if err != nil {
		t.Fatalf("ParseClaudeSidecarNotice: %v", err)
	}
	if notice == nil {
		t.Fatal("no notice for a measurement-less compaction marker")
	}
	if notice.NoticeKind != agent.ContextUsageCompaction {
		t.Errorf("kind = %q, want compaction", notice.NoticeKind)
	}
	if notice.HasMeasurements() {
		t.Errorf("marker carries measurements: %+v", notice)
	}
}

func TestParseClaudeSidecarNotice_EmptyIsNoSignal(t *testing.T) {
	t.Parallel()
	notice, err := agent.ParseClaudeSidecarNotice([]byte(`{}`), "claude-code")
	if err != nil {
		t.Fatalf("ParseClaudeSidecarNotice: %v", err)
	}
	if notice != nil {
		t.Fatalf("empty sidecar yielded %+v, want nil", notice)
	}
}

func TestParseClaudeSidecarNotice_RejectsImpossible(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"negative tokens":   `{"total_input_tokens": -1}`,
		"percent over 100":  `{"used_percentage": 101}`,
		"negative percent":  `{"used_percentage": -2}`,
		"zero window":       `{"context_window_size": 0}`,
		"fractional tokens": `{"total_input_tokens": 1.5}`,
		"malformed":         `not json`,
	}
	for name, doc := range cases {
		if _, err := agent.ParseClaudeSidecarNotice([]byte(doc), "claude-code"); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

// Fixture: rollout JSONL lines in the shape a session file takes — one
// token_count event per model call beside unrelated line shapes.
const rolloutTokenCountFixture = `{"type":"token_count","payload":{"thread_id":"thread-1","total_input_tokens":90000,"total_output_tokens":4000,"context_window_size":200000,"used_percentage":47.0}}`

func TestParseCodexTokenCountNotice_FixtureLine(t *testing.T) {
	t.Parallel()
	notice, err := agent.ParseCodexTokenCountNotice([]byte(rolloutTokenCountFixture), "codex")
	if err != nil {
		t.Fatalf("ParseCodexTokenCountNotice: %v", err)
	}
	if notice == nil {
		t.Fatal("no notice for the fixture token_count line")
	}
	if notice.Source != agent.ContextUsageSourceRolloutTokenCount {
		t.Errorf("source = %q, want rollout-token-count", notice.Source)
	}
	if notice.NoticeKind != agent.ContextUsageThreshold {
		t.Errorf("kind = %q, want threshold", notice.NoticeKind)
	}
	if notice.InputTokens == nil || *notice.InputTokens != 90000 {
		t.Errorf("inputTokens = %v, want 90000", notice.InputTokens)
	}
	if notice.OutputTokens == nil || *notice.OutputTokens != 4000 {
		t.Errorf("outputTokens = %v, want 4000", notice.OutputTokens)
	}
	if notice.ContextWindowSize == nil || *notice.ContextWindowSize != 200000 {
		t.Errorf("contextWindowSize = %v, want 200000", notice.ContextWindowSize)
	}
	if notice.UsedPercent == nil || *notice.UsedPercent != 47 {
		t.Errorf("usedPercent = %v, want 47", notice.UsedPercent)
	}
}

func TestParseCodexTokenCountNotice_SkipsOtherLines(t *testing.T) {
	t.Parallel()
	lines := []string{
		`{"type":"session_meta","payload":{"id":"thread-1"}}`,
		`{"type":"token_usage_record","payload":{"thread_id":"thread-1"}}`,
		`not json at all`,
		`{"type":"event_msg","payload":{"message":"done"}}`,
	}
	for _, line := range lines {
		notice, err := agent.ParseCodexTokenCountNotice([]byte(line), "codex")
		if err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		if notice != nil {
			t.Fatalf("line %q yielded %+v, want nil", line, notice)
		}
	}
}

func TestParseCodexTokenCountNotice_CombinedTotalReadsAsInput(t *testing.T) {
	t.Parallel()
	notice, err := agent.ParseCodexTokenCountNotice([]byte(`{"type":"token_count","payload":{"token_count": 5000}}`), "codex")
	if err != nil {
		t.Fatalf("ParseCodexTokenCountNotice: %v", err)
	}
	if notice == nil {
		t.Fatal("no notice for a combined-total token_count")
	}
	if notice.InputTokens == nil || *notice.InputTokens != 5000 {
		t.Errorf("inputTokens = %v, want 5000", notice.InputTokens)
	}
	if notice.OutputTokens != nil {
		t.Errorf("outputTokens = %v for a combined total; want nil", notice.OutputTokens)
	}
}

func TestParseCodexTokenCountNotice_RejectsImpossible(t *testing.T) {
	t.Parallel()
	cases := []string{
		`{"type":"token_count","payload":{"total_input_tokens": -1}}`,
		`{"type":"token_count","payload":{"used_percentage": 120}}`,
		`{"type":"token_count","payload":{"context_window_size": 0}}`,
	}
	for _, line := range cases {
		if _, err := agent.ParseCodexTokenCountNotice([]byte(line), "codex"); err == nil {
			t.Errorf("line %q: want error, got nil", line)
		}
	}
}

func TestParsePiRPCUsageNotice_FixtureObject(t *testing.T) {
	t.Parallel()
	usage := &agent.PiRPCUsage{
		InputTokens: intPtr(60000), OutputTokens: intPtr(3000),
		ContextWindowSize: intPtr(1000000), UsedPercentage: floatPtr(6.3),
	}
	notice, err := agent.ParsePiRPCUsageNotice(usage, "pi")
	if err != nil {
		t.Fatalf("ParsePiRPCUsageNotice: %v", err)
	}
	if notice == nil {
		t.Fatal("no notice for the fixture usage object")
	}
	if notice.Source != agent.ContextUsageSourceRPCUsage {
		t.Errorf("source = %q, want rpc-usage", notice.Source)
	}
	if notice.UsedPercent == nil || *notice.UsedPercent != 6.3 {
		t.Errorf("usedPercent = %v, want 6.3", notice.UsedPercent)
	}
	if notice.InputTokens == nil || *notice.InputTokens != 60000 {
		t.Errorf("inputTokens = %v, want 60000", notice.InputTokens)
	}
}

func TestParsePiRPCUsageNotice_NilAndEmptyAreNoSignal(t *testing.T) {
	t.Parallel()
	if notice, err := agent.ParsePiRPCUsageNotice(nil, "pi"); err != nil || notice != nil {
		t.Fatalf("nil usage = %+v, %v; want nil, nil", notice, err)
	}
	if notice, err := agent.ParsePiRPCUsageNotice(&agent.PiRPCUsage{}, "pi"); err != nil || notice != nil {
		t.Fatalf("empty usage = %+v, %v; want nil, nil", notice, err)
	}
}

func TestContextUsageEvent_RoundTripsThroughEventCodec(t *testing.T) {
	t.Parallel()
	original := agent.ContextUsageEvent{
		Harness: "codex", Source: agent.ContextUsageSourceRolloutTokenCount,
		NoticeKind: agent.ContextUsageCompaction,
		UsedPercent: floatPtr(88), InputTokens: intPtr(176000),
	}
	raw, err := agent.MarshalEvent(original)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	decoded, err := agent.UnmarshalEvent(raw)
	if err != nil {
		t.Fatalf("UnmarshalEvent: %v", err)
	}
	got, ok := decoded.(agent.ContextUsageEvent)
	if !ok {
		t.Fatalf("decoded type = %T, want ContextUsageEvent", decoded)
	}
	if got.NoticeKind != agent.ContextUsageCompaction || got.Harness != "codex" {
		t.Fatalf("decoded = %+v", got)
	}
	if got.UsedPercent == nil || *got.UsedPercent != 88 {
		t.Errorf("usedPercent = %v, want 88", got.UsedPercent)
	}
	if got.OutputTokens != nil || got.ContextWindowSize != nil {
		t.Errorf("round trip fabricated unknowns: %+v", got)
	}
	if decoded.Kind() != agent.EventContextUsage {
		t.Errorf("Kind() = %q, want context_usage", decoded.Kind())
	}
}
