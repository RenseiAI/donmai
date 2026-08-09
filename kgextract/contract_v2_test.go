package kgextract

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// platformV2Fixture is a REAL v2 kgExtractWork[] item, generated from the
// platform's own dispatcher (enqueueKgExtractBatch → the staged item, plus the
// resultAuth the poll route mints at claim time) and checked in verbatim. Only
// the tenant identifiers were replaced with same-shaped placeholders — this repo
// is public, and no closed-platform identifier belongs in it.
//
// It is the authority for what the executor must decode: the field set, the
// contractVersion, the `batch:kg_extract:stable:<projectId>:<hash>` id form, the
// stage-time `stagedAt` stamp the Go contract type has no field for, and the
// on-the-wire emit prompt + JSON-Schema.
const platformV2Fixture = "testdata/platform_v2_work_item.json"

// loadPlatformV2Item decodes the fixture into the contract type and swaps the
// placeholder result auth for a token claiming the fixture's org (the executor
// re-verifies that claim) and the endpoint for the caller's test server.
func loadPlatformV2Item(t *testing.T, resultEndpoint string) KgExtractWorkItem {
	t.Helper()
	raw, err := os.ReadFile(platformV2Fixture)
	if err != nil {
		t.Fatalf("read %s: %v", platformV2Fixture, err)
	}
	var item KgExtractWorkItem
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatalf("decode %s into KgExtractWorkItem: %v", platformV2Fixture, err)
	}
	item.ResultAuth = signedlessJWT(item.OrgID)
	item.ResultEndpoint = resultEndpoint
	return item
}

// TestPlatformV2Fixture_DecodesFaithfully pins the decode of the real wire item
// field-for-field. A platform-side contract move that the Go side does not
// follow fails HERE — in CI, on the change that causes it — instead of in a
// daemon log two months later.
func TestPlatformV2Fixture_DecodesFaithfully(t *testing.T) {
	t.Parallel()

	item := loadPlatformV2Item(t, "/api/factory/kg-extraction/results")

	if item.ContractVersion != KGExtractionContractVersion {
		t.Fatalf("fixture contractVersion = %d, worker speaks %d — the Go contract "+
			"has drifted from the platform's KG_EXTRACT_CONTRACT_VERSION",
			item.ContractVersion, KGExtractionContractVersion)
	}
	if item.WorkType != WorkTypeKGExtraction {
		t.Errorf("workType = %q, want %q", item.WorkType, WorkTypeKGExtraction)
	}
	if !strings.HasPrefix(item.BatchJobID, "batch:kg_extract:stable:") {
		t.Errorf("batchJobId = %q, want the stable dispatcher form", item.BatchJobID)
	}
	if item.OrgID == "" || item.ProjectID == "" {
		t.Errorf("tenant scope lost: orgId=%q projectId=%q", item.OrgID, item.ProjectID)
	}
	if item.AuthMode != AuthModeHostSession {
		t.Errorf("authMode = %q, want host-session", item.AuthMode)
	}
	if item.Provider != "claude" {
		t.Errorf("provider = %q, want claude", item.Provider)
	}
	if len(item.Observations) != 1 || item.Observations[0].ID == "" ||
		item.Observations[0].Content == "" {
		t.Errorf("observations lost content: %+v", item.Observations)
	}
	if item.ExtractionSystemPrompt == "" || item.TripleJSONSchema == nil {
		t.Fatal("the on-the-wire prompt/schema did not decode")
	}
	// v2 is defined by these three prompt/schema facts. Assert them against the
	// decoded item so a silent platform-side removal is caught here too.
	for _, want := range []string{"confidenceScore", "semantically_similar_to", "AMBIGUOUS"} {
		if !strings.Contains(item.ExtractionSystemPrompt, want) {
			t.Errorf("v2 emit prompt is missing %q", want)
		}
	}

	// Every node type the platform's JSON-Schema enumerates must be one this
	// worker accepts — a type the worker does not know is a node it silently
	// drops from the graph it posts back.
	for _, nt := range fixtureNodeTypeEnum(t, item.TripleJSONSchema) {
		if !isValidNodeType(NodeType(nt)) {
			t.Errorf("platform node type %q is not in the worker's closed set — "+
				"every node the model emits with that type would be dropped", nt)
		}
	}
}

