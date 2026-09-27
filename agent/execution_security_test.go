package agent_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/codex"
)

const indexZeroLevelsJSON = `{"toolApproval":"bypass","fileRead":"host","fileWrite":"host","network":"open","credentials":"ambient-host-login","isolation":"host-user"}`

func levelsJSON(override map[string]string) string {
	var levels map[string]string
	_ = json.Unmarshal([]byte(indexZeroLevelsJSON), &levels)
	for key, value := range override {
		if value == "<delete>" {
			delete(levels, key)
			continue
		}
		levels[key] = value
	}
	raw, _ := json.Marshal(levels)
	return string(raw)
}

func stampedLevels(override map[agent.ExecutionSecurityDimension]agent.ExecutionSecurityLevel) *agent.ExecutionSecurity {
	section := &agent.ExecutionSecurity{Version: 1, Levels: agent.IndexZeroExecutionSecurityLevels()}
	for dimension, level := range override {
		switch dimension {
		case agent.ExecutionSecurityToolApproval:
			section.Levels.ToolApproval = level
		case agent.ExecutionSecurityFileRead:
			section.Levels.FileRead = level
		case agent.ExecutionSecurityFileWrite:
			section.Levels.FileWrite = level
		case agent.ExecutionSecurityNetwork:
			section.Levels.Network = level
		case agent.ExecutionSecurityCredentials:
			section.Levels.Credentials = level
		case agent.ExecutionSecurityIsolation:
			section.Levels.Isolation = level
		}
	}
	return section
}

