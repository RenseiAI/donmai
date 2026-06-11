package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── stub registry for tests ───────────────────────────────────────────────────

// staticAgentRegistry is a test-only agentCardRegistry backed by a fixed slice.
type staticAgentRegistry struct {
	cards []agentCardStub
}

func (r *staticAgentRegistry) agentCards() []agentCardStub {
	return r.cards
}

func (r *staticAgentRegistry) agentCard(id string) (agentCardStub, bool) {
	for _, c := range r.cards {
		if c.ID == id {
			return c, true
		}
	}
	return agentCardStub{}, false
}

// newTestServer builds a bare-bones httptest.Server that hosts only the agent
// handlers backed by a staticAgentRegistry. It does not require a live Daemon.
func newTestAgentServer(t *testing.T, cards []agentCardStub) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	// Build a minimal Server with just the fields the agent handlers need.
	// We cannot use NewServer here because it requires a fully-wired Daemon.
	// The Server.agentReg field is the only dependency; we inject it directly.
	s := &Server{
		agentReg: &staticAgentRegistry{cards: cards},
		// daemon is nil — agentCardRegistryOrDefault returns s.agentReg above.
	}
	mux.HandleFunc("/api/daemon/agents", func(w http.ResponseWriter, r *http.Request) {
		// Enforce GET-only at the method level; the real mux does this via s.method().
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleListAgents(w, r)
	})
	mux.HandleFunc("/api/daemon/agents/", s.handleGetAgent)
	return httptest.NewServer(mux)
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestHandleAgents_List verifies GET /api/daemon/agents returns all cards with
// the H-workType contract shape (id, workType, poolRef, model, labels).
func TestHandleAgents_List(t *testing.T) {
	fixtures := []agentCardStub{
		{ID: "agent-alpha", WorkType: "interactive", PoolRef: "pool-local-default", Model: "claude-sonnet", Labels: []string{"trusted", "h-worktype"}},
		{ID: "agent-beta", WorkType: "batch", PoolRef: "pool-local-batch", Model: "claude-haiku", Labels: []string{"h-worktype"}},
		{ID: "agent-gamma", WorkType: "background", PoolRef: "pool-local-bg", Model: "claude-sonnet", Labels: nil},
	}
	srv := newTestAgentServer(t, fixtures)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/daemon/agents") //nolint:gosec
	if err != nil {
		t.Fatalf("GET /api/daemon/agents: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var list agentListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}

	if list.Count != len(fixtures) {
		t.Errorf("Count = %d, want %d", list.Count, len(fixtures))
	}
	if len(list.Agents) != len(fixtures) {
		t.Fatalf("len(Agents) = %d, want %d", len(list.Agents), len(fixtures))
	}
	if list.Timestamp == "" {
		t.Errorf("Timestamp is empty")
	}

	for i, got := range list.Agents {
		want := fixtures[i]
		if got.ID != want.ID {
			t.Errorf("[%d] ID = %q, want %q", i, got.ID, want.ID)
		}
		if got.WorkType != want.WorkType {
			t.Errorf("[%d] WorkType = %q, want %q", i, got.WorkType, want.WorkType)
		}
		if got.PoolRef != want.PoolRef {
			t.Errorf("[%d] PoolRef = %q, want %q", i, got.PoolRef, want.PoolRef)
		}
		if got.Model != want.Model {
			t.Errorf("[%d] Model = %q, want %q", i, got.Model, want.Model)
		}
	}
}

// TestHandleAgents_ListWithScopeQuery verifies that ?scope=local is accepted
// (and ignored) — matching the smoke contract which passes scope=local.
func TestHandleAgents_ListWithScopeQuery(t *testing.T) {
	fixtures := []agentCardStub{
		{ID: "sess-1", WorkType: "development"},
	}
	srv := newTestAgentServer(t, fixtures)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/daemon/agents?scope=local") //nolint:gosec
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var list agentListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Agents) != 1 {
		t.Errorf("len(Agents) = %d, want 1", len(list.Agents))
	}
}

// TestHandleAgents_ListWorkTypeFilter verifies ?workType=<val> filters results.
func TestHandleAgents_ListWorkTypeFilter(t *testing.T) {
	fixtures := []agentCardStub{
		{ID: "a", WorkType: "development"},
		{ID: "b", WorkType: "research"},
		{ID: "c", WorkType: "development"},
	}
	srv := newTestAgentServer(t, fixtures)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/daemon/agents?workType=development") //nolint:gosec
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var list agentListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Count != 2 {
		t.Errorf("Count = %d, want 2", list.Count)
	}
	for _, c := range list.Agents {
		if c.WorkType != "development" {
			t.Errorf("card %q has WorkType %q, want development", c.ID, c.WorkType)
		}
	}
}

// TestHandleAgents_Detail verifies GET /api/daemon/agents/:id returns the full
// card with all H-workType fields intact.
func TestHandleAgents_Detail(t *testing.T) {
	fixtures := []agentCardStub{
		{ID: "agent-alpha", WorkType: "interactive", PoolRef: "pool-local-default", Model: "claude-sonnet", Labels: []string{"trusted", "h-worktype"}},
		{ID: "agent-beta", WorkType: "batch", PoolRef: "pool-local-batch", Model: "claude-haiku", Labels: []string{"h-worktype"}},
		{ID: "agent-gamma", WorkType: "background", PoolRef: "pool-local-bg", Model: "claude-sonnet", Labels: nil},
	}
	srv := newTestAgentServer(t, fixtures)
	defer srv.Close()

	for _, want := range fixtures {
		want := want // capture
		t.Run(want.ID, func(t *testing.T) {
			resp, err := http.Get(srv.URL + "/api/daemon/agents/" + want.ID) //nolint:gosec
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 200\nbody: %s", resp.StatusCode, body)
			}

			var got agentCardStub
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("decode: %v", err)
			}

			if got.ID != want.ID {
				t.Errorf("ID = %q, want %q", got.ID, want.ID)
			}
			if got.WorkType != want.WorkType {
				t.Errorf("WorkType = %q, want %q", got.WorkType, want.WorkType)
			}
			if got.PoolRef != want.PoolRef {
				t.Errorf("PoolRef = %q, want %q", got.PoolRef, want.PoolRef)
			}
			if got.Model != want.Model {
				t.Errorf("Model = %q, want %q", got.Model, want.Model)
			}
			if len(want.Labels) > 0 {
				if len(got.Labels) != len(want.Labels) {
					t.Errorf("len(Labels) = %d, want %d", len(got.Labels), len(want.Labels))
				} else {
					for j, lbl := range want.Labels {
						if got.Labels[j] != lbl {
							t.Errorf("Labels[%d] = %q, want %q", j, got.Labels[j], lbl)
						}
					}
				}
			}
		})
	}
}

