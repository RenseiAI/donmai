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

type reconnectStubDataSource struct {
	stubDataSource
	reconnectResp  *afclient.ReconnectSessionResponse
	reconnectErr   error
	reconnectCalls int
	lastID         string
	lastReq        afclient.ReconnectSessionRequest
	failIfInvoked  *testing.T
}

func (r *reconnectStubDataSource) ReconnectSession(id string, req afclient.ReconnectSessionRequest) (*afclient.ReconnectSessionResponse, error) {
	r.reconnectCalls++
	r.lastID = id
	r.lastReq = req
	if r.failIfInvoked != nil {
		r.failIfInvoked.Fatal("ReconnectSession must not be called; validation should have rejected the request first")
	}
	if r.reconnectErr != nil {
		return nil, r.reconnectErr
	}
	if r.reconnectResp != nil {
		return r.reconnectResp, nil
	}
	return &afclient.ReconnectSessionResponse{}, nil
}

func runReconnectWithStub(t *testing.T, ds afclient.DataSource, args []string) (string, error) {
	t.Helper()

	var (
		jsonMode    bool
		cursor      string
		lastEventID string
	)

	cmd := &cobra.Command{
		Use:           "reconnect",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if strings.TrimSpace(id) == "" {
				return errors.New("session id must not be empty")
			}

			req := afclient.ReconnectSessionRequest{}
			if cursor != "" {
				req.Cursor = &cursor
			}
			if lastEventID != "" {
				req.LastEventID = &lastEventID
			}

			resp, err := ds.ReconnectSession(id, req)
			if err != nil {
				return fmt.Errorf("reconnect session %s: %w", id, err)
			}
			if !resp.Reconnected {
				return fmt.Errorf("reconnect declined for %s", id)
			}

			out := cmd.OutOrStdout()
			if jsonMode {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}
			_, _ = fmt.Fprintf(out, "reconnected to %s (status: %s, missed: %d %s)\n",
				resp.SessionID, resp.SessionStatus, resp.MissedEvents, missedEventsNoun(resp.MissedEvents))
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonMode, "json", false, "")
	cmd.Flags().StringVar(&cursor, "cursor", "", "")
	cmd.Flags().StringVar(&lastEventID, "last-event-id", "", "")

	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func TestAgentReconnectHelp(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"reconnect", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"<session-id>", "--cursor", "--last-event-id", "--json"} {
		if !strings.Contains(out, want) {
			t.Errorf("reconnect --help missing %q; got:\n%s", want, out)
		}
	}
}

func TestAgentParentHelpListsReconnect(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	for _, sub := range []string{"chat", "list", "reconnect", "status", "stop"} {
		if !strings.Contains(out, sub) {
			t.Errorf("agent --help missing %q subcommand listing; got:\n%s", sub, out)
		}
	}
}

func TestAgentReconnectArgValidation(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }

	tests := []struct {
		name string
		args []string
	}{
		{name: "zero_args", args: []string{"reconnect"}},
		{name: "two_args", args: []string{"reconnect", "a", "b"}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd, _ := newTestAgentCmd(ds, tt.args)
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected error for wrong arg count, got nil")
			}
			if !strings.Contains(err.Error(), "accepts 1 arg") {
				t.Errorf("expected cobra ExactArgs(1) error; got: %v", err)
			}
		})
	}
}

func TestAgentReconnectEmptyIDSkipsRPC(t *testing.T) {
	t.Parallel()

	ds := &reconnectStubDataSource{failIfInvoked: t}
	_, err := runReconnectWithStub(t, ds, []string{"   "})
	if err == nil {
		t.Fatal("expected error for whitespace-only id, got nil")
	}
	if !strings.Contains(err.Error(), "session id must not be empty") {
		t.Errorf("expected 'session id must not be empty'; got: %v", err)
	}
	if ds.reconnectCalls != 0 {
		t.Errorf("ReconnectSession call count = %d, want 0", ds.reconnectCalls)
	}
}

