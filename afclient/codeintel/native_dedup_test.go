package codeintel

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dedupBaseSource builds a small, varied source file used as the indexed corpus
// for the dedup tests.
func dedupBaseSource() string {
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet", "kilo", "lima"}
	var b strings.Builder
	b.WriteString("package records\n\n")
	for i, w := range words {
		b.WriteString(fmt.Sprintf("func handle_%s_%d(ctx Context, %s Payload) (Result, error) { return process_%s(ctx) }\n", w, i, w, w))
	}
	return b.String()
}

// TestCheckDuplicate_ExactContentFlagged proves exact dedup compares a query's
// normalised-content hash against the persisted ContentHash of real file
// content — not against serialized symbol text. A byte-identical copy of an
// indexed file's content must be flagged exact-dup.
//
// RED (against the old fileIndexToText symbol-text hashing):
//
//	native_dedup_test.go: byte-identical content got matchType "none"; want "exact"
func TestCheckDuplicate_ExactContentFlagged(t *testing.T) {
	dir := t.TempDir()
	base := dedupBaseSource()
	writeFile(t, filepath.Join(dir, "records.go"), base)

	nr := NewNativeRunner(dir)
	if _, err := nr.BuildIndex(GetRepoMapOptions{}); err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}

	out, err := nr.CheckDuplicateNative(CheckDuplicateOptions{Content: base})
	if err != nil {
		t.Fatalf("CheckDuplicateNative: %v", err)
	}
	m := out.(map[string]any)
	if m["matchType"] != "exact" {
		t.Errorf("byte-identical content got matchType %q; want \"exact\"", m["matchType"])
	}
	if m["isDuplicate"] != true {
		t.Errorf("byte-identical content isDuplicate=%v; want true", m["isDuplicate"])
	}
	if m["existingId"] != "records.go" {
		t.Errorf("existingId=%v; want records.go", m["existingId"])
	}
}

// TestCheckDuplicate_NearContentFlagged proves near dedup compares SimHash
// fingerprints computed over REAL file content. A lightly-edited copy (one
// renamed identifier) must be flagged near-dup within the default threshold.
//
// RED (against the old SimHash-over-symbol-text corpus):
//
//	native_dedup_test.go: lightly-edited content got matchType "none"; want "near"
func TestCheckDuplicate_NearContentFlagged(t *testing.T) {
	dir := t.TempDir()
	base := dedupBaseSource()
	writeFile(t, filepath.Join(dir, "records.go"), base)

	nr := NewNativeRunner(dir)
	if _, err := nr.BuildIndex(GetRepoMapOptions{}); err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}

	// One-line edit: rename the bravo handler + its param (measured Hamming=1).
	edited := strings.Replace(base,
		"handle_bravo_1(ctx Context, bravo Payload)",
		"handle_zulu_1(ctx Context, zulu Payload)", 1)
	if edited == base {
		t.Fatal("edit did not change content")
	}

	out, err := nr.CheckDuplicateNative(CheckDuplicateOptions{Content: edited})
	if err != nil {
		t.Fatalf("CheckDuplicateNative: %v", err)
	}
	m := out.(map[string]any)
	if m["matchType"] != "near" {
		t.Errorf("lightly-edited content got matchType %q; want \"near\"", m["matchType"])
	}
	if m["isDuplicate"] != true {
		t.Errorf("lightly-edited content isDuplicate=%v; want true", m["isDuplicate"])
	}
	if m["existingId"] != "records.go" {
		t.Errorf("existingId=%v; want records.go", m["existingId"])
	}
	if hd, _ := m["hammingDistance"].(int); hd < 0 || hd > SimHashDefaultThreshold {
		t.Errorf("hammingDistance=%v; want within [0,%d]", m["hammingDistance"], SimHashDefaultThreshold)
	}
}

// ── Symbol-granular dedup (schema v4) ────────────────────────────────────────

// dedupTargetFunc is the ~10-line function whose duplicate hides inside a
// large file — the exact failure shape from the decision-gate eval: file-level
// hashing drowns it out (matchType "none"), so the agent greps anyway.
const dedupTargetFunc = `func computeRollingChecksum(data []byte, window int) uint32 {
	var sum uint32
	for i, b := range data {
		sum += uint32(b) * uint32(i%window+1)
		if i >= window {
			sum -= uint32(data[i-window])
		}
		sum = sum<<1 | sum>>31
	}
	return sum
}`

// dedupLargeSource builds a 100+ line Go file that embeds dedupTargetFunc
// among lots of unrelated code, so the whole-file SimHash is dominated by the
// filler.
func dedupLargeSource() string {
	var b strings.Builder
	b.WriteString("package bigpkg\n\n")
	fillers := []string{"orders", "invoices", "payments", "refunds", "ledgers", "audits", "batches", "queues", "streams", "shards", "tokens", "cursors", "buffers", "windows", "frames"}
	for i, w := range fillers {
		fmt.Fprintf(&b, "// handle%d processes %s records for the %s subsystem.\n", i, w, w)
		fmt.Fprintf(&b, "func process_%s_%d(ctx Context, in %sInput) (Out, error) {\n", w, i, w)
		fmt.Fprintf(&b, "	validated, err := validate_%s(ctx, in)\n", w)
		b.WriteString("	if err != nil {\n		return Out{}, err\n	}\n")
		fmt.Fprintf(&b, "	return transform_%s(validated), nil\n}\n\n", w)
	}
	b.WriteString(dedupTargetFunc)
	b.WriteString("\n")
	return b.String()
}

