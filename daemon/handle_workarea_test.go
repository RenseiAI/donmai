package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// newServerForWorkareaTest constructs a Server suitable for httptest
// dispatch — the daemon is the bare minimum (state Running, config
// loaded) to satisfy registry construction. The mux is fresh per test
// so registration is isolated. The returned httptest.Server is auto-
// closed at test end; the URL is keyed in serverByURL so tests can
// retrieve the underlying *Server via serverFromTest.
func newServerForWorkareaTest(t *testing.T, archiveRoot string, threshold int) *httptest.Server {
	t.Helper()
	hsrv, _ := newServerForWorkareaTestRegistered(t, archiveRoot, threshold)
	return hsrv
}

func decodeBody(t *testing.T, resp *http.Response, target any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}

// ── List ──────────────────────────────────────────────────────────────────

func TestHandleWorkareas_List_EmptyRoot(t *testing.T) {
	root := t.TempDir() + "/never-existed"
	hsrv := newServerForWorkareaTest(t, root, 0)
	resp, err := http.Get(hsrv.URL + "/api/daemon/workareas")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: want 200, got %d", resp.StatusCode)
	}
	var body afclient.ListWorkareasResponse
	decodeBody(t, resp, &body)
	if len(body.Active) != 0 || len(body.Archived) != 0 {
		t.Errorf("expected empty lists, got %+v", body)
	}
}

func TestHandleWorkareas_List_PopulatedArchives(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{
		id:       "wa-listed-1",
		manifest: archiveManifest{SessionID: "sess-A"},
	})
	writeFixtureArchive(t, root, fixtureArchive{
		id:       "wa-listed-2",
		manifest: archiveManifest{SessionID: "sess-B"},
	})
	hsrv := newServerForWorkareaTest(t, root, 0)
	resp, _ := http.Get(hsrv.URL + "/api/daemon/workareas")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var body afclient.ListWorkareasResponse
	decodeBody(t, resp, &body)
	if len(body.Archived) != 2 {
		t.Errorf("expected 2 archives, got %d", len(body.Archived))
	}
}

// ── Inspect ───────────────────────────────────────────────────────────────

func TestHandleWorkareas_Inspect_Archived(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{
		id: "wa-inspect",
		manifest: archiveManifest{
			SessionID:  "sess-x",
			Repository: "github.com/acme/repo",
			Toolchain:  map[string]string{"node": "20"},
		},
	})
	hsrv := newServerForWorkareaTest(t, root, 0)
	resp, _ := http.Get(hsrv.URL + "/api/daemon/workareas/wa-inspect")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var env afclient.WorkareaEnvelope
	decodeBody(t, resp, &env)
	if env.Workarea.Kind != afclient.WorkareaKindArchived {
		t.Errorf("kind want archived, got %q", env.Workarea.Kind)
	}
	if env.Workarea.SessionID != "sess-x" {
		t.Errorf("sessionId not propagated: %q", env.Workarea.SessionID)
	}
	if env.Workarea.Toolchain["node"] != "20" {
		t.Errorf("toolchain not propagated: %+v", env.Workarea.Toolchain)
	}
}

func TestHandleWorkareas_Inspect_NotFound(t *testing.T) {
	root := t.TempDir()
	hsrv := newServerForWorkareaTest(t, root, 0)
	resp, _ := http.Get(hsrv.URL + "/api/daemon/workareas/ghost")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: want 404, got %d", resp.StatusCode)
	}
}

func TestHandleWorkareas_Inspect_BadRequestOnCorrupted(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "wa-broken")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{bad"), 0o600)
	hsrv := newServerForWorkareaTest(t, root, 0)
	resp, _ := http.Get(hsrv.URL + "/api/daemon/workareas/wa-broken")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: want 400, got %d", resp.StatusCode)
	}
}

// ── Restore (end-to-end) ──────────────────────────────────────────────────

