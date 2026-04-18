package main

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

// newAgentTestCmd builds a fresh `af agent ...` command tree wired to
// MockClient. Output/err are captured in the returned buffer via Cobra's
// SetOut/SetErr (no global stdout redirection). The caller passes args
// beginning with the subcommand, e.g. []string{"list", "--mock"}.
func newAgentTestCmd(args []string) (*cobra.Command, *bytes.Buffer) {
	cmd, _ := newRootCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(append([]string{"agent"}, args...))
	return cmd, buf
}

// stubDataSource is a local afclient.DataSource stub for tests that need to
// control the GetSessions result (e.g., injecting errors, empty lists,
// or custom fixtures). All other methods are no-ops returning zero
// values. We do not modify afclient.MockClient for these cases.
type stubDataSource struct {
	sessions []afclient.SessionResponse
	err      error
}

func (s *stubDataSource) GetStats() (*afclient.StatsResponse, error) {
	return &afclient.StatsResponse{}, nil
}

func (s *stubDataSource) GetSessions() (*afclient.SessionsListResponse, error) {
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

// runListWithStub builds the list subcommand with a custom DataSource
// stub so we can exercise error paths and empty-result paths without
// touching MockClient. It bypasses the factory's mock/url branching by
// constructing the RunE's effective behavior via a thin wrapper command
// that shares the same output formatting code paths.
func runListWithStub(t *testing.T, ds afclient.DataSource, args []string) (string, error) {
	t.Helper()

	// Mirror newAgentListCmd's flag surface + logic, substituting the
	// injected DataSource. We call the same helpers (filterSessions,
	// writeSessionTable) to keep coverage flowing through the real
	// formatting code.
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

	cmd, buf := newAgentTestCmd([]string{"list", "--mock"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()

	// Active statuses should be present.
	for _, want := range []string{"working", "queued", "parked"} {
		if !strings.Contains(out, want) {
			t.Errorf("active output missing status %q; got:\n%s", want, out)
		}
	}
	// Terminal statuses must NOT be present in default output.
	for _, reject := range []string{"completed", "failed", "stopped"} {
		if strings.Contains(out, reject) {
			t.Errorf("active-only output should not contain %q; got:\n%s", reject, out)
		}
	}
	// Header columns.
	for _, want := range []string{"SESSION ID", "IDENTIFIER", "STATUS", "DURATION", "WORK TYPE"} {
		if !strings.Contains(out, want) {
			t.Errorf("header missing %q; got:\n%s", want, out)
		}
	}
}

func TestAgentListAllFlag(t *testing.T) {
	t.Parallel()

	cmd, buf := newAgentTestCmd([]string{"list", "--mock", "--all"})
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

	cmd, buf := newAgentTestCmd([]string{"list", "--mock", "--json"})
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
	// Indented JSON: the encoder emits a leading "{\n" then a 2-space
	// indented field on the next line.
	if !strings.Contains(out, "\n  \"sessions\"") && !strings.Contains(out, "\n  \"count\"") {
		t.Errorf("expected indented JSON output, got:\n%s", out)
	}
}

func TestAgentListJSONAll(t *testing.T) {
	t.Parallel()

	cmd, buf := newAgentTestCmd([]string{"list", "--mock", "--json", "--all"})
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
	// --all should include at least one terminal status from mock data.
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

	cmd, buf := newAgentTestCmd([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), "list") {
		t.Errorf("agent --help missing 'list' subcommand; got:\n%s", buf.String())
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
