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

	"github.com/RenseiAI/donmai/afclient"
)

// chatStubDataSource is a DataSource stub specialized for agent chat
// tests. It exposes an injectable ChatSession response and error plus a
// call counter so the empty-message test can assert the RPC was never
// invoked. All other methods delegate to the shared stubDataSource
// zero-value behaviour to satisfy the interface.
type chatStubDataSource struct {
	stubDataSource
	chatResp      *afclient.ChatSessionResponse
	chatErr       error
	chatCalls     int
	lastID        string
	lastPrompt    string
	failIfInvoked *testing.T
}

func (c *chatStubDataSource) ChatSession(id string, req afclient.ChatSessionRequest) (*afclient.ChatSessionResponse, error) {
	c.chatCalls++
	c.lastID = id
	c.lastPrompt = req.Prompt
	if c.failIfInvoked != nil {
		c.failIfInvoked.Fatal("ChatSession must not be called; empty-message guard should have rejected the request first")
	}
	if c.chatErr != nil {
		return nil, c.chatErr
	}
	if c.chatResp != nil {
		return c.chatResp, nil
	}
	return &afclient.ChatSessionResponse{}, nil
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
		{name: "one_arg", args: []string{"chat", "mock-001"}},
		{name: "three_args", args: []string{"chat", "mock-001", "hello", "world"}},
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
			cmd, _ := newTestAgentCmd(func() afclient.DataSource { return ds }, []string{"chat", "mock-001", m.message})
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected error for empty message, got nil")
			}
			if !strings.Contains(err.Error(), "message must not be empty") {
				t.Errorf("expected 'message must not be empty' in error; got: %v", err)
			}
			if ds.chatCalls != 0 {
				t.Errorf("ChatSession call count = %d, want 0", ds.chatCalls)
			}
		})
	}
}