func TestHandleWorkareas_Restore_EndToEnd(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{
		id: "wa-e2e",
		manifest: archiveManifest{
			SessionID: "sess-original",
		},
		tree: map[string]string{"hello.txt": "world"},
	})
	hsrv := newServerForWorkareaTest(t, root, 0)

	body := bytes.NewBufferString(`{"reason":"investigation"}`)
	resp, err := http.Post(hsrv.URL+"/api/daemon/workareas/wa-e2e/restore",
		"application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("restore: want 201, got %d: %s", resp.StatusCode, buf)
	}
	var result afclient.WorkareaRestoreResult
	decodeBody(t, resp, &result)
	if result.Workarea.Kind != afclient.WorkareaKindActive {
		t.Errorf("restored kind: want active, got %q", result.Workarea.Kind)
	}
	if result.Workarea.ID == "wa-e2e" {
		t.Errorf("restored id should differ from archive id; got %q", result.Workarea.ID)
	}

	// Follow-up GET-by-new-id should return kind:"active". Note: the
	// inspect handler asks the active provider first; when there's no
	// active provider wired, it falls through to the archive registry,
	// which won't know about the restore output. So we don't assert the
	// follow-up GET succeeds here — that's a wire-up concern handled in
	// the dedicated test below with an active provider in place.
}

func TestHandleWorkareas_Restore_GetByNewIdViaActiveProvider(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "wa-active-test"})
	hsrv := newServerForWorkareaTest(t, root, 0)

	resp, err := http.Post(hsrv.URL+"/api/daemon/workareas/wa-active-test/restore",
		"application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var result afclient.WorkareaRestoreResult
	decodeBody(t, resp, &result)
	newID := result.Workarea.ID

	// Inject the restored member into the active provider so the
	// inspect handler returns it as kind=active.
	srv := serverFromTest(t, hsrv)
	srv.daemon.SetWorkareaArchiveRegistry(NewWorkareaArchiveRegistry(WorkareaArchiveOptions{
		Root: root,
		ActiveProvider: &fakeActiveProvider{members: []afclient.WorkareaSummary{{
			ID:     newID,
			Kind:   afclient.WorkareaKindActive,
			Status: afclient.WorkareaStatusReady,
		}}},
	}))

	resp2, err := http.Get(hsrv.URL + "/api/daemon/workareas/" + newID)
	if err != nil {
		t.Fatal(err)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("inspect: want 200, got %d", resp2.StatusCode)
	}
	var env afclient.WorkareaEnvelope
	decodeBody(t, resp2, &env)
	if env.Workarea.Kind != afclient.WorkareaKindActive {
		t.Errorf("expected kind=active for restored workarea, got %q", env.Workarea.Kind)
	}
}

func TestHandleWorkareas_Restore_ConflictOnDuplicateSession(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "wa-conf"})
	hsrv := newServerForWorkareaTest(t, root, 0)
	body := strings.NewReader(`{"intoSessionId":"sess-collide"}`)
	resp, _ := http.Post(hsrv.URL+"/api/daemon/workareas/wa-conf/restore", "application/json", body)
	if resp.StatusCode != http.StatusCreated {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("first restore: want 201, got %d: %s", resp.StatusCode, buf)
	}
	body2 := strings.NewReader(`{"intoSessionId":"sess-collide"}`)
	resp2, _ := http.Post(hsrv.URL+"/api/daemon/workareas/wa-conf/restore", "application/json", body2)
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("second restore: want 409, got %d", resp2.StatusCode)
	}
}

func TestHandleWorkareas_Restore_503OnSaturation(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "wa-busy"})
	d := New(Options{HTTPHost: "127.0.0.1", HTTPPort: 0})
	d.mu.Lock()
	d.config = &Config{Workarea: WorkareaConfig{ArchiveRoot: root}}
	d.mu.Unlock()
	d.state.Store(StateRunning)
	d.SetWorkareaArchiveRegistry(NewWorkareaArchiveRegistry(WorkareaArchiveOptions{
		Root:      root,
		PoolGuard: &fakePoolGuard{retryAfter: 17 * time.Second},
	}))
	srv := &Server{daemon: d}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/daemon/workareas", srv.handleWorkareasRoot)
	mux.HandleFunc("/api/daemon/workareas/", srv.handleWorkareaItem)
	hsrv := httptest.NewServer(mux)
	t.Cleanup(hsrv.Close)

	resp, _ := http.Post(hsrv.URL+"/api/daemon/workareas/wa-busy/restore",
		"application/json", strings.NewReader(`{}`))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: want 503, got %d", resp.StatusCode)
	}
	got := resp.Header.Get("Retry-After")
	want := strconv.Itoa(17)
	if got != want {
		t.Errorf("Retry-After: want %q, got %q", want, got)
	}
}

