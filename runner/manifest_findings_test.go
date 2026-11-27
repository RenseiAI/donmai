package runner

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestNormalizeManifestFindingsBoundsBlockingFirst(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	var in []agent.TurnManifestFinding
	for i := 1; i <= 80; i++ {
		f := agent.TurnManifestFinding{ID: "B" + strconv.Itoa(i), Title: "finding " + strconv.Itoa(i), Fix: "fix it", Blocking: &no}
		if i%20 == 0 {
			f.Blocking = &yes
		}
		in = append(in, f)
	}
	out := NormalizeManifestFindings(in)
	if len(out) == 0 || len(out) > MaxManifestFindings {
		t.Fatalf("kept %d findings; want 1..%d", len(out), MaxManifestFindings)
	}
	data, _ := json.Marshal(out)
	if len(data) > MaxManifestFindingsBytes {
		t.Fatalf("posted findings are %d bytes; cap is %d", len(data), MaxManifestFindingsBytes)
	}
	for i, id := range []string{"B20", "B40", "B60", "B80"} {
		if out[i].ID != id || !*out[i].Blocking {
			t.Fatalf("finding %d = %+v; every blocking finding must survive, first", i, out[i])
		}
	}
	if got := NormalizeManifestFindings([]agent.TurnManifestFinding{{Severity: "high"}}); got != nil {
		t.Fatalf("an empty finding survived: %+v", got)
	}
}

// TestNormalizeManifestFindingsClipsAnOversizedFinding pins that one finding
// whose encoded size alone exceeds the byte budget is shortened to fit, not
// shed: shedding it would post a REQUEST_CHANGES review with no finding.
// JSON escapes '<' as six bytes, so maximal text of it is the worst case.
func TestNormalizeManifestFindingsClipsAnOversizedFinding(t *testing.T) {
	t.Parallel()
	yes := true
	worst := strings.Repeat("<", 1000)
	out := NormalizeManifestFindings([]agent.TurnManifestFinding{{
		ID: "B1", Blocking: &yes, Severity: "high", Line: 7,
		Title: worst, Fix: worst, Path: worst, Category: worst, Kind: worst,
	}})
	if len(out) != 1 {
		t.Fatalf("kept %d findings; the one blocking finding must survive", len(out))
	}
	if f := out[0]; f.ID != "B1" || !*f.Blocking || f.Severity != "high" || f.Line != 7 || f.Title == "" {
		t.Fatalf("clipped finding lost its gate row: %+v", f)
	}
	data, _ := json.Marshal(out)
	if len(data) > MaxManifestFindingsBytes {
		t.Fatalf("posted findings are %d bytes; cap is %d", len(data), MaxManifestFindingsBytes)
	}
}
