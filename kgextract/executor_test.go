package kgextract

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// ── test doubles ─────────────────────────────────────────────────────────────

// stubEmitter is a table-driven Emitter: it returns a queued response per call
// in order, or a fixed response/err when set. Records every call for assertions.
type stubEmitter struct {
	// byContent keys the response on the userContent argument. When a content key
	// is present it wins over the sequence.
	byContent map[string]stubEmitResp
	// seq is consumed in order for any content not in byContent.
	seq  []stubEmitResp
	idx  int
	mu   atomicCounter
	last string
}

type stubEmitResp struct {
	out string
	err error
}

// atomicCounter is a tiny race-safe call counter.
type atomicCounter struct{ n atomic.Int32 }

func (e *stubEmitter) Emit(_ context.Context, _ /*systemPrompt*/, userContent string) (string, error) {
	e.mu.n.Add(1)
	e.last = userContent
	if r, ok := e.byContent[userContent]; ok {
		return r.out, r.err
	}
	if e.idx < len(e.seq) {
		r := e.seq[e.idx]
		e.idx++
		return r.out, r.err
	}
	return "", errors.New("stubEmitter: no response queued")
}

// stubFactory returns a factory that always yields the given emitter.
func stubFactory(em Emitter) EmitterFactory {
	return func(_ context.Context, _ KgExtractWorkItem) (Emitter, error) { return em, nil }
}

// failingFactory returns a factory that always fails to build an emitter.
func failingFactory(err error) EmitterFactory {
	return func(_ context.Context, _ KgExtractWorkItem) (Emitter, error) { return nil, err }
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// signedlessJWT builds a 3-segment token whose payload carries org_id=org. The
// signature segment is a placeholder — the worker re-verifies the CLAIM only.
func signedlessJWT(org string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payloadJSON, _ := json.Marshal(map[string]string{"org_id": org, "sub": "worker-1"})
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	return header + "." + payload + ".sig"
}

// captureServer records the last posted body + auth header into the provided
// pointers and acks 200.
func captureServer(t *testing.T, hits *atomic.Int32, gotAuth *string, gotResult *KGExtractionResult) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		*gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = json.Unmarshal(b, gotResult)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ingested":true}`))
	}))
}

// fixtureItem returns a valid kg-extraction item targeting resultEndpoint with
// org "org-1" and the given observations.
func fixtureItem(resultEndpoint string, obs ...Observation) KgExtractWorkItem {
	org := "org-1"
	return KgExtractWorkItem{
		BatchJobID:             "batch:kg_extract:1",
		WorkType:               WorkTypeKGExtraction,
		ContractVersion:        KGExtractionContractVersion,
		OrgID:                  org,
		ProjectID:              "proj-1",
		AuthMode:               AuthModeHostSession,
		Provider:               "claude",
		Observations:           obs,
		ExtractionSystemPrompt: "Emit ONLY {nodes,edges} JSON.",
		TripleJSONSchema:       map[string]any{"type": "object"},
		ResultEndpoint:         resultEndpoint,
		ResultAuth:             signedlessJWT(org),
	}
}

// goodGraphJSON is a valid emit with one node + one edge.
const goodGraphJSON = `{
  "nodes": [
    {"id":"n1","name":"AuthService","type":"Service","description":"handles auth"},
    {"id":"n2","name":"users","type":"Database","description":"user table"}
  ],
  "edges": [
    {"sourceNodeId":"n1","targetNodeId":"n2","relationshipName":"reads_from"}
  ]
}`

// ── tests ────────────────────────────────────────────────────────────────────

func TestExecutor_ValidEmit_OK(t *testing.T) {
	em := &stubEmitter{byContent: map[string]stubEmitResp{
		"content-A": {out: goodGraphJSON},
	}}

	var hits atomic.Int32
	var gotAuth string
	var got KGExtractionResult
	srv := captureServer(t, &hits, &gotAuth, &got)
	defer srv.Close()

	exec := NewExecutor(Options{
		EmitterFactory: stubFactory(em),
		HTTPClient:     srv.Client(),
		Logger:         discardLogger(),
		WorkerVersion:  "v0.1.0-test",
	})
	item := fixtureItem(srv.URL, Observation{ID: "obs-A", Type: "code", Content: "content-A"})

	if err := exec.Handle(context.Background(), item); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected exactly 1 result POST, got %d", hits.Load())
	}
	if gotAuth != "Bearer "+item.ResultAuth {
		t.Errorf("auth = %q, want bearer of resultAuth", gotAuth)
	}
	if got.Status != StatusOK {
		t.Errorf("status = %q, want ok", got.Status)
	}
	if got.BatchJobID != item.BatchJobID {
		t.Errorf("batchJobId = %q, want %q", got.BatchJobID, item.BatchJobID)
	}
	if got.ContractVersion != KGExtractionContractVersion {
		t.Errorf("contractVersion = %d", got.ContractVersion)
	}
	if len(got.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(got.Results))
	}
	entry := got.Results[0]
	if entry.ObservationID != "obs-A" {
		t.Errorf("observationId = %q, want obs-A", entry.ObservationID)
	}
	if len(entry.Graph.Nodes) != 2 || len(entry.Graph.Edges) != 1 {
		t.Errorf("graph nodes/edges = %d/%d, want 2/1", len(entry.Graph.Nodes), len(entry.Graph.Edges))
	}
	if entry.Graph.Nodes[0].Type != NodeTypeService {
		t.Errorf("node[0].type = %q, want Service", entry.Graph.Nodes[0].Type)
	}
}

