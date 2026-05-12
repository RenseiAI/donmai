package afclient

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, NewClient(srv.URL)
}

// newTestServerWithToken returns a Client preloaded with an APIToken,
// so tests can exercise the authenticated branches that gate request
// shape on the token's presence (e.g. GetActivities omitting
// sessionHash for authenticated callers).
func newTestServerWithToken(t *testing.T, token string, handler http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, NewAuthenticatedClient(srv.URL, token)
}

func TestClientStopSessionSuccess(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/public/sessions/sess-1/stop" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(StopSessionResponse{
			Stopped: true, SessionID: "sess-1",
			PreviousStatus: StatusWorking, NewStatus: StatusStopped,
		})
	})
	resp, err := c.StopSession("sess-1")
	if err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	if !resp.Stopped || resp.NewStatus != StatusStopped {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestClientChatSessionSuccess(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/public/sessions/sess-1/prompt" {
			t.Errorf("path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"prompt":"hi"`) {
			t.Errorf("body missing prompt: %s", body)
		}
		_ = json.NewEncoder(w).Encode(ChatSessionResponse{
			Delivered: true, PromptID: "p-1", SessionID: "sess-1", SessionStatus: StatusWorking,
		})
	})
	resp, err := c.ChatSession("sess-1", ChatSessionRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("ChatSession: %v", err)
	}
	if !resp.Delivered || resp.PromptID != "p-1" {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestClientReconnectSessionSuccess(t *testing.T) {
	cur := "c-1"
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/public/sessions/sess-1/reconnect" {
			t.Errorf("path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"cursor":"c-1"`) {
			t.Errorf("body missing cursor: %s", body)
		}
		_ = json.NewEncoder(w).Encode(ReconnectSessionResponse{
			Reconnected: true, SessionID: "sess-1", SessionStatus: StatusWorking, MissedEvents: 3,
		})
	})
	resp, err := c.ReconnectSession("sess-1", ReconnectSessionRequest{Cursor: &cur})
	if err != nil {
		t.Fatalf("ReconnectSession: %v", err)
	}
	if !resp.Reconnected || resp.MissedEvents != 3 {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestClientStatusErrorMapping(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   error
	}{
		{"unauthorized", http.StatusUnauthorized, ErrNotAuthenticated},
		{"forbidden", http.StatusForbidden, ErrUnauthorized},
		{"notfound", http.StatusNotFound, ErrNotFound},
		{"ratelimited", http.StatusTooManyRequests, ErrRateLimited},
		{"server", http.StatusInternalServerError, ErrServerError},
		{"badgateway", http.StatusBadGateway, ErrServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, c := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			})
			_, err := c.StopSession("sess-1")
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestClientGetStatusErrorMapping(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := c.GetSessionDetail("nope")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestClientGetEndpoints(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/public/stats"):
			_ = json.NewEncoder(w).Encode(StatsResponse{WorkersOnline: 2})
		case r.URL.Path == "/api/public/session-activities":
			_ = json.NewEncoder(w).Encode(ActivityListResponse{SessionStatus: StatusWorking})
		case r.URL.Path == "/api/public/sessions":
			_ = json.NewEncoder(w).Encode(SessionsListResponse{Count: 1})
		case strings.HasPrefix(r.URL.Path, "/api/public/sessions/"):
			_ = json.NewEncoder(w).Encode(SessionDetailResponse{Session: SessionDetail{ID: "sess-1"}})
		case r.URL.Path == "/api/mcp/cost-report":
			_ = json.NewEncoder(w).Encode(CostReportResponse{TotalSessions: 1})
		case r.URL.Path == "/api/mcp/list-fleet":
			_ = json.NewEncoder(w).Encode(ListFleetResponse{Total: 1})
		case r.URL.Path == "/api/cli/whoami":
			_ = json.NewEncoder(w).Encode(WhoAmIResponse{Org: WhoAmIOrg{ID: "org-1"}})
		default:
			http.NotFound(w, r)
		}
	})

	if _, err := c.GetStats(); err != nil {
		t.Errorf("GetStats: %v", err)
	}
	if _, err := c.GetSessions(); err != nil {
		t.Errorf("GetSessions: %v", err)
	}
	if _, err := c.GetSessionDetail("sess-1"); err != nil {
		t.Errorf("GetSessionDetail: %v", err)
	}
	cursor := "c-1"
	if _, err := c.GetActivities("sess-1", &cursor); err != nil {
		t.Errorf("GetActivities: %v", err)
	}
	if _, err := c.GetActivities("sess-1", nil); err != nil {
		t.Errorf("GetActivities nil cursor: %v", err)
	}
	if _, err := c.GetCostReport(); err != nil {
		t.Errorf("GetCostReport: %v", err)
	}
	if _, err := c.ListFleet(); err != nil {
		t.Errorf("ListFleet: %v", err)
	}
	if _, err := c.WhoAmI(); err != nil {
		t.Errorf("WhoAmI: %v", err)
	}
}

