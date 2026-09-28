package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	geminiprovider "github.com/RenseiAI/donmai/provider/harness/gemini"
	"github.com/RenseiAI/donmai/runtime/state"
)

// platformStamp is the exact stamp shape the control plane sends: every
// dimension in levels and sources, the "sha256:" digest of the canonical
// levels (computed independently of this package), and the parent session a
// "session" source names.
const platformStamp = `{"version":1,` +
	`"levels":{"toolApproval":"bypass","fileRead":"host","fileWrite":"host","network":"open","credentials":"ambient-host-login","isolation":"host-user"},` +
	`"sources":{"toolApproval":"system","fileRead":"system","fileWrite":"org","network":"system","credentials":"project","isolation":"session"},` +
	`"digest":"sha256:7a648e38b88ab49be6eb837709b6905be87abaa9e36dadbdf97bb56324a4ebe9",` +
	`"parentSessionId":"parent-session-1"}`

func executionSecuritySection(mutate func(*agent.ExecutionSecurityLevels)) *agent.ExecutionSecurity {
	section := &agent.ExecutionSecurity{Version: 1, Levels: agent.IndexZeroExecutionSecurityLevels()}
	if mutate != nil {
		mutate(&section.Levels)
	}
	return section
}

// TestResolveSandboxLevelFromExecutionSecurity pins which input decides the
// sandbox tier: permissionProfile keeps its legacy meaning for work without
// the section; with the section the full-access grant needs fileRead,
// fileWrite and network all at index 0, and permissionProfile is ignored in
// both directions.
func TestResolveSandboxLevelFromExecutionSecurity(t *testing.T) {
	t.Parallel()
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
		{
			"fileWrite workarea overrides an autonomous profile", PermissionProfileAutonomous,
			executionSecuritySection(func(l *agent.ExecutionSecurityLevels) { l.FileWrite = agent.FileWriteWorkarea }), agent.SandboxWorkspaceWrite,
		},
		{
			"fileRead above index 0 withholds full access", PermissionProfileAutonomous,
			executionSecuritySection(func(l *agent.ExecutionSecurityLevels) { l.FileRead = agent.FileReadWorkarea }), agent.SandboxWorkspaceWrite,
		},
		{
			"network above index 0 withholds full access", PermissionProfileAutonomous,
			executionSecuritySection(func(l *agent.ExecutionSecurityLevels) { l.Network = agent.NetworkNone }), agent.SandboxWorkspaceWrite,
		},
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

// TestQueuedWorkDecodeReadsExecutionSecurity covers the decode every lane
// shares: the exact control-plane stamp reaches QueuedWork, a malformed one
// fails the decode, an explicit null is refused by the raw-member check, and
// the section is admission-bound through the operational payload digest.
func TestQueuedWorkDecodeReadsExecutionSecurity(t *testing.T) {
	t.Parallel()
	var absent QueuedWork
	if err := json.Unmarshal([]byte(`{"sessionId":"s"}`), &absent); err != nil || absent.ExecutionSecurity != nil {
		t.Fatalf("absent: err=%v section=%+v", err, absent.ExecutionSecurity)
	}
	raw := `{"sessionId":"s","executionSecurity":` + platformStamp + `}`
	if err := ValidateExecutionSecurityMember(json.RawMessage(raw)); err != nil {
		t.Fatalf("platform stamp refused: %v", err)
	}
	var present QueuedWork
	if err := json.Unmarshal([]byte(raw), &present); err != nil || present.ExecutionSecurity == nil || present.ExecutionSecurity.ParentSessionID != "parent-session-1" {
		t.Fatalf("present: err=%v section=%+v", err, present.ExecutionSecurity)
	}
	var malformed QueuedWork
	err := json.Unmarshal([]byte(strings.Replace(raw, `"fileWrite":"host"`, `"fileWrite":"everywhere"`, 1)), &malformed)
	if agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnresolvable {
		t.Fatalf("malformed err = %v, want execution_security_unresolvable", err)
	}
	if err := ValidateExecutionSecurityMember(json.RawMessage(`{"sessionId":"s","executionSecurity":null}`)); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnresolvable {
		t.Fatalf("explicit null err = %v, want execution_security_unresolvable", err)
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

// TestExecutionSecurityReportChecksThePlanAgainstTheStamp pins the runner's
// own check on a receipt-bearing run: the host plan's report is returned only
// once it meets the session's stamp, and no report is returned for work
// without the section.
func TestExecutionSecurityReportChecksThePlanAgainstTheStamp(t *testing.T) {
	t.Parallel()
	stamped := agent.Spec{ExecutionSecurity: executionSecuritySection(func(l *agent.ExecutionSecurityLevels) { l.FileWrite = agent.FileWriteWorkarea })}
	indexZeroReport, err := agent.RenderExecutionSecurity(agent.Spec{ExecutionSecurity: executionSecuritySection(nil)}, agent.HarnessManifest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executionSecurityReport(stamped, nil, &agent.PreparedHarness{ExecutionSecurity: &indexZeroReport}); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityReceiptUnmet {
		t.Fatalf("a plan report short of the stamp err = %v, want execution_security_receipt_unmet", err)
	}
	if _, err := executionSecurityReport(stamped, nil, &agent.PreparedHarness{}); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityReceiptUnmet {
		t.Fatalf("a plan without a report for stamped work err = %v, want execution_security_receipt_unmet", err)
	}
	report, err := executionSecurityReport(agent.Spec{ExecutionSecurity: executionSecuritySection(nil)}, nil, &agent.PreparedHarness{ExecutionSecurity: &indexZeroReport})
	if err != nil || report == nil {
		t.Fatalf("a meeting plan report = %+v err=%v", report, err)
	}
	if report, err := executionSecurityReport(agent.Spec{}, nil, &agent.PreparedHarness{}); err != nil || report != nil {
		t.Fatalf("unstamped work report = %+v err=%v, want none", report, err)
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
// work without the section compiles a plan with no report (byte-compatible
// with receipts that predate the field), a stamped plan carries the report,
// a stamp the exact harness cannot render denies the host receipt before
// credentials exist, and a malformed or null stamp never reaches compilation.
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

	t.Run("no section, no report", func(t *testing.T) {
		t.Parallel()
		_, registry, detail := executionSecurityReceiptWork(t, "exec-security-absent", nil)
		raw, err := NewProviderView(registry).PreflightExecution(detail)
		if err != nil {
			t.Fatalf("preflight: %v", err)
		}
		host, plan := decodePlan(t, raw)
		if host.Decision != "ready" || plan.ExecutionSecurity != nil || strings.Contains(string(host.Plan), "executionSecurity") {
			t.Fatalf("decision=%q plan carries executionSecurity: %s", host.Decision, host.Plan)
		}
	})

	t.Run("explicit index 0 stamp is reported", func(t *testing.T) {
		t.Parallel()
		_, registry, detail := executionSecurityReceiptWork(t, "exec-security-index-zero", executionSecuritySection(nil))
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
		// Index 0 renders codex headless under the full-access grant, where
		// its approval bridge never sees a call.
		if plan.ExecutionSecurity.ToolApproval.DenyBaseline != agent.DenyBaselineUnavailable {
			t.Errorf("codex full-access deny baseline = %q, want unavailable", plan.ExecutionSecurity.ToolApproval.DenyBaseline)
		}
	})

	for _, tc := range []struct {
		name      string
		section   *agent.ExecutionSecurity
		dimension string
	}{
		{"fileWrite workarea is refused on codex", executionSecuritySection(func(l *agent.ExecutionSecurityLevels) { l.FileWrite = agent.FileWriteWorkarea }), "fileWrite"},
		{"network none is refused", executionSecuritySection(func(l *agent.ExecutionSecurityLevels) { l.Network = agent.NetworkNone }), "network"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, registry, detail := executionSecurityReceiptWork(t, "exec-security-"+tc.dimension, tc.section)
			raw, err := NewProviderView(registry).PreflightExecution(detail)
			if agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnrenderable {
				t.Fatalf("err = %v, want execution_security_unrenderable", err)
			}
			host, _ := decodePlan(t, raw)
			if host.Decision != "denied" || !strings.Contains(host.Denial, "execution_security_unrenderable") || !strings.Contains(host.Denial, tc.dimension) {
				t.Fatalf("host receipt decision=%q denial=%q", host.Decision, host.Denial)
			}
		})
	}

	for _, tc := range []struct {
		name  string
		stamp string
	}{
		{"malformed stamp", `{"version":1,"levels":{"toolApproval":"trust-me"}}`},
		{"explicit null", `null`},
	} {
		t.Run(tc.name+" never reaches compilation", func(t *testing.T) {
			t.Parallel()
			qw, registry, _ := executionSecurityReceiptWork(t, "exec-security-malformed", nil)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(qw.OperationalPayload, &fields); err != nil {
				t.Fatal(err)
			}
			fields["executionSecurity"] = json.RawMessage(tc.stamp)
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
}

// TestRun_ExecutionSecurityReportAndRefusal covers the poll lane end to end
// on the stub harness: work without the section runs as before and records
// no report; a stamped run records the report on the result and in the
// session state file; a stamp the harness cannot render is refused, typed,
// before the workarea is provisioned or the provider spawns.
func TestRun_ExecutionSecurityReportAndRefusal(t *testing.T) {
	h := newRunnerHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := h.runner.Run(ctx, h.queuedWork("EXEC-SECURITY-ABSENT"))
	if err != nil || res.Status != "completed" {
		t.Fatalf("unstamped run: status=%q err=%v (%s)", res.Status, err, res.Error)
	}
	if res.ExecutionSecurity != nil {
		t.Fatalf("unstamped run recorded a report: %+v", res.ExecutionSecurity)
	}

	stamped := h.queuedWork("EXEC-SECURITY-INDEX-ZERO")
	stamped.ExecutionSecurity = executionSecuritySection(nil)
	res, err = h.runner.Run(ctx, stamped)
	if err != nil || res.Status != "completed" {
		t.Fatalf("stamped run: status=%q err=%v (%s)", res.Status, err, res.Error)
	}
	if res.ExecutionSecurity == nil || res.ExecutionSecurity.ToolApproval.AchievedLevel != agent.ToolApprovalBypass ||
		res.ExecutionSecurity.ToolApproval.DenyBaseline != agent.DenyBaselineUnavailable || len(res.ExecutionSecurity.Isolation.EnforcingLayers) != 0 {
		t.Fatalf("stamped run report = %+v", res.ExecutionSecurity)
	}
	persisted, err := state.NewStore().Read(res.WorktreePath)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ExecutionSecurity == nil || !reflect.DeepEqual(*persisted.ExecutionSecurity, *res.ExecutionSecurity) {
		t.Fatalf("state file report = %+v, want %+v", persisted.ExecutionSecurity, res.ExecutionSecurity)
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
	if res.ExecutionSecurityRefusal == nil || res.ExecutionSecurityRefusal.Code != agent.ExecutionSecurityUnrenderable ||
		len(res.ExecutionSecurityRefusal.Dimensions) != 1 || res.ExecutionSecurityRefusal.Dimensions[0] != agent.ExecutionSecurityIsolation {
		t.Fatalf("typed refusal = %+v", res.ExecutionSecurityRefusal)
	}
	if res.WorktreePath != "" || res.ProviderSessionID != "" {
		t.Fatalf("refusal had side effects: worktree=%q providerSession=%q", res.WorktreePath, res.ProviderSessionID)
	}
}

// TestTranslatedSpecOffersGeminiItsNativeTools goes from the runner's
// translated spec for work with no configured allow list, through the
// gemini-direct provider, to the request the model receives: removing the
// runner's hidden allow list must not remove the native tools it used to
// declare.
func TestTranslatedSpecOffersGeminiItsNativeTools(t *testing.T) {
	t.Parallel()
	var (
		mu    sync.Mutex
		tools []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Tools []struct {
				FunctionDeclarations []struct {
					Name string `json:"name"`
				} `json:"functionDeclarations"`
			} `json:"tools"`
		}
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		for _, tool := range body.Tools {
			for _, decl := range tool.FunctionDeclarations {
				tools = append(tools, decl.Name)
			}
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`))
	}))
	t.Cleanup(server.Close)

	provider, err := geminiprovider.New(geminiprovider.Options{APIKey: "test-key", Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	qw := QueuedWork{ResolvedProfile: ResolvedProfile{Model: "gemini-3.5-flash"}}
	spec := translateSpec(qw, provider.Capabilities(), SpecInputs{Prompt: "hi", Autonomous: true, Cwd: t.TempDir()})
	handle, err := provider.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = handle.Stop(context.Background()) })
	for event := range handle.Events() {
		if _, done := event.(agent.ResultEvent); done {
			break
		}
		if _, failed := event.(agent.ErrorEvent); failed {
			break
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := strings.Join(tools, ","), "Bash,Edit,Write,Read,Grep,Glob,Task"; got != want {
		t.Fatalf("declared tools = %s, want %s", got, want)
	}
}
