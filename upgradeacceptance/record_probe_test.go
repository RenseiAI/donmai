package upgradeacceptance

import (
	"encoding/json"
	"flag"
	"os"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/daemon"
)

// recordProbePath is set only by the container driver
// (container/upgrade-acceptance.sh), which builds this package's test binary
// from the N+1 tree and runs TestRecordProbe with it. The in-repo suite
// leaves it empty.
var recordProbePath = flag.String("acceptance.record-probe", "",
	"write the container record probe (JSON) to this path")

// recordProbe is what the container driver decides its record from: the
// N+1 build's own selection-rule answers, the posture those answers were
// read under, the live matrix rows it records, and the live halves of the
// adoption-gated local rows. The halves travel as named slots with their
// own status — pending until the driver grows the live-seat assertions
// that record them — so a gated live half is listed in every record
// instead of silently absent from it. Nothing in it is read from
// configuration: the posture fields report the probe daemon's own resolved
// configuration, which the driver cross-checks against the posture the
// green slices would settle in.
type recordProbe struct {
	HeadlessOwned      bool              `json:"headlessOwned"`
	InteractiveOwned   bool              `json:"interactiveOwned"`
	CompositionPending bool              `json:"compositionPending"`
	OwnershipEnabled   bool              `json:"ownershipEnabled"`
	AdoptionEnabled    bool              `json:"adoptionEnabled"`
	LiveCases          []recordProbeCase `json:"liveCases"`
	LiveHalves         []recordProbeHalf `json:"liveHalves"`
}

type recordProbeCase struct {
	Name             string `json:"name"`
	Want             string `json:"want"`
	RequiresAdoption bool   `json:"requiresAdoption"`
}

// recordProbeHalf is one gated live half: the matrix row it belongs to,
// the slot the container records it under, and its status. "pending" is
// the only status this build emits: the live-seat assertions do not exist
// yet, so the driver lists each half without recording it. "recorded" is
// reserved for the green slices, which flip the driver's half assertions
// from listing to recording.
type recordProbeHalf struct {
	Row    string `json:"row"`
	Slot   string `json:"slot"`
	Status string `json:"status"`
}

// pendingLiveHalfStatus is the status every live half carries until the
// adoption slices land and the container driver records it.
const pendingLiveHalfStatus = "pending"

// TestRecordProbe is the container driver's probe and, in-repo, its
// contract: the probe asks the production selection rule (with ownership
// and adoption enabled — the posture the green slices land) about a
// headless spec and an interactive control, carries the live matrix rows
// so the record lists exactly the cases this revision defines, and carries
// the live halves of the adoption-gated local rows as pending slots so
// the record lists them too. A row whose live half the probe omits is a
// row the container silently stops recording.
func TestRecordProbe(t *testing.T) {
	t.Parallel()
	probe := adoptionProbeForHeadlessSpec(defaultAdoptionProbeDaemon())
	rec := recordProbe{
		HeadlessOwned:      probe.ownsHeadless,
		InteractiveOwned:   probe.ownsControl,
		CompositionPending: probe.compositionPending,
		OwnershipEnabled:   probe.ownershipEnabled,
		AdoptionEnabled:    probe.adoptionEnabled,
	}
	for _, c := range failureMatrix {
		if !c.local {
			rec.LiveCases = append(rec.LiveCases, recordProbeCase{Name: c.name, Want: c.want, RequiresAdoption: c.requiresAdoption})
		}
		if c.liveHalf != "" {
			rec.LiveHalves = append(rec.LiveHalves, recordProbeHalf{Row: c.name, Slot: c.liveHalf, Status: pendingLiveHalfStatus})
		}
	}
	if *recordProbePath != "" {
		raw, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal record probe: %v", err)
		}
		if err := os.WriteFile(*recordProbePath, raw, 0o600); err != nil {
			t.Fatalf("write record probe: %v", err)
		}
	}
	if !rec.InteractiveOwned {
		t.Fatal("the selection rule does not own the interactive control spec; the probe is not reading the rule")
	}
	// The green posture is settled ownership plus adoption: a probe that
	// answers from any other posture — a composition still pending, or
	// either flag off — has not proven the rule still refuses headless
	// seats. The container driver reports that record stale; this pins
	// the probe end of the contract so a posture regression fails
	// in-repo instead of passing as the red.
	if rec.CompositionPending {
		t.Fatal("the probe daemon's composition is pending; the selection-rule reading was taken under a posture the green slices would never settle in")
	}
	if !rec.OwnershipEnabled || !rec.AdoptionEnabled {
		t.Fatalf("probe posture ownership=%v adoption=%v, want both enabled: the stale check must read the posture, not just the rule", rec.OwnershipEnabled, rec.AdoptionEnabled)
	}
	if len(rec.LiveCases) == 0 || rec.LiveCases[0].Name != "seat-survives-upgrade" {
		t.Fatalf("live cases = %+v, want seat-survives-upgrade first: the container records it as the red case", rec.LiveCases)
	}
	wantHalves := matrixLiveHalves()
	if len(rec.LiveHalves) != len(wantHalves) {
		t.Fatalf("live halves = %+v, want the %d gated slots %+v: every gated live half must ride the probe", rec.LiveHalves, len(wantHalves), wantHalves)
	}
	for i, slot := range wantHalves {
		if rec.LiveHalves[i].Slot != slot || rec.LiveHalves[i].Status != pendingLiveHalfStatus {
			t.Fatalf("live halves = %+v, want slot %q pending at index %d", rec.LiveHalves, slot, i)
		}
	}
}

// TestOwnershipModeIsNotAHeadlessLaunchSignal pins why the container driver
// decides its record from the selection rule and never from the status
// route's sessionShim.ownershipMode: the projection is derived from the two
// configuration flags alone, so a daemon configured for adoption and
// ownership reports adoption_and_ownership while its selection rule still
// refuses headless seats. A driver that branched on the projection would
// record the live cases green on a build where the restart still kills the
// headless seat.
func TestOwnershipModeIsNotAHeadlessLaunchSignal(t *testing.T) {
	t.Parallel()
	d := defaultAdoptionProbeDaemon()
	if mode := d.SessionShimDiagnostics().OwnershipMode; mode != afclient.DaemonSessionShimAdoptionAndOwnership {
		t.Fatalf("ownershipMode = %q, want %q for a daemon configured for adoption and ownership", mode, afclient.DaemonSessionShimAdoptionAndOwnership)
	}
	if d.SessionShimOwnsSession(daemon.SessionSpec{SessionID: "acceptance-probe-headless"}) {
		t.Fatal("the selection rule owns the headless probe spec; replace the red record with the green acceptance assertion")
	}
}
