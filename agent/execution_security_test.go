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

// The exact stamp the control plane sends. The digests are computed
// independently of this package: sha256 over the "<dimension>=<level>" lines
// in canonical order, joined by newlines.
const (
	indexZeroDigest   = "sha256:7a648e38b88ab49be6eb837709b6905be87abaa9e36dadbdf97bb56324a4ebe9"
	workareaDigest    = "sha256:b9e51a0a19bb9cbc21112dcbe2a4e831ea284635aeb3a49fca1d6aefcdc44157"
	platformStampJSON = `{"version":1,"levels":` + indexZeroLevelsJSON +
		`,"sources":{"toolApproval":"system","fileRead":"system","fileWrite":"org","network":"system","credentials":"project","isolation":"session"}` +
		`,"digest":"` + indexZeroDigest + `","parentSessionId":"parent-session-1"}`
)

// TestExecutionSecurityLevelsDigestIsTheCanonicalForm pins the digest to the
// independently computed canonical values.
func TestExecutionSecurityLevelsDigestIsTheCanonicalForm(t *testing.T) {
	t.Parallel()
	if got := agent.ExecutionSecurityLevelsDigest(agent.IndexZeroExecutionSecurityLevels()); got != indexZeroDigest {
		t.Fatalf("index-0 digest = %s, want %s", got, indexZeroDigest)
	}
	workarea := agent.IndexZeroExecutionSecurityLevels()
	workarea.FileWrite = agent.FileWriteWorkarea
	if got := agent.ExecutionSecurityLevelsDigest(workarea); got != workareaDigest {
		t.Fatalf("workarea digest = %s, want %s", got, workareaDigest)
	}
}

// TestParseExecutionSecurity covers the closed decoder for a present
// section: the exact control-plane stamp decodes, and every malformed,
// unknown, missing or mismatched member — an explicit null included — is
// refused as execution_security_unresolvable, never guessed.
func TestParseExecutionSecurity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		raw           string
		wantLevels    *agent.ExecutionSecurityLevels
		wantSources   map[agent.ExecutionSecurityDimension]string
		wantDimension agent.ExecutionSecurityDimension
		wantErr       bool
	}{
		{name: "absent member", raw: "", wantErr: true},
		{name: "explicit null", raw: "null", wantErr: true},
		{
			name: "exact platform stamp with digest and parent session", raw: platformStampJSON,
			wantLevels:  func() *agent.ExecutionSecurityLevels { l := agent.IndexZeroExecutionSecurityLevels(); return &l }(),
			wantSources: map[agent.ExecutionSecurityDimension]string{"toolApproval": "system", "fileRead": "system", "fileWrite": "org", "network": "system", "credentials": "project", "isolation": "session"},
		},
		{
			name: "ruleset revision and resolution time", raw: `{"version":1,"levels":` + indexZeroLevelsJSON + `,"rulesetRevision":"rev-7","resolvedAt":"2026-09-27T00:00:00Z"}`,
			wantLevels: func() *agent.ExecutionSecurityLevels { l := agent.IndexZeroExecutionSecurityLevels(); return &l }(),
		},
		{name: "digest over other levels", raw: strings.Replace(platformStampJSON, indexZeroDigest, workareaDigest, 1), wantErr: true},
		{name: "digest without the sha256 prefix", raw: strings.Replace(platformStampJSON, "sha256:", "", 1), wantErr: true},
		{name: "empty digest", raw: `{"version":1,"levels":` + indexZeroLevelsJSON + `,"digest":""}`, wantErr: true},
		{name: "empty parent session", raw: `{"version":1,"levels":` + indexZeroLevelsJSON + `,"parentSessionId":""}`, wantErr: true},
		{name: "non-string parent session", raw: `{"version":1,"levels":` + indexZeroLevelsJSON + `,"parentSessionId":7}`, wantErr: true},
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
	if _, err := agent.ExecutionSecurityFromOperationalPayload([]byte(`{"executionSecurity":null}`)); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnresolvable {
		t.Fatalf("explicit null section err = %v, want execution_security_unresolvable", err)
	}
	if section, err := agent.ExecutionSecurityFromOperationalPayload([]byte(`{"sessionId":"s"}`)); err != nil || section != nil {
		t.Fatalf("absent section = %+v err=%v, want nil, nil", section, err)
	}
	section, err = agent.ExecutionSecurityFromOperationalPayload([]byte(`{"executionSecurity":` + platformStampJSON + `}`))
	if err != nil || section.Digest != indexZeroDigest || section.ParentSessionID != "parent-session-1" {
		t.Fatalf("platform stamp = %+v err=%v", section, err)
	}
}