// TestHandleAgents_DetailNotFound verifies GET /api/daemon/agents/<unknown> → 404.
func TestHandleAgents_DetailNotFound(t *testing.T) {
	srv := newTestAgentServer(t, nil)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/daemon/agents/does-not-exist") //nolint:gosec
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestHandleAgents_EmptyList verifies GET /api/daemon/agents returns an empty
// array (not null) when no sessions are active.
func TestHandleAgents_EmptyList(t *testing.T) {
	srv := newTestAgentServer(t, []agentCardStub{})
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/daemon/agents") //nolint:gosec
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	// Must not be null — must be "agents":[]
	if !strings.Contains(string(body), `"agents":[]`) {
		t.Errorf("body does not contain empty agents array: %s", body)
	}
}

// TestSessionDetailAgentRegistry_Mapping verifies that sessionDetailAgentRegistry
// correctly projects SessionDetail fields onto the H-workType stub.
func TestSessionDetailAgentRegistry_Mapping(t *testing.T) {
	store := newSessionDetailStore()
	store.Set(&SessionDetail{
		SessionID:       "sess-abc",
		WorkType:        "development",
		ResolvedProfile: &SessionResolvedProfile{Model: "claude-sonnet"},
	})
	store.Set(&SessionDetail{
		SessionID:    "sess-def",
		WorkType:     "research",
		ModelProfile: &SessionModelProfile{Model: "claude-haiku"},
	})

	reg := &sessionDetailAgentRegistry{
		store:    store,
		workerID: func() string { return "wkr-test-001" },
	}

	t.Run("agentCards returns all entries", func(t *testing.T) {
		cards := reg.agentCards()
		if len(cards) != 2 {
			t.Fatalf("len(cards) = %d, want 2", len(cards))
		}
	})

	t.Run("agentCard by id — ResolvedProfile model", func(t *testing.T) {
		c, ok := reg.agentCard("sess-abc")
		if !ok {
			t.Fatal("agentCard returned false")
		}
		if c.WorkType != "development" {
			t.Errorf("WorkType = %q", c.WorkType)
		}
		if c.Model != "claude-sonnet" {
			t.Errorf("Model = %q, want claude-sonnet", c.Model)
		}
		if c.PoolRef != "wkr-test-001" {
			t.Errorf("PoolRef = %q, want wkr-test-001", c.PoolRef)
		}
	})

	t.Run("agentCard by id — ModelProfile wins over ResolvedProfile", func(t *testing.T) {
		// Set a session with both fields; ModelProfile should win.
		store.Set(&SessionDetail{
			SessionID:       "sess-ghi",
			WorkType:        "qa",
			ResolvedProfile: &SessionResolvedProfile{Model: "claude-haiku"},
			ModelProfile:    &SessionModelProfile{Model: "claude-opus"},
		})
		c, ok := reg.agentCard("sess-ghi")
		if !ok {
			t.Fatal("agentCard returned false")
		}
		if c.Model != "claude-opus" {
			t.Errorf("Model = %q, want claude-opus (ModelProfile should win)", c.Model)
		}
	})

	t.Run("agentCard — ModelProfile preferred", func(t *testing.T) {
		c, ok := reg.agentCard("sess-def")
		if !ok {
			t.Fatal("agentCard returned false")
		}
		if c.Model != "claude-haiku" {
			t.Errorf("Model = %q, want claude-haiku", c.Model)
		}
	})

	t.Run("agentCard miss", func(t *testing.T) {
		_, ok := reg.agentCard("unknown-id")
		if ok {
			t.Errorf("expected false for unknown id")
		}
	})
}
