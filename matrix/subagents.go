package matrix

import (
	"context"
	"fmt"
	"reflect"
	"sort"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/clijsonl"
	"github.com/RenseiAI/donmai/runtime/span"
)

// SubagentFixtureCall is an independently witnessed delegation call. Expected
// child identities come from structured fixture data, never adapter output.
type SubagentFixtureCall struct {
	ToolUseID             string
	ToolName              string
	ChildSessionID        string
	ChildSessionTransport string
	TerminalPhase         agent.SubagentPhase
}

// SubagentFixtureExecution contains adapter output and the deliveries the
// fixture actually loaded. There is no caller-supplied pass bit. Downstream
// producers own loading/admission evidence, as with capability realizations.
type SubagentFixtureExecution struct {
	Events           []agent.Event
	LoadedDeliveries []agent.ExtensionDelivery
}

// SubagentFixtureSource binds an executable fixture to one exact cell and
// tool/lifecycle profile. A profile bump needs a newly registered fixture.
type SubagentFixtureSource struct {
	Cell           CellKey
	AdapterVersion string
	Mode           agent.PromptSessionMode
	Calls          []SubagentFixtureCall
	Execute        func(context.Context) (SubagentFixtureExecution, error)
}

// SubagentQualificationRow publishes the computed result, including the pi
// delivery input digest. A missing or failing fixture never promotes a cell.
type SubagentQualificationRow struct {
	Cell                CellKey                 `json:"cell"`
	AdapterVersion      string                  `json:"adapterVersion"`
	Mode                agent.PromptSessionMode `json:"mode"`
	DeliveryInputDigest string                  `json:"deliveryInputDigest,omitempty"`
	EmitsSubagentEvents bool                    `json:"emitsSubagentEvents"`
}

// NativeSubagentFixtures registers the shipped stream-json native-delegation
// fixture for its exact adapter profile. No bare pi cell loads a delivery.
func NativeSubagentFixtures() []SubagentFixtureSource {
	var sources []SubagentFixtureSource
	for _, cell := range validCells {
		if cell.Harness != agent.HarnessClaudeCode {
			continue
		}
		sources = append(sources, SubagentFixtureSource{
			Cell: cell.Key(), AdapterVersion: "claude-code/headless/tool-lifecycle-v1", Mode: agent.PromptModeAutonomous,
			Calls: []SubagentFixtureCall{
				{ToolUseID: "toolu_agent_1", ToolName: "Agent", TerminalPhase: agent.SubagentCompleted},
				{ToolUseID: "toolu_agent_2", ToolName: "Agent", TerminalPhase: agent.SubagentFailed},
			},
			Execute: func(ctx context.Context) (SubagentFixtureExecution, error) {
				if err := ctx.Err(); err != nil {
					return SubagentFixtureExecution{}, err
				}
				return SubagentFixtureExecution{Events: clijsonl.SubagentFixtureEvents()}, nil
			},
		})
	}
	return sources
}

func compileSubagentFixtures(ctx context.Context, sources []SubagentFixtureSource, cells []HarnessEndpointCell, harnesses map[agent.HarnessName]HarnessRow) ([]SubagentQualificationRow, error) {
	rows := make([]SubagentQualificationRow, 0, len(sources))
	seen := map[string]bool{}
	for _, source := range sources {
		key := fmt.Sprintf("%s/%s/%s/%s/%s", source.Cell.Harness, source.Cell.Endpoint, source.Cell.Host, source.AdapterVersion, source.Mode)
		if seen[key] {
			return nil, fmt.Errorf("duplicate subagent fixture tuple %s", key)
		}
		seen[key] = true
		knownCell := false
		for _, cell := range cells {
			knownCell = knownCell || cell.Key() == source.Cell
		}
		knownProfile := false
		for _, profile := range harnesses[source.Cell.Harness].ToolLifecycle {
			knownProfile = knownProfile || (profile.ID == source.AdapterVersion && profile.Mode == source.Mode)
		}
		if !knownCell || !knownProfile || source.Execute == nil || len(source.Calls) == 0 {
			return nil, fmt.Errorf("subagent fixture %s requires a known cell/profile, executor and witnessed calls", key)
		}
		callIDs := map[string]bool{}
		for _, call := range source.Calls {
			if call.ToolUseID == "" || call.ToolName == "" || callIDs[call.ToolUseID] ||
				(call.TerminalPhase != agent.SubagentCompleted && call.TerminalPhase != agent.SubagentFailed) {
				return nil, fmt.Errorf("subagent fixture %s has an invalid witnessed call", key)
			}
			callIDs[call.ToolUseID] = true
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("execute subagent fixture: %w", err)
		}
		execution, err := source.Execute(ctx)
		if ctx.Err() != nil {
			return nil, fmt.Errorf("execute subagent fixture: %w", ctx.Err())
		}
		row := SubagentQualificationRow{Cell: source.Cell, AdapterVersion: source.AdapterVersion, Mode: source.Mode}
		if len(execution.LoadedDeliveries) > 0 {
			row.DeliveryInputDigest = agent.CapabilityExtensionInputDigest(execution.LoadedDeliveries)
		}
		row.EmitsSubagentEvents = err == nil && subagentFixtureConforms(source, execution)
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Cell != rows[j].Cell {
			return cellKeyLess(rows[i].Cell, rows[j].Cell)
		}
		if rows[i].AdapterVersion != rows[j].AdapterVersion {
			return rows[i].AdapterVersion < rows[j].AdapterVersion
		}
		return rows[i].Mode < rows[j].Mode
	})
	return rows, nil
}

