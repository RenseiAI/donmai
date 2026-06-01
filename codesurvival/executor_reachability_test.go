package codesurvival

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
)

// noopReachability is a clean (non-degraded) Go pass that resolves no spans —
// used by the survival-only RW3 tests so RW4 wiring neither degrades the status
// nor down-weights anything (all surviving lines fall in no span → unknown→hot).
func noopReachability(_ context.Context, _ *slog.Logger, _ string, _ map[string][]int) reachabilityResult {
	return reachabilityResult{language: "go"}
}

// blameAtLine builds a --line-porcelain HEAD entry attributed to sha whose FINAL
// (HEAD) line number is n, so survivingLinesByCommit recovers line n.
func blameAtLine(sha string, n int) string {
	return sha + " " + itoa(n) + " " + itoa(n) + " 1\n" +
		"author T\nauthor-mail <t@e>\nsummary x\n\tsrc\n"
}

// rw4Setup wires an executor whose git stub reports one surviving file with
// known HEAD line numbers, capturing the posted result. goReach/tsRunner are
// injected so the test controls reachability without a real clone.
func rw4Setup(t *testing.T, file string, headLines []int, goReach func(ctx context.Context, log *slog.Logger, repoPath string, s map[string][]int) reachabilityResult, ts tsRunner) (*CodeSurvivalScanResult, *atomic.Int32) {
	t.Helper()
	merge := "abcdef0123456789abcdef0123456789abcdef01"
	git := newStubGit()
	git.responses[key([]string{"diff-tree", "--no-commit-id", "--name-only", "-r", "--diff-filter=AM", merge})] = stubResp{out: file}
	// at-merge blame: 4 lines authored.
	git.responses[key([]string{"blame", "-l", "--line-porcelain", merge, "--", file})] = stubResp{out: blameEntry(merge, 1) + blameEntry(merge, 2) + blameEntry(merge, 3) + blameEntry(merge, 4)}
	// at-HEAD blame: surviving lines at the given HEAD line numbers.
	head := ""
	for _, n := range headLines {
		head += blameAtLine(merge, n)
	}
	git.responses[key([]string{"blame", "-l", "--line-porcelain", "HEAD", "--", file})] = stubResp{out: head}

	var hits atomic.Int32
	var gotAuth string
	var got CodeSurvivalScanResult
	srv := captureServer(t, &hits, &gotAuth, &got)
	t.Cleanup(srv.Close)

	exec := NewExecutor(Options{
		GitRunner:        git,
		WorkareaProvider: func() (Workarea, error) { return Workarea{Path: "/tmp/wa", Release: func() {}}, nil },
		HTTPClient:       srv.Client(),
		Logger:           discardLogger(),
		GoReachability:   goReach,
		TSRunner:         ts,
	})
	item := fixtureItem(srv.URL)
	item.MergeSha = merge
	_ = exec.Handle(context.Background(), item)
	return &got, &hits
}

// TestExecutor_RW4_GoHotWeighted: a surviving dead Go function is cold and
// down-weighted vs a reachable one; hotWeighted is populated; survival intact.
func TestExecutor_RW4_GoHotWeighted(t *testing.T) {
	goReach := func(_ context.Context, _ *slog.Logger, _ string, _ map[string][]int) reachabilityResult {
		return reachabilityResult{language: "go", spans: []symbolSpan{
			{file: "main.go", symbol: "live", startLine: 1, endLine: 2, reachable: ReachableHot},
			{file: "main.go", symbol: "dead", startLine: 3, endLine: 4, reachable: ReachableCold},
		}}
	}
	// Surviving HEAD lines 1,2 (live → hot) and 3,4 (dead → cold). Disable TS.
	got, hits := rw4Setup(t, "main.go", []int{1, 2, 3, 4}, goReach, &stubTSRunner{avail: false})
	if hits.Load() != 1 {
		t.Fatalf("expected 1 POST, got %d", hits.Load())
	}
	if got.Status != StatusOK {
		t.Errorf("status = %q, want ok", got.Status)
	}
	// Survival preserved: 4 of 4 at merge survive.
	if got.Survival.LinesTotalAtMerge != 4 || got.Survival.LinesSurviving != 4 {
		t.Errorf("survival = %+v, want total=4 surviving=4", got.Survival)
	}
	if got.HotWeighted == nil {
		t.Fatalf("hotWeighted must be populated on a clean reachability run")
	}
	if got.HotWeighted.HotLinesSurviving != 2 || got.HotWeighted.ColdLinesSurviving != 2 {
		t.Errorf("hot/cold = %d/%d, want 2/2", got.HotWeighted.HotLinesSurviving, got.HotWeighted.ColdLinesSurviving)
	}
	// rate = 100*(2 + 0.25*2)/4 = 62.5
	if got.HotWeighted.HotWeightedRatePct == nil || *got.HotWeighted.HotWeightedRatePct != 62.5 {
		t.Errorf("hotWeightedRatePct = %v, want 62.5", got.HotWeighted.HotWeightedRatePct)
	}
	if got.HotWeighted.WCold != defaultWCold {
		t.Errorf("wCold = %v, want %v", got.HotWeighted.WCold, defaultWCold)
	}
	// perSymbol carries both, the dead one down-weighted by reachable=cold.
	if len(got.PerSymbol) != 2 {
		t.Errorf("perSymbol = %d rows, want 2", len(got.PerSymbol))
	}
}

