package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/kgextract"
)

// TestEffectiveRegistrationCapabilities covers the tag list the daemon puts on
// the wire at registration. The kg-extraction tag is appended in every case
// because EVERY poll service executes that lane (NewPollService wires the
// executor), so the advertisement can never outrun the implementation. The
// base substrate set no longer claims a sandbox.
func TestEffectiveRegistrationCapabilities(t *testing.T) {
	t.Parallel()

	lanes := []string{kgextract.WorkTypeKGExtraction, receiptPreflightNackReasonCapability}
	cases := []struct {
		name     string
		embedder []string
		want     []string
	}{
		{
			name:     "nil_embedder_gets_base_substrate_plus_lanes",
			embedder: nil,
			want:     append([]string{"local", "workarea"}, lanes...),
		},
		{
			name:     "embedder_list_is_preserved_and_extended",
			embedder: []string{"local", "sandbox", "workarea", "merge-queue"},
			want:     append([]string{"local", "sandbox", "workarea", "merge-queue"}, lanes...),
		},
		{
			name:     "embedder_that_already_advertises_the_lane_is_not_duplicated",
			embedder: []string{"local", kgextract.WorkTypeKGExtraction},
			want:     []string{"local", kgextract.WorkTypeKGExtraction, receiptPreflightNackReasonCapability},
		},
		{
			name:     "explicit_empty_list_is_an_opinion_and_only_gains_the_lanes",
			embedder: []string{},
			want:     lanes,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := effectiveRegistrationCapabilities(tc.embedder)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("effectiveRegistrationCapabilities(%v) = %v, want %v", tc.embedder, got, tc.want)
			}
		})
	}
}

