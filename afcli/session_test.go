package afcli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient"
)

// newTestSessionCmd builds a fresh session command tree wired to the
// given DataSource factory. Output/err are captured in the returned
// buffer. No project scoping is applied.
func newTestSessionCmd(ds func() afclient.DataSource, args []string) (*cobra.Command, *bytes.Buffer) {
	return newTestSessionCmdWithProject(ds, nil, args)
}

// newTestSessionCmdWithProject is like newTestSessionCmd but lets the
// caller inject a ProjectFunc to exercise project-scoping paths.
func newTestSessionCmdWithProject(ds func() afclient.DataSource, projectFunc func() string, args []string) (*cobra.Command, *bytes.Buffer) {
	cmd := newSessionCmd(ds, projectFunc)
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)
	return cmd, buf
}

func TestSessionParentListsAllSubcommands(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestSessionCmd(ds, []string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	for _, sub := range []string{"list", "show", "stop", "prompt", "stream"} {
		if !strings.Contains(out, sub) {
			t.Errorf("session --help missing %q subcommand; got:\n%s", sub, out)
		}
	}
}

func TestSessionListMatchesAgentList(t *testing.T) {
	t.Parallel()

	stub := &stubDataSource{sessions: []afclient.SessionResponse{
		{ID: "working", Status: afclient.StatusWorking},
		{ID: "queued", Status: afclient.StatusQueued},
		{ID: "parked", Status: afclient.StatusParked},
		{ID: "starting", Status: afclient.StatusStarting},
		{ID: "completed", Status: afclient.StatusCompleted},
		{ID: "failed", Status: afclient.StatusFailed},
		{ID: "stopped", Status: afclient.StatusStopped},
		{ID: "timed-out", Status: afclient.StatusTimedOut},
	}}
	ds := func() afclient.DataSource { return stub }
	cmd, buf := newTestSessionCmd(ds, []string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()

	for _, want := range []string{"SESSION ID", "IDENTIFIER", "STATUS", "DURATION", "WORK TYPE"} {
		if !strings.Contains(out, want) {
			t.Errorf("session list header missing %q; got:\n%s", want, out)
		}
	}
	for _, want := range []string{"working", "queued", "parked", "starting"} {
		if !strings.Contains(out, want) {
			t.Errorf("session list missing active status %q; got:\n%s", want, out)
		}
	}
	for _, reject := range []string{"completed", "failed", "stopped", "timed_out"} {
		if strings.Contains(out, reject) {
			t.Errorf("session list should not contain terminal status %q; got:\n%s", reject, out)
		}
	}
}

func TestSessionShowMatchesAgentStatus(t *testing.T) {
	t.Parallel()

	const id = "mock-001"

	// session show <id>
	sessionMock := afclient.NewMockClient()
	sessionDS := func() afclient.DataSource { return sessionMock }
	sessionCmd, sessionBuf := newTestSessionCmd(sessionDS, []string{"show", id})
	if err := sessionCmd.Execute(); err != nil {
		t.Fatalf("session show execute: %v", err)
	}

	// agent status <id>
	agentMock := afclient.NewMockClient()
	agentDS := func() afclient.DataSource { return agentMock }
	agentCmd, agentBuf := newTestAgentCmd(agentDS, []string{"status", id})
	if err := agentCmd.Execute(); err != nil {
		t.Fatalf("agent status execute: %v", err)
	}

	if sessionBuf.String() != agentBuf.String() {
		t.Errorf("session show output != agent status output\nsession show:\n%s\nagent status:\n%s",
			sessionBuf.String(), agentBuf.String())
	}
}

func TestSessionStopMatchesAgentStop(t *testing.T) {
	t.Parallel()

	const id = "mock-001"

	// Use distinct mocks because StopSession mutates mock state.
	sessionMock := afclient.NewMockClient()
	sessionDS := func() afclient.DataSource { return sessionMock }
	sessionCmd, sessionBuf := newTestSessionCmd(sessionDS, []string{"stop", id})
	if err := sessionCmd.Execute(); err != nil {
		t.Fatalf("session stop execute: %v", err)
	}

	agentMock := afclient.NewMockClient()
	agentDS := func() afclient.DataSource { return agentMock }
	agentCmd, agentBuf := newTestAgentCmd(agentDS, []string{"stop", id})
	if err := agentCmd.Execute(); err != nil {
		t.Fatalf("agent stop execute: %v", err)
	}

	if sessionBuf.String() != agentBuf.String() {
		t.Errorf("session stop output != agent stop output\nsession stop:\n%s\nagent stop:\n%s",
			sessionBuf.String(), agentBuf.String())
	}
}

func TestSessionPromptMatchesAgentChat(t *testing.T) {
	t.Parallel()

	const (
		id  = "mock-001" // public session id known to the mock
		msg = "hello"
	)

	sessionMock := afclient.NewMockClient()
	sessionDS := func() afclient.DataSource { return sessionMock }
	sessionCmd, sessionBuf := newTestSessionCmd(sessionDS, []string{"prompt", id, msg})
	if err := sessionCmd.Execute(); err != nil {
		t.Fatalf("session prompt execute: %v", err)
	}

	agentMock := afclient.NewMockClient()
	agentDS := func() afclient.DataSource { return agentMock }
	agentCmd, agentBuf := newTestAgentCmd(agentDS, []string{"chat", id, msg})
	if err := agentCmd.Execute(); err != nil {
		t.Fatalf("agent chat execute: %v", err)
	}

	if sessionBuf.String() != agentBuf.String() {
		t.Errorf("session prompt output != agent chat output\nsession prompt:\n%s\nagent chat:\n%s",
			sessionBuf.String(), agentBuf.String())
	}
}

func TestSessionListProjectScoping(t *testing.T) {
	t.Parallel()

	sessions := []afclient.SessionResponse{
		{ID: "a", Identifier: "X-1", Status: afclient.StatusWorking, WorkType: "dev", Duration: 10},
	}

	tests := []struct {
		name             string
		projectFunc      func() string
		wantFilteredArgs []string
		wantPlainCalls   int
	}{
		{
			name:             "nil_project_func_uses_plain_get_sessions",
			projectFunc:      nil,
			wantFilteredArgs: nil,
			wantPlainCalls:   1,
		},
		{
			name:             "empty_project_uses_plain_get_sessions",
			projectFunc:      func() string { return "" },
			wantFilteredArgs: nil,
			wantPlainCalls:   1,
		},
		{
			name:             "non_empty_project_uses_filtered_get_sessions",
			projectFunc:      func() string { return "my-project" },
			wantFilteredArgs: []string{"my-project"},
			wantPlainCalls:   0,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stub := &stubDataSource{sessions: sessions}
			ds := func() afclient.DataSource { return stub }

			cmd, _ := newTestSessionCmdWithProject(ds, tc.projectFunc, []string{"list"})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}

			if got, want := stub.plainCalls, tc.wantPlainCalls; got != want {
				t.Errorf("GetSessions calls = %d, want %d", got, want)
			}
			if got, want := len(stub.filteredProjects), len(tc.wantFilteredArgs); got != want {
				t.Fatalf("GetSessionsFiltered call count = %d, want %d (args=%v)", got, want, stub.filteredProjects)
			}
			for i, want := range tc.wantFilteredArgs {
				if stub.filteredProjects[i] != want {
					t.Errorf("GetSessionsFiltered call %d arg = %q, want %q", i, stub.filteredProjects[i], want)
				}
			}
		})
	}
}

