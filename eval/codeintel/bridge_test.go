package codeintel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBridge_Disabled_NoOp(t *testing.T) {
	b := NewBridge("", "tok", "")
	resp, err := b.Post(context.Background(), IngestRequest{})
	if err != nil {
		t.Fatalf("disabled bridge should not error: %v", err)
	}
	if resp != nil {
		t.Error("disabled bridge must not post")
	}
}

// TestBridge_DefaultPath_MatchesPlatformRoute locks the default ingest path to
// the route the platform lane actually shipped (/api/evals/ingest), not the
// earlier placeholder (/api/evals/runs/ingest).
func TestBridge_DefaultPath_MatchesPlatformRoute(t *testing.T) {
	if DefaultBridgePath != "/api/evals/ingest" {
		t.Errorf("DefaultBridgePath = %q, want /api/evals/ingest", DefaultBridgePath)
	}
	b := NewBridge("http://x", "tok", "")
	if b.Path != "/api/evals/ingest" {
		t.Errorf("empty --platform-path should default to /api/evals/ingest, got %q", b.Path)
	}
}

func TestBridge_Post_FlatIngestShapePathAndAuth(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusCreated)
		// Mirror the platform route's 201 body so Post can parse the runId.
		_, _ = w.Write([]byte(`{"runId":"evr_abc","traceId":"evt_abc","datasetId":"evd_ci","gradersRun":["structural/codeintel-task-v1","tool-use-correctness/codeintel-v1"],"gradeResults":[{"graderId":"structural/codeintel-task-v1","status":"scored","score":1,"pass":true},{"graderId":"tool-use-correctness/codeintel-v1","status":"scored","score":1,"pass":true}]}`))
	}))
	defer srv.Close()

	tr := Transcript{
		Arm:         ArmWith,
		FinalAnswer: "afcli/agent_run.go:80",
		ToolCalls:   []ToolCall{{Name: "mcp__af-code-intelligence__af_code_search_symbols", ResultText: "afcli/agent_run.go:79"}},
		TurnCount:   2,
		TokenCounts: TokenCounts{Input: 10, Output: 5},
	}
	req := BuildIngestRequest(sampleCase(), tr, 1, "disp-1", "evd_ci", "proj-1")

	// Empty --platform-path must resolve to the reconciled default route.
	b := NewBridge(srv.URL, "rsk_test", "")
	resp, err := b.Post(context.Background(), req)
	if err != nil {
		t.Fatalf("Post error: %v", err)
	}
	if resp == nil {
		t.Fatal("Post returned nil response on a 2xx")
	}
	if resp.RunID != "evr_abc" {
		t.Errorf("parsed runId = %q, want evr_abc", resp.RunID)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/api/evals/ingest" {
		t.Errorf("path = %s, want /api/evals/ingest", gotPath)
	}
	if gotAuth != "Bearer rsk_test" {
		t.Errorf("auth = %q, want Bearer rsk_test", gotAuth)
	}

	// The body must be the platform's FLAT per-trial ingest shape — NOT the old
	// {run, trace, meta} envelope.
	for _, k := range []string{"arm", "datasetId", "datasetCase", "outputPayload", "toolCalls", "turnCount", "tokenCounts"} {
		if _, ok := gotBody[k]; !ok {
			t.Errorf("posted body missing %q key", k)
		}
	}
	for _, k := range []string{"run", "trace", "meta", "graderIds", "experiment"} {
		if _, ok := gotBody[k]; ok {
			t.Errorf("legacy code-intel body must NOT carry key %q", k)
		}
	}
	if gotBody["arm"] != "with" {
		t.Errorf("arm = %v, want with", gotBody["arm"])
	}
	if gotBody["datasetId"] != "evd_ci" {
		t.Errorf("datasetId = %v, want evd_ci", gotBody["datasetId"])
	}
	dc, _ := gotBody["datasetCase"].(map[string]any)
	if dc["id"] != "codeintel-find-symbol-donmai-001" {
		t.Errorf("datasetCase.id = %v, want codeintel-find-symbol-donmai-001", dc["id"])
	}
	// datasetCase must carry the full input the platform graders grade against.
	if in, _ := dc["input"].(map[string]any); in["taskType"] != "find-symbol" {
		t.Errorf("datasetCase.input.taskType = %v, want find-symbol", in["taskType"])
	}
	if gotBody["outputPayload"] != "afcli/agent_run.go:80" {
		t.Errorf("outputPayload = %v", gotBody["outputPayload"])
	}
}