func TestAgentReconnectEmptyIDCommandPath(t *testing.T) {
	t.Parallel()

	ds := &reconnectStubDataSource{failIfInvoked: t}
	cmd, _ := newTestAgentCmd(func() afclient.DataSource { return ds }, []string{"reconnect", "   "})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for whitespace-only id, got nil")
	}
	if !strings.Contains(err.Error(), "session id must not be empty") {
		t.Errorf("expected 'session id must not be empty'; got: %v", err)
	}
	if ds.reconnectCalls != 0 {
		t.Errorf("ReconnectSession call count = %d, want 0", ds.reconnectCalls)
	}
}

func TestAgentReconnectMockHumanMode(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"reconnect", "mock-001"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	const want = "reconnected to mock-001 (status: working, missed: 0 events)\n"
	if got := buf.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestAgentReconnectMockJSONMode(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"reconnect", "--json", "mock-001"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	var resp afclient.ReconnectSessionResponse
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if !resp.Reconnected {
		t.Errorf("Reconnected = false, want true")
	}
	if resp.SessionID != "mock-001" {
		t.Errorf("SessionID = %q, want %q", resp.SessionID, "mock-001")
	}
	if resp.SessionStatus != afclient.StatusWorking {
		t.Errorf("SessionStatus = %q, want %q", resp.SessionStatus, afclient.StatusWorking)
	}
	if resp.MissedEvents != 0 {
		t.Errorf("MissedEvents = %d, want 0", resp.MissedEvents)
	}
	if !strings.Contains(buf.String(), "\n  \"reconnected\"") {
		t.Errorf("expected indented JSON output; got:\n%s", buf.String())
	}
}

func TestAgentReconnectPassesResumeHints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		args            []string
		wantCursor      *string
		wantLastEventID *string
	}{
		{
			name:       "cursor_only",
			args:       []string{"--cursor", "2026-04-15T10:00:00Z", "SUP-674"},
			wantCursor: ptr("2026-04-15T10:00:00Z"),
		},
		{
			name:            "last_event_id_only",
			args:            []string{"--last-event-id", "evt_abc123", "SUP-674"},
			wantLastEventID: ptr("evt_abc123"),
		},
		{
			name:            "both_hints",
			args:            []string{"--cursor", "2026-04-15T10:00:00Z", "--last-event-id", "evt_abc123", "SUP-674"},
			wantCursor:      ptr("2026-04-15T10:00:00Z"),
			wantLastEventID: ptr("evt_abc123"),
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ds := &reconnectStubDataSource{
				reconnectResp: &afclient.ReconnectSessionResponse{
					Reconnected:   true,
					SessionID:     "SUP-674",
					SessionStatus: afclient.StatusWorking,
					MissedEvents:  2,
				},
			}

			out, err := runReconnectWithStub(t, ds, tc.args)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if ds.reconnectCalls != 1 {
				t.Fatalf("ReconnectSession call count = %d, want 1", ds.reconnectCalls)
			}
			if ds.lastID != "SUP-674" {
				t.Errorf("session id = %q, want %q", ds.lastID, "SUP-674")
			}
			switch {
			case tc.wantCursor == nil && ds.lastReq.Cursor != nil:
				t.Fatalf("cursor = %q, want nil", *ds.lastReq.Cursor)
			case tc.wantCursor != nil && (ds.lastReq.Cursor == nil || *ds.lastReq.Cursor != *tc.wantCursor):
				t.Fatalf("cursor = %v, want %q", ds.lastReq.Cursor, *tc.wantCursor)
			}
			switch {
			case tc.wantLastEventID == nil && ds.lastReq.LastEventID != nil:
				t.Fatalf("lastEventID = %q, want nil", *ds.lastReq.LastEventID)
			case tc.wantLastEventID != nil && (ds.lastReq.LastEventID == nil || *ds.lastReq.LastEventID != *tc.wantLastEventID):
				t.Fatalf("lastEventID = %v, want %q", ds.lastReq.LastEventID, *tc.wantLastEventID)
			}
			if out != "reconnected to SUP-674 (status: working, missed: 2 events)\n" {
				t.Errorf("unexpected output: %q", out)
			}
		})
	}
}

