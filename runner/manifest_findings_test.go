package runner

import (
	"encoding/json"
	"strconv"
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