// newParkReasonServer serves raw session-detail JSON on the detail route
// and an empty activity list on the activities route, so the test drives
// the real client decoder and the real `session show --json` renderer.
func newParkReasonServer(t *testing.T, detailRaw string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/public/session-activities") {
			_ = json.NewEncoder(w).Encode(afclient.ActivityListResponse{
				Activities:    []afclient.ActivityEvent{},
				SessionStatus: afclient.StatusParked,
			})
			return
		}
		_, _ = w.Write([]byte(detailRaw))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSessionShowJSONPreservesParkReason pins the optional public
// session-detail parkReason field through `session show --json`:
// a non-null reason round-trips unchanged, and an older response
// without the field omits the key via omitempty.
func TestSessionShowJSONPreservesParkReason(t *testing.T) {
	t.Parallel()

	t.Run("non_null_reason_round_trips", func(t *testing.T) {
		t.Parallel()

		srv := newParkReasonServer(t, `{"session":{"id":"sess-parked","identifier":"OPS-1202","status":"parked","workType":"refinement","startedAt":"2026-10-26T12:00:00Z","duration":480,"timeline":{"created":"2026-10-26T11:52:00Z"},"parkReason":"waiting for required status"},"timestamp":"2026-10-26T12:08:00Z"}`)
		ds := func() afclient.DataSource { return afclient.NewClient(srv.URL) }
		cmd, buf := newTestSessionCmd(ds, []string{"show", "sess-parked", "--json"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}

		var payload map[string]any
		if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
			t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
		}
		session, ok := payload["session"].(map[string]any)
		if !ok {
			t.Fatalf("expected top-level 'session' object; got: %v", payload)
		}
		got, ok := session["parkReason"].(string)
		if !ok || got != "waiting for required status" {
			t.Errorf("expected session.parkReason %q, got %v (session=%v)", "waiting for required status", session["parkReason"], session)
		}
	})

	t.Run("absent_reason_stays_omitted", func(t *testing.T) {
		t.Parallel()

		srv := newParkReasonServer(t, `{"session":{"id":"sess-parked","identifier":"OPS-1202","status":"parked","workType":"refinement","startedAt":"2026-10-26T12:00:00Z","duration":480,"timeline":{"created":"2026-10-26T11:52:00Z"}},"timestamp":"2026-10-26T12:08:00Z"}`)
		ds := func() afclient.DataSource { return afclient.NewClient(srv.URL) }
		cmd, buf := newTestSessionCmd(ds, []string{"show", "sess-parked", "--json"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}

		var payload map[string]any
		if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
			t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
		}
		session, ok := payload["session"].(map[string]any)
		if !ok {
			t.Fatalf("expected top-level 'session' object; got: %v", payload)
		}
		if _, present := session["parkReason"]; present {
			t.Errorf("expected no session.parkReason key for older response; got: %v", session["parkReason"])
		}
	})
}