// TestParseExecutionSecurity covers the closed queued-work decoder: absence is
// the only input read as index 0, and every malformed, unknown or missing
// member is refused as execution_security_unresolvable — never guessed.
func TestParseExecutionSecurity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		raw           string
		wantNil       bool
		wantLevels    *agent.ExecutionSecurityLevels
		wantSources   map[agent.ExecutionSecurityDimension]string
		wantDimension agent.ExecutionSecurityDimension
		wantErr       bool
	}{
		{name: "absent member", raw: "", wantNil: true},
		{name: "json null", raw: "null", wantNil: true},
		{
			name: "every dimension at index 0", raw: `{"version":1,"levels":` + indexZeroLevelsJSON + `}`,
			wantLevels: func() *agent.ExecutionSecurityLevels { l := agent.IndexZeroExecutionSecurityLevels(); return &l }(),
		},
		{
			name: "strong levels with sources",
			raw: `{"version":1,"levels":` + levelsJSON(map[string]string{"toolApproval": "allow-list", "fileWrite": "workarea", "network": "none", "isolation": "microvm"}) +
				`,"sources":{"fileWrite":"scope:project","network":"scope:org"}}`,
			wantLevels: &agent.ExecutionSecurityLevels{
				ToolApproval: "allow-list", FileRead: "host", FileWrite: "workarea",
				Network: "none", Credentials: "ambient-host-login", Isolation: "microvm",
			},
			wantSources: map[agent.ExecutionSecurityDimension]string{"fileWrite": "scope:project", "network": "scope:org"},
		},
		{name: "unsupported version", raw: `{"version":2,"levels":` + indexZeroLevelsJSON + `}`, wantErr: true},
		{name: "missing version", raw: `{"levels":` + indexZeroLevelsJSON + `}`, wantErr: true},
		{name: "string version", raw: `{"version":"1","levels":` + indexZeroLevelsJSON + `}`, wantErr: true},
		{name: "missing levels", raw: `{"version":1}`, wantErr: true},
		{name: "missing dimension", raw: `{"version":1,"levels":` + levelsJSON(map[string]string{"isolation": "<delete>"}) + `}`, wantErr: true, wantDimension: agent.ExecutionSecurityIsolation},
		{name: "unknown dimension", raw: `{"version":1,"levels":` + levelsJSON(map[string]string{"gpu": "none"}) + `}`, wantErr: true, wantDimension: "gpu"},
		{name: "unknown level", raw: `{"version":1,"levels":` + levelsJSON(map[string]string{"fileRead": "sandboxed"}) + `}`, wantErr: true, wantDimension: agent.ExecutionSecurityFileRead},
		{name: "level from another ladder", raw: `{"version":1,"levels":` + levelsJSON(map[string]string{"network": "workarea"}) + `}`, wantErr: true, wantDimension: agent.ExecutionSecurityNetwork},
		{name: "null level", raw: `{"version":1,"levels":{"toolApproval":null,"fileRead":"host","fileWrite":"host","network":"open","credentials":"ambient-host-login","isolation":"host-user"}}`, wantErr: true, wantDimension: agent.ExecutionSecurityToolApproval},
		{name: "non-string level", raw: `{"version":1,"levels":` + strings.Replace(indexZeroLevelsJSON, `"open"`, `3`, 1) + `}`, wantErr: true},
		{name: "unknown member", raw: `{"version":1,"levels":` + indexZeroLevelsJSON + `,"override":true}`, wantErr: true},
		{name: "unknown source dimension", raw: `{"version":1,"levels":` + indexZeroLevelsJSON + `,"sources":{"gpu":"scope:org"}}`, wantErr: true, wantDimension: "gpu"},
		{name: "not an object", raw: `["bypass"]`, wantErr: true},
		{name: "trailing data", raw: `{"version":1,"levels":` + indexZeroLevelsJSON + `} {}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := agent.ParseExecutionSecurity([]byte(tc.raw))
			if tc.wantErr {
				var typed *agent.ExecutionSecurityError
				if !errors.As(err, &typed) || typed.Code != agent.ExecutionSecurityUnresolvable {
					t.Fatalf("err = %v, want execution_security_unresolvable", err)
				}
				if tc.wantDimension != "" && typed.Dimension != tc.wantDimension {
					t.Fatalf("dimension = %q, want %q", typed.Dimension, tc.wantDimension)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantNil {
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
				return
			}
			if got == nil || got.Version != 1 || got.Levels != *tc.wantLevels {
				t.Fatalf("got %+v, want levels %+v", got, tc.wantLevels)
			}
			if !reflect.DeepEqual(got.Sources, tc.wantSources) {
				t.Fatalf("sources = %v, want %v", got.Sources, tc.wantSources)
			}
		})
	}
}

// TestExecutionSecurityUnmarshalIsTheClosedDecoder proves a struct field of
// the section type goes through the same closed decoder, so an ordinary
// json.Unmarshal of queued work refuses an unknown level.
func TestExecutionSecurityUnmarshalIsTheClosedDecoder(t *testing.T) {
	t.Parallel()
	var work struct {
		ExecutionSecurity *agent.ExecutionSecurity `json:"executionSecurity,omitempty"`
	}
	err := json.Unmarshal([]byte(`{"executionSecurity":{"version":1,"levels":`+levelsJSON(map[string]string{"credentials": "vaulted"})+`}}`), &work)
	if agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnresolvable {
		t.Fatalf("err = %v, want execution_security_unresolvable", err)
	}
	var absent struct {
		ExecutionSecurity *agent.ExecutionSecurity `json:"executionSecurity,omitempty"`
	}
	if err := json.Unmarshal([]byte(`{}`), &absent); err != nil || absent.ExecutionSecurity != nil {
		t.Fatalf("absent section: err=%v section=%+v", err, absent.ExecutionSecurity)
	}
	section, err := agent.ExecutionSecurityFromOperationalPayload([]byte(`{"sessionId":"s","executionSecurity":{"version":1,"levels":` + indexZeroLevelsJSON + `}}`))
	if err != nil || section == nil || section.Levels != agent.IndexZeroExecutionSecurityLevels() {
		t.Fatalf("operational payload section = %+v err=%v", section, err)
	}
	if _, err := agent.ExecutionSecurityFromOperationalPayload([]byte(`{"executionSecurity":{"version":1}}`)); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnresolvable {
		t.Fatalf("malformed operational payload section err = %v", err)
	}
}

// TestRenderExecutionSecurityIndexZeroReport pins the report every run gets
// at index 0: the stamped index-0 level achieved, no enforcing layer, and the
// harness's honest deny-baseline status on toolApproval.
func TestRenderExecutionSecurityIndexZeroReport(t *testing.T) {
	t.Parallel()
	for _, section := range []*agent.ExecutionSecurity{nil, stampedLevels(nil)} {
		for _, tc := range []struct {
			name      string
			rendering agent.ExecutionSecurityRendering
			want      agent.DenyBaselineStatus
		}{
			{"no deny channel", agent.ExecutionSecurityRendering{}, agent.DenyBaselineUnavailable},
			{"best-effort deny channel", agent.ExecutionSecurityRendering{DenyBaseline: agent.DenyBaselineBestEffort}, agent.DenyBaselineBestEffort},
		} {
			report, err := agent.RenderExecutionSecurity(agent.Spec{ExecutionSecurity: section}, agent.HarnessManifest{Name: "test", ExecutionSecurity: tc.rendering})
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			raw, _ := json.Marshal(report)
			want := `{"toolApproval":{"required":"bypass","achievedLevel":"bypass","enforcingLayers":[],"denyBaseline":"` + string(tc.want) + `"},` +
				`"fileRead":{"required":"host","achievedLevel":"host","enforcingLayers":[]},` +
				`"fileWrite":{"required":"host","achievedLevel":"host","enforcingLayers":[]},` +
				`"network":{"required":"open","achievedLevel":"open","enforcingLayers":[]},` +
				`"credentials":{"required":"ambient-host-login","achievedLevel":"ambient-host-login","enforcingLayers":[]},` +
				`"isolation":{"required":"host-user","achievedLevel":"host-user","enforcingLayers":[]}}`
			if string(raw) != want {
				t.Fatalf("%s: report =\n%s\nwant\n%s", tc.name, raw, want)
			}
			if err := agent.ValidateExecutionSecurityReport(report); err != nil {
				t.Fatalf("%s: index-0 report failed its own validation: %v", tc.name, err)
			}
		}
	}
}

// TestRenderExecutionSecurityRefusesUnrenderable raises each dimension above
// index 0 on a harness that declares no channel for it; each is refused
// before any side effect with execution_security_unrenderable naming exactly
// that dimension, and several at once are all named.
func TestRenderExecutionSecurityRefusesUnrenderable(t *testing.T) {
	t.Parallel()
	manifest := agent.HarnessManifest{Name: "test", ExecutionSecurity: agent.ExecutionSecurityRendering{DenyBaseline: agent.DenyBaselineBestEffort}}
	for _, dimension := range agent.ExecutionSecurityDimensions() {
		for _, level := range agent.ExecutionSecurityLadder(dimension)[1:] {
			spec := agent.Spec{ExecutionSecurity: stampedLevels(map[agent.ExecutionSecurityDimension]agent.ExecutionSecurityLevel{dimension: level})}
			_, err := agent.RenderExecutionSecurity(spec, manifest)
			var typed *agent.ExecutionSecurityError
			if !errors.As(err, &typed) || typed.Code != agent.ExecutionSecurityUnrenderable || typed.Dimension != dimension || typed.Level != level || typed.Harness != "test" {
				t.Errorf("%s=%s: err = %v, want unrenderable naming the dimension", dimension, level, err)
			}
			if _, prepErr := agent.PrepareHarness(spec, manifest); agent.ExecutionSecurityErrorCode(prepErr) != agent.ExecutionSecurityUnrenderable {
				t.Errorf("%s=%s: PrepareHarness err = %v, want the same refusal before spawn", dimension, level, prepErr)
			}
		}
	}
	both := agent.Spec{ExecutionSecurity: stampedLevels(map[agent.ExecutionSecurityDimension]agent.ExecutionSecurityLevel{
		agent.ExecutionSecurityNetwork: agent.NetworkNone, agent.ExecutionSecurityFileRead: agent.FileReadWorkarea,
	})}
	_, err := agent.RenderExecutionSecurity(both, manifest)
	var typed *agent.ExecutionSecurityError
	if !errors.As(err, &typed) || !reflect.DeepEqual(typed.Dimensions, []agent.ExecutionSecurityDimension{agent.ExecutionSecurityFileRead, agent.ExecutionSecurityNetwork}) {
		t.Fatalf("multi-dimension refusal = %v, want both dimensions in canonical order", err)
	}
}

// TestRenderExecutionSecurityDeclaredLevelIsModeScoped: a declared rendering
// achieves the level with its layer only in the modes it covers; the same
// stamp in another mode is refused, never rendered weaker.
func TestRenderExecutionSecurityDeclaredLevelIsModeScoped(t *testing.T) {
	t.Parallel()
	manifest := agent.HarnessManifest{Name: "test", ExecutionSecurity: agent.ExecutionSecurityRendering{
		Levels: []agent.RenderedExecutionSecurityLevel{{
			Dimension: agent.ExecutionSecurityFileWrite, Level: agent.FileWriteWorkarea,
			Modes: []agent.PromptSessionMode{agent.PromptModeAutonomous}, Layers: []agent.EnforcingLayer{agent.LayerHarnessNative},
		}},
	}}
	section := stampedLevels(map[agent.ExecutionSecurityDimension]agent.ExecutionSecurityLevel{agent.ExecutionSecurityFileWrite: agent.FileWriteWorkarea})
	report, err := agent.RenderExecutionSecurity(agent.Spec{ExecutionSecurity: section}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if got := report.FileWrite; got.AchievedLevel != agent.FileWriteWorkarea || !reflect.DeepEqual(got.EnforcingLayers, []agent.EnforcingLayer{agent.LayerHarnessNative}) {
		t.Fatalf("fileWrite report = %+v", got)
	}
	interactive := agent.Spec{ExecutionSecurity: section, PromptMode: agent.PromptModeHumanControlled}
	if _, err := agent.RenderExecutionSecurity(interactive, manifest); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnrenderable {
		t.Fatalf("interactive err = %v, want unrenderable", err)
	}
}

// TestExecutionSecurityReportMeets is the receipt check: a report meets a
// stamp only on its achieved levels, a missing report achieves exactly index
// 0, and a claim above index 0 without an enforcing layer — or a toolApproval
// claim above bypass without an enforced deny baseline — counts as index 0.
func TestExecutionSecurityReportMeets(t *testing.T) {
	t.Parallel()
	indexZero := agent.IndexZeroExecutionSecurityLevels()
	workarea := indexZero
	workarea.FileWrite = agent.FileWriteWorkarea
	denyList := indexZero
	denyList.ToolApproval = agent.ToolApprovalDenyList
	report := func(mutate func(*agent.ExecutionSecurityReport)) *agent.ExecutionSecurityReport {
		r, err := agent.RenderExecutionSecurity(agent.Spec{}, agent.HarnessManifest{})
		if err != nil {
			t.Fatal(err)
		}
		mutate(&r)
		return &r
	}
	tests := []struct {
		name      string
		report    *agent.ExecutionSecurityReport
		stamp     agent.ExecutionSecurityLevels
		wantCode  agent.ExecutionSecurityRefusalCode
		wantUnmet agent.ExecutionSecurityDimension
	}{
		{name: "absent report meets an index-0 stamp", report: nil, stamp: indexZero},
		{name: "absent report does not meet a stronger stamp", report: nil, stamp: workarea, wantCode: agent.ExecutionSecurityReceiptUnmet, wantUnmet: agent.ExecutionSecurityFileWrite},
		{name: "achieved workarea with a layer meets", report: report(func(r *agent.ExecutionSecurityReport) {
			r.FileWrite = agent.ExecutionSecurityDimensionReport{Required: "workarea", AchievedLevel: "workarea", EnforcingLayers: []agent.EnforcingLayer{agent.LayerHarnessNative}}
		}), stamp: workarea},
		{name: "achieved workarea without a layer is index 0", report: report(func(r *agent.ExecutionSecurityReport) {
			r.FileWrite = agent.ExecutionSecurityDimensionReport{Required: "workarea", AchievedLevel: "workarea", EnforcingLayers: []agent.EnforcingLayer{}}
		}), stamp: workarea, wantCode: agent.ExecutionSecurityReceiptUnmet, wantUnmet: agent.ExecutionSecurityFileWrite},
		{name: "required echo is never compared", report: report(func(r *agent.ExecutionSecurityReport) {
			r.FileWrite.Required = "workarea"
		}), stamp: workarea, wantCode: agent.ExecutionSecurityReceiptUnmet, wantUnmet: agent.ExecutionSecurityFileWrite},
		{name: "deny-list without an enforced baseline is bypass", report: report(func(r *agent.ExecutionSecurityReport) {
			r.ToolApproval = agent.ExecutionSecurityDimensionReport{Required: "deny-list", AchievedLevel: "deny-list", EnforcingLayers: []agent.EnforcingLayer{agent.LayerHarnessNative}, DenyBaseline: agent.DenyBaselineBestEffort}
		}), stamp: denyList, wantCode: agent.ExecutionSecurityReceiptUnmet, wantUnmet: agent.ExecutionSecurityToolApproval},
		{name: "deny-list with an enforced baseline meets", report: report(func(r *agent.ExecutionSecurityReport) {
			r.ToolApproval = agent.ExecutionSecurityDimensionReport{Required: "deny-list", AchievedLevel: "deny-list", EnforcingLayers: []agent.EnforcingLayer{agent.LayerHarnessNative}, DenyBaseline: agent.DenyBaselineEnforced}
		}), stamp: denyList},
		{name: "unknown achieved level meets nothing", report: report(func(r *agent.ExecutionSecurityReport) {
			r.Network.AchievedLevel = "sealed"
		}), stamp: indexZero, wantCode: agent.ExecutionSecurityReceiptUnmet, wantUnmet: agent.ExecutionSecurityNetwork},
		{name: "unknown stamp is unresolvable", report: nil, stamp: agent.ExecutionSecurityLevels{}, wantCode: agent.ExecutionSecurityUnresolvable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := agent.ExecutionSecurityReportMeets(tc.report, tc.stamp)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			var typed *agent.ExecutionSecurityError
			if !errors.As(err, &typed) || typed.Code != tc.wantCode {
				t.Fatalf("err = %v, want %s", err, tc.wantCode)
			}
			if tc.wantUnmet != "" && typed.Dimension != tc.wantUnmet {
				t.Fatalf("dimension = %q, want %q", typed.Dimension, tc.wantUnmet)
			}
		})
	}
}

// TestValidateExecutionSecurityReport pins the closed report shape the
// registration validator enforces.
func TestValidateExecutionSecurityReport(t *testing.T) {
	t.Parallel()
	valid, err := agent.RenderExecutionSecurity(agent.Spec{}, agent.HarnessManifest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*agent.ExecutionSecurityReport)
	}{
		{"null layers", func(r *agent.ExecutionSecurityReport) { r.Network.EnforcingLayers = nil }},
		{"unknown layer", func(r *agent.ExecutionSecurityReport) {
			r.Isolation.EnforcingLayers = []agent.EnforcingLayer{"good_intentions"}
		}},
		{"level above index 0 without a layer", func(r *agent.ExecutionSecurityReport) { r.FileRead.AchievedLevel = agent.FileReadWorkarea }},
		{"unknown required echo", func(r *agent.ExecutionSecurityReport) { r.Credentials.Required = "vaulted" }},
		{"deny baseline off toolApproval", func(r *agent.ExecutionSecurityReport) { r.FileWrite.DenyBaseline = agent.DenyBaselineEnforced }},
		{"toolApproval without a deny baseline", func(r *agent.ExecutionSecurityReport) { r.ToolApproval.DenyBaseline = "" }},
	} {
		report := valid
		report.ToolApproval.EnforcingLayers = []agent.EnforcingLayer{}
		tc.mutate(&report)
		if err := agent.ValidateExecutionSecurityReport(report); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityReceiptUnmet {
			t.Errorf("%s: err = %v, want execution_security_receipt_unmet", tc.name, err)
		}
	}
}

func preparedMaterializations(digest string) []agent.HarnessMaterialization {
	var out []agent.HarnessMaterialization
	for _, channel := range []string{"worktree", "environment", "credentials", "config", "endpoint_delivery", "services", "child_process", "runtime", "cleanup"} {
		out = append(out, agent.HarnessMaterialization{Channel: channel, SourceDigest: digest, Required: true})
	}
	return out
}

func codexAutonomousSource(section *agent.ExecutionSecurity) agent.Spec {
	return agent.Spec{
		PromptMode: agent.PromptModeAutonomous, Autonomous: true,
		SandboxEnabled: true, SandboxLevel: agent.SandboxWorkspaceWrite, Model: "gpt-test",
		PromptPlan: &agent.PromptPlan{
			ContractVersion:  agent.PromptContractVersion,
			BaseInstructions: agent.BaseInstructionPlan{Strategy: agent.BaseInstructionsPreserve},
			UserPrompt:       agent.PromptContent{ID: "actual-user-task", Text: "inspect the actual input", Required: true},
		},
		ExecutionSecurity: section,
	}
}

// TestCompilePreparedHarnessRecordsExecutionSecurityReport covers the applied
// receipt at index 0 and above: every compiled plan carries the report, the
// stamp is bound into the authority digest (absent keeps the pre-field
// digests), a stamp the harness cannot render denies the plan, and a child
// applying a plan whose report no longer meets the stamp is refused.
func TestCompilePreparedHarnessRecordsExecutionSecurityReport(t *testing.T) {
	t.Parallel()
	manifest := (&codex.Provider{}).Manifest()
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	legacy, err := agent.CompilePreparedHarness(codexAutonomousSource(nil), manifest, digest, nil, preparedMaterializations(digest))
	if err != nil {
		t.Fatalf("index-0 compile: %v", err)
	}
	if legacy.ExecutionSecurity == nil || legacy.ExecutionSecurity.FileWrite.AchievedLevel != agent.FileWriteHost ||
		len(legacy.ExecutionSecurity.FileWrite.EnforcingLayers) != 0 || legacy.ExecutionSecurity.ToolApproval.DenyBaseline != agent.DenyBaselineBestEffort {
		t.Fatalf("index-0 plan report = %+v", legacy.ExecutionSecurity)
	}
	if _, bound := legacy.AuthorityFieldDigests["executionSecurity"]; bound {
		t.Fatal("a plan for work without the section gained an executionSecurity field digest")
	}
	if err := agent.ValidatePreparedHarnessRegistration(legacy, digest); err != nil {
		t.Fatalf("index-0 plan failed registration validation: %v", err)
	}

	stamp := stampedLevels(map[agent.ExecutionSecurityDimension]agent.ExecutionSecurityLevel{agent.ExecutionSecurityFileWrite: agent.FileWriteWorkarea})
	strong, err := agent.CompilePreparedHarness(codexAutonomousSource(stamp), manifest, digest, nil, preparedMaterializations(digest))
	if err != nil {
		t.Fatalf("workarea compile: %v", err)
	}
	if got := strong.ExecutionSecurity.FileWrite; got.AchievedLevel != agent.FileWriteWorkarea || !reflect.DeepEqual(got.EnforcingLayers, []agent.EnforcingLayer{agent.LayerHarnessNative}) {
		t.Fatalf("workarea plan fileWrite report = %+v", got)
	}
	if strong.AuthorityFieldDigests["executionSecurity"] == "" || strong.AuthorityDigest == legacy.AuthorityDigest {
		t.Fatal("the stamp is not bound into the authority digest")
	}

	child := codexAutonomousSource(stamp.Clone())
	child.PreparedHarness = strong
	if _, err := agent.PrepareHarness(child, manifest); err != nil {
		t.Fatalf("child application of a meeting plan: %v", err)
	}

	loosened := codexAutonomousSource(nil)
	loosened.PreparedHarness = strong
	var drift *agent.AuthorityDriftError
	if _, err := agent.PrepareHarness(loosened, manifest); !errors.As(err, &drift) || !reflect.DeepEqual(drift.Fields, []string{"executionSecurity"}) {
		t.Fatalf("a child without the stamp was not refused as executionSecurity drift: %v", err)
	}

	tampered := *strong
	weakened := *strong.ExecutionSecurity
	weakened.FileWrite = agent.ExecutionSecurityDimensionReport{Required: "workarea", AchievedLevel: "host", EnforcingLayers: []agent.EnforcingLayer{}}
	tampered.ExecutionSecurity = &weakened
	tamperedChild := codexAutonomousSource(stamp.Clone())
	tamperedChild.PreparedHarness = &tampered
	if _, err := agent.PrepareHarness(tamperedChild, manifest); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityReceiptUnmet {
		t.Fatalf("a plan whose report differs from the child's rendering err = %v, want execution_security_receipt_unmet", err)
	}

	// A report claiming more than the exact harness renders is refused too:
	// the child re-derives the report and never trusts a stronger claim.
	overclaimed := *legacy
	inflated := *legacy.ExecutionSecurity
	inflated.Network = agent.ExecutionSecurityDimensionReport{Required: "open", AchievedLevel: "none", EnforcingLayers: []agent.EnforcingLayer{agent.LayerEgressProxy}}
	overclaimed.ExecutionSecurity = &inflated
	overclaimedChild := codexAutonomousSource(nil)
	overclaimedChild.PreparedHarness = &overclaimed
	if _, err := agent.PrepareHarness(overclaimedChild, manifest); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityReceiptUnmet {
		t.Fatalf("an overclaiming plan report err = %v, want execution_security_receipt_unmet", err)
	}

	unrenderable := stampedLevels(map[agent.ExecutionSecurityDimension]agent.ExecutionSecurityLevel{agent.ExecutionSecurityNetwork: agent.NetworkNone})
	if _, err := agent.CompilePreparedHarness(codexAutonomousSource(unrenderable), manifest, digest, nil, preparedMaterializations(digest)); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnrenderable {
		t.Fatalf("network none compile err = %v, want execution_security_unrenderable", err)
	}
}

// TestUncontainedHostEnforcement pins what a host with no containment
// attests, and that only an isolation boundary entitles a sandbox claim.
func TestUncontainedHostEnforcement(t *testing.T) {
	t.Parallel()
	host := agent.UncontainedHostEnforcement()
	if err := host.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, dimension := range agent.ExecutionSecurityDimensions() {
		if index, ok := agent.ExecutionSecurityLevelIndex(dimension, host.Level(dimension)); !ok || index != 0 {
			t.Errorf("%s attested %q, want index 0", dimension, host.Level(dimension))
		}
	}
	if host.AttestsSandbox() {
		t.Fatal("an uncontained host attests a sandbox")
	}
	host.Isolation = agent.IsolationOSSandbox
	if !host.AttestsSandbox() {
		t.Fatal("an attested os-sandbox does not count as a sandbox")
	}
	if err := (agent.ExecutionSecurityEnforcement{Isolation: "chroot"}).Validate(); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnresolvable {
		t.Fatalf("unknown attested level err = %v", err)
	}
}
