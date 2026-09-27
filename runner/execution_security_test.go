package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

func executionSecuritySection(mutate func(*agent.ExecutionSecurityLevels)) *agent.ExecutionSecurity {
	section := &agent.ExecutionSecurity{Version: 1, Levels: agent.IndexZeroExecutionSecurityLevels()}
	if mutate != nil {
		mutate(&section.Levels)
	}
	return section
}

// TestResolveSandboxLevelFromExecutionSecurity pins which input decides the
// sandbox tier: permissionProfile keeps its legacy meaning for work without
// the section; with the section, fileWrite decides and permissionProfile is
// ignored in both directions.
func TestResolveSandboxLevelFromExecutionSecurity(t *testing.T) {
	t.Parallel()
	workarea := executionSecuritySection(func(l *agent.ExecutionSecurityLevels) { l.FileWrite = agent.FileWriteWorkarea })
	tests := []struct {
		name    string
		profile PermissionProfile
		section *agent.ExecutionSecurity
		want    agent.SandboxLevel
	}{
		{"legacy absent profile", "", nil, agent.SandboxWorkspaceWrite},
		{"legacy autonomous profile", PermissionProfileAutonomous, nil, agent.SandboxFullAccess},
		{"index 0 grants full access whatever the profile asks", PermissionProfileWorkspaceWrite, executionSecuritySection(nil), agent.SandboxFullAccess},
		{"index 0 without a profile grants full access", "", executionSecuritySection(nil), agent.SandboxFullAccess},
		{"workarea overrides an autonomous profile", PermissionProfileAutonomous, workarea, agent.SandboxWorkspaceWrite},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			qw := QueuedWork{PermissionProfile: tc.profile, ExecutionSecurity: tc.section}
			if got := resolveSandboxLevel(qw, nil); got != tc.want {
				t.Fatalf("resolveSandboxLevel = %q, want %q", got, tc.want)
			}
			if spec := translateSpec(qw, agent.Capabilities{}, SpecInputs{}); spec.SandboxLevel != tc.want || (tc.section == nil) != (spec.ExecutionSecurity == nil) {
				t.Fatalf("translated spec sandbox=%q section=%+v", spec.SandboxLevel, spec.ExecutionSecurity)
			}
		})
	}
}

// TestQueuedWorkDecodeReadsExecutionSecurity covers the poll lane's decode:
// both lanes build QueuedWork with encoding/json over the operational
// payload, so the section's closed decoder runs there, and the section is
// admission-bound through the operational payload digest.
func TestQueuedWorkDecodeReadsExecutionSecurity(t *testing.T) {
	t.Parallel()
	var absent QueuedWork
	if err := json.Unmarshal([]byte(`{"sessionId":"s"}`), &absent); err != nil || absent.ExecutionSecurity != nil {
		t.Fatalf("absent: err=%v section=%+v", err, absent.ExecutionSecurity)
	}
	var present QueuedWork
	raw := `{"sessionId":"s","executionSecurity":{"version":1,"levels":{"toolApproval":"bypass","fileRead":"host","fileWrite":"workarea","network":"open","credentials":"ambient-host-login","isolation":"host-user"}}}`
	if err := json.Unmarshal([]byte(raw), &present); err != nil || present.ExecutionSecurity == nil || present.ExecutionSecurity.Levels.FileWrite != agent.FileWriteWorkarea {
		t.Fatalf("present: err=%v section=%+v", err, present.ExecutionSecurity)
	}
	var malformed QueuedWork
	err := json.Unmarshal([]byte(strings.Replace(raw, `"workarea"`, `"everywhere"`, 1)), &malformed)
	if agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnresolvable {
		t.Fatalf("malformed err = %v, want execution_security_unresolvable", err)
	}

	withoutDigest, err := DigestOperationalPayload(absent)
	if err != nil {
		t.Fatal(err)
	}
	withDigest, err := DigestOperationalPayload(present)
	if err != nil {
		t.Fatal(err)
	}
	if withDigest == withoutDigest {
		t.Fatal("the stamped levels do not change the admission digest")
	}
	canonical, err := CanonicalOperationalPayload(QueuedWork{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), "executionSecurity") {
		t.Fatalf("absent section leaked into the canonical payload: %s", canonical)
	}
}