// fixtureNodeTypeEnum reads properties.nodes.items.properties.type.enum out of
// the wire JSON-Schema.
func fixtureNodeTypeEnum(t *testing.T, schema map[string]any) []string {
	t.Helper()
	dig := func(m map[string]any, key string) map[string]any {
		next, ok := m[key].(map[string]any)
		if !ok {
			t.Fatalf("wire tripleJsonSchema has no %q object", key)
		}
		return next
	}
	typeSchema := dig(dig(dig(dig(dig(schema, "properties"), "nodes"), "items"), "properties"), "type")
	rawEnum, ok := typeSchema["enum"].([]any)
	if !ok || len(rawEnum) == 0 {
		t.Fatal("wire node type schema carries no enum")
	}
	out := make([]string, 0, len(rawEnum))
	for _, v := range rawEnum {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("node type enum member %v is not a string", v)
		}
		out = append(out, s)
	}
	return out
}

// v2GraphEmit is a model emit in the shape the v2 prompt asks for: the two node
// types v2 added, edge confidence labels + discrete scores, and a
// semantically_similar_to relation.
const v2GraphEmit = `{
  "nodes": [
    {"id":"n1","name":"PollService","type":"Service","description":"claims work"},
    {"id":"n2","name":"worktree-per-agent","type":"Convention","description":"team convention"},
    {"id":"n3","name":"direct-main-commit","type":"Deviation","description":"breaks the convention"}
  ],
  "edges": [
    {"sourceNodeId":"n1","targetNodeId":"n2","relationshipName":"follows",
     "confidence":"EXTRACTED","confidenceScore":1.0},
    {"sourceNodeId":"n2","targetNodeId":"n3","relationshipName":"semantically_similar_to",
     "confidence":"INFERRED","confidenceScore":0.75},
    {"sourceNodeId":"n3","targetNodeId":"n1","relationshipName":"touches",
     "confidence":"AMBIGUOUS","confidenceScore":0.2}
  ]
}`

