package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/pi"
)

func TestSubagentFixtureGeneration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		breakFixture func(*SubagentFixtureSource)
		want         bool
	}{
		{name: "passing", want: true},
		{name: "failed", breakFixture: func(s *SubagentFixtureSource) {
			s.Execute = func(context.Context) (SubagentFixtureExecution, error) {
				return SubagentFixtureExecution{}, errors.New("fixture failed")
			}
		}},
		{name: "skipped", breakFixture: func(s *SubagentFixtureSource) {
			s.Execute = func(context.Context) (SubagentFixtureExecution, error) {
				return SubagentFixtureExecution{}, ErrCapabilityFixtureSkipped
			}
		}},
		{name: "missing-terminal", breakFixture: func(s *SubagentFixtureSource) {
			execute := s.Execute
			s.Execute = func(ctx context.Context) (SubagentFixtureExecution, error) {
				execution, err := execute(ctx)
				var events []agent.Event
				for _, event := range execution.Events {
					if sub, ok := event.(agent.SubagentEvent); ok && sub.Phase == agent.SubagentFailed {
						continue
					}
					events = append(events, event)
				}
				execution.Events = events
				return execution, err
			}
		}},
		{name: "wrong-phase", breakFixture: func(s *SubagentFixtureSource) {
			s.Calls[1].TerminalPhase = agent.SubagentCompleted
		}},
		{name: "wrong-child", breakFixture: func(s *SubagentFixtureSource) {
			s.Calls[0].ChildSessionID = "independently-witnessed-child"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sources := NativeSubagentFixtures()
			if len(sources) == 0 {
				t.Fatal("native fixture registry is empty")
			}
			for i := range sources {
				if tc.breakFixture != nil {
					tc.breakFixture(&sources[i])
				}
			}
			built, err := BuildWithSubagentFixtures(t.Context(), nil, sources)
			if err != nil {
				t.Fatal(err)
			}
			assertSubagentArtifacts(t, built, sources[0].Cell, tc.want)
			for _, h := range built.Harnesses {
				want := h.Name == agent.HarnessClaudeCode && tc.want
				if h.Caps.EmitsSubagentEvents != want {
					t.Fatalf("harness %s subagents = %v, want %v from executed fixtures", h.Name, h.Caps.EmitsSubagentEvents, want)
				}
			}
		})
	}
}

func TestSubagentPiDeliveryGeneration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		tools  []string
		loaded bool
		fail   bool
		want   bool
	}{
		{name: "declared-loaded-passing", tools: []string{"dispatch_child"}, loaded: true, want: true},
		{name: "bare"},
		{name: "undeclared-loaded", loaded: true},
		{name: "declared-not-loaded", tools: []string{"dispatch_child"}},
		{name: "different-tool", tools: []string{"other_child"}, loaded: true},
		{name: "invalid-delivery", tools: []string{"dispatch_child", "dispatch_child"}, loaded: true},
		{name: "declared-failed", tools: []string{"dispatch_child"}, loaded: true, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			source := piSubagentSource(t, tc.tools, tc.loaded, tc.fail)
			sources := append(NativeSubagentFixtures(), source)
			built, err := BuildWithSubagentFixtures(t.Context(), nil, sources)
			if err != nil {
				t.Fatal(err)
			}
			assertSubagentArtifacts(t, built, source.Cell, tc.want)
			for _, h := range built.Harnesses {
				if h.Name == agent.HarnessPi && h.Caps.EmitsSubagentEvents {
					t.Fatal("delivery-specific fixture promoted the bare pi harness")
				}
			}
			for _, cell := range built.Matrix.Cells {
				if cell.Harness == agent.HarnessPi && cell.Key() != source.Cell && cell.EmitsSubagentEvents {
					t.Fatal("pi delivery evidence escaped its cell")
				}
			}
			for _, row := range built.Matrix.SubagentQualification {
				if row.Cell == source.Cell && tc.loaded && row.DeliveryInputDigest == "" {
					t.Fatal("loaded delivery was not bound to the fixture result")
				}
			}
		})
	}
}