// TestCheckDuplicate_SymbolInLargeFileExact proves symbol-granular dedup: an
// exact copy of a ~10-line function that lives inside a 100+ line file must be
// flagged as an exact duplicate WITH the symbol name and line, so the agent
// needs no grep follow-up.
//
// RED (against whole-file-only hashing):
//
//	native_dedup_test.go: symbol-in-large-file copy got matchType "none"; want "exact"
func TestCheckDuplicate_SymbolInLargeFileExact(t *testing.T) {
	dir := t.TempDir()
	src := dedupLargeSource()
	if n := strings.Count(src, "\n"); n < 100 {
		t.Fatalf("fixture too small: %d lines; want 100+", n)
	}
	writeFile(t, filepath.Join(dir, "big.go"), src)

	nr := NewNativeRunner(dir)
	if _, err := nr.BuildIndex(GetRepoMapOptions{}); err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}

	out, err := nr.CheckDuplicateNative(CheckDuplicateOptions{Content: dedupTargetFunc})
	if err != nil {
		t.Fatalf("CheckDuplicateNative: %v", err)
	}
	m := out.(map[string]any)
	if m["matchType"] != "exact" {
		t.Fatalf("symbol-in-large-file copy got matchType %q; want \"exact\" (whole-file hashing drowns out the symbol)", m["matchType"])
	}
	if m["isDuplicate"] != true {
		t.Errorf("isDuplicate=%v; want true", m["isDuplicate"])
	}
	if m["existingId"] != "big.go" {
		t.Errorf("existingId=%v; want big.go", m["existingId"])
	}
	if m["filePath"] != "big.go" {
		t.Errorf("filePath=%v; want big.go", m["filePath"])
	}
	if m["symbolName"] != "computeRollingChecksum" {
		t.Errorf("symbolName=%v; want computeRollingChecksum", m["symbolName"])
	}
	// The declaration line of the target inside the large file (1-based).
	wantLine := strings.Count(strings.SplitAfter(src, "func computeRollingChecksum")[0], "\n") + 1
	if m["line"] != wantLine {
		t.Errorf("line=%v; want %d", m["line"], wantLine)
	}
}

// TestCheckDuplicate_SymbolInLargeFileNear proves the near tier at symbol
// granularity: a lightly-edited copy (renamed function) of the buried function
// must be flagged near with the symbol identity carried through.
func TestCheckDuplicate_SymbolInLargeFileNear(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "big.go"), dedupLargeSource())

	nr := NewNativeRunner(dir)
	if _, err := nr.BuildIndex(GetRepoMapOptions{}); err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}

	// One-token edit: rename the window parameter (measured Hamming=2 over the
	// symbol body — comfortably inside the default threshold of 3, while the
	// whole-file fingerprint stays far outside it).
	edited := strings.Replace(dedupTargetFunc, "window int", "windowSize int", 1)
	out, err := nr.CheckDuplicateNative(CheckDuplicateOptions{Content: edited})
	if err != nil {
		t.Fatalf("CheckDuplicateNative: %v", err)
	}
	m := out.(map[string]any)
	if m["matchType"] != "near" {
		t.Fatalf("edited symbol copy got matchType %q; want \"near\"", m["matchType"])
	}
	if m["symbolName"] != "computeRollingChecksum" {
		t.Errorf("symbolName=%v; want computeRollingChecksum", m["symbolName"])
	}
	if m["filePath"] != "big.go" {
		t.Errorf("filePath=%v; want big.go", m["filePath"])
	}
	if hd, _ := m["hammingDistance"].(int); hd <= 0 || hd > SimHashDefaultThreshold {
		t.Errorf("hammingDistance=%v; want within (0,%d]", m["hammingDistance"], SimHashDefaultThreshold)
	}
}

