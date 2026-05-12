package afcli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

func TestAgentStatusMockHumanOutput(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"status", "mock-001"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()

	for _, label := range []string{
		"Session:",
		"Identifier:",
		"Status:",
		"Duration:",
		"Input Tokens:",
		"Output Tokens:",
		"Cost (USD):",
		"Current Activity:",
	} {
		if !strings.Contains(out, label) {
			t.Errorf("output missing label %q; got:\n%s", label, out)
		}
	}

	for _, want := range []string{
		"mock-001",
		"SUP-1180",
		"working",
		"45200",
		"12800",
		"$3.4200",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing value %q; got:\n%s", want, out)
		}
	}
}

func TestAgentStatusMockJSONOutput(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, buf := newTestAgentCmd(ds, []string{"status", "mock-001", "--json"})
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
	if got, _ := session["id"].(string); got != "mock-001" {
		t.Errorf("expected session.id %q, got %q", "mock-001", got)
	}
	if _, ok := payload["currentActivity"]; !ok {
		t.Errorf("expected top-level 'currentActivity' key; got: %v", payload)
	}

	if !strings.Contains(buf.String(), "\n  \"session\"") {
		t.Errorf("expected indented JSON output; got:\n%s", buf.String())
	}
}

func TestAgentStatusHTTPNotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	client := afclient.NewClient(srv.URL)
	ds := func() afclient.DataSource { return client }
	cmd, _ := newTestAgentCmd(ds, []string{"status", "sess-unknown"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from 404, got nil")
	}
	if !errors.Is(err, afclient.ErrNotFound) {
		t.Errorf("expected errors.Is(err, afclient.ErrNotFound); got: %v", err)
	}
	if !strings.Contains(err.Error(), "sess-unknown") {
		t.Errorf("expected session id in error message; got: %v", err)
	}
}

func TestAgentStatusNilPointerFields(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/activities"):
			_ = json.NewEncoder(w).Encode(afclient.ActivityListResponse{
				Activities:    []afclient.ActivityEvent{},
				SessionStatus: afclient.StatusQueued,
			})
		default:
			_ = json.NewEncoder(w).Encode(afclient.SessionDetailResponse{
				Session: afclient.SessionDetail{
					ID:         "sess-nils",
					Identifier: "ISSUE-1",
					Status:     afclient.StatusQueued,
					WorkType:   "development",
					Duration:   90,
				},
				Timestamp: "2026-04-17T00:00:00Z",
			})
		}
	}))
	t.Cleanup(srv.Close)

	t.Run("human", func(t *testing.T) {
		t.Parallel()
		client := afclient.NewClient(srv.URL)
		ds := func() afclient.DataSource { return client }
		cmd, buf := newTestAgentCmd(ds, []string{"status", "sess-nils"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
		out := buf.String()
		for _, line := range []string{
			"Input Tokens:",
			"Output Tokens:",
			"Cost (USD):",
			"Current Activity:",
		} {
			if !strings.Contains(out, line) {
				t.Errorf("missing row %q; got:\n%s", line, out)
			}
		}
		if got := strings.Count(out, "—"); got < 4 {
			t.Errorf("expected em-dash on >= 4 rows, got %d; out:\n%s", got, out)
		}
	})

	t.Run("json", func(t *testing.T) {
		t.Parallel()
		client := afclient.NewClient(srv.URL)
		ds := func() afclient.DataSource { return client }
		cmd, buf := newTestAgentCmd(ds, []string{"status", "sess-nils", "--json"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
		raw := buf.String()

		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, raw)
		}
		session, ok := payload["session"].(map[string]any)
		if !ok {
			t.Fatalf("missing 'session' object; got: %v", payload)
		}
		for _, key := range []string{"costUsd", "inputTokens", "outputTokens"} {
			if _, present := session[key]; present {
				t.Errorf("expected %q to be omitted via omitempty; got: %v", key, session[key])
			}
		}
		if _, present := payload["currentActivity"]; present {
			t.Errorf("expected 'currentActivity' to be omitted when nil; got: %v", payload["currentActivity"])
		}
	})
}

func TestLatestActivity(t *testing.T) {
	t.Parallel()

	first := afclient.ActivityEvent{ID: "1", Type: afclient.ActivityThought, Content: "first"}
	second := afclient.ActivityEvent{ID: "2", Type: afclient.ActivityAction, Content: "second"}
	third := afclient.ActivityEvent{ID: "3", Type: afclient.ActivityResponse, Content: "third"}

	cases := []struct {
		name   string
		events []afclient.ActivityEvent
		want   *afclient.ActivityEvent
	}{
		{name: "nil", events: nil, want: nil},
		{name: "empty", events: []afclient.ActivityEvent{}, want: nil},
		{name: "single", events: []afclient.ActivityEvent{first}, want: &first},
		{name: "multiple", events: []afclient.ActivityEvent{first, second, third}, want: &third},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := latestActivity(tc.events)
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("want nil, got %+v", got)
			case tc.want != nil && got == nil:
				t.Errorf("want %+v, got nil", tc.want)
			case tc.want != nil && got != nil:
				if got.ID != tc.want.ID {
					t.Errorf("want id=%q, got id=%q", tc.want.ID, got.ID)
				}
				if len(tc.events) > 0 && got != &tc.events[len(tc.events)-1] {
					t.Errorf("expected pointer to last slice element, got a different address")
				}
			}
		})
	}
}

// TestAgentStatusBareCommandHintsAtAgentList pins the friendly error
// emitted when an operator runs `agent status` with no arguments. The
// default cobra.ExactArgs(1) message ("accepts 1 arg(s), received 0")
// is opaque to anyone who doesn't already know about `agent list`; the
// custom Args validator must point them at the discovery command and
// show the expected shape.
func TestAgentStatusBareCommandHintsAtAgentList(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, _ := newTestAgentCmd(ds, []string{"status"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from `agent status` with no args, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"agent status needs a session id",
		"agent list",
		"agent status <session-id>",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to contain %q; got: %q", want, msg)
		}
	}
	// Belt-and-suspenders: the bare cobra default must not be the only
	// message the operator sees.
	if strings.TrimSpace(msg) == "accepts 1 arg(s), received 0" {
		t.Errorf("regressed to bare cobra ExactArgs error: %q", msg)
	}
}

// TestAgentStatusTooManyArgsKeepsExactArgsParity confirms the >1-arg
// path still produces cobra's standard error so callers piping into
// scripts that string-match on it are not broken.
func TestAgentStatusTooManyArgsKeepsExactArgsParity(t *testing.T) {
	t.Parallel()

	mock := afclient.NewMockClient()
	ds := func() afclient.DataSource { return mock }
	cmd, _ := newTestAgentCmd(ds, []string{"status", "sess-a", "sess-b"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from `agent status` with two args, got nil")
	}
	if !strings.Contains(err.Error(), "accepts 1 arg") {
		t.Errorf("expected cobra ExactArgs(1) too-many error; got: %v", err)
	}
}