func TestExecutor_InvalidNodesDropped(t *testing.T) {
	// An emit mixing a valid node, an out-of-set type, a missing id, and a
	// fenced wrapper. The valid node survives; the invalid ones are dropped.
	emit := "```json\n" + `{
      "nodes": [
        {"id":"n1","name":"Cache","type":"Module","description":"lru cache"},
        {"id":"n2","name":"Bad","type":"Spaceship","description":"not a real type"},
        {"id":"","name":"NoID","type":"Service","description":"missing id"}
      ],
      "edges": [
        {"sourceNodeId":"n1","targetNodeId":"n2","relationshipName":"uses"},
        {"sourceNodeId":"n1","targetNodeId":"","relationshipName":"broken"}
      ]
    }` + "\n```"
	em := &stubEmitter{byContent: map[string]stubEmitResp{"c": {out: emit}}}

	var hits atomic.Int32
	var gotAuth string
	var got KGExtractionResult
	srv := captureServer(t, &hits, &gotAuth, &got)
	defer srv.Close()

	exec := NewExecutor(Options{EmitterFactory: stubFactory(em), HTTPClient: srv.Client(), Logger: discardLogger()})
	item := fixtureItem(srv.URL, Observation{ID: "obs-1", Content: "c"})

	if err := exec.Handle(context.Background(), item); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got.Status != StatusOK {
		t.Errorf("status = %q, want ok (a parseable emit with dropped triples is still ok)", got.Status)
	}
	if len(got.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(got.Results))
	}
	g := got.Results[0].Graph
	if len(g.Nodes) != 1 || g.Nodes[0].ID != "n1" {
		t.Errorf("nodes = %+v, want only the valid n1", g.Nodes)
	}
	// Edge to dropped node n2 is still wire-valid (non-empty fields) and kept;
	// the empty-target edge is dropped. The platform re-validates referential
	// integrity, so the worker keeps structurally-valid edges.
	if len(g.Edges) != 1 || g.Edges[0].RelationshipName != "uses" {
		t.Errorf("edges = %+v, want only the structurally-valid edge", g.Edges)
	}
}

func TestExecutor_PartialWhenSomeObservationsFail(t *testing.T) {
	em := &stubEmitter{byContent: map[string]stubEmitResp{
		"ok-content":  {out: goodGraphJSON},
		"bad-content": {err: errors.New("provider exploded")},
	}}

	var hits atomic.Int32
	var gotAuth string
	var got KGExtractionResult
	srv := captureServer(t, &hits, &gotAuth, &got)
	defer srv.Close()

	exec := NewExecutor(Options{EmitterFactory: stubFactory(em), HTTPClient: srv.Client(), Logger: discardLogger()})
	item := fixtureItem(srv.URL,
		Observation{ID: "obs-ok", Content: "ok-content"},
		Observation{ID: "obs-bad", Content: "bad-content"},
	)

	if err := exec.Handle(context.Background(), item); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got.Status != StatusPartial {
		t.Errorf("status = %q, want partial", got.Status)
	}
	if len(got.Results) != 1 || got.Results[0].ObservationID != "obs-ok" {
		t.Errorf("results = %+v, want only obs-ok", got.Results)
	}
	if got.Error == "" {
		t.Error("partial result should carry an error summary")
	}
}

func TestExecutor_ErrorWhenAllObservationsFail(t *testing.T) {
	em := &stubEmitter{seq: []stubEmitResp{
		{err: errors.New("boom1")},
		{out: "this is not json at all, no braces here"},
	}}

	var hits atomic.Int32
	var gotAuth string
	var got KGExtractionResult
	srv := captureServer(t, &hits, &gotAuth, &got)
	defer srv.Close()

	exec := NewExecutor(Options{EmitterFactory: stubFactory(em), HTTPClient: srv.Client(), Logger: discardLogger()})
	item := fixtureItem(srv.URL,
		Observation{ID: "o1", Content: "a"},
		Observation{ID: "o2", Content: "b"},
	)

	if err := exec.Handle(context.Background(), item); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got.Status != StatusError {
		t.Errorf("status = %q, want error", got.Status)
	}
	if len(got.Results) != 0 {
		t.Errorf("results = %+v, want empty", got.Results)
	}
	// Results must serialize as [] (never null) per the contract.
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), `"results":[]`) {
		t.Errorf("results did not serialize as []: %s", raw)
	}
}

