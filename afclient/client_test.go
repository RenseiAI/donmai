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
		case strings.HasPrefix(r.URL.Path, "/api/public/sessions/") && strings.Contains(r.URL.Path, "/activities"):
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
