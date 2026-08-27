package afcli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
)

func TestAgentStopHelp(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"stop", "--help"})
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

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, _ := newTestAgentCmd(ds, []string{"stop"})
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

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"stop", "mock-001"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "Stopped mock-001") {
		t.Errorf("expected 'Stopped mock-001' in output; got:\n%s", out)
	}
	if !strings.Contains(out, "working") || !strings.Contains(out, "stopped") {
		t.Errorf("expected 'working -> stopped' transition; got:\n%s", out)
	}
}

func TestAgentStopMockJSONMode(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"stop", "mock-001", "--json"})
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
	if !strings.Contains(buf.String(), "\n  \"stopped\"") &&
		!strings.Contains(buf.String(), "\n  \"sessionId\"") {
		t.Errorf("expected indented JSON output; got:\n%s", buf.String())
	}
}

func TestAgentStopMockNotFound(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, _ := newTestAgentCmd(ds, []string{"stop", "nope"})
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

	client := afclient.NewClient(srv.URL)
	ds := func() afclient.DataSource { return client }
	cmd, _ := newTestAgentCmd(ds, []string{"stop", "sess-1"})
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

	client := afclient.NewClient(srv.URL)
	ds := func() afclient.DataSource { return client }
	cmd, _ := newTestAgentCmd(ds, []string{"stop", "sess-2"})
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

func TestAgentStopJSONPreservesTypedReconciliationRequiredReceipt(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/stop") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"stopped":false,"sessionId":"sess-live-red","previousStatus":"starting","code":"SESSION_LIFECYCLE_RECONCILIATION_REQUIRED","refusal":"session_lifecycle_reconciliation_required","retryable":false,"disposition":"reconciliation_required","ownerLiveness":"unverified","preparedAgeMs":19019000,"mutationId":"cleanup:claimed-stale:sess-live-red"}`))
	}))
	t.Cleanup(srv.Close)

	client := afclient.NewClient(srv.URL)
	ds := func() afclient.DataSource { return client }
	cmd := newAgentStopCmd(ds)
	var stdout strings.Builder
	var stderr strings.Builder
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"sess-live-red", "--json"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected typed 409 to retain a nonzero command result")
	}
	if !errors.Is(err, afclient.ErrConflict) {
		t.Fatalf("expected errors.Is(err, ErrConflict); got: %v", err)
	}
	var receipt map[string]any
	if decodeErr := json.Unmarshal([]byte(stdout.String()), &receipt); decodeErr != nil {
		t.Fatalf("typed 409 did not emit machine JSON: %v\nstdout=%q\nstderr=%q\nerror=%v", decodeErr, stdout.String(), stderr.String(), err)
	}
	if receipt["code"] != "SESSION_LIFECYCLE_RECONCILIATION_REQUIRED" ||
		receipt["disposition"] != "reconciliation_required" ||
		receipt["ownerLiveness"] != "unverified" || receipt["retryable"] != false {
		t.Fatalf("typed receipt fields lost: %#v", receipt)
	}
	if _, ok := receipt["retryAfterSeconds"]; ok {
		t.Fatalf("reconciliation-required receipt invented Retry-After: %#v", receipt)
	}
	if !strings.Contains(err.Error(), "reconciliation_required") {
		t.Fatalf("human error omitted typed disposition: %v", err)
	}
}

func TestAgentStopJSONPreservesDurableSuccessReceipt(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/stop") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stopped":true,"sessionId":"public-hash","previousStatus":"claimed","newStatus":"stopped","receipt":{"version":1,"kind":"session_stop","sessionId":"storage-uuid","mutationId":"linear-stop:reconcile:7:storage-uuid","intentRevision":"7","disposition":"stopped","idempotentReplay":false}}`))
	}))
	t.Cleanup(srv.Close)

	client := afclient.NewClient(srv.URL)
	ds := func() afclient.DataSource { return client }
	cmd := newAgentStopCmd(ds)
	var stdout strings.Builder
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"public-hash", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var output map[string]any
	if err := json.Unmarshal([]byte(stdout.String()), &output); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	receipt, ok := output["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("durable success receipt was dropped: %#v", output)
	}
	if output["sessionId"] != "public-hash" || receipt["sessionId"] != "storage-uuid" ||
		receipt["mutationId"] != "linear-stop:reconcile:7:storage-uuid" ||
		receipt["intentRevision"] != "7" || receipt["disposition"] != "stopped" ||
		receipt["idempotentReplay"] != false {
		t.Fatalf("receipt identity/truth changed: %#v", output)
	}
}

func TestAgentStopJSONRejectsSecretBearingSuccessReceiptWithoutEcho(t *testing.T) {
	t.Parallel()
	const secret = "rsk_do_not_echo"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stopped":true,"sessionId":"public-hash","previousStatus":"claimed","newStatus":"stopped","receipt":{"version":1,"kind":"session_stop","sessionId":"` + secret + `","mutationId":"stop:storage","intentRevision":"7","disposition":"stopped","idempotentReplay":false}}`))
	}))
	t.Cleanup(srv.Close)
	client := afclient.NewClient(srv.URL)
	cmd := newAgentStopCmd(func() afclient.DataSource { return client })
	var stdout strings.Builder
	var stderr strings.Builder
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"public-hash", "--json"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("secret-bearing success receipt decoded without error")
	}
	combined := stdout.String() + stderr.String() + err.Error()
	if strings.Contains(combined, secret) {
		t.Fatalf("secret reflected in output: %q", combined)
	}
	if stdout.Len() != 0 {
		t.Fatalf("failed success receipt emitted stdout: %q", stdout.String())
	}
}
