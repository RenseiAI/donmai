package afcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// stubDataSource is a local afclient.DataSource stub for tests that need to
// control the GetSessions result (e.g., injecting errors, empty lists,
// or custom fixtures). All other methods are no-ops returning zero
// values. We do not modify afclient.MockClient for these cases.
type stubDataSource struct {
	sessions         []afclient.SessionResponse
	err              error
	filteredProjects []string // records each project passed to GetSessionsFiltered
	plainCalls       int      // records calls to GetSessions (no project)
}

func (s *stubDataSource) GetStats() (*afclient.StatsResponse, error) {
	return &afclient.StatsResponse{}, nil
}

func (s *stubDataSource) GetSessions() (*afclient.SessionsListResponse, error) {
	s.plainCalls++
	if s.err != nil {
		return nil, s.err
	}
	return &afclient.SessionsListResponse{
		Sessions:  s.sessions,
		Count:     len(s.sessions),
		Timestamp: "2026-04-17T00:00:00Z",
	}, nil
}

func (s *stubDataSource) GetSessionsFiltered(project string) (*afclient.SessionsListResponse, error) {
	s.filteredProjects = append(s.filteredProjects, project)
	if s.err != nil {
		return nil, s.err
	}
	return &afclient.SessionsListResponse{
		Sessions:  s.sessions,
		Count:     len(s.sessions),
		Timestamp: "2026-04-17T00:00:00Z",
	}, nil
}

func (s *stubDataSource) GetSessionDetail(_ string) (*afclient.SessionDetailResponse, error) {
	return nil, afclient.ErrNotFound
}

func (s *stubDataSource) GetActivities(_ string, _ *string) (*afclient.ActivityListResponse, error) {
	return &afclient.ActivityListResponse{}, nil
}

func (s *stubDataSource) StopSession(_ string) (*afclient.StopSessionResponse, error) {
	return nil, nil
}

func (s *stubDataSource) ChatSession(_ string, _ afclient.ChatSessionRequest) (*afclient.ChatSessionResponse, error) {
	return nil, nil
}

func (s *stubDataSource) ReconnectSession(_ string, _ afclient.ReconnectSessionRequest) (*afclient.ReconnectSessionResponse, error) {
	return nil, nil
}

func (s *stubDataSource) GetCostReport() (*afclient.CostReportResponse, error) {
	return &afclient.CostReportResponse{}, nil
}

func (s *stubDataSource) ListFleet() (*afclient.ListFleetResponse, error) {
	return &afclient.ListFleetResponse{}, nil
}

func (s *stubDataSource) SubmitTask(_ afclient.SubmitTaskRequest) (*afclient.SubmitTaskResponse, error) {
	return &afclient.SubmitTaskResponse{}, nil
}

func (s *stubDataSource) StopAgent(_ afclient.StopAgentRequest) (*afclient.StopAgentResponse, error) {
	return &afclient.StopAgentResponse{}, nil
}

func (s *stubDataSource) ForwardPrompt(_ afclient.ForwardPromptRequest) (*afclient.ForwardPromptResponse, error) {
	return &afclient.ForwardPromptResponse{}, nil
}

// newTestAgentCmd builds a fresh agent command tree wired to the given
// DataSource factory. Output/err are captured in the returned buffer.
// No project scoping is applied.
func newTestAgentCmd(ds func() afclient.DataSource, args []string) (*cobra.Command, *bytes.Buffer) {
	return newTestAgentCmdWithProject(ds, nil, args)
}

// newTestAgentCmdWithProject is like newTestAgentCmd but lets the caller
// inject a ProjectFunc to exercise project-scoping paths.
func newTestAgentCmdWithProject(ds func() afclient.DataSource, projectFunc func() string, args []string) (*cobra.Command, *bytes.Buffer) {
	cmd := newAgentCmd(ds, projectFunc)
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)
	return cmd, buf
}