func executionSecurityReceiptWork(t *testing.T, sessionID string, section *agent.ExecutionSecurity) (QueuedWork, *Registry, json.RawMessage) {
	t.Helper()
	provider := &manifestSelectorProvider{
		selectorFakeProvider: &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex},
		manifest:             codexManifestForTest(), capabilities: codexCapabilitiesForTest(),
	}
	registry := NewRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	qw := exactReceiptQueuedWork(sessionID)
	qw.Body = "exercise execution security preflight"
	qw.ExecutionSecurity = section
	qw = attachAdmittedExecutionCell(t, qw, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, nil))
	operational, err := CanonicalOperationalPayload(qw)
	if err != nil {
		t.Fatal(err)
	}
	qw.OperationalPayload = operational
	detail := rawJSONForRunner(t, map[string]any{
		"sessionId": qw.SessionID, "workerId": qw.WorkerID, "admissionReceipt": qw.AdmissionReceipt,
		"effectiveCell": qw.EffectiveCell, "executionRuntimeBinding": qw.ExecutionRuntimeBinding,
		"operationalPayload": qw.OperationalPayload,
	})
	return qw, registry, detail
}

// TestPreflightExecutionReportsExecutionSecurity covers the receipt lane:
// the host-compiled plan carries the report for every run (index 0 and
// above), a stamp the exact harness cannot render denies the host receipt
// before credentials exist, and a malformed stamp never reaches compilation.
func TestPreflightExecutionReportsExecutionSecurity(t *testing.T) {
	t.Parallel()
	decodePlan := func(t *testing.T, raw json.RawMessage) (executioncell.HostAdaptationReceipt, agent.PreparedHarness) {
		t.Helper()
		host, err := executioncell.DecodeHostAdaptationReceipt(raw)
		if err != nil {
			t.Fatal(err)
		}
		var plan agent.PreparedHarness
		if len(host.Plan) > 0 {
			if err := json.Unmarshal(host.Plan, &plan); err != nil {
				t.Fatal(err)
			}
		}
		return host, plan
	}

	t.Run("index 0 without the section", func(t *testing.T) {
		t.Parallel()
		_, registry, detail := executionSecurityReceiptWork(t, "exec-security-index-zero", nil)
		raw, err := NewProviderView(registry).PreflightExecution(detail)
		if err != nil {
			t.Fatalf("preflight: %v", err)
		}
		host, plan := decodePlan(t, raw)
		if host.Decision != "ready" || plan.ExecutionSecurity == nil {
			t.Fatalf("decision=%q report=%+v", host.Decision, plan.ExecutionSecurity)
		}
		for _, dimension := range agent.ExecutionSecurityDimensions() {
			entry := plan.ExecutionSecurity.Dimension(dimension)
			if index, _ := agent.ExecutionSecurityLevelIndex(dimension, entry.AchievedLevel); index != 0 || len(entry.EnforcingLayers) != 0 {
				t.Errorf("%s = %+v, want index 0 with no layer", dimension, entry)
			}
		}
	})

	t.Run("fileWrite workarea renders natively", func(t *testing.T) {
		t.Parallel()
		section := executionSecuritySection(func(l *agent.ExecutionSecurityLevels) { l.FileWrite = agent.FileWriteWorkarea })
		_, registry, detail := executionSecurityReceiptWork(t, "exec-security-workarea", section)
		raw, err := NewProviderView(registry).PreflightExecution(detail)
		if err != nil {
			t.Fatalf("preflight: %v", err)
		}
		_, plan := decodePlan(t, raw)
		if got := plan.ExecutionSecurity.FileWrite; got.AchievedLevel != agent.FileWriteWorkarea || len(got.EnforcingLayers) != 1 || got.EnforcingLayers[0] != agent.LayerHarnessNative {
			t.Fatalf("fileWrite report = %+v", got)
		}
	})

	t.Run("network none is refused before credentials", func(t *testing.T) {
		t.Parallel()
		section := executionSecuritySection(func(l *agent.ExecutionSecurityLevels) { l.Network = agent.NetworkNone })
		_, registry, detail := executionSecurityReceiptWork(t, "exec-security-network", section)
		raw, err := NewProviderView(registry).PreflightExecution(detail)
		if agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnrenderable {
			t.Fatalf("err = %v, want execution_security_unrenderable", err)
		}
		host, _ := decodePlan(t, raw)
		if host.Decision != "denied" || !strings.Contains(host.Denial, "execution_security_unrenderable") || !strings.Contains(host.Denial, "network") {
			t.Fatalf("host receipt decision=%q denial=%q", host.Decision, host.Denial)
		}
	})

	t.Run("malformed stamp never reaches compilation", func(t *testing.T) {
		t.Parallel()
		qw, registry, _ := executionSecurityReceiptWork(t, "exec-security-malformed", nil)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(qw.OperationalPayload, &fields); err != nil {
			t.Fatal(err)
		}
		fields["executionSecurity"] = json.RawMessage(`{"version":1,"levels":{"toolApproval":"trust-me"}}`)
		detail := rawJSONForRunner(t, map[string]any{
			"sessionId": qw.SessionID, "workerId": qw.WorkerID, "admissionReceipt": qw.AdmissionReceipt,
			"effectiveCell": qw.EffectiveCell, "executionRuntimeBinding": qw.ExecutionRuntimeBinding,
			"operationalPayload": fields,
		})
		if _, err := NewProviderView(registry).PreflightExecution(detail); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnresolvable {
			t.Fatalf("err = %v, want execution_security_unresolvable", err)
		}
	})
}

