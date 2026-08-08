package runner

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// captureLogger returns a logger writing JSON records into buf, so a test can
// assert on what the runner actually emitted rather than on a stub's opinion.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// capturedRecords decodes the JSON lines captured by captureLogger.
func capturedRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("captured log line is not JSON: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// platformGatewayWork builds the minimum QueuedWork that makes the platform
// per-session MCP gateway emittable, carrying only the worker bearer.
func platformGatewayWork(sessionID string) QueuedWork {
	qw := QueuedWork{AuthToken: workerRuntimeBearerFixture}
	qw.SessionID = sessionID
	qw.PlatformURL = "https://platform.example.com"
	return qw
}

const (
	sessionScopedBearerFixture = "session-scoped-bearer"
	workerRuntimeBearerFixture = "worker-runtime-bearer"
)

// TestMCPGatewayBearer_PrefersSessionScopedToken pins the selection rule: the
// platform-stamped, session-scoped bearer wins whenever it is present, and the
// worker runtime bearer remains the fallback for a platform that stamps none
// (self-hosted / older). That fallback is the standalone contract, not a
// migration shim, so "worker only" must keep working forever.
//
// Absent and empty-string are required to be indistinguishable on the wire, so
// a whitespace-only value must fall back rather than emit a blank bearer.
func TestMCPGatewayBearer_PrefersSessionScopedToken(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		session string
		worker  string
		want    string
	}{
		{"both present — session-scoped wins", sessionScopedBearerFixture, workerRuntimeBearerFixture, sessionScopedBearerFixture},
		{"session-scoped only", sessionScopedBearerFixture, "", sessionScopedBearerFixture},
		{"worker only — standalone fallback", "", workerRuntimeBearerFixture, workerRuntimeBearerFixture},
		{"neither", "", "", ""},
		{"blank session-scoped falls back", "   ", workerRuntimeBearerFixture, workerRuntimeBearerFixture},
		{"both blank", " ", "\t\n", ""},
		{"session-scoped is trimmed", " " + sessionScopedBearerFixture + "\n", workerRuntimeBearerFixture, sessionScopedBearerFixture},
		{"worker fallback is trimmed", "", "  " + workerRuntimeBearerFixture + " ", workerRuntimeBearerFixture},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			qw := QueuedWork{AuthToken: tc.worker, McpAuthToken: tc.session}
			if got := mcpGatewayBearer(qw); got != tc.want {
				t.Fatalf("mcpGatewayBearer(session=%q, worker=%q) = %q, want %q",
					tc.session, tc.worker, got, tc.want)
			}
		})
	}
}

// TestDefaultMCPServersForHarness_GatewayHeaderCarriesSessionScopedBearer is
// the defect's direct guard. The gateway's Authorization header is written once
// into an MCP config file that nothing ever rewrites, so whichever bearer lands
// there is the one the harness presents for the whole session. It must be the
// session-scoped one when the platform stamped it — and the worker runtime
// bearer must not appear in the header at all.
func TestDefaultMCPServersForHarness_GatewayHeaderCarriesSessionScopedBearer(t *testing.T) {
	t.Parallel()

	qw := platformGatewayWork("sess_abc")
	qw.McpAuthToken = sessionScopedBearerFixture

	servers := defaultMCPServersForHarness(qw, "/abs/wt", mcpDeliveringHarness(), agent.PromptModeAutonomous)
	if len(servers) != 1 {
		t.Fatalf("len(servers) = %d, want 1 (the platform gateway)", len(servers))
	}
	got := servers[0].Headers["Authorization"]
	if want := "Bearer " + sessionScopedBearerFixture; got != want {
		t.Fatalf("gateway Authorization = %q, want %q", got, want)
	}
	if strings.Contains(got, workerRuntimeBearerFixture) {
		t.Fatalf("worker runtime bearer leaked into the gateway header: %q", got)
	}
}