// TestCheckDuplicate_MaxResultsBoundsMatches verifies the result is bounded to
// the single top match by default, and that MaxResults>1 opts into a ranked
// matches list.
func TestCheckDuplicate_MaxResultsBoundsMatches(t *testing.T) {
	dir := t.TempDir()
	// The same target function duplicated in two files.
	writeFile(t, filepath.Join(dir, "a.go"), "package a\n\n"+dedupTargetFunc+"\n")
	writeFile(t, filepath.Join(dir, "b.go"), "package b\n\n"+dedupTargetFunc+"\n")

	nr := NewNativeRunner(dir)
	if _, err := nr.BuildIndex(GetRepoMapOptions{}); err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}

	// Default: top match only — no matches array.
	out, err := nr.CheckDuplicateNative(CheckDuplicateOptions{Content: dedupTargetFunc})
	if err != nil {
		t.Fatalf("CheckDuplicateNative: %v", err)
	}
	m := out.(map[string]any)
	if m["matchType"] != "exact" {
		t.Fatalf("matchType=%q; want exact", m["matchType"])
	}
	if _, present := m["matches"]; present {
		t.Errorf("default result carries a matches array; want top match only")
	}

	// MaxResults=2: both duplicate sites, deterministic order (a.go first).
	out, err = nr.CheckDuplicateNative(CheckDuplicateOptions{Content: dedupTargetFunc, MaxResults: 2})
	if err != nil {
		t.Fatalf("CheckDuplicateNative(MaxResults=2): %v", err)
	}
	m = out.(map[string]any)
	matches, ok := m["matches"].([]DupMatch)
	if !ok {
		t.Fatalf("matches = %T; want []DupMatch", m["matches"])
	}
	if len(matches) != 2 {
		t.Fatalf("len(matches)=%d; want 2", len(matches))
	}
	if matches[0].FilePath != "a.go" || matches[1].FilePath != "b.go" {
		t.Errorf("match order = %s, %s; want a.go, b.go (deterministic)", matches[0].FilePath, matches[1].FilePath)
	}
}

// TestCheckDuplicate_ShortSymbolsNotHashed documents the cost bound: symbols
// spanning fewer than symbolHashMinLines lines are not hashed at symbol
// granularity (whole-file hashing still covers them), keeping the per-symbol
// index overhead sane on big repos.
func TestCheckDuplicate_ShortSymbolsNotHashed(t *testing.T) {
	dir := t.TempDir()
	// dedupBaseSource is all 1-line functions — below the threshold.
	writeFile(t, filepath.Join(dir, "records.go"), dedupBaseSource())

	nr := NewNativeRunner(dir)
	if _, err := nr.BuildIndex(GetRepoMapOptions{}); err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	idx := nr.loadIndex()
	fi, ok := idx.Files["records.go"]
	if !ok {
		t.Fatal("records.go missing from index")
	}
	if len(fi.SymbolHashes) != 0 {
		t.Errorf("1-line symbols got %d symbol hashes; want 0 (below %d-line threshold)", len(fi.SymbolHashes), symbolHashMinLines)
	}
}

// TestSymbolHashing_BuildCostSane guards the warm-index build floor (W0
// benchmark: ~0.6s cold on 4.3k files): per-symbol hashing runs only at
// extraction time and must not blow up the cold build. 200 synthetic files ×
// 12 hashable symbols each must index in well under the generous ceiling —
// a pathological (e.g. quadratic) regression trips it, timing noise does not.
func TestSymbolHashing_BuildCostSane(t *testing.T) {
	if testing.Short() {
		t.Skip("cost guard skipped in -short")
	}
	dir := t.TempDir()
	for f := 0; f < 200; f++ {
		var b strings.Builder
		fmt.Fprintf(&b, "package p%d\n\n", f)
		for s := 0; s < 12; s++ {
			fmt.Fprintf(&b, "func fn_%d_%d(a, b int) int {\n	c := a*%d + b\n	c += a %% (b + %d)\n	return c\n}\n\n", f, s, s+1, s+2)
		}
		writeFile(t, filepath.Join(dir, fmt.Sprintf("f%03d.go", f)), b.String())
	}

	nr := NewNativeRunner(dir)
	start := time.Now()
	if _, err := nr.BuildIndex(GetRepoMapOptions{}); err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Errorf("cold BuildIndex with symbol hashing took %v on 200 files; want well under 10s (W0 floor was ~0.6s on 4.3k files)", elapsed)
	}

	// And the hashes actually landed.
	idx := nr.loadIndex()
	fi := idx.Files["f000.go"]
	if len(fi.SymbolHashes) != 12 {
		t.Errorf("f000.go has %d symbol hashes; want 12", len(fi.SymbolHashes))
	}
}

// BenchmarkComputeSymbolHashes measures the per-file marginal cost of
// symbol-granular hashing (schema v4) so the warm-build floor stays visible.
func BenchmarkComputeSymbolHashes(b *testing.B) {
	src := dedupLargeSource()
	ast := (&GoExtractor{}).Extract(src, "big.go")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := ComputeSymbolHashes(src, ast.Symbols); len(got) == 0 {
			b.Fatal("no symbol hashes computed")
		}
	}
}

// TestCheckDuplicate_UnrelatedContentNone verifies unrelated content is not a
// duplicate.
func TestCheckDuplicate_UnrelatedContentNone(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "records.go"), dedupBaseSource())

	nr := NewNativeRunner(dir)
	if _, err := nr.BuildIndex(GetRepoMapOptions{}); err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}

	out, err := nr.CheckDuplicateNative(CheckDuplicateOptions{
		Content: "package other\n\nvar unrelatedGlobalTable = map[string]int{\"x\": 1, \"y\": 2, \"z\": 3}\n",
	})
	if err != nil {
		t.Fatalf("CheckDuplicateNative: %v", err)
	}
	m := out.(map[string]any)
	if m["matchType"] != "none" {
		t.Errorf("unrelated content got matchType %q; want \"none\"", m["matchType"])
	}
}