func TestHandleWorkareas_Restore_400OnCorrupted(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "wa-rotten")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{bad"), 0o600)
	hsrv := newServerForWorkareaTest(t, root, 0)
	resp, _ := http.Post(hsrv.URL+"/api/daemon/workareas/wa-rotten/restore",
		"application/json", strings.NewReader(`{}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: want 400, got %d", resp.StatusCode)
	}
}

func TestHandleWorkareas_Restore_NotFound(t *testing.T) {
	root := t.TempDir()
	hsrv := newServerForWorkareaTest(t, root, 0)
	resp, _ := http.Post(hsrv.URL+"/api/daemon/workareas/ghost/restore",
		"application/json", strings.NewReader(`{}`))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: want 404, got %d", resp.StatusCode)
	}
}

// ── Diff (JSON path) ─────────────────────────────────────────────────────

func TestHandleWorkareas_Diff_JSONPath(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "wa-d-a", tree: map[string]string{
		"shared.txt":   "v1",
		"only-in-a.go": "package a",
	}})
	writeFixtureArchive(t, root, fixtureArchive{id: "wa-d-b", tree: map[string]string{
		"shared.txt":   "v2",
		"only-in-b.go": "package b",
	}})
	hsrv := newServerForWorkareaTest(t, root, 1000) // threshold above entry count

	resp, err := http.Get(hsrv.URL + "/api/daemon/workareas/wa-d-a/diff/wa-d-b")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type: want application/json, got %q", ct)
	}
	var env afclient.WorkareaDiffEnvelope
	decodeBody(t, resp, &env)
	if env.Diff.Summary.Total != 3 {
		t.Errorf("summary total mismatch: %+v", env.Diff.Summary)
	}
	if env.Diff.Summary.Added != 1 || env.Diff.Summary.Removed != 1 || env.Diff.Summary.Modified != 1 {
		t.Errorf("summary breakdown mismatch: %+v", env.Diff.Summary)
	}
}

// ── Diff (NDJSON streaming path) ──────────────────────────────────────────

func TestHandleWorkareas_Diff_NDJSONPath(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	files2 := map[string]string{}
	for i := 0; i < 30; i++ {
		files[fmt.Sprintf("f-%03d.txt", i)] = "old"
		files2[fmt.Sprintf("f-%03d.txt", i)] = "new" // each one modified
	}
	writeFixtureArchive(t, root, fixtureArchive{id: "ndj-a", tree: files})
	writeFixtureArchive(t, root, fixtureArchive{id: "ndj-b", tree: files2})

	// Threshold of 5 forces NDJSON for 30 entries.
	hsrv := newServerForWorkareaTest(t, root, 5)

	resp, err := http.Get(hsrv.URL + "/api/daemon/workareas/ndj-a/diff/ndj-b")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-ndjson") {
		t.Errorf("content-type: want application/x-ndjson, got %q", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 31 { // 30 entries + 1 summary
		t.Errorf("expected 31 lines (30 entries + summary), got %d", len(lines))
	}
	// First 30 lines: entries.
	for i := 0; i < 30; i++ {
		var entry afclient.WorkareaDiffEntry
		if err := json.Unmarshal([]byte(lines[i]), &entry); err != nil {
			t.Errorf("line %d not entry JSON: %v", i, err)
			continue
		}
		if entry.Status != afclient.WorkareaDiffStatusModified {
			t.Errorf("line %d status: %q", i, entry.Status)
		}
	}
	// Last line: summary.
	var tail struct {
		Summary afclient.WorkareaDiffSummary `json:"summary"`
	}
	if err := json.Unmarshal([]byte(lines[30]), &tail); err != nil {
		t.Fatalf("trailing summary not parseable: %v", err)
	}
	if tail.Summary.Modified != 30 {
		t.Errorf("summary.Modified mismatch: %d", tail.Summary.Modified)
	}
}

func TestHandleWorkareas_Diff_SelfReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "self", tree: map[string]string{"x": "1"}})
	hsrv := newServerForWorkareaTest(t, root, 0)
	resp, _ := http.Get(hsrv.URL + "/api/daemon/workareas/self/diff/self")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type: want application/json, got %q", ct)
	}
	var env afclient.WorkareaDiffEnvelope
	decodeBody(t, resp, &env)
	if len(env.Diff.Entries) != 0 {
		t.Errorf("self-diff should be empty, got %+v", env.Diff.Entries)
	}
}

func TestHandleWorkareas_Diff_MissingArchive(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "exists"})
	hsrv := newServerForWorkareaTest(t, root, 0)
	resp, _ := http.Get(hsrv.URL + "/api/daemon/workareas/exists/diff/missing")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: want 404, got %d", resp.StatusCode)
	}
}

// ── Method enforcement ────────────────────────────────────────────────────

func TestHandleWorkareas_RejectsBadMethods(t *testing.T) {
	root := t.TempDir()
	hsrv := newServerForWorkareaTest(t, root, 0)
	cases := []struct {
		method, path string
		wantStatus   int
	}{
		{http.MethodPost, "/api/daemon/workareas", http.StatusMethodNotAllowed},
		{http.MethodPut, "/api/daemon/workareas/x", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/daemon/workareas/x/restore", http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest(tc.method, hsrv.URL+tc.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		if resp.StatusCode != tc.wantStatus {
			t.Errorf("%s %s: want %d, got %d", tc.method, tc.path, tc.wantStatus, resp.StatusCode)
		}
	}
}

// ── Concurrency: restore + diff under load through the HTTP layer ─────────

func TestHandleWorkareas_ConcurrentDiffAndRestore(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 4; i++ {
		writeFixtureArchive(t, root, fixtureArchive{
			id:   fmt.Sprintf("wa-x-%d", i),
			tree: map[string]string{fmt.Sprintf("f-%d", i): "v"},
		})
	}
	hsrv := newServerForWorkareaTest(t, root, 0)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("wa-x-%d", i)
			resp, err := http.Post(hsrv.URL+"/api/daemon/workareas/"+id+"/restore",
				"application/json", strings.NewReader(`{}`))
			if err != nil {
				t.Errorf("restore %s: %v", id, err)
				return
			}
			_ = resp.Body.Close()
		}(i)
		go func(i int) {
			defer wg.Done()
			a := fmt.Sprintf("wa-x-%d", i)
			b := fmt.Sprintf("wa-x-%d", (i+1)%4)
			resp, err := http.Get(hsrv.URL + "/api/daemon/workareas/" + a + "/diff/" + b)
			if err != nil {
				t.Errorf("diff: %v", err)
				return
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}(i)
	}
	wg.Wait()
}

// ── helper plumbing for tests that need to reach into the daemon  ────────

// serverByURL is a test-only lookup table mapping the httptest server's
// URL to its *Server, so tests can override the registry post-spawn
// (e.g. inject an ActiveWorkareaProvider after a restore).
var serverByURL sync.Map

func serverFromTest(t *testing.T, hsrv *httptest.Server) *Server {
	t.Helper()
	v, ok := serverByURL.Load(hsrv.URL)
	if !ok {
		t.Fatalf("server not registered for url %s", hsrv.URL)
	}
	return v.(*Server)
}

// newServerForWorkareaTestRegistered is the canonical constructor.
// Returns both the httptest server and the underlying *Server so tests
// can mutate it (mostly: SetWorkareaArchiveRegistry to inject an
// ActiveWorkareaProvider).
func newServerForWorkareaTestRegistered(t *testing.T, archiveRoot string, threshold int) (*httptest.Server, *Server) {
	t.Helper()
	d := New(Options{HTTPHost: "127.0.0.1", HTTPPort: 0})
	d.mu.Lock()
	d.config = &Config{Workarea: WorkareaConfig{
		ArchiveRoot:            archiveRoot,
		DiffStreamingThreshold: threshold,
	}}
	d.mu.Unlock()
	d.state.Store(StateRunning)
	srv := &Server{daemon: d}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/daemon/workareas", srv.handleWorkareasRoot)
	mux.HandleFunc("/api/daemon/workareas/", srv.handleWorkareaItem)
	hsrv := httptest.NewServer(mux)
	t.Cleanup(hsrv.Close)
	t.Cleanup(func() { serverByURL.Delete(hsrv.URL) })
	serverByURL.Store(hsrv.URL, srv)
	return hsrv, srv
}