// TestDefaultMCPServersForHarness_EmitsGatewayOnSessionBearerAlone pins the
// emit condition against the session-scoped bearer rather than the worker one.
// A platform that stamps only the session bearer must still get a gateway; a
// work item with neither bearer must still get none (standalone back-compat).
func TestDefaultMCPServersForHarness_EmitsGatewayOnSessionBearerAlone(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		session    string
		worker     string
		wantHeader string // "" means: no gateway at all
	}{
		{"session-scoped only", sessionScopedBearerFixture, "", "Bearer " + sessionScopedBearerFixture},
		{"worker only", "", workerRuntimeBearerFixture, "Bearer " + workerRuntimeBearerFixture},
		{"both", sessionScopedBearerFixture, workerRuntimeBearerFixture, "Bearer " + sessionScopedBearerFixture},
		{"neither — standalone", "", "", ""},
		{"both blank — standalone", "  ", " ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			qw := platformGatewayWork("sess_" + tc.name)
			qw.AuthToken = tc.worker
			qw.McpAuthToken = tc.session

			servers := defaultMCPServersForHarness(qw, "/abs/wt", mcpDeliveringHarness(), agent.PromptModeAutonomous)
			if tc.wantHeader == "" {
				if servers != nil {
					t.Fatalf("got %v, want no gateway when neither bearer is usable", mcpServerNames(servers))
				}
				return
			}
			if len(servers) != 1 {
				t.Fatalf("len(servers) = %d, want 1", len(servers))
			}
			if got := servers[0].Headers["Authorization"]; got != tc.wantHeader {
				t.Fatalf("gateway Authorization = %q, want %q", got, tc.wantHeader)
			}
		})
	}
}

// TestDefaultMCPServersForHarness_SessionBearerNeverChangesGatewayEmission is
// the regression guard for the failure shape this file's change could
// reintroduce: deciding anything about the gateway from the harness or provider
// NAME. Hardcoding a name here once denied the spawn outright for every harness
// that declares no MCP delivery.
//
// The invariant asserted per harness is that the session-scoped bearer changes
// the gateway's HEADER and nothing else — never WHICH harnesses mount it. That
// stays keyed on the declared MCPDelivery, exactly as before.
func TestDefaultMCPServersForHarness_SessionBearerNeverChangesGatewayEmission(t *testing.T) {
	t.Parallel()

	for _, tc := range harnessMCPCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			workerOnly := platformGatewayWork("sess_" + tc.name)
			withSession := workerOnly
			withSession.McpAuthToken = sessionScopedBearerFixture

			workerLane := defaultMCPServersForHarness(workerOnly, "/abs/wt", tc.provider, tc.mode)
			sessionLane := defaultMCPServersForHarness(withSession, "/abs/wt", tc.provider, tc.mode)

			if got, want := mcpServerNames(sessionLane), mcpServerNames(workerLane); !slices.Equal(got, want) {
				t.Fatalf("session-scoped bearer changed the emitted server set for %s: got %v, want %v",
					tc.name, got, want)
			}
			if !tc.deliversMCP {
				if sessionLane != nil {
					t.Fatalf("%s declares no MCP delivery; got gateway %v", tc.name, mcpServerNames(sessionLane))
				}
				return
			}
			if len(sessionLane) != 1 {
				t.Fatalf("%s: len(servers) = %d, want 1", tc.name, len(sessionLane))
			}
			if got, want := sessionLane[0].Headers["Authorization"], "Bearer "+sessionScopedBearerFixture; got != want {
				t.Fatalf("%s: gateway Authorization = %q, want %q", tc.name, got, want)
			}
		})
	}
}