// TestRun_ExecutionSecurityReportAndRefusal covers the poll lane end to end
// on the stub harness: work without the section runs exactly as before and
// reports index 0 on every dimension; a stamp the harness cannot render is
// refused before the workarea is provisioned or the provider spawns.
func TestRun_ExecutionSecurityReportAndRefusal(t *testing.T) {
	h := newRunnerHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := h.runner.Run(ctx, h.queuedWork("EXEC-SECURITY-INDEX-ZERO"))
	if err != nil || res.Status != "completed" {
		t.Fatalf("index-0 run: status=%q err=%v (%s)", res.Status, err, res.Error)
	}
	if res.ExecutionSecurity == nil || res.ExecutionSecurity.ToolApproval.AchievedLevel != agent.ToolApprovalBypass ||
		res.ExecutionSecurity.ToolApproval.DenyBaseline != agent.DenyBaselineUnavailable || len(res.ExecutionSecurity.Isolation.EnforcingLayers) != 0 {
		t.Fatalf("index-0 report = %+v", res.ExecutionSecurity)
	}

	refused := h.queuedWork("EXEC-SECURITY-UNRENDERABLE")
	refused.ExecutionSecurity = executionSecuritySection(func(l *agent.ExecutionSecurityLevels) { l.Isolation = agent.IsolationContainer })
	res, err = h.runner.Run(ctx, refused)
	if agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnrenderable {
		t.Fatalf("err = %v, want execution_security_unrenderable", err)
	}
	if res.Status != "failed" || res.FailureMode != FailureExecutionSecurity || !strings.Contains(res.Error, "isolation") {
		t.Fatalf("refusal result status=%q mode=%q error=%q", res.Status, res.FailureMode, res.Error)
	}
	if res.WorktreePath != "" || res.ProviderSessionID != "" {
		t.Fatalf("refusal had side effects: worktree=%q providerSession=%q", res.WorktreePath, res.ProviderSessionID)
	}
}
