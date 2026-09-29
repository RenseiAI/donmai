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
//   - index 0 renders everywhere, reports network's deny baseline as
//     unavailable, and reports the tool deny baseline per harness AND mode,
//     derived from the mode's tool/lifecycle profile;
//   - every level above index 0 is refused with
//     execution_security_unrenderable naming its dimension, except the
//     renderings listed in wantAbove (none today).
//
// A harness or mode that starts declaring a level, or whose deny channel
// changes, fails here until this table — the reviewed statement of what each
// exact adapter enforces — says so too.
func TestExecutionSecurityRenderMatrix(t *testing.T) {
	t.Parallel()
	const (
		auto  = agent.PromptModeAutonomous
		human = agent.PromptModeHumanControlled
	)
	best, none := agent.DenyBaselineBestEffort, agent.DenyBaselineUnavailable
	wantDenyBaseline := map[agent.HarnessName]map[agent.PromptSessionMode]agent.DenyBaselineStatus{
		agent.HarnessClaudeCode:   {auto: best, human: none},
		agent.HarnessCodex:        {auto: best, human: none},
		agent.HarnessGeminiDirect: {auto: best},
		agent.HarnessOpenCode:     {auto: best},
		agent.HarnessPi:           {auto: best, human: best},
		agent.HarnessAntigravity:  {auto: none},
		agent.HarnessOllama:       {auto: none},
		agent.HarnessShell:        {human: none},
		agent.HarnessStub:         {auto: none, human: none},
	}
	wantAbove := map[agent.HarnessName][]renderedAbove{}

	for _, harvest := range HarnessHarvestList() {
		manifest := harvest.Manifest()
		wantModes, known := wantDenyBaseline[harvest.Name]
		if !known {
			t.Errorf("harness %q has no row in the execution-security render matrix", harvest.Name)
			continue
		}
		modes := map[agent.PromptSessionMode]bool{}
		for _, profile := range manifest.PromptDelivery {
			modes[profile.Mode] = true
		}
		for mode := range modes {
			want, ok := wantModes[mode]
			if !ok {
				t.Errorf("%s/%s has no deny-baseline row in the render matrix", harvest.Name, mode)
				continue
			}
			base := agent.Spec{PromptMode: mode, SandboxLevel: agent.SandboxWorkspaceWrite}
			base.ExecutionSecurity = &agent.ExecutionSecurity{Version: 1, Levels: agent.IndexZeroExecutionSecurityLevels()}
			report, err := agent.RenderExecutionSecurity(base, manifest)
			if err != nil {
				t.Errorf("%s/%s: index 0 refused: %v", harvest.Name, mode, err)
				continue
			}
			if report.ToolApproval.DenyBaseline != want || report.Network.DenyBaseline != none {
				t.Errorf("%s/%s: deny baseline tool=%q network=%q, want tool=%q network=%q",
					harvest.Name, mode, report.ToolApproval.DenyBaseline, report.Network.DenyBaseline, want, none)
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

// TestCodexDenyBaselineUnderFullAccess: codex's approval bridge only sees
// sandbox escalations, so under the full-access grant — what an index-0
// stamp renders headless — the tool deny entries have nothing to act on and
// the report says so.
func TestCodexDenyBaselineUnderFullAccess(t *testing.T) {
	t.Parallel()
	for _, harvest := range HarnessHarvestList() {
		if harvest.Name != agent.HarnessCodex {
			continue
		}
		spec := agent.Spec{PromptMode: agent.PromptModeAutonomous, SandboxLevel: agent.SandboxFullAccess}
		spec.ExecutionSecurity = &agent.ExecutionSecurity{Version: 1, Levels: agent.IndexZeroExecutionSecurityLevels()}
		report, err := agent.RenderExecutionSecurity(spec, harvest.Manifest())
		if err != nil {
			t.Fatal(err)
		}
		if report.ToolApproval.DenyBaseline != agent.DenyBaselineUnavailable {
			t.Fatalf("codex full-access deny baseline = %q, want unavailable", report.ToolApproval.DenyBaseline)
		}
		return
	}
	t.Fatal("codex is not in the harvest list")
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