// TestDefaultMCPServersForHarness_ExpiryHintNeverChangesTheEmittedSet pins the
// expiry as strictly advisory. The runner must not branch on it — not to refuse
// a spawn, not to shorten a session, not to drop the gateway — so every value,
// including a past instant and an unparseable one, must produce a byte-
// identical server set.
func TestDefaultMCPServersForHarness_ExpiryHintNeverChangesTheEmittedSet(t *testing.T) {
	t.Parallel()

	base := platformGatewayWork("sess_expiry")
	base.McpAuthToken = sessionScopedBearerFixture
	want := defaultMCPServersForHarness(base, "/abs/wt", mcpDeliveringHarness(), agent.PromptModeAutonomous)
	if len(want) != 1 {
		t.Fatalf("fixture emitted %d servers, want 1", len(want))
	}

	for _, expiry := range []string{
		"2020-01-01T00:00:00Z", // long expired
		"2099-01-01T00:00:00Z", // far future
		"not-a-timestamp",
		" ",
	} {
		t.Run(expiry, func(t *testing.T) {
			t.Parallel()
			qw := base
			qw.McpAuthTokenExpiresAt = expiry
			got := defaultMCPServersForHarness(qw, "/abs/wt", mcpDeliveringHarness(), agent.PromptModeAutonomous)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("expiry %q changed the emitted set:\n got %+v\nwant %+v", expiry, got, want)
			}
		})
	}
}

// TestLogMCPGatewayBearerExpiry_AdvisoryOnly pins the operator-facing receipt
// for the one case this change does NOT close: a session that outlives its
// bearer still loses its tools silently, just far later. The line makes that
// cliff greppable ahead of time.
//
// It fires only when there is something to say — an expiry hint AND a gateway
// actually mounted — so a standalone session and a harness that mounts no
// gateway stay silent.
func TestLogMCPGatewayBearerExpiry_AdvisoryOnly(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	gateway := []agent.MCPServerConfig{{Name: platformMCPServerName(), Type: "http"}}
	other := []agent.MCPServerConfig{{Name: "card-tools", Type: "stdio"}}

	cases := []struct {
		name        string
		expiresAt   string
		servers     []agent.MCPServerConfig
		wantLevel   string
		wantMessage string
	}{
		{
			name:        "future expiry with a mounted gateway",
			expiresAt:   "2026-08-08T19:00:00Z",
			servers:     gateway,
			wantLevel:   "INFO",
			wantMessage: "[runner] platform MCP gateway bearer expires at 2026-08-08T19:00:00Z (420m from now)",
		},
		{
			name:        "offset form is normalised to UTC",
			expiresAt:   "2026-08-08T14:30:00+02:00",
			servers:     gateway,
			wantLevel:   "INFO",
			wantMessage: "[runner] platform MCP gateway bearer expires at 2026-08-08T12:30:00Z (30m from now)",
		},
		{
			name:      "unparseable expiry warns and is ignored",
			expiresAt: "not-a-timestamp",
			servers:   gateway,
			wantLevel: "WARN",
		},
		{"no expiry hint", "", gateway, "", ""},
		{"no gateway mounted", "2026-08-08T19:00:00Z", other, "", ""},
		{"no servers at all", "2026-08-08T19:00:00Z", nil, "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			qw := platformGatewayWork("sess_log")
			qw.McpAuthToken = sessionScopedBearerFixture
			qw.McpAuthTokenExpiresAt = tc.expiresAt

			var buf bytes.Buffer
			logMCPGatewayBearerExpiry(captureLogger(&buf), qw, tc.servers, now)
			records := capturedRecords(t, &buf)

			if tc.wantLevel == "" {
				if len(records) != 0 {
					t.Fatalf("want silence, got %d record(s): %s", len(records), buf.String())
				}
				return
			}
			if len(records) != 1 {
				t.Fatalf("want exactly one record, got %d: %s", len(records), buf.String())
			}
			if got := records[0]["level"]; got != tc.wantLevel {
				t.Fatalf("level = %v, want %v", got, tc.wantLevel)
			}
			if tc.wantMessage != "" {
				if got := records[0]["msg"]; got != tc.wantMessage {
					t.Fatalf("msg = %v, want %v", got, tc.wantMessage)
				}
			}
			if got := records[0]["sessionId"]; got != "sess_log" {
				t.Fatalf("sessionId = %v, want sess_log", got)
			}
		})
	}
}