// TestExecutor_PlatformV2WorkItem_Executes is the end-to-end red: the real
// platform item runs to a posted result, carrying the v2 node types and edge
// confidence through. Against the v1 worker this test cannot get past Handle's
// version gate at all.
func TestExecutor_PlatformV2WorkItem_Executes(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	var got KGExtractionResult
	var rawBody atomic.Value
	srv := rawCaptureServer(t, &hits, &got, &rawBody)
	defer srv.Close()

	item := loadPlatformV2Item(t, srv.URL)
	em := &stubEmitter{byContent: map[string]stubEmitResp{
		item.Observations[0].Content: {out: v2GraphEmit},
	}}
	exec := NewExecutor(Options{
		EmitterFactory: stubFactory(em),
		HTTPClient:     srv.Client(),
		Logger:         discardLogger(),
	})

	if err := exec.Handle(context.Background(), item); err != nil {
		t.Fatalf("Handle rejected the real platform v2 item: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("result POSTs = %d, want exactly 1", hits.Load())
	}
	if got.Status != StatusOK {
		t.Errorf("status = %q, want ok (error=%q)", got.Status, got.Error)
	}
	if got.BatchJobID != item.BatchJobID {
		t.Errorf("batchJobId = %q, want %q", got.BatchJobID, item.BatchJobID)
	}
	if got.ContractVersion != KGExtractionContractVersion {
		t.Errorf("contractVersion = %d, want %d", got.ContractVersion, KGExtractionContractVersion)
	}
	if len(got.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(got.Results))
	}

	// Assert against the POSTED BYTES, not the typed struct. Decoding into the
	// contract type would make a dropped field a compile error rather than a
	// failed assertion — and a compile error proves the test names a symbol, not
	// that the worker puts the value on the wire.
	graph := postedGraph(t, rawBody.Load())

	// v2 node types survive the worker's closed-set validation.
	nodeTypes := stringsAt(t, graph, "nodes", "type")
	wantNodeTypes := []string{"Service", "Convention", "Deviation"}
	if len(nodeTypes) != len(wantNodeTypes) {
		t.Fatalf("posted node types = %v, want %v (Convention/Deviation must not be dropped)",
			nodeTypes, wantNodeTypes)
	}
	for i, want := range wantNodeTypes {
		if nodeTypes[i] != want {
			t.Errorf("nodes[%d].type = %q, want %q", i, nodeTypes[i], want)
		}
	}

	// v2 edge confidence rides through the round trip, on the wire.
	edges := objectsAt(t, graph, "edges")
	if len(edges) != 3 {
		t.Fatalf("posted edges = %d, want 3", len(edges))
	}
	wantConfidence := []string{"EXTRACTED", "INFERRED", "AMBIGUOUS"}
	wantScore := []float64{1.0, 0.75, 0.2}
	for i, edge := range edges {
		if got, _ := edge["confidence"].(string); got != wantConfidence[i] {
			t.Errorf("posted edges[%d].confidence = %v, want %q — the v2 label was "+
				"dropped on the way back", i, edge["confidence"], wantConfidence[i])
		}
		score, ok := edge["confidenceScore"].(float64)
		if !ok {
			t.Errorf("posted edges[%d] carries no confidenceScore: %v", i, edge)
			continue
		}
		if score != wantScore[i] {
			t.Errorf("posted edges[%d].confidenceScore = %v, want %v", i, score, wantScore[i])
		}
	}
	if name, _ := edges[1]["relationshipName"].(string); name != "semantically_similar_to" {
		t.Errorf("posted edges[1].relationshipName = %v, want semantically_similar_to",
			edges[1]["relationshipName"])
	}
}

// rawCaptureServer records the posted body BYTES alongside the decoded result,
// so assertions can be made against what actually went over the wire.
func rawCaptureServer(t *testing.T, hits *atomic.Int32, got *KGExtractionResult, rawBody *atomic.Value) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		rawBody.Store(b)
		_ = json.Unmarshal(b, got)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ingested":true}`))
	}))
}

