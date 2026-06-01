package codesurvival

import (
	"context"
	"errors"
	"testing"
)

// stubTSRunner drives a golden JSON output (or an error / absence) without
// spawning node. It mirrors what reachability.js prints — the same contract
// codesurvival/reachability/ts-morph/reachability.test.js asserts.
type stubTSRunner struct {
	out      []byte
	err      error
	avail    bool
	gotFiles []string
}

func (s *stubTSRunner) available() (string, bool) { return "/baked/reachability.js", s.avail }

func (s *stubTSRunner) run(_ context.Context, _, _ string, files []string) ([]byte, error) {
	s.gotFiles = files
	return s.out, s.err
}

// goldenTSReport is the byte-for-byte output the node fixture test asserts:
// GET + liveHelper hot, deadHelper cold. The Go side parses the same shape.
const goldenTSReport = `{"status":"ok","language":"ts","symbols":[` +
	`{"file":"app/api/hello/route.ts","symbol":"liveHelper","startLine":1,"endLine":3,"reachable":"hot"},` +
	`{"file":"app/api/hello/route.ts","symbol":"deadHelper","startLine":5,"endLine":7,"reachable":"cold"},` +
	`{"file":"app/api/hello/route.ts","symbol":"GET","startLine":9,"endLine":11,"reachable":"hot"}]}`

func TestAnalyzeTSReachability_GoldenOutput(t *testing.T) {
	runner := &stubTSRunner{out: []byte(goldenTSReport), avail: true}
	surviving := map[string][]int{"app/api/hello/route.ts": {2, 6, 10}}

	res := analyzeTSReachability(context.Background(), discardLogger(), runner, "/repo", surviving)
	if res.partial {
		t.Fatalf("golden output is status:ok, should not be partial")
	}
	if len(res.spans) != 3 {
		t.Fatalf("spans = %d, want 3", len(res.spans))
	}
	bySym := map[string]symbolSpan{}
	for _, s := range res.spans {
		bySym[s.symbol] = s
	}
	if bySym["GET"].reachable != ReachableHot || bySym["liveHelper"].reachable != ReachableHot {
		t.Errorf("GET/liveHelper should be hot: %+v", bySym)
	}
	if bySym["deadHelper"].reachable != ReachableCold {
		t.Errorf("deadHelper should be cold: %+v", bySym["deadHelper"])
	}

	// Classification: line 2 (liveHelper) hot, 6 (deadHelper) cold, 10 (GET) hot.
	cls := classifySurvivingLines(surviving, res)
	if cls.hotLines != 2 || cls.coldLines != 1 {
		t.Errorf("cls = %+v, want hot=2 cold=1", cls)
	}
}

func TestAnalyzeTSReachability_ToolchainAbsent_Partial(t *testing.T) {
	runner := &stubTSRunner{avail: false}
	surviving := map[string][]int{"a.ts": {1}}
	res := analyzeTSReachability(context.Background(), discardLogger(), runner, "/repo", surviving)
	if !res.partial {
		t.Errorf("absent node/script must degrade to partial")
	}
	if len(res.spans) != 0 {
		t.Errorf("no spans expected when toolchain absent, got %d", len(res.spans))
	}
}

func TestAnalyzeTSReachability_SubprocessError_Partial(t *testing.T) {
	runner := &stubTSRunner{avail: true, err: errors.New("node: killed (timeout)")}
	surviving := map[string][]int{"a.ts": {1}}
	res := analyzeTSReachability(context.Background(), discardLogger(), runner, "/repo", surviving)
	if !res.partial {
		t.Errorf("subprocess crash/timeout must degrade to partial")
	}
}

func TestAnalyzeTSReachability_ScriptReportsPartial(t *testing.T) {
	runner := &stubTSRunner{avail: true, out: []byte(`{"status":"partial","language":"ts","symbols":[]}`)}
	surviving := map[string][]int{"a.ts": {1}}
	res := analyzeTSReachability(context.Background(), discardLogger(), runner, "/repo", surviving)
	if !res.partial {
		t.Errorf("script status:partial must propagate")
	}
}

func TestAnalyzeTSReachability_UnparseableOutput_Partial(t *testing.T) {
	runner := &stubTSRunner{avail: true, out: []byte(`not json at all`)}
	surviving := map[string][]int{"a.ts": {1}}
	res := analyzeTSReachability(context.Background(), discardLogger(), runner, "/repo", surviving)
	if !res.partial {
		t.Errorf("unparseable stdout must degrade to partial")
	}
}

func TestAnalyzeTSReachability_NoTSFiles_NoOp(t *testing.T) {
	runner := &stubTSRunner{avail: true, out: []byte(goldenTSReport)}
	surviving := map[string][]int{"main.go": {1}}
	res := analyzeTSReachability(context.Background(), discardLogger(), runner, "/repo", surviving)
	if res.partial {
		t.Errorf("no TS files should be a clean no-op, not partial")
	}
	if len(res.spans) != 0 {
		t.Errorf("no TS files → no spans")
	}
}