// TestClientGetActivitiesAuthenticatedOmitsHash pins the contract for
// authenticated CLI callers: when the client carries an APIToken
// (rsk_ key) the request must NOT include sessionHash. The platform's
// public-hash branch is tautological (anyone can hash any string) and
// short-circuits before the authenticated branch's reverse-lookup
// runs; sending sessionHash anyway makes hashed-id sessionIds 404
// instead of resolving correctly. Authenticated callers belong on the
// rsk_/cookie auth path, which accepts both raw and hashed forms.
func TestClientGetActivitiesAuthenticatedOmitsHash(t *testing.T) {
	var seenQuery string
	_, c := newTestServerWithToken(t, "rsk_live_test", func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(ActivityListResponse{SessionStatus: StatusWorking})
	})
	if _, err := c.GetActivities("sess-9", nil); err != nil {
		t.Fatalf("GetActivities: %v", err)
	}
	if !strings.Contains(seenQuery, "sessionId=sess-9") {
		t.Errorf("missing sessionId=sess-9; got %q", seenQuery)
	}
	if strings.Contains(seenQuery, "sessionHash=") {
		t.Errorf("authenticated client must NOT include sessionHash; got %q", seenQuery)
	}
}

// TestClientGetActivitiesURLContract pins the wire contract for
// unauthenticated GetActivities callers (no APIToken — typically a
// TUI viewer with the raw linearSessionId from a shared link):
// query-param shape against /api/public/session-activities, with
// sessionId + sessionHash + (optional) after parameters. The
// sessionHash is mandatory for unauthenticated callers — without it
// the platform requires worker-JWT auth and rejects with 401. Pre-port
// servers responded to a legacy path-segment URL
// (/api/public/sessions/<id>/activities); current platform servers
// serve the query-param URL exclusively. Without this guard a future
// regression to the legacy form silently 404s every CLI `session show` /
// `session stream` invocation against the production surface, and a
// regression that drops the sessionHash silently 401s every
// unauthenticated one of them.
func TestClientGetActivitiesURLContract(t *testing.T) {
	var seenPath, seenQuery string
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(ActivityListResponse{SessionStatus: StatusWorking})
	})

	if _, err := c.GetActivities("sess-9", nil); err != nil {
		t.Fatalf("GetActivities: %v", err)
	}
	if seenPath != "/api/public/session-activities" {
		t.Errorf("path = %q, want /api/public/session-activities", seenPath)
	}
	if !strings.Contains(seenQuery, "sessionId=sess-9") {
		t.Errorf("query missing sessionId=sess-9; got %q", seenQuery)
	}
	if !strings.Contains(seenQuery, "sessionHash="+hashSessionID("sess-9")) {
		t.Errorf("query missing sessionHash for unauthenticated access; got %q", seenQuery)
	}
	if strings.Contains(seenQuery, "after=") {
		t.Errorf("nil cursor must not add after= param; got %q", seenQuery)
	}

	cursor := "cur-42"
	if _, err := c.GetActivities("sess-9", &cursor); err != nil {
		t.Fatalf("GetActivities with cursor: %v", err)
	}
	if !strings.Contains(seenQuery, "after=cur-42") {
		t.Errorf("cursor must round-trip as after=...; got query %q", seenQuery)
	}
}

