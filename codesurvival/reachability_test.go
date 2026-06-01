package codesurvival

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// --- W_COLD weighting math -------------------------------------------------

func TestComputeHotWeighted(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	tests := []struct {
		name    string
		cls     classifyResult
		total   int
		wCold   float64
		wantHot int
		wantCol int
		wantPct *float64
	}{
		{
			// 8 hot + 4 cold of 100 total, W_COLD=0.25 → (8 + 0.25*4)/100 = 9%.
			name:  "default w_cold down-weights cold",
			cls:   classifyResult{hotLines: 8, coldLines: 4},
			total: 100, wCold: 0.25, wantHot: 8, wantCol: 4, wantPct: f(9),
		},
		{
			// unknown folds into hot: (6+2 unknown + 0.25*0)/100 = 8%.
			name:  "unknown counts as hot",
			cls:   classifyResult{hotLines: 6, unknownLines: 2, coldLines: 0},
			total: 100, wCold: 0.25, wantHot: 8, wantCol: 0, wantPct: f(8),
		},
		{
			name:  "all cold, w_cold=0.25",
			cls:   classifyResult{coldLines: 10},
			total: 40, wCold: 0.25, wantHot: 0, wantCol: 10, wantPct: f(6.25),
		},
		{
			name:  "zero total → nil rate",
			cls:   classifyResult{hotLines: 0},
			total: 0, wCold: 0.25, wantHot: 0, wantCol: 0, wantPct: nil,
		},
		{
			// w_cold=1.0 means cold lines weigh fully: (5+1.0*5)/10 = 100%.
			name:  "w_cold=1 no down-weight",
			cls:   classifyResult{hotLines: 5, coldLines: 5},
			total: 10, wCold: 1.0, wantHot: 5, wantCol: 5, wantPct: f(100),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hw := computeHotWeighted(tt.cls, tt.total, tt.wCold)
			if hw.HotLinesSurviving != tt.wantHot {
				t.Errorf("hot = %d, want %d", hw.HotLinesSurviving, tt.wantHot)
			}
			if hw.ColdLinesSurviving != tt.wantCol {
				t.Errorf("cold = %d, want %d", hw.ColdLinesSurviving, tt.wantCol)
			}
			if hw.WCold != tt.wCold {
				t.Errorf("wCold = %v, want %v", hw.WCold, tt.wCold)
			}
			switch {
			case tt.wantPct == nil && hw.HotWeightedRatePct != nil:
				t.Errorf("pct = %v, want nil", *hw.HotWeightedRatePct)
			case tt.wantPct != nil && hw.HotWeightedRatePct == nil:
				t.Errorf("pct = nil, want %v", *tt.wantPct)
			case tt.wantPct != nil && *hw.HotWeightedRatePct != *tt.wantPct:
				t.Errorf("pct = %v, want %v", *hw.HotWeightedRatePct, *tt.wantPct)
			}
		})
	}
}

// --- classification: hot / cold / unknown ---------------------------------

func TestClassifySurvivingLines(t *testing.T) {
	// One file with two symbols: liveFn (hot, lines 1-5) and deadFn (cold, 7-9).
	// Surviving lines: 2,3 (in liveFn → hot), 8 (in deadFn → cold), 20 (covered
	// by no span → unknown).
	passes := []reachabilityResult{{
		language: "go",
		spans: []symbolSpan{
			{file: "a.go", symbol: "liveFn", startLine: 1, endLine: 5, reachable: ReachableHot},
			{file: "a.go", symbol: "deadFn", startLine: 7, endLine: 9, reachable: ReachableCold},
		},
	}}
	surviving := map[string][]int{"a.go": {2, 3, 8, 20}}

	cls := classifySurvivingLines(surviving, passes...)
	if cls.hotLines != 2 {
		t.Errorf("hot = %d, want 2 (lines 2,3 in liveFn)", cls.hotLines)
	}
	if cls.coldLines != 1 {
		t.Errorf("cold = %d, want 1 (line 8 in deadFn)", cls.coldLines)
	}
	if cls.unknownLines != 1 {
		t.Errorf("unknown = %d, want 1 (line 20 covered by no span)", cls.unknownLines)
	}
	// perSymbol: liveFn(2), deadFn(1). Line 20 matched no span → no row.
	bySym := map[string]ScanSymbolBreakdown{}
	for _, s := range cls.perSymbol {
		bySym[s.Symbol] = s
	}
	if bySym["liveFn"].LinesSurviving != 2 || bySym["liveFn"].Reachable != ReachableHot {
		t.Errorf("liveFn row = %+v", bySym["liveFn"])
	}
	if bySym["deadFn"].LinesSurviving != 1 || bySym["deadFn"].Reachable != ReachableCold {
		t.Errorf("deadFn row = %+v", bySym["deadFn"])
	}
}

// TestClassify_UnknownSpanIsHot verifies a dynamic/unresolved symbol (reachable
// == unknown) is folded into hot by computeHotWeighted (no down-weight).
func TestClassify_UnknownSpanIsHot(t *testing.T) {
	passes := []reachabilityResult{{
		language: "ts",
		spans: []symbolSpan{
			{file: "x.ts", symbol: "dyn", startLine: 1, endLine: 3, reachable: ReachableUnknown},
		},
	}}
	surviving := map[string][]int{"x.ts": {2}}
	cls := classifySurvivingLines(surviving, passes...)
	if cls.unknownLines != 1 || cls.hotLines != 0 || cls.coldLines != 0 {
		t.Fatalf("cls = %+v, want 1 unknown", cls)
	}
	hw := computeHotWeighted(cls, 1, 0.25)
	// unknown → hot, so hot=1, cold=0, rate = 100%.
	if hw.HotLinesSurviving != 1 || hw.ColdLinesSurviving != 0 {
		t.Errorf("hw = %+v, want hot=1 cold=0", hw)
	}
	if hw.HotWeightedRatePct == nil || *hw.HotWeightedRatePct != 100 {
		t.Errorf("rate = %v, want 100 (unknown not down-weighted)", hw.HotWeightedRatePct)
	}
}

