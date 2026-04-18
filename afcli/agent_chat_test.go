package afcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// chatStubDataSource is a DataSource stub specialized for agent chat
// tests. It exposes an injectable ForwardPrompt response and error plus
// a call counter so the empty-message test can assert the RPC was never
// invoked. All other methods delegate to the shared stubDataSource
// zero-value behaviour to satisfy the interface.
type chatStubDataSource struct {
	stubDataSource
	forwardResp   *afclient.ForwardPromptResponse
	forwardErr    error
	forwardCalls  int
	failIfInvoked *testing.T
}

func (c *chatStubDataSource) ForwardPrompt(_ afclient.ForwardPromptRequest) (*afclient.ForwardPromptResponse, error) {
	c.forwardCalls++
	if c.failIfInvoked != nil {
		c.failIfInvoked.Fatal("ForwardPrompt must not be called; empty-message guard should have rejected the request first")
	}
	if c.forwardErr != nil {
		return nil, c.forwardErr
	}
	if c.forwardResp != nil {
		return c.forwardResp, nil
	}
	return &afclient.ForwardPromptResponse{}, nil
}

// runChatWithStub mirrors newAgentChatCmd's flag surface and logic with
// an injected DataSource so tests can exercise error propagation and
// RPC-avoidance guarantees without touching MockClient.
func runChatWithStub(t *testing.T, ds afclient.DataSource, args []string) (string, error) {
	t.Helper()

	var jsonMode bool
	cmd := &cobra.Command{
		Use:           "chat",
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			taskID := args[0]
			message := args[1]
			if strings.TrimSpace(message) == "" {
				return errors.New("message must not be empty")
			}
			resp, err := ds.ForwardPrompt(afclient.ForwardPromptRequest{TaskID: taskID, Message: message})
			if err != nil {
				return fmt.Errorf("forward prompt: %w", err)
			}
			out := cmd.OutOrStdout()
			if jsonMode {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}
			_, _ = fmt.Fprintf(out, "forwarded prompt %s to %s (status: %s)\n",
				resp.PromptID, resp.TaskID, resp.SessionStatus)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonMode, "json", false, "")

	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func TestAgentChatHelp(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"chat", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "<session-id>") || !strings.Contains(out, "<message>") {
		t.Errorf("chat --help missing '<session-id> <message>' usage; got:\n%s", out)
	}
	if !strings.Contains(out, "--json") {
		t.Errorf("chat --help missing --json flag; got:\n%s", out)
	}
}

func TestAgentParentHelpListsChat(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	for _, sub := range []string{"chat", "list", "status", "stop"} {
		if !strings.Contains(out, sub) {
			t.Errorf("agent --help missing %q subcommand listing; got:\n%s", sub, out)
		}
	}
}

func TestAgentChatArgValidation(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }

	tests := []struct {
		name string
		args []string
	}{
		{name: "zero_args", args: []string{"chat"}},
		{name: "one_arg", args: []string{"chat", "SUP-674"}},
		{name: "three_args", args: []string{"chat", "SUP-674", "hello", "world"}},
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
			if !strings.Contains(err.Error(), "accepts 2 arg") {
				t.Errorf("expected cobra ExactArgs(2) error; got: %v", err)
			}
		})
	}
}

func TestAgentChatEmptyMessageSkipsRPC(t *testing.T) {
	t.Parallel()

	messages := []struct {
		name    string
		message string
	}{
		{name: "empty", message: ""},
		{name: "spaces", message: "   "},
		{name: "tabs_newlines", message: "\t\n"},
	}
	for _, m := range messages {
		m := m
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			ds := &chatStubDataSource{failIfInvoked: t}
			_, err := runChatWithStub(t, ds, []string{"SUP-674", m.message})
			if err == nil {
				t.Fatal("expected error for empty message, got nil")
			}
			if !strings.Contains(err.Error(), "message must not be empty") {
				t.Errorf("expected 'message must not be empty' in error; got: %v", err)
			}
			if ds.forwardCalls != 0 {
				t.Errorf("ForwardPrompt call count = %d, want 0", ds.forwardCalls)
			}
		})
	}
}

func TestAgentChatMockHumanMode(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"chat", "SUP-674", "hello"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	const want = "forwarded prompt mock-prm-1 to SUP-674 (status: running)\n"
	if got := buf.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestAgentChatMockJSONMode(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"chat", "--json", "SUP-674", "hello"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	var resp afclient.ForwardPromptResponse
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if !resp.Forwarded {
		t.Errorf("Forwarded = false, want true")
	}
	if resp.PromptID != "mock-prm-1" {
		t.Errorf("PromptID = %q, want %q", resp.PromptID, "mock-prm-1")
	}
	if resp.TaskID != "SUP-674" {
		t.Errorf("TaskID = %q, want %q", resp.TaskID, "SUP-674")
	}
	if resp.IssueID != "SUP-674" {
		t.Errorf("IssueID = %q, want %q", resp.IssueID, "SUP-674")
	}
	if resp.SessionStatus != "running" {
		t.Errorf("SessionStatus = %q, want %q", resp.SessionStatus, "running")
	}
	if !strings.Contains(buf.String(), "\n  \"promptId\"") {
		t.Errorf("expected indented JSON output; got:\n%s", buf.String())
	}
}

func TestAgentChatSentinelPropagation(t *testing.T) {
	t.Parallel()

	ds := &chatStubDataSource{forwardErr: afclient.ErrNotFound}
	_, err := runChatWithStub(t, ds, []string{"SUP-674", "hello"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, afclient.ErrNotFound) {
		t.Errorf("errors.Is(err, afclient.ErrNotFound) = false; err = %v", err)
	}
	if !strings.Contains(err.Error(), "forward prompt") {
		t.Errorf("expected 'forward prompt' prefix; got: %v", err)
	}
}

func TestAgentChatHTTPNotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	client := afclient.NewClient(srv.URL)
	ds := func() afclient.DataSource { return client }
	cmd, _ := newTestAgentCmd(ds, []string{"chat", "SUP-674", "hello"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	if !errors.Is(err, afclient.ErrNotFound) {
		t.Errorf("expected errors.Is(err, afclient.ErrNotFound); got: %v", err)
	}
	if !strings.Contains(err.Error(), "forward prompt") {
		t.Errorf("expected 'forward prompt' wrap in error; got: %v", err)
	}
}