// TestAttestedRegistrationCapabilities pins the sandbox rule Register applies
// on every path: the tag is dropped unless the attestation proves an
// isolation boundary, added when it does, and a nil list stays nil.
func TestAttestedRegistrationCapabilities(t *testing.T) {
	t.Parallel()
	host := agent.UncontainedHostEnforcement()
	sandboxed := agent.UncontainedHostEnforcement()
	sandboxed.Isolation = agent.IsolationOSSandbox
	cases := []struct {
		name        string
		in          []string
		enforcement agent.ExecutionSecurityEnforcement
		want        []string
	}{
		{"unattested sandbox tag is dropped", []string{"local", "sandbox", "workarea", "merge-queue"}, host, []string{"local", "workarea", "merge-queue"}},
		{"attested boundary adds the tag", []string{"local", "workarea"}, sandboxed, []string{"local", "workarea", "sandbox"}},
		{"attested embedder tag is kept once", []string{"local", "sandbox", "workarea"}, sandboxed, []string{"local", "workarea", "sandbox"}},
		{"nil list stays nil", nil, sandboxed, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := attestedRegistrationCapabilities(tc.in, tc.enforcement)
			if (got == nil) != (tc.want == nil) || strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("attestedRegistrationCapabilities(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestRegistrationExecutionSecurityEnforcement pins the attestation a
// registration publishes: with none configured it states index 0 on every
// substrate dimension explicitly; a configured one is validated and
// normalized; a level off its ladder refuses the registration.
func TestRegistrationExecutionSecurityEnforcement(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		configured *agent.ExecutionSecurityEnforcement
		want       agent.ExecutionSecurityEnforcement
		wantErr    bool
	}{
		{
			name: "unconfigured_host_attests_index_zero",
			want: agent.ExecutionSecurityEnforcement{
				FileRead: "host", FileWrite: "host", Network: "open",
				Credentials: "ambient-host-login", Isolation: "host-user",
			},
		},
		{
			name:       "partial_attestation_names_every_substrate_dimension",
			configured: &agent.ExecutionSecurityEnforcement{Isolation: agent.IsolationContainer},
			want: agent.ExecutionSecurityEnforcement{
				FileRead: "host", FileWrite: "host", Network: "open",
				Credentials: "ambient-host-login", Isolation: "container",
			},
		},
		{
			name:       "unknown_level_is_refused",
			configured: &agent.ExecutionSecurityEnforcement{Network: "firewalled"},
			wantErr:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := registrationExecutionSecurityEnforcement(tc.configured)
			if tc.wantErr {
				if agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnresolvable {
					t.Fatalf("err = %v, want execution_security_unresolvable", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("attestation = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// registerCapturing runs Register against a capturing orchestrator and
// returns the decoded request body.
func registerCapturing(t *testing.T, opts RegistrationOptions) (map[string]json.RawMessage, error) {
	t.Helper()
	var body map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(RegisterResponse{
			WorkerID: "worker-es", RuntimeToken: "runtime-es", HeartbeatInterval: 30_000, PollInterval: 5_000,
		})
	}))
	t.Cleanup(server.Close)
	opts.OrchestratorURL, opts.RegistrationToken, opts.Hostname = server.URL, "rsp_live_fixture", "host"
	opts.JWTPath, opts.HTTPClient = filepath.Join(t.TempDir(), "daemon.jwt"), server.Client()
	_, err := Register(context.Background(), opts)
	return body, err
}

// TestRegisterReconcilesSandboxAndAttestationOnEveryPath covers direct
// Register callers (not only Daemon.Start): an unattested sandbox tag never
// reaches the wire, the index-0 attestation is always published, an attested
// isolation boundary keeps the tag, and an invalid attestation refuses the
// registration before any request.
func TestRegisterReconcilesSandboxAndAttestationOnEveryPath(t *testing.T) {
	t.Parallel()
	indexZero := `{"fileRead":"host","fileWrite":"host","network":"open","credentials":"ambient-host-login","isolation":"host-user"}`

	body, err := registerCapturing(t, RegistrationOptions{Capabilities: []string{"local", "sandbox", "workarea"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body["capabilities"]); got != `["local","workarea"]` {
		t.Errorf("unattested capabilities = %s, want the sandbox tag dropped", got)
	}
	if got := string(body["executionSecurityEnforcement"]); got != indexZero {
		t.Errorf("executionSecurityEnforcement = %s, want %s", got, indexZero)
	}

	sandboxed := agent.UncontainedHostEnforcement()
	sandboxed.Isolation = agent.IsolationOSSandbox
	body, err = registerCapturing(t, RegistrationOptions{Capabilities: []string{"local", "sandbox", "workarea"}, ExecutionSecurityEnforcement: &sandboxed})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body["capabilities"]); got != `["local","workarea","sandbox"]` {
		t.Errorf("attested capabilities = %s, want the sandbox tag kept", got)
	}

	if _, err := registerCapturing(t, RegistrationOptions{ExecutionSecurityEnforcement: &agent.ExecutionSecurityEnforcement{Isolation: "chroot"}}); agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityUnresolvable {
		t.Fatalf("invalid attestation err = %v, want execution_security_unresolvable", err)
	}
}

// TestEffectiveRegistrationCapabilities_AdvertisesOnlyExecutedLanes is the
// atomicity guard. Every tag this function appends beyond the substrate set must
// name a lane the poll service actually runs; a tag added here without wiring
// its executor makes the daemon claim work it silently drops.
func TestEffectiveRegistrationCapabilities_AdvertisesOnlyExecutedLanes(t *testing.T) {
	t.Parallel()

	executed := map[string]bool{
		// Wired unconditionally in NewPollService.
		kgextract.WorkTypeKGExtraction: true,
		// handlePollWorkItem always runs the NACK producer after every local
		// accept-work rejection, and receiptPreflightNackReasonForError only
		// emits the closed reason for the canonical typed denial.
		receiptPreflightNackReasonCapability: true,
	}
	substrate := map[string]bool{}
	for _, c := range baseSubstrateCapabilities {
		substrate[c] = true
	}

	for _, tag := range effectiveRegistrationCapabilities(nil) {
		if substrate[tag] || executed[tag] {
			continue
		}
		t.Errorf("capability %q is advertised but no poll-service lane executes it; "+
			"claimed items for it would be popped off the queue and dropped", tag)
	}
	for _, tag := range laneCapabilities {
		if !executed[tag] {
			t.Errorf("lane capability %q has no executor recorded in this test — "+
				"wire the executor in NewPollService before advertising the tag", tag)
		}
	}
	for _, tag := range producerCapabilities {
		if !executed[tag] {
			t.Errorf("producer capability %q has no daemon implementation recorded in this test", tag)
		}
	}
}