// TestExecutor_RW4_ToolchainAbsent_Partial: reachability degraded (Go pass
// reports partial) → status:partial, hotWeighted=null, survival intact.
func TestExecutor_RW4_ToolchainAbsent_Partial(t *testing.T) {
	goReach := func(_ context.Context, _ *slog.Logger, _ string, _ map[string][]int) reachabilityResult {
		return reachabilityResult{language: "go", partial: true} // toolchain absent / load fail
	}
	got, _ := rw4Setup(t, "main.go", []int{1, 2, 3, 4}, goReach, &stubTSRunner{avail: false})
	if got.Status != StatusPartial {
		t.Errorf("status = %q, want partial (reachability degraded)", got.Status)
	}
	if got.HotWeighted != nil {
		t.Errorf("hotWeighted = %+v, want null on degrade", got.HotWeighted)
	}
	// Survival MUST be preserved — RW4 never regresses RW3.
	if got.Survival.LinesTotalAtMerge != 4 || got.Survival.LinesSurviving != 4 {
		t.Errorf("survival regressed on reachability degrade: %+v", got.Survival)
	}
	if got.PerSymbol == nil || len(got.PerSymbol) != 0 {
		t.Errorf("perSymbol = %v, want empty slice on degrade", got.PerSymbol)
	}
}

// TestExecutor_RW4_MixedLanguageUnion: a PR touching both .go and .ts runs BOTH
// passes and unions per file.
func TestExecutor_RW4_MixedLanguageUnion(t *testing.T) {
	// The git stub only emits one file via diff-tree, so drive the union at the
	// reachability layer: the Go pass classifies main.go, the TS runner returns
	// spans for route.ts. We feed survivingByFile through both via the stub.
	merge := "abcdef0123456789abcdef0123456789abcdef01"
	git := newStubGit()
	git.responses[key([]string{"diff-tree", "--no-commit-id", "--name-only", "-r", "--diff-filter=AM", merge})] = stubResp{out: "main.go\nroute.ts"}
	for _, f := range []string{"main.go", "route.ts"} {
		git.responses[key([]string{"blame", "-l", "--line-porcelain", merge, "--", f})] = stubResp{out: blameEntry(merge, 1) + blameEntry(merge, 2)}
		git.responses[key([]string{"blame", "-l", "--line-porcelain", "HEAD", "--", f})] = stubResp{out: blameAtLine(merge, 1) + blameAtLine(merge, 2)}
	}

	goReach := func(_ context.Context, _ *slog.Logger, _ string, _ map[string][]int) reachabilityResult {
		return reachabilityResult{language: "go", spans: []symbolSpan{
			{file: "main.go", symbol: "main", startLine: 1, endLine: 2, reachable: ReachableHot},
		}}
	}
	ts := &stubTSRunner{avail: true, out: []byte(`{"status":"ok","language":"ts","symbols":[` +
		`{"file":"route.ts","symbol":"GET","startLine":1,"endLine":1,"reachable":"hot"},` +
		`{"file":"route.ts","symbol":"dead","startLine":2,"endLine":2,"reachable":"cold"}]}`)}

	var hits atomic.Int32
	var gotAuth string
	var got CodeSurvivalScanResult
	srv := captureServer(t, &hits, &gotAuth, &got)
	defer srv.Close()
	exec := NewExecutor(Options{
		GitRunner:        git,
		WorkareaProvider: func() (Workarea, error) { return Workarea{Path: "/tmp/wa", Release: func() {}}, nil },
		HTTPClient:       srv.Client(),
		Logger:           discardLogger(),
		GoReachability:   goReach,
		TSRunner:         ts,
	})
	item := fixtureItem(srv.URL)
	item.MergeSha = merge
	_ = exec.Handle(context.Background(), item)

	if got.Status != StatusOK {
		t.Fatalf("status = %q, want ok", got.Status)
	}
	if got.HotWeighted == nil {
		t.Fatalf("hotWeighted must be populated for mixed-language clean run")
	}
	// main.go: lines 1,2 hot. route.ts: line1 hot (GET), line2 cold (dead).
	// hot = 3 (main 1,2 + route 1), cold = 1 (route 2).
	if got.HotWeighted.HotLinesSurviving != 3 || got.HotWeighted.ColdLinesSurviving != 1 {
		t.Errorf("hot/cold = %d/%d, want 3/1", got.HotWeighted.HotLinesSurviving, got.HotWeighted.ColdLinesSurviving)
	}
	if ts.gotFiles == nil {
		t.Errorf("ts runner should have been invoked for the .ts file")
	}
}

// TestExecutor_RW4_UnsupportedLanguageOnly_Partial: surviving lines exist but
// only in a language no pass analyses (.py) → partial, hotWeighted=null.
func TestExecutor_RW4_UnsupportedLanguageOnly_Partial(t *testing.T) {
	goReach := func(_ context.Context, _ *slog.Logger, _ string, _ map[string][]int) reachabilityResult {
		return reachabilityResult{} // never invoked for .py
	}
	got, _ := rw4Setup(t, "app.py", []int{1, 2}, goReach, &stubTSRunner{avail: false})
	if got.Status != StatusPartial {
		t.Errorf("status = %q, want partial (unsupported language)", got.Status)
	}
	if got.HotWeighted != nil {
		t.Errorf("hotWeighted should be null for unsupported-language-only PR")
	}
}
