package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/kgextract"
)

// TestEffectiveRegistrationCapabilities covers the tag list the daemon puts on
// the wire at registration. The kg-extraction tag is appended in every case
// because EVERY poll service executes that lane (NewPollService wires the
// executor), so the advertisement can never outrun the implementation.
func TestEffectiveRegistrationCapabilities(t *testing.T) {
	t.Parallel()

	host := agent.UncontainedHostEnforcement()
	sandboxed := agent.UncontainedHostEnforcement()
	sandboxed.Isolation = agent.IsolationOSSandbox
	lanes := []string{kgextract.WorkTypeKGExtraction, receiptPreflightNackReasonCapability}

	cases := []struct {
		name        string
		embedder    []string
		enforcement agent.ExecutionSecurityEnforcement
		want        []string
	}{
		{
			name:        "nil_embedder_on_an_uncontained_host_advertises_no_sandbox",
			embedder:    nil,
			enforcement: host,
			want:        append([]string{"local", "workarea"}, lanes...),
		},
		{
			name:        "embedder_sandbox_tag_is_stripped_when_nothing_attests_it",
			embedder:    []string{"local", "sandbox", "workarea", "merge-queue"},
			enforcement: host,
			want:        append([]string{"local", "workarea", "merge-queue"}, lanes...),
		},
		{
			name:        "attested_isolation_boundary_advertises_the_sandbox_tag",
			embedder:    nil,
			enforcement: sandboxed,
			want:        append([]string{"local", "workarea", "sandbox"}, lanes...),
		},
		{
			name:        "attested_embedder_sandbox_tag_is_kept_once",
			embedder:    []string{"local", "sandbox", "workarea"},
			enforcement: sandboxed,
			want:        append([]string{"local", "workarea", "sandbox"}, lanes...),
		},
		{
			name:        "embedder_that_already_advertises_the_lane_is_not_duplicated",
			embedder:    []string{"local", kgextract.WorkTypeKGExtraction},
			enforcement: host,
			want:        []string{"local", kgextract.WorkTypeKGExtraction, receiptPreflightNackReasonCapability},
		},
		{
			name:        "explicit_empty_list_is_an_opinion_and_only_gains_the_lanes",
			embedder:    []string{},
			enforcement: host,
			want:        lanes,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := effectiveRegistrationCapabilities(tc.embedder, tc.enforcement)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("effectiveRegistrationCapabilities(%v) = %v, want %v", tc.embedder, got, tc.want)
			}
		})
	}
}

// TestRegistrationExecutionSecurityEnforcement pins the attestation a daemon
// publishes: with none configured it states index 0 on every substrate
// dimension explicitly; a configured one is validated and normalized; a
// level off its ladder refuses to start the registration.
func TestRegistrationExecutionSecurityEnforcement(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		configured *agent.ExecutionSecurityEnforcement
		want       agent.ExecutionSecurityEnforcement
		wantErr    bool
	}{
		{
			name:       "unconfigured_host_attests_index_zero",
			configured: nil,
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

// TestRegisterRequestCarriesExecutionSecurityEnforcement pins the wire key
// the attestation rides at registration.
func TestRegisterRequestCarriesExecutionSecurityEnforcement(t *testing.T) {
	t.Parallel()
	enforcement := agent.UncontainedHostEnforcement()
	raw, err := json.Marshal(RegisterRequest{ExecutionSecurityEnforcement: &enforcement})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	want := `{"fileRead":"host","fileWrite":"host","network":"open","credentials":"ambient-host-login","isolation":"host-user"}`
	if got := string(body["executionSecurityEnforcement"]); got != want {
		t.Fatalf("executionSecurityEnforcement = %s, want %s", got, want)
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

	for _, tag := range effectiveRegistrationCapabilities(nil, agent.UncontainedHostEnforcement()) {
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