func piSubagentSource(t *testing.T, tools []string, loaded, fail bool) SubagentFixtureSource {
	t.Helper()
	var key CellKey
	for _, cell := range validCells {
		if cell.Harness == agent.HarnessPi {
			key = cell.Key()
			break
		}
	}
	if key.Harness == "" {
		t.Fatal("no pi cell")
	}
	delivery := agent.ExtensionDelivery{
		ID: "delegation-fixture", Kind: agent.ExtensionDeliveryInline,
		Basename: "delegation.ts", Source: []byte("export default function activate(pi) {}\n"), DelegationTools: tools,
	}
	delivery.Digest = delivery.ComputeDigest(delivery.Source)
	return SubagentFixtureSource{
		Cell: key, AdapterVersion: "pi/headless/tool-lifecycle-v2", Mode: agent.PromptModeAutonomous,
		Calls: []SubagentFixtureCall{{ToolUseID: "call-1", ToolName: "dispatch_child", ChildSessionID: "child-1", ChildSessionTransport: "host_cli", TerminalPhase: agent.SubagentCompleted}},
		Execute: func(context.Context) (SubagentFixtureExecution, error) {
			var deliveries []agent.ExtensionDelivery
			if loaded {
				deliveries = []agent.ExtensionDelivery{delivery}
			}
			events, err := pi.SubagentFixtureEvents(deliveries)
			if fail {
				// A child witnessed independently by the fixture disappears from
				// adapter output. Normal tool events alone must not qualify.
				var filtered []agent.Event
				for _, event := range events {
					if _, ok := event.(agent.SubagentEvent); !ok {
						filtered = append(filtered, event)
					}
				}
				events = filtered
			}
			return SubagentFixtureExecution{Events: events, LoadedDeliveries: deliveries}, err
		},
	}
}

func TestSubagentPiRequiresLoadedDeclarationsIndependentlyOfEvents(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*SubagentFixtureExecution)
	}{
		{name: "not-loaded", mutate: func(e *SubagentFixtureExecution) { e.LoadedDeliveries = nil }},
		{name: "not-declared", mutate: func(e *SubagentFixtureExecution) { e.LoadedDeliveries[0].DelegationTools = nil }},
		{name: "wrong-declaration", mutate: func(e *SubagentFixtureExecution) { e.LoadedDeliveries[0].DelegationTools = []string{"other_child"} }},
		{name: "invalid-delivery", mutate: func(e *SubagentFixtureExecution) { e.LoadedDeliveries[0].Digest = "invalid" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			source := piSubagentSource(t, []string{"dispatch_child"}, true, false)
			execute := source.Execute
			source.Execute = func(ctx context.Context) (SubagentFixtureExecution, error) {
				execution, err := execute(ctx)
				tc.mutate(&execution)
				return execution, err
			}
			built, err := BuildWithSubagentFixtures(t.Context(), nil, []SubagentFixtureSource{source})
			if err != nil {
				t.Fatal(err)
			}
			assertSubagentArtifacts(t, built, source.Cell, false)
		})
	}
}

func TestSubagentPiDeliveryDigestBindsDeclarations(t *testing.T) {
	t.Parallel()
	var digests []string
	for _, tools := range [][]string{{"dispatch_child"}, {"dispatch_child", "other_child"}} {
		source := piSubagentSource(t, tools, true, false)
		built, err := BuildWithSubagentFixtures(t.Context(), nil, []SubagentFixtureSource{source})
		if err != nil {
			t.Fatal(err)
		}
		assertSubagentArtifacts(t, built, source.Cell, true)
		digests = append(digests, built.Matrix.SubagentQualification[0].DeliveryInputDigest)
	}
	if digests[0] == digests[1] {
		t.Fatal("changing loaded delegation declarations did not change qualification input digest")
	}
}

func assertSubagentArtifacts(t *testing.T, built *Built, key CellKey, want bool) {
	t.Helper()
	artifacts, err := built.Render()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{FileCapabilityMatrix, FileMatrix} {
		var doc struct {
			Cells []HarnessEndpointCell `json:"cells"`
		}
		if err := json.Unmarshal(artifacts.Files[name], &doc); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, cell := range doc.Cells {
			if cell.Key() == key {
				found = true
				if cell.EmitsSubagentEvents != want {
					t.Fatalf("%s cell %s subagents = %v, want %v from executed fixture", name, key.Harness, cell.EmitsSubagentEvents, want)
				}
			}
		}
		if !found {
			t.Fatalf("%s did not render fixture cell", name)
		}
	}
	for _, row := range built.Matrix.SubagentQualification {
		if row.Cell == key && row.EmitsSubagentEvents != want {
			t.Fatalf("qualification subagents = %v, want %v", row.EmitsSubagentEvents, want)
		}
	}
}

