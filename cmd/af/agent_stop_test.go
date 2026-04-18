package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

func TestAgentStopHelp(t *testing.T) {
	t.Parallel()

	cmd, buf := newAgentTestCmd([]string{"stop", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "<session-id>") {
		t.Errorf("stop --help missing '<session-id>' in usage; got:\n%s", out)
	}
}

func TestAgentStopMissingArg(t *testing.T) {
	t.Parallel()

	cmd, _ := newAgentTestCmd([]string{"stop"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing session-id, got nil")
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("expected cobra ExactArgs(1) error; got: %v", err)
	}
}

func TestAgentStopMockHumanMode(t *testing.T) {
	t.Parallel()

	cmd, buf := newAgentTestCmd([]string{"stop", "mock-001", "--mock"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()

	// Single concise line with the transition.
	if !strings.Contains(out, "Stopped mock-001") {
		t.Errorf("expected 'Stopped mock-001' in output; got:\n%s", out)
	}
	// Previous status for mock-001 is "working"; new status is "stopped".
	if !strings.Contains(out, "working") || !strings.Contains(out, "stopped") {
		t.Errorf("expected 'working → stopped' transition; got:\n%s", out)
	}
}

func TestAgentStopMockJSONMode(t *testing.T) {
	t.Parallel()

	cmd, buf := newAgentTestCmd([]string{"stop", "mock-001", "--mock", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	var resp afclient.StopSessionResponse
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if !resp.Stopped {
		t.Errorf("expected Stopped=true; got: %+v", resp)
	}
	if resp.SessionID != "mock-001" {
		t.Errorf("expected SessionID 'mock-001'; got %q", resp.SessionID)
	}
	if resp.NewStatus != afclient.StatusStopped {
		t.Errorf("expected NewStatus 'stopped'; got %q", resp.NewStatus)
	}
	// Indented output check: the encoder emits a leading "{\n" then a
	// 2-space indented field on the next line.
	if !strings.Contains(buf.String(), "\n  \"stopped\"") &&
		!strings.Contains(buf.String(), "\n  \"sessionId\"") {
		t.Errorf("expected indented JSON output; got:\n%s", buf.String())
	}
}

func TestAgentStopMockNotFound(t *testing.T) {
	t.Parallel()

	cmd, _ := newAgentTestCmd([]string{"stop", "nope", "--mock"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for unknown session, got nil")
	}
	if !errors.Is(err, afclient.ErrNotFound) {
		t.Errorf("expected errors.Is(err, afclient.ErrNotFound); got: %v", err)
	}
	if !strings.Contains(err.Error(), "session not found") {
		t.Errorf("expected 'session not found' in error; got: %v", err)
	}
}

func TestAgentStopHTTPServerError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/stop") {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	cmd, _ := newAgentTestCmd([]string{"stop", "sess-1", "--url", srv.URL})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from 500, got nil")
	}
	if !strings.Contains(err.Error(), "stop agent sess-1") {
		t.Errorf("expected wrapped 'stop agent sess-1'; got: %v", err)
	}
	if !errors.Is(err, afclient.ErrServerError) {
		t.Errorf("expected errors.Is(err, afclient.ErrServerError); got: %v", err)
	}
}

func TestAgentStopHTTPNotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	cmd, _ := newAgentTestCmd([]string{"stop", "sess-2", "--url", srv.URL})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from 404, got nil")
	}
	if !errors.Is(err, afclient.ErrNotFound) {
		t.Errorf("expected errors.Is(err, afclient.ErrNotFound); got: %v", err)
	}
	if !strings.Contains(err.Error(), "session not found") {
		t.Errorf("expected 'session not found' messaging; got: %v", err)
	}
}
