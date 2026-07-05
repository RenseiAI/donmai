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
	posted, err := b.Post(context.Background(), ReportEnvelope{})
	if err != nil {
		t.Fatalf("disabled bridge should not error: %v", err)
	}
	if posted {
		t.Error("disabled bridge must not post")
	}
}

func TestBridge_Post_ShapePathAndAuth(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	b := NewBridge(srv.URL, "rsk_test", "/api/evals/runs/ingest")
	env, err := BuildEnvelope("run-1", "trace-1", "disp-1", "org-1", "proj-1", "ds-1", sampleCase(),
		Transcript{Arm: ArmWith, FinalAnswer: "afcli/agent_run.go:80", TokenCounts: TokenCounts{Input: 10, Output: 5}},
		[]GradeResult{{GraderID: GraderFindSymbol, Score: 1, Pass: true}},
		ReportMeta{CaseID: "codeintel-find-symbol-donmai-001", Arm: ArmWith, Family: "find-symbol", Repo: "RenseiAI/donmai", Trial: 1, Advertisement: "mcp"})
	if err != nil {
		t.Fatal(err)
	}

	posted, err := b.Post(context.Background(), env)
	if err != nil || !posted {
		t.Fatalf("Post = (%v, %v), want (true, nil)", posted, err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/api/evals/runs/ingest" {
		t.Errorf("path = %s", gotPath)
	}
	if gotAuth != "Bearer rsk_test" {
		t.Errorf("auth = %q, want Bearer rsk_test", gotAuth)
	}
	// The body must carry the run + trace + meta the platform ingests.
	for _, k := range []string{"run", "trace", "meta"} {
		if _, ok := gotBody[k]; !ok {
			t.Errorf("posted body missing %q key", k)
		}
	}
	run, _ := gotBody["run"].(map[string]any)
	if run["orgId"] != "org-1" || run["datasetCaseId"] != "codeintel-find-symbol-donmai-001" {
		t.Errorf("run row shape wrong: %v", run)
	}
}

func TestBridge_Post_Non2xxErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	b := NewBridge(srv.URL, "", "")
	if _, err := b.Post(context.Background(), ReportEnvelope{}); err == nil {
		t.Error("a 5xx response must surface as an error")
	}
}