func TestBuildIngestRequest_PreservesResetPhasesAndActualCostSemantics(t *testing.T) {
	reset := combineResetTranscripts(
		Transcript{ToolCalls: []ToolCall{{Name: "Write"}}},
		Transcript{ToolCalls: []ToolCall{{Name: "Read"}}},
	)
	reset.CostUSD = 0
	reset.CostReported = true
	reset.CostComplete = true
	req := BuildIngestRequest(sampleCase(), reset, 1, "dispatch-reset", "evd_reset", "proj-1")
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got, ok := wire["actualCostUsd"]; !ok || got != float64(0) {
		t.Fatalf("explicit zero actual cost = %v present=%v, want present zero (wire=%s)", got, ok, body)
	}
	calls, _ := wire["toolCalls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("toolCalls = %#v", wire["toolCalls"])
	}
	for i, wantPhase := range []float64{1, 2} {
		call, _ := calls[i].(map[string]any)
		if call["phase"] != wantPhase {
			t.Errorf("tool call %d phase = %v, want %v", i, call["phase"], wantPhase)
		}
	}

	positive := BuildIngestRequest(sampleCase(), Transcript{CostUSD: 0.125, CostReported: true, CostComplete: true}, 1, "d", "evd", "p")
	positiveBody, err := json.Marshal(positive)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(positiveBody, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["actualCostUsd"] != 0.125 {
		t.Fatalf("positive actual cost = %v, want 0.125", wire["actualCostUsd"])
	}

	partial := BuildIngestRequest(sampleCase(), Transcript{CostUSD: 0.075, CostReported: true, CostComplete: false}, 1, "d", "evd", "p")
	partialBody, err := json.Marshal(partial)
	if err != nil {
		t.Fatal(err)
	}
	wire = nil
	if err := json.Unmarshal(partialBody, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["actualCostUsd"]; ok {
		t.Fatalf("partial provider cost must not be labeled actualCostUsd, got %s", partialBody)
	}

	missing := BuildIngestRequest(sampleCase(), Transcript{CostUSD: 9.99, CostReported: false}, 1, "d", "evd", "p")
	missingBody, err := json.Marshal(missing)
	if err != nil {
		t.Fatal(err)
	}
	wire = nil
	if err := json.Unmarshal(missingBody, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["actualCostUsd"]; ok {
		t.Fatalf("missing provider cost must stay absent, got %s", missingBody)
	}
}

func TestToolCallOrdinaryJSONOmitsResetPhase(t *testing.T) {
	body, err := json.Marshal(ToolCall{Name: "Read"})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["phase"]; ok {
		t.Fatalf("ordinary tool call unexpectedly carried phase: %s", body)
	}
}

func TestBridge_Post_PromptExperimentReceipt(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"runId":"evr_prompt","traceId":"evt_prompt","datasetId":"evd_prompt","gradersRun":["safety/injection-follow-v1"],"gradeResults":[{"graderId":"safety/injection-follow-v1","status":"scored","score":1,"pass":true}]}`))
	}))
	defer srv.Close()

	req := BuildIngestRequest(sampleCase(), Transcript{Arm: "candidate", FinalAnswer: "done"}, 2, "dispatch-2", "evd_prompt", "proj-1")
	req.GraderIDs = []string{"safety/injection-follow-v1"}
	req.Experiment = &ExperimentReceipt{
		ExperimentID: "injection-clause-v1",
		SubjectRef:   "agent/development",
		VariantRef:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	resp, err := NewBridge(srv.URL, "", "").Post(context.Background(), req)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if resp == nil || resp.RunID != "evr_prompt" {
		t.Fatalf("response = %+v", resp)
	}
	if gotBody["arm"] != "candidate" || gotBody["trialIndex"] != float64(2) {
		t.Fatalf("trial identity = arm:%v trial:%v", gotBody["arm"], gotBody["trialIndex"])
	}
	graders, _ := gotBody["graderIds"].([]any)
	if len(graders) != 1 || graders[0] != "safety/injection-follow-v1" {
		t.Fatalf("graderIds = %#v", gotBody["graderIds"])
	}
	experiment, _ := gotBody["experiment"].(map[string]any)
	if experiment["experimentId"] != "injection-clause-v1" || experiment["subjectRef"] != "agent/development" {
		t.Fatalf("experiment = %#v", experiment)
	}
	if _, ok := experiment["systemPrompt"]; ok {
		t.Fatal("raw system prompt must never enter the durable receipt")
	}
}

func TestBridge_Post_Malformed2xxErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>not the ingest route</html>"))
	}))
	defer srv.Close()
	b := NewBridge(srv.URL, "", "")
	if _, err := b.Post(context.Background(), IngestRequest{}); err == nil {
		t.Error("a malformed 2xx response must surface as an error")
	}
}

func TestBridge_Post_MissingGradeResultErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"runId":"evr_bad","traceId":"evt_bad","datasetId":"evd_bad","gradersRun":["safety/injection-follow-v1"],"gradeResults":[]}`))
	}))
	defer srv.Close()
	b := NewBridge(srv.URL, "", "")
	if _, err := b.Post(context.Background(), IngestRequest{}); err == nil {
		t.Error("a 2xx response without grader results must surface as an error")
	}
}

func TestBridge_Post_GraderErrorStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"runId":"evr_bad","traceId":"evt_bad","datasetId":"evd_bad","gradersRun":["safety/injection-follow-v1"],"gradeResults":[{"graderId":"safety/injection-follow-v1","status":"error","score":null,"pass":null}]}`))
	}))
	defer srv.Close()
	req := IngestRequest{GraderIDs: []string{"safety/injection-follow-v1"}}
	if _, err := NewBridge(srv.URL, "", "").Post(context.Background(), req); err == nil {
		t.Error("a grader error status must surface as an error")
	}
}

func TestBridge_Post_Non2xxErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	b := NewBridge(srv.URL, "", "")
	if _, err := b.Post(context.Background(), IngestRequest{}); err == nil {
		t.Error("a 5xx response must surface as an error")
	}
}