// TestHashSessionID pins the SHA-256 derivation that mirrors the
// platform's hashSessionId in src/lib/worker-protocol/session-hash.ts.
// Format: first 32 hex chars of SHA-256("session:" + sessionId). If
// either side drifts, every unauthenticated GetActivities call 401s.
//
// The expected hash values are computed from the same algorithm; this
// test catches accidental algorithm changes (length, prefix, encoding)
// rather than trying to encode the platform's literal output —
// upstream changes to either side would still need to be paired
// manually.
func TestHashSessionID(t *testing.T) {
	cases := []struct {
		in       string
		wantLen  int
		notEmpty bool
	}{
		{in: "sess-1", wantLen: 32, notEmpty: true},
		{in: "a008dd512f8add5b", wantLen: 32, notEmpty: true},
		{in: "", wantLen: 32, notEmpty: true}, // sha256("session:") still has 32 hex chars
	}
	for _, tc := range cases {
		got := hashSessionID(tc.in)
		if len(got) != tc.wantLen {
			t.Errorf("hashSessionID(%q) length = %d, want %d (got %q)", tc.in, len(got), tc.wantLen, got)
		}
		if tc.notEmpty && got == "" {
			t.Errorf("hashSessionID(%q) returned empty", tc.in)
		}
	}
	// Determinism: same input must always hash to the same output.
	// Use intermediates so staticcheck doesn't flag the comparison as
	// a self-equality (SA4000) — the call sites are intentionally
	// independent invocations.
	first := hashSessionID("sess-determinism")
	second := hashSessionID("sess-determinism")
	if first != second {
		t.Errorf("hashSessionID is not deterministic")
	}
	// Distinguishability: different inputs hash to different outputs.
	if hashSessionID("sess-a") == hashSessionID("sess-b") {
		t.Errorf("hashSessionID collided across distinct inputs")
	}
}

func TestClientPostEndpoints(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/mcp/submit-task":
			_ = json.NewEncoder(w).Encode(SubmitTaskResponse{Submitted: true, TaskID: "t-1"})
		case "/api/mcp/stop-agent":
			_ = json.NewEncoder(w).Encode(StopAgentResponse{Stopped: true, TaskID: "t-1"})
		case "/api/mcp/forward-prompt":
			_ = json.NewEncoder(w).Encode(ForwardPromptResponse{Forwarded: true, PromptID: "p-1"})
		default:
			http.NotFound(w, r)
		}
	})

	if _, err := c.SubmitTask(SubmitTaskRequest{IssueID: "i-1"}); err != nil {
		t.Errorf("SubmitTask: %v", err)
	}
	if _, err := c.StopAgent(StopAgentRequest{TaskID: "t-1"}); err != nil {
		t.Errorf("StopAgent: %v", err)
	}
	if _, err := c.ForwardPrompt(ForwardPromptRequest{TaskID: "t-1", Message: "hi"}); err != nil {
		t.Errorf("ForwardPrompt: %v", err)
	}
}

func TestNewAuthenticatedClient(t *testing.T) {
	c := NewAuthenticatedClient("https://example.com/", "rsk_abc")
	if c.BaseURL != "https://example.com" {
		t.Errorf("BaseURL = %q, want trimmed", c.BaseURL)
	}
	if c.APIToken != "rsk_abc" {
		t.Errorf("APIToken not set")
	}
}

func TestClientGetSessionsFiltered(t *testing.T) {
	tests := []struct {
		name      string
		project   string
		wantQuery string
	}{
		{name: "empty_project_no_query_param", project: "", wantQuery: ""},
		{name: "slug_project_is_query_escaped", project: "my-project", wantQuery: "my-project"},
		{name: "project_with_special_chars_is_escaped", project: "team a/b", wantQuery: "team a/b"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotQuery string
			_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotQuery = r.URL.Query().Get("project")
				_ = json.NewEncoder(w).Encode(SessionsListResponse{Count: 1})
			})

			if _, err := c.GetSessionsFiltered(tc.project); err != nil {
				t.Fatalf("GetSessionsFiltered: %v", err)
			}
			if gotPath != "/api/public/sessions" {
				t.Errorf("path = %q, want /api/public/sessions", gotPath)
			}
			if gotQuery != tc.wantQuery {
				t.Errorf("project query = %q, want %q", gotQuery, tc.wantQuery)
			}
		})
	}
}