// runListWithStub builds the list subcommand with a custom DataSource
// stub so we can exercise error paths and empty-result paths without
// touching MockClient.
func runListWithStub(t *testing.T, ds afclient.DataSource, args []string) (string, error) {
	t.Helper()

	var (
		allMode  bool
		jsonMode bool
	)
	cmd := &cobra.Command{
		Use: "list",
		RunE: func(cmd *cobra.Command, _ []string) error {
			resp, err := ds.GetSessions()
			if err != nil {
				return fmt.Errorf("get sessions: %w", err)
			}
			filtered := filterSessions(resp.Sessions, allMode)
			out := cmd.OutOrStdout()
			if jsonMode {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				payload := afclient.SessionsListResponse{
					Sessions:  filtered,
					Count:     len(filtered),
					Timestamp: resp.Timestamp,
				}
				return enc.Encode(payload)
			}
			if len(filtered) == 0 {
				if allMode {
					_, _ = fmt.Fprintln(out, "No sessions.")
				} else {
					_, _ = fmt.Fprintln(out, "No active sessions.")
				}
				return nil
			}
			return writeSessionTable(out, filtered)
		},
	}
	cmd.Flags().BoolVar(&allMode, "all", false, "")
	cmd.Flags().BoolVar(&jsonMode, "json", false, "")
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func TestAgentListActiveOnlyDefault(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()

	for _, want := range []string{"working", "queued", "parked"} {
		if !strings.Contains(out, want) {
			t.Errorf("active output missing status %q; got:\n%s", want, out)
		}
	}
	for _, reject := range []string{"completed", "failed", "stopped"} {
		if strings.Contains(out, reject) {
			t.Errorf("active-only output should not contain %q; got:\n%s", reject, out)
		}
	}
	for _, want := range []string{"SESSION ID", "IDENTIFIER", "STATUS", "DURATION", "WORK TYPE"} {
		if !strings.Contains(out, want) {
			t.Errorf("header missing %q; got:\n%s", want, out)
		}
	}
}

func TestAgentListAllFlag(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"list", "--all"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()

	for _, want := range []string{"working", "queued", "parked", "completed", "failed", "stopped"} {
		if !strings.Contains(out, want) {
			t.Errorf("--all output missing status %q; got:\n%s", want, out)
		}
	}
}

func TestAgentListJSONDefault(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"list", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()

	var resp afclient.SessionsListResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}
	if resp.Count != len(resp.Sessions) {
		t.Errorf("count %d != len(sessions) %d", resp.Count, len(resp.Sessions))
	}
	if len(resp.Sessions) == 0 {
		t.Fatal("expected some active sessions in mock data")
	}
	for _, s := range resp.Sessions {
		if !isActive(s.Status) {
			t.Errorf("json default included non-active session %q with status %q", s.ID, s.Status)
		}
	}
	if !strings.Contains(out, "\n  \"sessions\"") && !strings.Contains(out, "\n  \"count\"") {
		t.Errorf("expected indented JSON output, got:\n%s", out)
	}
}

func TestAgentListJSONAll(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"list", "--json", "--all"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	var resp afclient.SessionsListResponse
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	seen := map[afclient.SessionStatus]bool{}
	for _, s := range resp.Sessions {
		seen[s.Status] = true
	}
	if !seen[afclient.StatusCompleted] && !seen[afclient.StatusFailed] && !seen[afclient.StatusStopped] {
		t.Errorf("--all JSON missing terminal statuses; seen: %v", seen)
	}
}

func TestAgentListEmptyActive(t *testing.T) {
	t.Parallel()

	ds := &stubDataSource{sessions: []afclient.SessionResponse{
		{ID: "a", Identifier: "X-1", Status: afclient.StatusCompleted, WorkType: "dev", Duration: 10},
		{ID: "b", Identifier: "X-2", Status: afclient.StatusFailed, WorkType: "qa", Duration: 20},
	}}

	out, err := runListWithStub(t, ds, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "No active sessions.") {
		t.Errorf("expected 'No active sessions.' line, got:\n%s", out)
	}
}

func TestAgentListErrorPropagation(t *testing.T) {
	t.Parallel()

	sentinel := afclient.ErrServerError
	ds := &stubDataSource{err: fmt.Errorf("api call: %w", sentinel)}

	_, err := runListWithStub(t, ds, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("errors.Is(err, ErrServerError) = false; err: %v", err)
	}
	if !strings.Contains(err.Error(), "get sessions") {
		t.Errorf("expected wrapped error to contain 'get sessions'; got: %v", err)
	}
}

func TestIsActive(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status afclient.SessionStatus
		want   bool
	}{
		{afclient.StatusQueued, true},
		{afclient.StatusParked, true},
		{afclient.StatusWorking, true},
		{afclient.StatusCompleted, false},
		{afclient.StatusFailed, false},
		{afclient.StatusStopped, false},
		{afclient.SessionStatus("unknown"), false},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			t.Parallel()
			if got := isActive(tc.status); got != tc.want {
				t.Errorf("isActive(%q) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

func TestAgentListEmptyAll(t *testing.T) {
	t.Parallel()

	ds := &stubDataSource{sessions: nil}

	out, err := runListWithStub(t, ds, []string{"--all"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out, "No sessions.") {
		t.Errorf("expected 'No sessions.' line with --all, got:\n%s", out)
	}
}

func TestAgentParentHelp(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), "list") {
		t.Errorf("agent --help missing 'list' subcommand; got:\n%s", buf.String())
	}
}

func TestAgentListProjectScoping(t *testing.T) {
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

			cmd, _ := newTestAgentCmdWithProject(ds, tc.projectFunc, []string{"list"})
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

func TestFormatDuration(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   int
		want string
	}{
		{0, "0s"},
		{-5, "0s"},
		{45, "45s"},
		{60, "1m"},
		{125, "2m5s"},
		{3600, "1h"},
		{3725, "1h2m5s"},
		{3660, "1h1m"},
		{3605, "1h5s"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("%d", tc.in), func(t *testing.T) {
			t.Parallel()
			if got := formatDuration(tc.in); got != tc.want {
				t.Errorf("formatDuration(%d) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