// policyManifest is a harness manifest with one tool/lifecycle profile per
// mode whose tool-policy deliveries are the given values.
func policyManifest(native, permission agent.ToolDeliveryKind, rendering agent.ExecutionSecurityRendering) agent.HarnessManifest {
	manifest := agent.HarnessManifest{Name: "test", ExecutionSecurity: rendering}
	for _, mode := range []agent.PromptSessionMode{agent.PromptModeAutonomous, agent.PromptModeHumanControlled} {
		manifest.ToolLifecycle = append(manifest.ToolLifecycle, agent.ToolLifecycleProfile{
			ID: "test/" + string(mode), Mode: mode,
			NativeToolPolicyDelivery: native, PermissionConfigDelivery: permission,
		})
	}
	return manifest
}

// TestRenderExecutionSecurityIndexZeroReport pins the index-0 report: the
// stamped level achieved with no enforcing layer; toolApproval's deny
// baseline derived from the selected profile's tool-policy delivery (and
// unavailable under a full-access grant when the channel only sees sandbox
// escalations); network's always unavailable on this runner.
func TestRenderExecutionSecurityIndexZeroReport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		manifest agent.HarnessManifest
		sandbox  agent.SandboxLevel
		want     agent.DenyBaselineStatus
	}{
		{"no tool policy channel", policyManifest(agent.ToolDeliveryUnsupported, agent.ToolDeliveryUnsupported, agent.ExecutionSecurityRendering{}), "", agent.DenyBaselineUnavailable},
		{"a test oracle is no channel", policyManifest(agent.ToolDeliveryStubOracle, agent.ToolDeliveryStubOracle, agent.ExecutionSecurityRendering{}), "", agent.DenyBaselineUnavailable},
		{"native allow/deny grammar", policyManifest(agent.ToolDeliveryClaudeCLIAllowDeny, agent.ToolDeliveryUnsupported, agent.ExecutionSecurityRendering{}), "", agent.DenyBaselineBestEffort},
		{"permission bridge inside a sandbox", policyManifest(agent.ToolDeliveryUnsupported, agent.ToolDeliveryCodexApprovalBridge, agent.ExecutionSecurityRendering{DenyChannelNeedsSandbox: true}), agent.SandboxWorkspaceWrite, agent.DenyBaselineBestEffort},
		{"permission bridge under full access", policyManifest(agent.ToolDeliveryUnsupported, agent.ToolDeliveryCodexApprovalBridge, agent.ExecutionSecurityRendering{DenyChannelNeedsSandbox: true}), agent.SandboxFullAccess, agent.DenyBaselineUnavailable},
		{"no profile for the mode", agent.HarnessManifest{Name: "test"}, "", agent.DenyBaselineUnavailable},
	} {
		report, err := agent.RenderExecutionSecurity(agent.Spec{ExecutionSecurity: stampedLevels(nil), SandboxLevel: tc.sandbox}, tc.manifest)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		raw, _ := json.Marshal(report)
		want := `{"toolApproval":{"required":"bypass","achievedLevel":"bypass","enforcingLayers":[],"denyBaseline":"` + string(tc.want) + `"},` +
			`"fileRead":{"required":"host","achievedLevel":"host","enforcingLayers":[]},` +
			`"fileWrite":{"required":"host","achievedLevel":"host","enforcingLayers":[]},` +
			`"network":{"required":"open","achievedLevel":"open","enforcingLayers":[],"denyBaseline":"unavailable"},` +
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

// TestRenderExecutionSecurityRefusesUnrenderable raises each dimension above
// index 0 on a harness that declares no channel for it; each is refused
// before any side effect with execution_security_unrenderable naming exactly
// that dimension, and several at once are all named.
func TestRenderExecutionSecurityRefusesUnrenderable(t *testing.T) {
	t.Parallel()
	manifest := agent.HarnessManifest{Name: "test"}
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

// TestRenderExecutionSecurityDeclaredDenyBaseline: a declared rendering on a
// dimension that carries a deny baseline reports the baseline it declares;
// where the level needs an enforced baseline and the declaration is not
// enforced, the level is refused rather than reported.
func TestRenderExecutionSecurityDeclaredDenyBaseline(t *testing.T) {
	t.Parallel()
	native := []agent.EnforcingLayer{agent.LayerHarnessNative}
	egress := []agent.EnforcingLayer{agent.LayerEgressProxy}
	for _, tc := range []struct {
		name      string
		dimension agent.ExecutionSecurityDimension
		level     agent.ExecutionSecurityLevel
		declared  agent.RenderedExecutionSecurityLevel
		want      agent.DenyBaselineStatus
		refused   bool
	}{
		{
			"deny-list declared best effort is refused", agent.ExecutionSecurityToolApproval, agent.ToolApprovalDenyList,
			agent.RenderedExecutionSecurityLevel{Dimension: agent.ExecutionSecurityToolApproval, Level: agent.ToolApprovalDenyList, Layers: native, DenyBaseline: agent.DenyBaselineBestEffort},
			"", true,
		},
		{
			"deny-list declared enforced renders", agent.ExecutionSecurityToolApproval, agent.ToolApprovalDenyList,
			agent.RenderedExecutionSecurityLevel{Dimension: agent.ExecutionSecurityToolApproval, Level: agent.ToolApprovalDenyList, Layers: native, DenyBaseline: agent.DenyBaselineEnforced},
			agent.DenyBaselineEnforced, false,
		},
		{
			"network allow-list without a baseline is refused", agent.ExecutionSecurityNetwork, agent.NetworkAllowList,
			agent.RenderedExecutionSecurityLevel{Dimension: agent.ExecutionSecurityNetwork, Level: agent.NetworkAllowList, Layers: egress},
			"", true,
		},
		{
			"network logged reports an undeclared baseline as unavailable", agent.ExecutionSecurityNetwork, agent.NetworkLogged,
			agent.RenderedExecutionSecurityLevel{Dimension: agent.ExecutionSecurityNetwork, Level: agent.NetworkLogged, Layers: egress},
			agent.DenyBaselineUnavailable, false,
		},
	} {
		manifest := agent.HarnessManifest{Name: "test", ExecutionSecurity: agent.ExecutionSecurityRendering{Levels: []agent.RenderedExecutionSecurityLevel{tc.declared}}}
		report, err := agent.RenderExecutionSecurity(agent.Spec{ExecutionSecurity: stampedLevels(map[agent.ExecutionSecurityDimension]agent.ExecutionSecurityLevel{tc.dimension: tc.level})}, manifest)
		if tc.refused {
			if agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnrenderable {
				t.Errorf("%s: err = %v, want execution_security_unrenderable", tc.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got := report.Dimension(tc.dimension); got.AchievedLevel != tc.level || got.DenyBaseline != tc.want {
			t.Errorf("%s: report = %+v, want %s with %s", tc.name, got, tc.level, tc.want)
		}
	}
}

// TestExecutionSecurityReportMeets is the receipt check: a report meets a
// stamp only on its achieved levels, a missing report achieves exactly index
// 0, a claim above index 0 without an enforcing layer counts as index 0, and
// a toolApproval claim above bypass, or a network claim at allow-list or
// above, without an enforced deny baseline counts as the level below.
func TestExecutionSecurityReportMeets(t *testing.T) {
	t.Parallel()
	indexZero := agent.IndexZeroExecutionSecurityLevels()
	workarea := indexZero
	workarea.FileWrite = agent.FileWriteWorkarea
	denyList := indexZero
	denyList.ToolApproval = agent.ToolApprovalDenyList
	networkAllowList := indexZero
	networkAllowList.Network = agent.NetworkAllowList
	networkLogged := indexZero
	networkLogged.Network = agent.NetworkLogged
	egress := []agent.EnforcingLayer{agent.LayerEgressProxy}
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
		{name: "network allow-list without an enforced baseline is only logged", report: report(func(r *agent.ExecutionSecurityReport) {
			r.Network = agent.ExecutionSecurityDimensionReport{Required: "allow-list", AchievedLevel: "allow-list", EnforcingLayers: egress, DenyBaseline: agent.DenyBaselineUnavailable}
		}), stamp: networkAllowList, wantCode: agent.ExecutionSecurityReceiptUnmet, wantUnmet: agent.ExecutionSecurityNetwork},
		{name: "network allow-list without an enforced baseline still meets logged", report: report(func(r *agent.ExecutionSecurityReport) {
			r.Network = agent.ExecutionSecurityDimensionReport{Required: "logged", AchievedLevel: "allow-list", EnforcingLayers: egress, DenyBaseline: agent.DenyBaselineUnavailable}
		}), stamp: networkLogged},
		{name: "network allow-list with an enforced baseline meets", report: report(func(r *agent.ExecutionSecurityReport) {
			r.Network = agent.ExecutionSecurityDimensionReport{Required: "allow-list", AchievedLevel: "allow-list", EnforcingLayers: egress, DenyBaseline: agent.DenyBaselineEnforced}
		}), stamp: networkAllowList},
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
// registration validator enforces: a report that carries network's deny
// baseline is valid, and one that drops it, or puts one on another
// dimension, is not.
func TestValidateExecutionSecurityReport(t *testing.T) {
	t.Parallel()
	valid, err := agent.RenderExecutionSecurity(agent.Spec{}, agent.HarnessManifest{})
	if err != nil {
		t.Fatal(err)
	}
	if valid.Network.DenyBaseline != agent.DenyBaselineUnavailable {
		t.Fatalf("network deny baseline = %q, want unavailable", valid.Network.DenyBaseline)
	}
	if err := agent.ValidateExecutionSecurityReport(valid); err != nil {
		t.Fatalf("a conformant report carrying network.denyBaseline was refused: %v", err)
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
		{"deny baseline off toolApproval and network", func(r *agent.ExecutionSecurityReport) { r.FileWrite.DenyBaseline = agent.DenyBaselineEnforced }},
		{"toolApproval without a deny baseline", func(r *agent.ExecutionSecurityReport) { r.ToolApproval.DenyBaseline = "" }},
		{"network without a deny baseline", func(r *agent.ExecutionSecurityReport) { r.Network.DenyBaseline = "" }},
		{"network with an unknown deny baseline", func(r *agent.ExecutionSecurityReport) { r.Network.DenyBaseline = "mostly" }},
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
// receipt: work without the section compiles the pre-field plan (no report,
// no field digest), stamped work records the report and binds the stamp into
// the authority digest, a child re-derives the report and refuses a plan
// whose report differs or no longer meets the stamp, and a stamp the exact
// harness cannot render denies the plan.
func TestCompilePreparedHarnessRecordsExecutionSecurityReport(t *testing.T) {
	t.Parallel()
	manifest := (&codex.Provider{}).Manifest()
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	compile := func(t *testing.T, source agent.Spec, manifest agent.HarnessManifest) *agent.PreparedHarness {
		t.Helper()
		plan, err := agent.CompilePreparedHarness(source, manifest, digest, nil, preparedMaterializations(digest))
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		return plan
	}

	legacy := compile(t, codexAutonomousSource(nil), manifest)
	raw, _ := json.Marshal(legacy)
	if legacy.ExecutionSecurity != nil || strings.Contains(string(raw), "executionSecurity") {
		t.Fatalf("a plan for work without the section carries executionSecurity: %s", raw)
	}
	if err := agent.ValidatePreparedHarnessRegistration(legacy, digest); err != nil {
		t.Fatalf("unstamped plan failed registration validation: %v", err)
	}

	indexZero := compile(t, codexAutonomousSource(stampedLevels(nil)), manifest)
	if indexZero.ExecutionSecurity == nil || indexZero.ExecutionSecurity.ToolApproval.DenyBaseline != agent.DenyBaselineBestEffort ||
		indexZero.ExecutionSecurity.Network.DenyBaseline != agent.DenyBaselineUnavailable || len(indexZero.ExecutionSecurity.FileWrite.EnforcingLayers) != 0 {
		t.Fatalf("index-0 stamped plan report = %+v", indexZero.ExecutionSecurity)
	}
	if indexZero.AuthorityFieldDigests["executionSecurity"] == "" || indexZero.AuthorityDigest == legacy.AuthorityDigest {
		t.Fatal("the stamp is not bound into the authority digest")
	}
	if err := agent.ValidatePreparedHarnessRegistration(indexZero, digest); err != nil {
		t.Fatalf("index-0 stamped plan failed registration validation: %v", err)
	}
	fullAccess := codexAutonomousSource(stampedLevels(nil))
	fullAccess.SandboxLevel = agent.SandboxFullAccess
	if got := compile(t, fullAccess, manifest).ExecutionSecurity.ToolApproval.DenyBaseline; got != agent.DenyBaselineUnavailable {
		t.Fatalf("codex under full access deny baseline = %q, want unavailable (the bridge never sees a call)", got)
	}

	// A report claiming more than the exact harness renders is refused: the
	// child re-derives the report and never trusts a stronger claim.
	overclaimed := *indexZero
	inflated := *indexZero.ExecutionSecurity
	inflated.Network = agent.ExecutionSecurityDimensionReport{Required: "open", AchievedLevel: "none", EnforcingLayers: []agent.EnforcingLayer{agent.LayerEgressProxy}, DenyBaseline: agent.DenyBaselineEnforced}
	overclaimed.ExecutionSecurity = &inflated
	overclaimedChild := codexAutonomousSource(stampedLevels(nil))
	overclaimedChild.PreparedHarness = &overclaimed
	if _, err := agent.PrepareHarness(overclaimedChild, manifest); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityReceiptUnmet {
		t.Fatalf("an overclaiming plan report err = %v, want execution_security_receipt_unmet", err)
	}

	// The shipped codex adapter declares nothing above index 0.
	workareaStamp := stampedLevels(map[agent.ExecutionSecurityDimension]agent.ExecutionSecurityLevel{agent.ExecutionSecurityFileWrite: agent.FileWriteWorkarea})
	if _, err := agent.CompilePreparedHarness(codexAutonomousSource(workareaStamp), manifest, digest, nil, preparedMaterializations(digest)); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnrenderable {
		t.Fatalf("codex fileWrite workarea compile err = %v, want execution_security_unrenderable", err)
	}

	// Above index 0, through a test-only declaration on the same adapter.
	declared := manifest
	declared.ExecutionSecurity.Levels = []agent.RenderedExecutionSecurityLevel{{
		Dimension: agent.ExecutionSecurityFileWrite, Level: agent.FileWriteWorkarea, Layers: []agent.EnforcingLayer{agent.LayerHarnessNative},
	}}
	strong := compile(t, codexAutonomousSource(workareaStamp), declared)
	if got := strong.ExecutionSecurity.FileWrite; got.AchievedLevel != agent.FileWriteWorkarea || !reflect.DeepEqual(got.EnforcingLayers, []agent.EnforcingLayer{agent.LayerHarnessNative}) {
		t.Fatalf("declared workarea plan fileWrite report = %+v", got)
	}
	child := codexAutonomousSource(workareaStamp.Clone())
	child.PreparedHarness = strong
	if _, err := agent.PrepareHarness(child, declared); err != nil {
		t.Fatalf("child application of a meeting plan: %v", err)
	}
	loosened := codexAutonomousSource(nil)
	loosened.PreparedHarness = strong
	var drift *agent.AuthorityDriftError
	if _, err := agent.PrepareHarness(loosened, declared); !errors.As(err, &drift) || !reflect.DeepEqual(drift.Fields, []string{"executionSecurity"}) {
		t.Fatalf("a child without the stamp was not refused as executionSecurity drift: %v", err)
	}
	tampered := *strong
	weakened := *strong.ExecutionSecurity
	weakened.FileWrite = agent.ExecutionSecurityDimensionReport{Required: "workarea", AchievedLevel: "host", EnforcingLayers: []agent.EnforcingLayer{}}
	tampered.ExecutionSecurity = &weakened
	tamperedChild := codexAutonomousSource(workareaStamp.Clone())
	tamperedChild.PreparedHarness = &tampered
	if _, err := agent.PrepareHarness(tamperedChild, declared); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityReceiptUnmet {
		t.Fatalf("a plan whose report differs from the child's rendering err = %v, want execution_security_receipt_unmet", err)
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