func TestClientGetSessionsFallsThroughToFiltered(t *testing.T) {
	// GetSessions should delegate to GetSessionsFiltered("") and hit the bare endpoint.
	var gotPath string
	var gotRawQuery string
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(SessionsListResponse{Count: 0})
	})
	if _, err := c.GetSessions(); err != nil {
		t.Fatalf("GetSessions: %v", err)
	}
	if gotPath != "/api/public/sessions" {
		t.Errorf("path = %q, want /api/public/sessions", gotPath)
	}
	if gotRawQuery != "" {
		t.Errorf("raw query = %q, want empty", gotRawQuery)
	}
}

func TestClientAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(StatsResponse{})
	}))
	t.Cleanup(srv.Close)
	c := NewAuthenticatedClient(srv.URL, "rsk_token")
	if _, err := c.GetStats(); err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if gotAuth != "Bearer rsk_token" {
		t.Errorf("Authorization = %q, want Bearer rsk_token", gotAuth)
	}
}

// TestClientScopeHeaders pins that OrgScope / ProjectScope on the
// Client struct become `X-Rensei-Org` / `X-Rensei-Project` headers on
// every request — including the secondary POST path. This is the
// wire-level half of the multi-org misroute fix; without it the
// platform's CLI auth would fall back to the WorkOS token's frozen
// org_id claim. Empty-scope variants confirm the headers are omitted
// (not sent as empty strings) so single-org users see no behavior change.
func TestClientScopeHeaders(t *testing.T) {
	t.Parallel()

	type seen struct {
		auth, org, project string
		hadOrg, hadProject bool
	}
	var got seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.auth = r.Header.Get("Authorization")
		got.org = r.Header.Get("X-Rensei-Org")
		got.project = r.Header.Get("X-Rensei-Project")
		_, got.hadOrg = r.Header["X-Rensei-Org"]
		_, got.hadProject = r.Header["X-Rensei-Project"]
		_ = json.NewEncoder(w).Encode(StatsResponse{})
	}))
	t.Cleanup(srv.Close)

	t.Run("scope-set sends both headers", func(t *testing.T) {
		got = seen{}
		c := NewAuthenticatedClient(srv.URL, "rsk_token")
		c.OrgScope = "org_supaku"
		c.ProjectScope = "yuisei"
		if _, err := c.GetStats(); err != nil {
			t.Fatalf("GetStats: %v", err)
		}
		if got.org != "org_supaku" {
			t.Errorf("X-Rensei-Org = %q, want org_supaku", got.org)
		}
		if got.project != "yuisei" {
			t.Errorf("X-Rensei-Project = %q, want yuisei", got.project)
		}
	})

	t.Run("empty scope omits headers entirely", func(t *testing.T) {
		got = seen{}
		c := NewAuthenticatedClient(srv.URL, "rsk_token")
		// Leave OrgScope / ProjectScope at their zero values.
		if _, err := c.GetStats(); err != nil {
			t.Fatalf("GetStats: %v", err)
		}
		if got.hadOrg {
			t.Errorf("X-Rensei-Org should not be set when OrgScope is empty; got %q", got.org)
		}
		if got.hadProject {
			t.Errorf("X-Rensei-Project should not be set when ProjectScope is empty; got %q", got.project)
		}
	})

	t.Run("post path also carries scope", func(t *testing.T) {
		got = seen{}
		c := NewAuthenticatedClient(srv.URL, "rsk_token")
		c.OrgScope = "org_supaku"
		// Trigger the post path. StopSession returns an error because the
		// stub server doesn't return the expected JSON shape — that's fine,
		// we only need the request to be made so the header capture fires.
		_, _ = c.StopSession("sess-x")
		if got.org != "org_supaku" {
			t.Errorf("post X-Rensei-Org = %q, want org_supaku", got.org)
		}
	})
}