func subagentFixtureConforms(source SubagentFixtureSource, execution SubagentFixtureExecution) bool {
	if source.Cell.Harness == agent.HarnessPi {
		if agent.ValidateExtensionDeliveries(execution.LoadedDeliveries) != nil {
			return false
		}
		for _, call := range source.Calls {
			declared := false
			for _, delivery := range execution.LoadedDeliveries {
				for _, tool := range delivery.DelegationTools {
					declared = declared || tool == call.ToolName
				}
			}
			if !declared || call.ChildSessionID == "" || call.ChildSessionTransport == "" {
				return false
			}
		}
	}
	var spans []agent.SubagentSpan
	processor, err := span.NewProcessor(span.ProcessorConfig{
		SessionID: "fixture-parent", OrgID: "fixture-org", WorkspaceID: "fixture-workspace",
		Sender: span.SendFunc(func(s agent.Span) bool {
			if sub, ok := s.(agent.SubagentSpan); ok {
				spans = append(spans, sub)
			}
			return true
		}),
	})
	if err != nil {
		return false
	}
	var subs []agent.SubagentEvent
	for _, event := range execution.Events {
		for _, mapped := range processor.Process(event) {
			if sub, ok := mapped.(agent.SubagentEvent); ok {
				subs = append(subs, sub)
			}
		}
	}
	if len(subs) != 2*len(source.Calls) || len(spans) != len(source.Calls) {
		return false
	}
	for _, call := range source.Calls {
		var lifecycle []agent.SubagentEvent
		for _, sub := range subs {
			if sub.ToolUseID == call.ToolUseID {
				lifecycle = append(lifecycle, sub)
			}
		}
		if len(lifecycle) != 2 || lifecycle[0].Phase != agent.SubagentStarted || lifecycle[1].Phase != call.TerminalPhase {
			return false
		}
		for _, sub := range lifecycle {
			if sub.ToolName != call.ToolName || sub.ChildSessionID != call.ChildSessionID || sub.ChildSessionTransport != call.ChildSessionTransport {
				return false
			}
		}
		matchingSpan := false
		for _, s := range spans {
			status := agent.StatusOK
			if call.TerminalPhase == agent.SubagentFailed {
				status = agent.StatusError
			}
			matchingSpan = matchingSpan || (s.Kind == agent.SpanKindSubagent && s.SpanID == lifecycle[0].SpanID &&
				s.SpanID == lifecycle[1].SpanID && s.Status.Code == status)
		}
		if !matchingSpan {
			return false
		}
	}
	return true
}

func applySubagentQualification(harnesses []HarnessRow, cells []HarnessEndpointCell, rows []SubagentQualificationRow) {
	for i := range harnesses {
		harnesses[i].Caps.EmitsSubagentEvents = false
		for _, row := range rows {
			// Delivery-specific pi evidence cannot promote the bare harness.
			if row.Cell.Harness == harnesses[i].Name && row.Cell.Harness != agent.HarnessPi && row.EmitsSubagentEvents {
				harnesses[i].Caps.EmitsSubagentEvents = true
			}
		}
	}
	for i := range cells {
		cells[i].EmitsSubagentEvents = false
		for _, row := range rows {
			if row.Cell == cells[i].Key() && row.EmitsSubagentEvents {
				cells[i].EmitsSubagentEvents = true
			}
		}
	}
}

func (b *Built) validateSubagentQualification() error {
	if !reflect.DeepEqual(b.Matrix.SubagentQualification, b.canonicalSubagentQualification) {
		return fmt.Errorf("subagent qualification does not match executed fixtures")
	}
	harnesses := append([]HarnessRow(nil), b.Harnesses...)
	cells := append([]HarnessEndpointCell(nil), b.Matrix.Cells...)
	applySubagentQualification(harnesses, cells, b.canonicalSubagentQualification)
	if !reflect.DeepEqual(harnesses, b.Harnesses) || !reflect.DeepEqual(cells, b.Matrix.Cells) || !reflect.DeepEqual(b.Matrix.Harnesses, b.Harnesses) {
		return fmt.Errorf("subagent capability projection does not match executed fixtures")
	}
	return nil
}