func TestSubagentFixtureInputsRejected(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*SubagentFixtureSource)
	}{
		{name: "unknown-cell", mutate: func(s *SubagentFixtureSource) { s.Cell.Host = "unknown" }},
		{name: "new-version", mutate: func(s *SubagentFixtureSource) { s.AdapterVersion += "-new" }},
		{name: "wrong-mode", mutate: func(s *SubagentFixtureSource) { s.Mode = agent.PromptModeHumanControlled }},
		{name: "no-executor", mutate: func(s *SubagentFixtureSource) { s.Execute = nil }},
		{name: "no-witness", mutate: func(s *SubagentFixtureSource) { s.Calls = nil }},
		{name: "empty-id", mutate: func(s *SubagentFixtureSource) { s.Calls[0].ToolUseID = "" }},
		{name: "empty-tool", mutate: func(s *SubagentFixtureSource) { s.Calls[0].ToolName = "" }},
		{name: "invalid-phase", mutate: func(s *SubagentFixtureSource) { s.Calls[0].TerminalPhase = agent.SubagentStarted }},
		{name: "duplicate-call", mutate: func(s *SubagentFixtureSource) { s.Calls[1] = s.Calls[0] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			source := NativeSubagentFixtures()[0]
			tc.mutate(&source)
			if _, err := BuildWithSubagentFixtures(t.Context(), nil, []SubagentFixtureSource{source}); err == nil {
				t.Fatal("invalid fixture identity accepted")
			}
		})
	}
}

func TestSubagentFixtureDuplicateAndCancellation(t *testing.T) {
	t.Parallel()
	source := NativeSubagentFixtures()[0]
	if _, err := BuildWithSubagentFixtures(t.Context(), nil, []SubagentFixtureSource{source, source}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := BuildWithSubagentFixtures(ctx, nil, []SubagentFixtureSource{source}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	execute := source.Execute
	source.Execute = func(context.Context) (SubagentFixtureExecution, error) {
		execution, err := execute(t.Context())
		cancel()
		return execution, err
	}
	if _, err := BuildWithSubagentFixtures(ctx, nil, []SubagentFixtureSource{source}); !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight cancel error = %v", err)
	}
}

func TestSubagentAuthoredCellBitsRejected(t *testing.T) {
	t.Parallel()
	_, harnesses, err := buildHarnesses()
	if err != nil {
		t.Fatal(err)
	}
	_, endpoints := buildEndpoints()
	for _, tc := range []struct {
		name   string
		mutate func(*HarnessEndpointCell)
	}{
		{name: "direct", mutate: func(c *HarnessEndpointCell) { c.EmitsSubagentEvents = true }},
		{name: "override-true", mutate: func(c *HarnessEndpointCell) { c.Caps = &CapsOverride{EmitsSubagentEvents: boolp(true)} }},
		{name: "override-false", mutate: func(c *HarnessEndpointCell) { c.Caps = &CapsOverride{EmitsSubagentEvents: boolp(false)} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cell := validCells[0]
			tc.mutate(&cell)
			if _, err := buildCells([]HarnessEndpointCell{cell}, harnesses, endpoints); err == nil || !strings.Contains(err.Error(), "subagent events are derived") {
				t.Fatalf("authored cell error = %v", err)
			}
		})
	}
}

func TestSubagentMissingFixturesIgnoreManifestClaims(t *testing.T) {
	t.Parallel()
	built, err := BuildWithSubagentFixtures(t.Context(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, harness := range built.Harnesses {
		if harness.Caps.EmitsSubagentEvents {
			t.Fatalf("%s inherited a manifest subagent claim without a fixture", harness.Name)
		}
	}
	for _, cell := range built.Matrix.Cells {
		if cell.EmitsSubagentEvents {
			t.Fatalf("%s inherited a subagent claim without a fixture", cell.Harness)
		}
	}
	if len(built.Matrix.SubagentQualification) != 0 {
		t.Fatal("missing fixtures produced qualification evidence")
	}
	if _, err := built.Render(); err != nil {
		t.Fatal(err)
	}
}

func TestSubagentRenderRejectsAuthoredEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(*Built)
	}{
		{name: "qualification", mutate: func(b *Built) { b.Matrix.SubagentQualification[0].EmitsSubagentEvents = false }},
		{name: "harness", mutate: func(b *Built) { b.Harnesses[0].Caps.EmitsSubagentEvents = !b.Harnesses[0].Caps.EmitsSubagentEvents }},
		{name: "matrix-harness", mutate: func(b *Built) {
			b.Matrix.Harnesses = append([]HarnessRow(nil), b.Matrix.Harnesses...)
			b.Matrix.Harnesses[0].Caps.EmitsSubagentEvents = !b.Matrix.Harnesses[0].Caps.EmitsSubagentEvents
		}},
		{name: "cell", mutate: func(b *Built) { b.Matrix.Cells[0].EmitsSubagentEvents = !b.Matrix.Cells[0].EmitsSubagentEvents }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			built, err := Build()
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(built)
			if _, err := built.Render(); err == nil || !strings.Contains(err.Error(), "subagent") {
				t.Fatalf("Render error = %v, want authored subagent evidence rejection", err)
			}
		})
	}
}