// TestLogMCPGatewayBearerExpiry_NeverLogsTheBearer pins the OSS hygiene rule:
// the bearer is opaque to this repo. The advisory line reports WHEN it dies,
// never WHAT it is.
func TestLogMCPGatewayBearerExpiry_NeverLogsTheBearer(t *testing.T) {
	t.Parallel()

	qw := platformGatewayWork("sess_secret")
	qw.McpAuthToken = sessionScopedBearerFixture
	qw.McpAuthTokenExpiresAt = "2026-08-08T19:00:00Z"

	var buf bytes.Buffer
	logMCPGatewayBearerExpiry(captureLogger(&buf),
		qw,
		[]agent.MCPServerConfig{{Name: platformMCPServerName(), Type: "http"}},
		time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC))

	for _, bearer := range []string{sessionScopedBearerFixture, workerRuntimeBearerFixture} {
		if strings.Contains(buf.String(), bearer) {
			t.Fatalf("bearer %q leaked into the log: %s", bearer, buf.String())
		}
	}
}

// TestQueuedWork_SessionBearerNeverSerializes pins the wire tag. QueuedWork is
// re-marshalled onto other payloads; a bearer that round-trips through those
// paths would widen its exposure well past the MCP config file. The worker
// bearer is already excluded for the same reason — the session-scoped one must
// match it.
func TestQueuedWork_SessionBearerNeverSerializes(t *testing.T) {
	t.Parallel()

	qw := platformGatewayWork("sess_serialize")
	qw.McpAuthToken = sessionScopedBearerFixture
	qw.McpAuthTokenExpiresAt = "2026-08-08T19:00:00Z"

	raw, err := json.Marshal(qw)
	if err != nil {
		t.Fatalf("marshal QueuedWork: %v", err)
	}
	for _, needle := range []string{
		sessionScopedBearerFixture,
		workerRuntimeBearerFixture,
		"mcpAuthToken",
		"mcpAuthTokenExpiresAt",
	} {
		if strings.Contains(string(raw), needle) {
			t.Fatalf("QueuedWork JSON leaked %q: %s", needle, raw)
		}
	}
}

// TestMaterializeRuntimeAuthority_MaterializesBothBearers keeps the
// prepared-source lane and the spawn lane computing the same implicit MCP set.
// The gateway's bearer is mcpGatewayBearer(qw), so materializing only the
// worker one would make the two lanes diverge the moment the platform stamps a
// session-scoped bearer.
func TestMaterializeRuntimeAuthority_MaterializesBothBearers(t *testing.T) {
	t.Parallel()

	got := materializeRuntimeAuthority(QueuedWork{})
	if got.PlatformURL == "" {
		t.Fatal("PlatformURL must be materialized")
	}
	if got.AuthToken != runtimeMaterializedCredential {
		t.Fatalf("AuthToken = %q, want %q", got.AuthToken, runtimeMaterializedCredential)
	}
	if got.McpAuthToken != runtimeMaterializedCredential {
		t.Fatalf("McpAuthToken = %q, want %q", got.McpAuthToken, runtimeMaterializedCredential)
	}
	if bearer := mcpGatewayBearer(got); bearer != runtimeMaterializedCredential {
		t.Fatalf("mcpGatewayBearer(materialized) = %q, want %q", bearer, runtimeMaterializedCredential)
	}
}

// TestPlatformMCPServerName_IsBrandDerived documents that the gateway's
// client-side label follows the build's brand, so a rebranded build renders its
// own label byte-identically.
func TestPlatformMCPServerName_IsBrandDerived(t *testing.T) {
	t.Parallel()

	name := platformMCPServerName()
	if !strings.HasSuffix(name, "-platform") {
		t.Fatalf("platformMCPServerName() = %q, want a %q suffix", name, "-platform")
	}
	qw := platformGatewayWork("sess_name")
	servers := defaultMCPServersForHarness(qw, "/abs/wt", mcpDeliveringHarness(), agent.PromptModeAutonomous)
	if len(servers) != 1 || servers[0].Name != name {
		t.Fatalf("emitted gateway = %v, want a single %q", mcpServerNames(servers), name)
	}
}