// TestAgentChatInvokesChatSession asserts the command routes the prompt to
// ChatSession with the session id and message verbatim.
func TestAgentChatInvokesChatSession(t *testing.T) {
	t.Parallel()

	ds := &chatStubDataSource{chatResp: &afclient.ChatSessionResponse{
		Delivered:     true,
		PromptID:      "prm-1",
		SessionID:     "sess-abc",
		SessionStatus: afclient.StatusWorking,
	}}
	cmd, buf := newTestAgentCmd(func() afclient.DataSource { return ds }, []string{"chat", "sess-abc", "hello there"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if ds.chatCalls != 1 {
		t.Errorf("ChatSession call count = %d, want 1", ds.chatCalls)
	}
	if ds.lastID != "sess-abc" {
		t.Errorf("ChatSession id = %q, want %q", ds.lastID, "sess-abc")
	}
	if ds.lastPrompt != "hello there" {
		t.Errorf("ChatSession prompt = %q, want %q", ds.lastPrompt, "hello there")
	}
	const want = "Prompt prm-1 delivered to sess-abc (status: working)\n"
	if got := buf.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

// TestAgentChatQueuedVerb asserts the human-mode line reads "queued" when the
// platform accepted but did not yet deliver the prompt.
func TestAgentChatQueuedVerb(t *testing.T) {
	t.Parallel()

	ds := &chatStubDataSource{chatResp: &afclient.ChatSessionResponse{
		Delivered:     false,
		PromptID:      "prm-2",
		SessionID:     "sess-xyz",
		SessionStatus: afclient.StatusQueued,
	}}
	cmd, buf := newTestAgentCmd(func() afclient.DataSource { return ds }, []string{"chat", "sess-xyz", "later"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	const want = "Prompt prm-2 queued to sess-xyz (status: queued)\n"
	if got := buf.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestAgentChatMockHumanMode(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"chat", "mock-001", "hello"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	const want = "Prompt mock-prompt-mock-001 delivered to mock-001 (status: working)\n"
	if got := buf.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestAgentChatMockJSONMode(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"chat", "--json", "mock-001", "hello"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	var resp afclient.ChatSessionResponse
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if !resp.Delivered {
		t.Errorf("Delivered = false, want true")
	}
	if resp.PromptID != "mock-prompt-mock-001" {
		t.Errorf("PromptID = %q, want %q", resp.PromptID, "mock-prompt-mock-001")
	}
	if resp.SessionID != "mock-001" {
		t.Errorf("SessionID = %q, want %q", resp.SessionID, "mock-001")
	}
	if resp.SessionStatus != afclient.StatusWorking {
		t.Errorf("SessionStatus = %q, want %q", resp.SessionStatus, afclient.StatusWorking)
	}
	if !strings.Contains(buf.String(), "\n  \"promptId\"") {
		t.Errorf("expected indented JSON output; got:\n%s", buf.String())
	}
}

// TestAgentChatNotFoundFriendly asserts an unknown id yields a clear
// "session not found" message and preserves the ErrNotFound sentinel.
func TestAgentChatNotFoundFriendly(t *testing.T) {
	t.Parallel()

	ds := &chatStubDataSource{chatErr: afclient.ErrNotFound}
	cmd, _ := newTestAgentCmd(func() afclient.DataSource { return ds }, []string{"chat", "ghost", "hello"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, afclient.ErrNotFound) {
		t.Errorf("errors.Is(err, afclient.ErrNotFound) = false; err = %v", err)
	}
	if !strings.Contains(err.Error(), "session not found") {
		t.Errorf("expected 'session not found' in error; got: %v", err)
	}
	if !strings.Contains(err.Error(), "prompt session ghost") {
		t.Errorf("expected 'prompt session ghost' prefix; got: %v", err)
	}
}

// TestAgentChatGenericErrorWrap asserts a non-sentinel error is wrapped with
// the prompt-session context (and is not mislabeled "not found").
func TestAgentChatGenericErrorWrap(t *testing.T) {
	t.Parallel()

	ds := &chatStubDataSource{chatErr: fmt.Errorf("boom")}
	cmd, _ := newTestAgentCmd(func() afclient.DataSource { return ds }, []string{"chat", "sess-1", "hello"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), "session not found") {
		t.Errorf("non-sentinel error must not be labeled 'session not found'; got: %v", err)
	}
	if !strings.Contains(err.Error(), "prompt session sess-1") {
		t.Errorf("expected 'prompt session sess-1' prefix; got: %v", err)
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
	cmd, _ := newTestAgentCmd(ds, []string{"chat", "sess-1", "hello"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	if !errors.Is(err, afclient.ErrNotFound) {
		t.Errorf("expected errors.Is(err, afclient.ErrNotFound); got: %v", err)
	}
	if !strings.Contains(err.Error(), "session not found") {
		t.Errorf("expected friendly 'session not found' wrap in error; got: %v", err)
	}
}

// TestAgentChatHTTPPostsToPublicEndpoint asserts the live HTTP path targets
// POST /api/public/sessions/:id/prompt (not the dead /api/mcp/forward-prompt).
func TestAgentChatHTTPPostsToPublicEndpoint(t *testing.T) {
	t.Parallel()

	var gotPath, gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		buf := &bytes.Buffer{}
		_, _ = buf.ReadFrom(r.Body)
		gotBody = buf.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"delivered":true,"promptId":"p","sessionId":"sess-1","sessionStatus":"working"}`))
	}))
	t.Cleanup(srv.Close)

	client := afclient.NewClient(srv.URL)
	ds := func() afclient.DataSource { return client }
	cmd, _ := newTestAgentCmd(ds, []string{"chat", "sess-1", "ping"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/public/sessions/sess-1/prompt" {
		t.Errorf("path = %q, want /api/public/sessions/sess-1/prompt", gotPath)
	}
	if !strings.Contains(gotBody, `"prompt":"ping"`) {
		t.Errorf("body = %q, want prompt:ping", gotBody)
	}
}

// runChatWithStub retained for documentation parity with the cobra wiring;
// asserts the empty-message guard short-circuits before any RPC. It mirrors
// newAgentChatCmd's flag surface using an injected DataSource.
func runChatWithStub(t *testing.T, ds afclient.DataSource, args []string) (string, error) {
	t.Helper()

	var jsonMode bool
	cmd := &cobra.Command{
		Use:           "chat",
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			message := args[1]
			if strings.TrimSpace(message) == "" {
				return errors.New("message must not be empty")
			}
			resp, err := ds.ChatSession(id, afclient.ChatSessionRequest{Prompt: message})
			if err != nil {
				if errors.Is(err, afclient.ErrNotFound) {
					return fmt.Errorf("prompt session %s: session not found: %w", id, err)
				}
				return fmt.Errorf("prompt session %s: %w", id, err)
			}
			out := cmd.OutOrStdout()
			if jsonMode {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(resp)
			}
			_, _ = fmt.Fprintf(out, "Prompt %s delivered to %s (status: %s)\n",
				resp.PromptID, resp.SessionID, resp.SessionStatus)
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

// TestAgentChatStubSentinelPropagation exercises the standalone stub wiring's
// ErrNotFound path so the sentinel survives the friendly wrap.
func TestAgentChatStubSentinelPropagation(t *testing.T) {
	t.Parallel()

	ds := &chatStubDataSource{chatErr: afclient.ErrNotFound}
	_, err := runChatWithStub(t, ds, []string{"ghost", "hello"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, afclient.ErrNotFound) {
		t.Errorf("errors.Is(err, afclient.ErrNotFound) = false; err = %v", err)
	}
	if !strings.Contains(err.Error(), "prompt session ghost") {
		t.Errorf("expected 'prompt session ghost' prefix; got: %v", err)
	}
}
