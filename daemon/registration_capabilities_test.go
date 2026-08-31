package daemon

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/kgextract"
)

// TestEffectiveRegistrationCapabilities covers the tag list the daemon puts on
// the wire at registration. The kg-extraction tag is appended in every case
// because EVERY poll service executes that lane (NewPollService wires the
// executor), so the advertisement can never outrun the implementation.
func TestEffectiveRegistrationCapabilities(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		embedder []string
		want     []string
	}{
		{
			name:     "nil_embedder_gets_base_substrate_plus_lanes",
			embedder: nil,
			want:     []string{"local", "sandbox", "workarea", kgextract.WorkTypeKGExtraction, receiptPreflightNackReasonCapability},
		},
		{
			name:     "embedder_list_is_preserved_and_extended",
			embedder: []string{"local", "sandbox", "workarea", "merge-queue"},
			want:     []string{"local", "sandbox", "workarea", "merge-queue", kgextract.WorkTypeKGExtraction, receiptPreflightNackReasonCapability},
		},
		{
			name:     "embedder_that_already_advertises_the_lane_is_not_duplicated",
			embedder: []string{"local", kgextract.WorkTypeKGExtraction},
			want:     []string{"local", kgextract.WorkTypeKGExtraction, receiptPreflightNackReasonCapability},
		},
		{
			name:     "explicit_empty_list_is_an_opinion_and_only_gains_the_lanes",
			embedder: []string{},
			want:     []string{kgextract.WorkTypeKGExtraction, receiptPreflightNackReasonCapability},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := effectiveRegistrationCapabilities(tc.embedder)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("effectiveRegistrationCapabilities(%v) = %v, want %v", tc.embedder, got, tc.want)
			}
		})
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
