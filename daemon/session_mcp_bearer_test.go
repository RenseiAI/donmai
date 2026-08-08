package daemon

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	sessionMCPBearerFixture = "session-scoped-bearer"
	workerBearerFixture     = "worker-runtime-bearer"
	sessionMCPExpiryFixture = "2026-08-08T19:04:05Z"
)

// TestPollWorkItem_DecodesSessionMCPBearer pins the platform→daemon half of the
// wire. Go's decoder drops unknown keys silently, so a missing struct field is
// invisible: the platform emits the bearer, the daemon never sees it, and the
// session falls back to the worker bearer with no error anywhere. That is the
// exact shape of several past wire gaps in this repo.
//
// The assertion is on raw platform-shaped JSON — the input the daemon really
// receives — not on a re-marshalled struct, which would agree with itself.
func TestPollWorkItem_DecodesSessionMCPBearer(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantToken  string
		wantExpiry string
	}{
		{
			name: "both keys present",
			body: `{
				"sessionId": "sess_1",
				"projectId": "proj_1",
				"mcpAuthToken": "` + sessionMCPBearerFixture + `",
				"mcpAuthTokenExpiresAt": "` + sessionMCPExpiryFixture + `"
			}`,
			wantToken:  sessionMCPBearerFixture,
			wantExpiry: sessionMCPExpiryFixture,
		},
		{
			name:      "token without expiry",
			body:      `{"sessionId":"sess_2","mcpAuthToken":"` + sessionMCPBearerFixture + `"}`,
			wantToken: sessionMCPBearerFixture,
		},
		{
			name: "absent — older platform",
			body: `{"sessionId":"sess_3","projectId":"proj_1"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var item PollWorkItem
			if err := json.Unmarshal([]byte(tc.body), &item); err != nil {
				t.Fatalf("unmarshal PollWorkItem: %v", err)
			}
			if item.McpAuthToken != tc.wantToken {
				t.Errorf("McpAuthToken = %q, want %q", item.McpAuthToken, tc.wantToken)
			}
			if item.McpAuthTokenExpiresAt != tc.wantExpiry {
				t.Errorf("McpAuthTokenExpiresAt = %q, want %q", item.McpAuthTokenExpiresAt, tc.wantExpiry)
			}
		})
	}
}

// TestPollItemToSessionDetail_ForwardsSessionMCPBearer pins the daemon→runner
// half. The daemon is a pure forwarder here: it never parses or validates the
// value, and the worker bearer it resolves from its own registration store
// stays a separate field — the two credentials are not interchangeable.
func TestPollItemToSessionDetail_ForwardsSessionMCPBearer(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		item := PollWorkItem{
			SessionID:             "sess_1",
			McpAuthToken:          sessionMCPBearerFixture,
			McpAuthTokenExpiresAt: sessionMCPExpiryFixture,
		}
		detail := PollItemToSessionDetail(item, nil, "https://platform.example.com", workerBearerFixture, "worker_1")
		if detail.McpAuthToken != sessionMCPBearerFixture {
			t.Errorf("McpAuthToken = %q, want %q", detail.McpAuthToken, sessionMCPBearerFixture)
		}
		if detail.McpAuthTokenExpiresAt != sessionMCPExpiryFixture {
			t.Errorf("McpAuthTokenExpiresAt = %q, want %q", detail.McpAuthTokenExpiresAt, sessionMCPExpiryFixture)
		}
		if detail.AuthToken != workerBearerFixture {
			t.Errorf("AuthToken = %q, want the worker bearer %q — the two must stay independent",
				detail.AuthToken, workerBearerFixture)
		}
	})

	t.Run("absent — older platform", func(t *testing.T) {
		detail := PollItemToSessionDetail(PollWorkItem{SessionID: "sess_2"}, nil,
			"https://platform.example.com", workerBearerFixture, "worker_1")
		if detail.McpAuthToken != "" || detail.McpAuthTokenExpiresAt != "" {
			t.Errorf("want both empty when the platform stamps none; got %q / %q",
				detail.McpAuthToken, detail.McpAuthTokenExpiresAt)
		}
		if detail.AuthToken != workerBearerFixture {
			t.Errorf("AuthToken = %q, want %q", detail.AuthToken, workerBearerFixture)
		}
	})
}

// TestSessionMCPBearerKeysAreOmittedWhenAbsent pins the "absent means absent"
// half of the contract on both wire types: the keys must vanish entirely rather
// than serialize as null or "". Consumers are required to treat absent and
// empty identically, and omitempty is what makes that free.
func TestSessionMCPBearerKeysAreOmittedWhenAbsent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"PollWorkItem", PollWorkItem{SessionID: "sess_1"}},
		{"SessionDetail", SessionDetail{SessionID: "sess_1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatalf("marshal %s: %v", tc.name, err)
			}
			for _, key := range []string{"mcpAuthToken", "mcpAuthTokenExpiresAt"} {
				if strings.Contains(string(raw), key) {
					t.Errorf("%s JSON contains %q when unset; want the key omitted: %s", tc.name, key, raw)
				}
			}
		})
	}
}

// TestUpdateRuntimeCredentials_LeavesSessionMCPBearerIntact is the guard for
// the interaction that motivated the whole change. The daemon refreshes the
// WORKER bearer in place on long-running sessions; the session-scoped MCP
// bearer is not refreshable that way (it lives in a config file no one
// rewrites), so the refresh must leave it untouched rather than blank it.
func TestUpdateRuntimeCredentials_LeavesSessionMCPBearerIntact(t *testing.T) {
	store := newSessionDetailStore()
	store.Set(&SessionDetail{
		SessionID:             "sess_1",
		WorkerID:              "worker_1",
		AuthToken:             "stale-worker-bearer",
		McpAuthToken:          sessionMCPBearerFixture,
		McpAuthTokenExpiresAt: sessionMCPExpiryFixture,
	})

	store.UpdateRuntimeCredentials("worker_2", "fresh-worker-bearer")

	got, ok := store.Get("sess_1")
	if !ok {
		t.Fatal("session detail vanished")
	}
	if got.AuthToken != "fresh-worker-bearer" || got.WorkerID != "worker_2" {
		t.Errorf("worker credentials not refreshed: worker=%q token=%q", got.WorkerID, got.AuthToken)
	}
	if got.McpAuthToken != sessionMCPBearerFixture {
		t.Errorf("McpAuthToken = %q, want it untouched (%q)", got.McpAuthToken, sessionMCPBearerFixture)
	}
	if got.McpAuthTokenExpiresAt != sessionMCPExpiryFixture {
		t.Errorf("McpAuthTokenExpiresAt = %q, want it untouched (%q)",
			got.McpAuthTokenExpiresAt, sessionMCPExpiryFixture)
	}
}
