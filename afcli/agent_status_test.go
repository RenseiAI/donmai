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
		"OPS-1180",
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
		if got := strings.Count(out, "-\n"); got < 4 {
			t.Errorf("expected '-' placeholder on >= 4 rows, got %d; out:\n%s", got, out)
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

func newDiagnosticServer(t *testing.T, detailRaw string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/public/session-activities") {
			_ = json.NewEncoder(w).Encode(afclient.ActivityListResponse{
				Activities:    []afclient.ActivityEvent{},
				SessionStatus: afclient.StatusFailed,
			})
			return
		}
		_, _ = w.Write([]byte(detailRaw))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAgentStatusDiagnosticDetail(t *testing.T) {
	t.Parallel()

	const diagnosticRaw = `{"session":{"id":"sess-diag-001","identifier":"OPS-0001","status":"failed","workType":"development","startedAt":"2026-10-27T12:00:00Z","duration":1234,"timeline":{"created":"2026-10-27T11:40:00Z"},"terminalReason":{"code":"provision_failed","message":"Workarea provisioning failed","phase":"spawn","decidedBy":"platform","at":"2026-10-27T12:01:00Z"},"budgetBreach":{"cap":"max-tokens","detail":"max-tokens exceeded: observed=5100000 limit=5000000"},"pullRequestUrl":"https://example.invalid/repo/pull/42","pullRequest":{"number":42,"url":"https://example.invalid/repo/pull/42","headSha":"abc123def456abc123def456abc123def456abcd"},"summary":"Migrated middleware and opened PR."},"timestamp":"2026-10-27T12:02:00Z"}`
	const legacyRaw = `{"session":{"id":"sess-diag-001","identifier":"OPS-0001","status":"failed","workType":"development","startedAt":"2026-10-27T12:00:00Z","duration":1234,"timeline":{"created":"2026-10-27T11:40:00Z"}},"timestamp":"2026-10-27T12:02:00Z"}`

	t.Run("diagnostic_round_trips_json", func(t *testing.T) {
		t.Parallel()
		srv := newDiagnosticServer(t, diagnosticRaw)
		ds := func() afclient.DataSource { return afclient.NewClient(srv.URL) }
		cmd, buf := newTestAgentCmd(ds, []string{"status", "sess-diag-001", "--json"})
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
		reason, ok := session["terminalReason"].(map[string]any)
		if !ok {
			t.Fatalf("expected session.terminalReason object; got: %v", session["terminalReason"])
		}
		for _, want := range [][2]string{
			{"code", "provision_failed"},
			{"message", "Workarea provisioning failed"},
			{"phase", "spawn"},
			{"decidedBy", "platform"},
			{"at", "2026-10-27T12:01:00Z"},
		} {
			if got := reason[want[0]]; got != want[1] {
				t.Errorf("terminalReason.%s = %v, want %q", want[0], got, want[1])
			}
		}
		breach, ok := session["budgetBreach"].(map[string]any)
		if !ok {
			t.Fatalf("expected session.budgetBreach object; got: %v", session["budgetBreach"])
		}
		if got := breach["cap"]; got != "max-tokens" {
			t.Errorf("budgetBreach.cap = %v, want max-tokens", got)
		}
		if got := breach["detail"]; got != "max-tokens exceeded: observed=5100000 limit=5000000" {
			t.Errorf("budgetBreach.detail = %v, want breach detail", got)
		}
		if got := session["pullRequestUrl"]; got != "https://example.invalid/repo/pull/42" {
			t.Errorf("pullRequestUrl = %v, want PR url", got)
		}
		pr, ok := session["pullRequest"].(map[string]any)
		if !ok {
			t.Fatalf("expected session.pullRequest object; got: %v", session["pullRequest"])
		}
		if got := pr["number"]; got != float64(42) {
			t.Errorf("pullRequest.number = %v, want 42", got)
		}
		if got := pr["url"]; got != "https://example.invalid/repo/pull/42" {
			t.Errorf("pullRequest.url = %v, want PR url", got)
		}
		if got := session["summary"]; got != "Migrated middleware and opened PR." {
			t.Errorf("summary = %v, want summary text", got)
		}
	})

	t.Run("diagnostic_renders_text", func(t *testing.T) {
		t.Parallel()
		srv := newDiagnosticServer(t, diagnosticRaw)
		ds := func() afclient.DataSource { return afclient.NewClient(srv.URL) }
		cmd, buf := newTestAgentCmd(ds, []string{"status", "sess-diag-001"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
		out := buf.String()
		for _, want := range []string{
			"Terminal Reason:",
			"provision_failed",
			"Workarea provisioning failed",
			"Budget Breach:",
			"max-tokens",
			"max-tokens exceeded",
			"Pull Request:",
			"https://example.invalid/repo/pull/42",
			"Summary:",
			"Migrated middleware and opened PR.",
			"Status:",
			"failed",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("text output missing %q; got:\n%s", want, out)
			}
		}
		if strings.Contains(out, "completed") {
			t.Errorf("failed session must not render as completed; got:\n%s", out)
		}
	})

	t.Run("legacy_without_diagnostics_stays_valid", func(t *testing.T) {
		t.Parallel()
		srv := newDiagnosticServer(t, legacyRaw)
		ds := func() afclient.DataSource { return afclient.NewClient(srv.URL) }
		cmd, buf := newTestAgentCmd(ds, []string{"status", "sess-diag-001", "--json"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute legacy: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
			t.Fatalf("legacy output not valid JSON: %v\n%s", err, buf.String())
		}
		session, ok := payload["session"].(map[string]any)
		if !ok {
			t.Fatalf("expected top-level 'session' object; got: %v", payload)
		}
		for _, key := range []string{"terminalReason", "budgetBreach", "pullRequest", "pullRequestUrl", "summary"} {
			if _, present := session[key]; present {
				t.Errorf("expected no session.%s key for older response; got: %v", key, session[key])
			}
		}
		cmd2, buf2 := newTestAgentCmd(ds, []string{"status", "sess-diag-001"})
		if err := cmd2.Execute(); err != nil {
			t.Fatalf("execute legacy text: %v", err)
		}
		out := buf2.String()
		if !strings.Contains(out, "failed") {
			t.Errorf("legacy failed status lost; got:\n%s", out)
		}
		if strings.Contains(out, "completed") {
			t.Errorf("legacy failed session must not render as completed; got:\n%s", out)
		}
		for _, label := range []string{"Terminal Reason:", "Budget Breach:", "Pull Request:", "Summary:"} {
			if !strings.Contains(out, label) {
				t.Errorf("legacy text missing row %q; got:\n%s", label, out)
			}
		}
		if got := strings.Count(out, "unavailable"); got < 4 {
			t.Errorf("expected unavailable placeholder on >= 4 rows, got %d; out:\n%s", got, out)
		}
	})
}
