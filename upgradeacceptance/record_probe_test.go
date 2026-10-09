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
// N+1 build's own selection-rule answers and the live matrix rows it
// records. Nothing in it is read from configuration.
type recordProbe struct {
	HeadlessOwned    bool              `json:"headlessOwned"`
	InteractiveOwned bool              `json:"interactiveOwned"`
	LiveCases        []recordProbeCase `json:"liveCases"`
}

type recordProbeCase struct {
	Name             string `json:"name"`
	Want             string `json:"want"`
	RequiresAdoption bool   `json:"requiresAdoption"`
}

// TestRecordProbe is the container driver's probe and, in-repo, its
// contract: the probe asks the production selection rule (with ownership
// and adoption enabled — the posture the green slices land) about a
// headless spec and an interactive control, and carries the live matrix
// rows so the record lists exactly the cases this revision defines.
func TestRecordProbe(t *testing.T) {
	t.Parallel()
	probe := adoptionProbeForHeadlessSpec(defaultAdoptionProbeDaemon())
	rec := recordProbe{HeadlessOwned: probe.ownsHeadless, InteractiveOwned: probe.ownsControl}
	for _, c := range failureMatrix {
		if !c.local {
			rec.LiveCases = append(rec.LiveCases, recordProbeCase{Name: c.name, Want: c.want, RequiresAdoption: c.requiresAdoption})
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
	if len(rec.LiveCases) == 0 || rec.LiveCases[0].Name != "seat-survives-upgrade" {
		t.Fatalf("live cases = %+v, want seat-survives-upgrade first: the container records it as the red case", rec.LiveCases)
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