// TestClassify_MixedLanguageUnion verifies a .go and a .ts file each classified
// by its own pass union into one classification, and that the stronger signal
// wins when two spans overlap on the same line.
func TestClassify_MixedLanguageUnion(t *testing.T) {
	goPass := reachabilityResult{language: "go", spans: []symbolSpan{
		{file: "main.go", symbol: "main", startLine: 1, endLine: 4, reachable: ReachableHot},
		// overlapping spans on api.go line 2: one cold, one hot → hot must win.
		{file: "api.go", symbol: "lo", startLine: 1, endLine: 3, reachable: ReachableCold},
		{file: "api.go", symbol: "hi", startLine: 2, endLine: 2, reachable: ReachableHot},
	}}
	tsPass := reachabilityResult{language: "ts", spans: []symbolSpan{
		{file: "route.ts", symbol: "GET", startLine: 1, endLine: 5, reachable: ReachableHot},
		{file: "route.ts", symbol: "dead", startLine: 7, endLine: 9, reachable: ReachableCold},
	}}
	surviving := map[string][]int{
		"main.go":  {2},    // hot
		"api.go":   {2},    // overlap hot+cold → hot
		"route.ts": {3, 8}, // 3 hot (GET), 8 cold (dead)
	}
	cls := classifySurvivingLines(surviving, goPass, tsPass)
	if cls.hotLines != 3 {
		t.Errorf("hot = %d, want 3 (main.go:2, api.go:2 union-hot, route.ts:3)", cls.hotLines)
	}
	if cls.coldLines != 1 {
		t.Errorf("cold = %d, want 1 (route.ts:8)", cls.coldLines)
	}
}

// --- Go reachability over a fixture module --------------------------------

// writeGoFixture builds a minimal main-package module with a reachable function
// (called from main) and a dead one (never referenced). Returns the module dir.
func writeGoFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module fixture\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(dir, "main.go"), `package main

func liveHelper() int {
	return 42
}

func deadHelper() int {
	return 7
}

func main() {
	_ = liveHelper()
}
`)
	return dir
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAnalyzeGoReachability_DeadVsLive(t *testing.T) {
	dir := writeGoFixture(t)
	surviving := map[string][]int{"main.go": {2, 3, 4, 6, 7, 8, 11, 12}}

	res := analyzeGoReachability(context.Background(), discardLogger(), dir, surviving)
	if res.partial {
		t.Fatalf("expected a clean (non-partial) Go pass, got partial")
	}
	bySym := map[string]symbolSpan{}
	for _, s := range res.spans {
		bySym[s.symbol] = s
	}
	if bySym["main"].reachable != ReachableHot {
		t.Errorf("main = %q, want hot", bySym["main"].reachable)
	}
	if bySym["liveHelper"].reachable != ReachableHot {
		t.Errorf("liveHelper = %q, want hot (called from main)", bySym["liveHelper"].reachable)
	}
	if bySym["deadHelper"].reachable != ReachableCold {
		t.Errorf("deadHelper = %q, want cold (unreferenced)", bySym["deadHelper"].reachable)
	}

	// Classification: surviving lines inside liveHelper(2-4) hot, deadHelper(6-8)
	// cold, main(10-12) hot.
	cls := classifySurvivingLines(surviving, res)
	if cls.coldLines == 0 {
		t.Errorf("expected some cold lines (deadHelper), got 0")
	}
	if cls.hotLines == 0 {
		t.Errorf("expected some hot lines (live/main), got 0")
	}
}

// TestAnalyzeGoReachability_LibraryOnlyCHA exercises the CHA fallback: a module
// with NO package main, seeded only by an exported handler/ServeHTTP-shaped fn.
func TestAnalyzeGoReachability_LibraryOnlyCHA(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module lib\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(dir, "lib.go"), `package lib

import "net/http"

func helper() string { return "x" }

// Handler has the http.HandlerFunc shape → a user-facing entrypoint seed.
func Handler(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte(helper()))
}

func deadLibFn() string { return "unused" }
`)
	surviving := map[string][]int{"lib.go": {5, 8, 9, 10, 12}}
	res := analyzeGoReachability(context.Background(), discardLogger(), dir, surviving)
	bySym := map[string]symbolSpan{}
	for _, s := range res.spans {
		bySym[s.symbol] = s
	}
	if bySym["Handler"].reachable != ReachableHot {
		t.Errorf("Handler = %q, want hot (http entrypoint via CHA)", bySym["Handler"].reachable)
	}
	if bySym["helper"].reachable != ReachableHot {
		t.Errorf("helper = %q, want hot (called from Handler)", bySym["helper"].reachable)
	}
	if bySym["deadLibFn"].reachable != ReachableCold {
		t.Errorf("deadLibFn = %q, want cold", bySym["deadLibFn"].reachable)
	}
}
