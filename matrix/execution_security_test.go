package matrix

import (
	"errors"
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// renderedAbove is one declared rendering above index 0 in the expected
// matrix: dimension, level, the modes it covers, and its enforcing layers.
type renderedAbove struct {
	dimension agent.ExecutionSecurityDimension
	level     agent.ExecutionSecurityLevel
	mode      agent.PromptSessionMode
	layers    []agent.EnforcingLayer
}

// TestExecutionSecurityRenderMatrix is the per-harness render matrix
// (ADR-2026-09-27-execution-security-levels.md; checklist row 9), asserted
// against every harvested manifest and every session mode it admits:
//
//   - index 0 renders everywhere and reports the harness's honest
//     deny-baseline status;
//   - every level above index 0 is refused with
//     execution_security_unrenderable naming its dimension, except the
//     renderings listed here, which achieve the level with their layers.
//
// A harness that starts declaring a level fails here until this table — the
// reviewed statement of what each exact adapter enforces — says so too.
func TestExecutionSecurityRenderMatrix(t *testing.T) {
	t.Parallel()
	wantDenyBaseline := map[agent.HarnessName]agent.DenyBaselineStatus{
		agent.HarnessClaudeCode:   agent.DenyBaselineBestEffort,
		agent.HarnessCodex:        agent.DenyBaselineBestEffort,
		agent.HarnessGeminiDirect: agent.DenyBaselineBestEffort,
		agent.HarnessOpenCode:     agent.DenyBaselineBestEffort,
		agent.HarnessPi:           agent.DenyBaselineBestEffort,
		agent.HarnessAntigravity:  agent.DenyBaselineUnavailable,
		agent.HarnessOllama:       agent.DenyBaselineUnavailable,
		agent.HarnessAmp:          agent.DenyBaselineUnavailable,
		agent.HarnessShell:        agent.DenyBaselineUnavailable,
		agent.HarnessStub:         agent.DenyBaselineUnavailable,
	}
	wantAbove := map[agent.HarnessName][]renderedAbove{
		agent.HarnessCodex: {{
			dimension: agent.ExecutionSecurityFileWrite, level: agent.FileWriteWorkarea,
			mode: agent.PromptModeAutonomous, layers: []agent.EnforcingLayer{agent.LayerHarnessNative},
		}},
	}

	for _, harvest := range HarnessHarvestList() {
		manifest := harvest.Manifest()
		want, known := wantDenyBaseline[harvest.Name]
		if !known {
			t.Errorf("harness %q has no row in the execution-security render matrix", harvest.Name)
			continue
		}
		modes := map[agent.PromptSessionMode]bool{}
		for _, profile := range manifest.PromptDelivery {
			modes[profile.Mode] = true
		}
		for mode := range modes {
			base := agent.Spec{PromptMode: mode}
			report, err := agent.RenderExecutionSecurity(base, manifest)
			if err != nil {
				t.Errorf("%s/%s: index 0 refused: %v", harvest.Name, mode, err)
				continue
			}
			if report.ToolApproval.DenyBaseline != want {
				t.Errorf("%s/%s: deny baseline = %q, want %q", harvest.Name, mode, report.ToolApproval.DenyBaseline, want)
			}
			for _, dimension := range agent.ExecutionSecurityDimensions() {
				for _, level := range agent.ExecutionSecurityLadder(dimension)[1:] {
					spec := base
					spec.ExecutionSecurity = &agent.ExecutionSecurity{Version: 1, Levels: agent.IndexZeroExecutionSecurityLevels()}
					setLevel(&spec.ExecutionSecurity.Levels, dimension, level)
					report, err := agent.RenderExecutionSecurity(spec, manifest)
					declared := findRendered(wantAbove[harvest.Name], dimension, level, mode)
					if declared != nil {
						got := report.Dimension(dimension)
						if err != nil || got.AchievedLevel != level || !reflect.DeepEqual(got.EnforcingLayers, declared.layers) {
							t.Errorf("%s/%s %s=%s: report=%+v err=%v, want achieved with %v", harvest.Name, mode, dimension, level, got, err, declared.layers)
						}
						continue
					}
					var typed *agent.ExecutionSecurityError
					if !errors.As(err, &typed) || typed.Code != agent.ExecutionSecurityUnrenderable || typed.Dimension != dimension {
						t.Errorf("%s/%s %s=%s: err = %v, want execution_security_unrenderable naming the dimension", harvest.Name, mode, dimension, level, err)
					}
				}
			}
		}
	}
}

func findRendered(entries []renderedAbove, dimension agent.ExecutionSecurityDimension, level agent.ExecutionSecurityLevel, mode agent.PromptSessionMode) *renderedAbove {
	for i := range entries {
		if entries[i].dimension == dimension && entries[i].level == level && entries[i].mode == mode {
			return &entries[i]
		}
	}
	return nil
}

func setLevel(levels *agent.ExecutionSecurityLevels, dimension agent.ExecutionSecurityDimension, level agent.ExecutionSecurityLevel) {
	switch dimension {
	case agent.ExecutionSecurityToolApproval:
		levels.ToolApproval = level
	case agent.ExecutionSecurityFileRead:
		levels.FileRead = level
	case agent.ExecutionSecurityFileWrite:
		levels.FileWrite = level
	case agent.ExecutionSecurityNetwork:
		levels.Network = level
	case agent.ExecutionSecurityCredentials:
		levels.Credentials = level
	case agent.ExecutionSecurityIsolation:
		levels.Isolation = level
	}
}