func TestExecutor_NoEmitterFactory_ReportsError(t *testing.T) {
	var hits atomic.Int32
	var gotAuth string
	var got KGExtractionResult
	srv := captureServer(t, &hits, &gotAuth, &got)
	defer srv.Close()

	// No EmitterFactory configured → executor cannot run any emit.
	exec := NewExecutor(Options{HTTPClient: srv.Client(), Logger: discardLogger()})
	item := fixtureItem(srv.URL, Observation{ID: "o1", Content: "a"})

	if err := exec.Handle(context.Background(), item); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got.Status != StatusError {
		t.Errorf("status = %q, want error", got.Status)
	}
	if got.Error == "" {
		t.Error("missing-emitter error should carry a summary")
	}
}

func TestExecutor_EmitterFactoryFailure_ReportsError(t *testing.T) {
	var hits atomic.Int32
	var gotAuth string
	var got KGExtractionResult
	srv := captureServer(t, &hits, &gotAuth, &got)
	defer srv.Close()

	exec := NewExecutor(Options{
		EmitterFactory: failingFactory(errors.New("claude CLI not on PATH")),
		HTTPClient:     srv.Client(),
		Logger:         discardLogger(),
	})
	item := fixtureItem(srv.URL, Observation{ID: "o1", Content: "a"})

	if err := exec.Handle(context.Background(), item); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected exactly 1 result POST even on emitter failure, got %d", hits.Load())
	}
	if got.Status != StatusError {
		t.Errorf("status = %q, want error", got.Status)
	}
	if !strings.Contains(got.Error, "emitter unavailable") {
		t.Errorf("error = %q, want it to mention emitter unavailable", got.Error)
	}
}

func TestExecutor_NoObservations_OK(t *testing.T) {
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
	item := fixtureItem(srv.URL) // no observations

	if err := exec.Handle(context.Background(), item); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got.Status != StatusOK {
		t.Errorf("status = %q, want ok for an empty observation batch", got.Status)
	}
	if len(got.Results) != 0 {
		t.Errorf("results = %+v, want empty", got.Results)
	}
}

func TestExecutor_OrgMismatchRejected(t *testing.T) {
	var hits atomic.Int32
	var gotAuth string
	var got KGExtractionResult
	srv := captureServer(t, &hits, &gotAuth, &got)
	defer srv.Close()

	emitterBuilt := false
	exec := NewExecutor(Options{
		EmitterFactory: func(_ context.Context, _ KgExtractWorkItem) (Emitter, error) {
			emitterBuilt = true // building an emitter means we got past the org guard
			return &stubEmitter{}, nil
		},
		HTTPClient: srv.Client(),
		Logger:     discardLogger(),
	})
	item := fixtureItem(srv.URL, Observation{ID: "o1", Content: "a"})
	item.ResultAuth = signedlessJWT("EVIL-ORG") // claim != item.OrgID ("org-1")

	err := exec.Handle(context.Background(), item)
	if err == nil {
		t.Fatal("expected rejection error for org mismatch")
	}
	if !errors.Is(err, ErrOrgClaimMismatch) {
		t.Errorf("err = %v, want ErrOrgClaimMismatch", err)
	}
	if emitterBuilt {
		t.Error("emitter built despite org mismatch — guard must reject before any emit")
	}
	if hits.Load() != 0 {
		t.Errorf("posted %d results despite rejection, want 0", hits.Load())
	}
}

func TestExecutor_UnknownContractVersion_Rejected(t *testing.T) {
	exec := NewExecutor(Options{EmitterFactory: stubFactory(&stubEmitter{}), Logger: discardLogger()})
	item := fixtureItem("", Observation{ID: "o1", Content: "a"})
	item.ContractVersion = 999
	err := exec.Handle(context.Background(), item)
	if err == nil || !strings.Contains(err.Error(), "contract version") {
		t.Errorf("err = %v, want contract-version rejection", err)
	}
}

func TestExecutor_PostResolvesRelativeEndpoint(t *testing.T) {
	// When the item carries a PATH (not an absolute URL), the executor prefixes
	// it with PlatformBaseURL.
	em := &stubEmitter{byContent: map[string]stubEmitResp{"x": {out: goodGraphJSON}}}

	var hits atomic.Int32
	var got KGExtractionResult
	var gotPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotPath.Store(r.URL.Path)
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = json.Unmarshal(b, &got)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	exec := NewExecutor(Options{
		EmitterFactory:  stubFactory(em),
		HTTPClient:      srv.Client(),
		Logger:          discardLogger(),
		PlatformBaseURL: srv.URL + "/", // trailing slash trimmed by NewExecutor
	})
	item := fixtureItem("/api/factory/kg-extraction/results", Observation{ID: "o1", Content: "x"})

	if err := exec.Handle(context.Background(), item); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 1 POST, got %d", hits.Load())
	}
	if p, _ := gotPath.Load().(string); p != "/api/factory/kg-extraction/results" {
		t.Errorf("posted path = %q, want /api/factory/kg-extraction/results", p)
	}
	if got.Status != StatusOK {
		t.Errorf("status = %q, want ok", got.Status)
	}
}