func TestAgentReconnectCommandPassesResumeHints(t *testing.T) {
	t.Parallel()

	ds := &reconnectStubDataSource{
		reconnectResp: &afclient.ReconnectSessionResponse{
			Reconnected:   true,
			SessionID:     "SUP-674",
			SessionStatus: afclient.StatusWorking,
			MissedEvents:  2,
		},
	}

	cmd, _ := newTestAgentCmd(func() afclient.DataSource { return ds }, []string{
		"reconnect",
		"--cursor", "2026-04-15T10:00:00Z",
		"--last-event-id", "evt_abc123",
		"SUP-674",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if ds.reconnectCalls != 1 {
		t.Fatalf("ReconnectSession call count = %d, want 1", ds.reconnectCalls)
	}
	if ds.lastReq.Cursor == nil || *ds.lastReq.Cursor != "2026-04-15T10:00:00Z" {
		t.Fatalf("cursor = %v, want %q", ds.lastReq.Cursor, "2026-04-15T10:00:00Z")
	}
	if ds.lastReq.LastEventID == nil || *ds.lastReq.LastEventID != "evt_abc123" {
		t.Fatalf("lastEventID = %v, want %q", ds.lastReq.LastEventID, "evt_abc123")
	}
}

func TestAgentReconnectSentinelPropagation(t *testing.T) {
	t.Parallel()

	ds := &reconnectStubDataSource{reconnectErr: afclient.ErrNotFound}
	_, err := runReconnectWithStub(t, ds, []string{"SUP-674"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, afclient.ErrNotFound) {
		t.Errorf("errors.Is(err, afclient.ErrNotFound) = false; err = %v", err)
	}
	if !strings.Contains(err.Error(), "reconnect session SUP-674") {
		t.Errorf("expected reconnect wrapper; got: %v", err)
	}
}

func TestAgentReconnectDeclined(t *testing.T) {
	t.Parallel()

	ds := &reconnectStubDataSource{
		reconnectResp: &afclient.ReconnectSessionResponse{
			Reconnected:   false,
			SessionID:     "SUP-674",
			SessionStatus: afclient.StatusQueued,
			MissedEvents:  0,
		},
	}

	out, err := runReconnectWithStub(t, ds, []string{"SUP-674"})
	if err == nil {
		t.Fatal("expected declined reconnect error, got nil")
	}
	if out != "" {
		t.Errorf("stdout = %q, want empty output", out)
	}
	if !strings.Contains(err.Error(), "reconnect declined for SUP-674") {
		t.Errorf("expected declined reconnect error; got: %v", err)
	}
}

func TestAgentReconnectCommandDeclined(t *testing.T) {
	t.Parallel()

	ds := &reconnectStubDataSource{
		reconnectResp: &afclient.ReconnectSessionResponse{
			Reconnected:   false,
			SessionID:     "SUP-674",
			SessionStatus: afclient.StatusQueued,
			MissedEvents:  0,
		},
	}

	cmd, _ := newTestAgentCmd(func() afclient.DataSource { return ds }, []string{"reconnect", "SUP-674"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected declined reconnect error, got nil")
	}
	if !strings.Contains(err.Error(), "reconnect declined for SUP-674") {
		t.Errorf("expected declined reconnect error; got: %v", err)
	}
}

func TestMissedEventsNoun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		n    int
		want string
	}{
		{n: 0, want: "events"},
		{n: 1, want: "event"},
		{n: 2, want: "events"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("%d", tc.n), func(t *testing.T) {
			t.Parallel()
			if got := missedEventsNoun(tc.n); got != tc.want {
				t.Errorf("missedEventsNoun(%d) = %q, want %q", tc.n, got, tc.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
