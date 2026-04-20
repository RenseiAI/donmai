package afcli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
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

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
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
	for _, want := range []string{"working", "queued", "parked"} {
		if !strings.Contains(out, want) {
			t.Errorf("session list missing active status %q; got:\n%s", want, out)
		}
	}
	for _, reject := range []string{"completed", "failed", "stopped"} {
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
		id  = "SUP-674"
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