// postedGraph digs results[0].graph out of the raw posted result body.
func postedGraph(t *testing.T, raw any) map[string]any {
	t.Helper()
	body, ok := raw.([]byte)
	if !ok {
		t.Fatal("no result body was captured")
	}
	var decoded struct {
		Results []struct {
			Graph map[string]any `json:"graph"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode posted body: %v (%s)", err, body)
	}
	if len(decoded.Results) != 1 {
		t.Fatalf("posted results = %d, want 1: %s", len(decoded.Results), body)
	}
	return decoded.Results[0].Graph
}

// objectsAt reads graph[key] as a list of JSON objects.
func objectsAt(t *testing.T, graph map[string]any, key string) []map[string]any {
	t.Helper()
	list, ok := graph[key].([]any)
	if !ok {
		t.Fatalf("posted graph has no %q array: %v", key, graph)
	}
	out := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		obj, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("posted %s entry is not an object: %v", key, entry)
		}
		out = append(out, obj)
	}
	return out
}

// stringsAt reads one string field off every object in graph[key].
func stringsAt(t *testing.T, graph map[string]any, key, field string) []string {
	t.Helper()
	objs := objectsAt(t, graph, key)
	out := make([]string, 0, len(objs))
	for _, obj := range objs {
		s, _ := obj[field].(string)
		out = append(out, s)
	}
	return out
}

// TestExtractedEdge_V1ShapeOmitsV2Fields proves the addition is additive on the
// wire: an edge with no confidence serializes exactly as it did under v1, so the
// platform's optional fields stay absent rather than becoming empty strings.
func TestExtractedEdge_V1ShapeOmitsV2Fields(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(ExtractedEdge{
		SourceNodeID: "a", TargetNodeID: "b", RelationshipName: "calls",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "confidence") {
		t.Errorf("v1-shaped edge serialized a confidence field: %s", raw)
	}
}

// TestExecutor_ContractVersionRejection_PostsTerminalFailure is the
// future-proofing red: a version this worker cannot speak must become a VISIBLE
// failed row, not a silently destroyed item. Before this behaviour existed the
// executor returned an error and POSTed nothing — so the platform's FSM row sat
// 'pending' forever while the claim key suppressed every re-stage for an hour.
func TestExecutor_ContractVersionRejection_PostsTerminalFailure(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	var gotAuth string
	var got KGExtractionResult
	srv := captureServer(t, &hits, &gotAuth, &got)
	defer srv.Close()

	emitterBuilt := false
	exec := NewExecutor(Options{
		EmitterFactory: func(context.Context, KgExtractWorkItem) (Emitter, error) {
			emitterBuilt = true
			return &stubEmitter{}, nil
		},
		HTTPClient: srv.Client(),
		Logger:     discardLogger(),
	})

	item := loadPlatformV2Item(t, srv.URL)
	item.ContractVersion = KGExtractionContractVersion + 1 // a future platform

	err := exec.Handle(context.Background(), item)
	if err == nil || !strings.Contains(err.Error(), "contract version") {
		t.Fatalf("err = %v, want a contract-version rejection", err)
	}
	if emitterBuilt {
		t.Error("emitter built despite the version rejection — the gate must reject first")
	}
	if hits.Load() != 1 {
		t.Fatalf("terminal-failure POSTs = %d, want exactly 1 — a rejected item must "+
			"become a visible failed row", hits.Load())
	}
	if gotAuth != "Bearer "+item.ResultAuth {
		t.Errorf("auth = %q, want the item's resultAuth as bearer", gotAuth)
	}
	if got.Status != StatusError {
		t.Errorf("status = %q, want error", got.Status)
	}
	if got.BatchJobID != item.BatchJobID {
		t.Errorf("batchJobId = %q, want %q", got.BatchJobID, item.BatchJobID)
	}
	// The ITEM's version is echoed, not the worker's: the platform pins
	// contractVersion with a literal, so the worker's version would 400 and the
	// failure would stay just as invisible as posting nothing.
	if got.ContractVersion != item.ContractVersion {
		t.Errorf("contractVersion = %d, want the item's %d so ingestion accepts the "+
			"failure notice", got.ContractVersion, item.ContractVersion)
	}
	if len(got.Results) != 0 {
		t.Errorf("results = %+v, want empty (a rejection carries no graph)", got.Results)
	}
	if !strings.Contains(got.Error, "unsupported contract version") {
		t.Errorf("error = %q, want it to name the version mismatch", got.Error)
	}
	// Results must still serialize as [] (never null) per the contract.
	rawPosted, _ := json.Marshal(got)
	if !strings.Contains(string(rawPosted), `"results":[]`) {
		t.Errorf("results did not serialize as []: %s", rawPosted)
	}
}

// TestExecutor_OrgMismatch_PostsNothing pins the deliberate asymmetry: the
// cross-tenant guard fires precisely because the item's resultAuth does not
// claim the item's org, so the executor must NOT turn around and POST with that
// suspect token.
func TestExecutor_OrgMismatch_PostsNothing(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	var gotAuth string
	var got KGExtractionResult
	srv := captureServer(t, &hits, &gotAuth, &got)
	defer srv.Close()

	exec := NewExecutor(Options{
		EmitterFactory: stubFactory(&stubEmitter{}),
		HTTPClient:     srv.Client(),
		Logger:         discardLogger(),
	})
	item := loadPlatformV2Item(t, srv.URL)
	item.ResultAuth = signedlessJWT("org-somebody-else")

	if err := exec.Handle(context.Background(), item); err == nil {
		t.Fatal("expected an org-claim rejection")
	}
	if hits.Load() != 0 {
		t.Errorf("POSTs = %d, want 0 — a cross-tenant rejection must not use the "+
			"suspect token", hits.Load())
	}
}
