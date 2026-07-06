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

// dedupTargetFuncTS is the TS mirror of dedupTargetFunc: a ~10-line function
// whose duplicate hides inside a large .ts file.
const dedupTargetFuncTS = `function computeRollingChecksumTS(data: number[], window: number): number {
  let sum = 0
  for (let i = 0; i < data.length; i++) {
    sum += data[i] * ((i % window) + 1)
    if (i >= window) {
      sum -= data[i - window]
    }
    sum = ((sum << 1) | (sum >>> 31)) >>> 0
  }
  return sum
}`

// dedupLargeSourceTS builds a 100+ line TS file that embeds dedupTargetFuncTS
// among unrelated functions, so the whole-file SimHash is dominated by filler.
func dedupLargeSourceTS() string {
	var b strings.Builder
	b.WriteString("// bigmod: assorted subsystem handlers\n\n")
	fillers := []string{"orders", "invoices", "payments", "refunds", "ledgers", "audits", "batches", "queues", "streams", "shards", "tokens", "cursors", "buffers", "windows", "frames"}
	for i, w := range fillers {
		fmt.Fprintf(&b, "// handle%d processes %s records for the %s subsystem.\n", i, w, w)
		fmt.Fprintf(&b, "export function process_%s_%d(input: %sInput): Out {\n", w, i, w)
		fmt.Fprintf(&b, "  const validated = validate_%s(input)\n", w)
		b.WriteString("  if (!validated) {\n    throw new Error(\"invalid\")\n  }\n")
		fmt.Fprintf(&b, "  return transform_%s(validated)\n}\n\n", w)
	}
	b.WriteString(dedupTargetFuncTS)
	b.WriteString("\n")
	return b.String()
}

// TestCheckDuplicate_SymbolInLargeFileExact_TS is the TS mirror of
// TestCheckDuplicate_SymbolInLargeFileExact: an exact copy of a ~10-line TS
// function buried in a 100+ line .ts file must be flagged as a symbol-level
// exact duplicate with the symbol name and line.
//
// RED (against the extents-only-for-classes TS extractor): TS functions carry
// no EndLine, so no symbol fingerprints exist and the copy comes back "none".
func TestCheckDuplicate_SymbolInLargeFileExact_TS(t *testing.T) {
	dir := t.TempDir()
	src := dedupLargeSourceTS()
	if n := strings.Count(src, "\n"); n < 100 {
		t.Fatalf("fixture too small: %d lines; want 100+", n)
	}
	writeFile(t, filepath.Join(dir, "big.ts"), src)

	nr := NewNativeRunner(dir)
	if _, err := nr.BuildIndex(GetRepoMapOptions{}); err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}

	out, err := nr.CheckDuplicateNative(CheckDuplicateOptions{Content: dedupTargetFuncTS})
	if err != nil {
		t.Fatalf("CheckDuplicateNative: %v", err)
	}
	m := out.(map[string]any)
	if m["matchType"] != "exact" {
		t.Fatalf("TS symbol-in-large-file copy got matchType %q; want \"exact\" (TS functions must carry dedup fingerprints)", m["matchType"])
	}
	if m["filePath"] != "big.ts" {
		t.Errorf("filePath=%v; want big.ts", m["filePath"])
	}
	if m["symbolName"] != "computeRollingChecksumTS" {
		t.Errorf("symbolName=%v; want computeRollingChecksumTS", m["symbolName"])
	}
	wantLine := strings.Count(strings.SplitAfter(src, "function computeRollingChecksumTS")[0], "\n") + 1
	if m["line"] != wantLine {
		t.Errorf("line=%v; want %d", m["line"], wantLine)
	}
}

// dedupBraceyTargetFunc is a function whose body is full of braces inside
// string literals and comments — the shapes that fooled the naive brace
// counter into truncating the hashed extent.
const dedupBraceyTargetFunc = `func renderBraces(names []string) string {
	out := "{"
	// } keep the scanner honest
	for _, n := range names {
		out += n + "},{"
	}
	out += "}"
	return out
}`

// TestCheckDuplicate_SymbolWithBraceInString_Exact proves the hashed extent
// survives braces in strings/comments: an exact paste of the brace-laden
// function must produce a symbol-level exact match.
//
// RED (against the naive brace counter): the extent is truncated at the first
// '}' inside a string, so the persisted fingerprint never equals the paste.
func TestCheckDuplicate_SymbolWithBraceInString_Exact(t *testing.T) {
	dir := t.TempDir()
	src := dedupLargeSource() + "\n" + dedupBraceyTargetFunc + "\n"
	writeFile(t, filepath.Join(dir, "bracey.go"), src)

	nr := NewNativeRunner(dir)
	if _, err := nr.BuildIndex(GetRepoMapOptions{}); err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}

	out, err := nr.CheckDuplicateNative(CheckDuplicateOptions{Content: dedupBraceyTargetFunc})
	if err != nil {
		t.Fatalf("CheckDuplicateNative: %v", err)
	}
	m := out.(map[string]any)
	if m["matchType"] != "exact" {
		t.Fatalf("brace-laden symbol paste got matchType %q; want \"exact\" (string/comment braces truncated the hashed extent)", m["matchType"])
	}
	if m["symbolName"] != "renderBraces" {
		t.Errorf("symbolName=%v; want renderBraces", m["symbolName"])
	}
}

// ── Doc-comment/rename dedup ladder (live A/B regression, codeintel-dedup-donmai-001) ──
//
// The live eval regressed because the WITH agent pasted a near-duplicate of
// FindGitRoot WITH its doc comment, renamed (LocateRepoRoot, start vs
// startDir) and with the comment reworded — and the engine said "none".
// Root cause: symbol hashes cover the BODY (from the func keyword line), but
// the query was hashed as-is, so comment tokens polluted the query's
// xxHash/SimHash. The empirical ladder before the comment-stripping fix:
//
//	rung 1  body-only verbatim              → symbol EXACT      (worked)
//	rung 2  verbatim including doc comment  → file-level "near" (symbol match lost)
//	rung 3  reworded comment, same idents   → NONE
//	rung 4  reworded comment + 2 renames    → NONE  (the benchmark shape)
//
// After the fix (comments stripped from BOTH index-side and query-side
// normalization) rungs 2–3 are symbol EXACT. Rung 4 measures Hamming 4 —
// the two identifier renames alone (funcRename=0 bits, paramRename=5 bits,
// both=4 bits over a 43-token body) exceed the default near threshold of 3,
// so the default tool path still answers "none"; the threshold is NOT
// loosened globally to force it (false-positive risk on every other query).
// Instead the advertisement's none-branch was softened (4b): a "none" on
// code suspected to be RENAMED from existing code warrants one targeted
// grep, while positive matches stay authoritative.

// dedupGitrootFixture is the FindGitRoot-shaped repo file: a doc-commented
// walking-upward helper with an interior comment and backtick-quoted terms in
// the doc text — the exact shape from the live regression.
const dedupGitrootFixture = "package codeintel\n\n" +
	"import (\n\t\"os\"\n\t\"path/filepath\"\n)\n\n" +
	"// FindGitRoot walks upward from startDir looking for the enclosing git\n" +
	"// repository root, i.e. the nearest ancestor (including startDir itself) that\n" +
	"// contains a `.git` entry.\n" +
	"//\n" +
	"// Both forms of `.git` are accepted:\n" +
	"//   - a DIRECTORY, the normal case for a primary checkout.\n" +
	"//   - a FILE containing a `gitdir: <path>` pointer, the form used by\n" +
	"//     `git worktree add` checkouts.\n" +
	"//\n" +
	"// Returns the absolute path to the discovered root and true, or (\"\", false)\n" +
	"// if no `.git` entry is found before reaching the filesystem root.\n" +
	dedupGitrootBody + "\n"

// dedupGitrootBody is the fixture function from the `func` keyword line —
// the exact span the index fingerprints at symbol granularity.
const dedupGitrootBody = `func FindGitRoot(startDir string) (string, bool) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", false
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the filesystem root without finding .git.
			return "", false
		}
		dir = parent
	}
}`

// dedupRewordedDoc is a from-scratch rewording of the fixture's doc comment —
// zero token overlap is not required, just realistic doc-drift.
const dedupRewordedDoc = `// LocateRepoRoot scans parent directories to locate the repository
// top-level folder: any ancestor holding a .git marker (directory or
// worktree pointer file form).
`

// gitrootLadderRunner indexes the fixture repo file once for the ladder tests.
func gitrootLadderRunner(t *testing.T) *NativeRunner {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gitroot.go"), dedupGitrootFixture)
	nr := NewNativeRunner(dir)
	if _, err := nr.BuildIndex(GetRepoMapOptions{}); err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	return nr
}

// checkDup runs one query through CheckDuplicateNative and returns the map.
func checkDup(t *testing.T, nr *NativeRunner, content string) map[string]any {
	t.Helper()
	out, err := nr.CheckDuplicateNative(CheckDuplicateOptions{Content: content})
	if err != nil {
		t.Fatalf("CheckDuplicateNative: %v", err)
	}
	return out.(map[string]any)
}

// TestCheckDuplicate_Ladder_BodyOnlyExact pins rung 1 (the pre-fix baseline
// that already worked): a verbatim body-only paste is a symbol-level exact.
func TestCheckDuplicate_Ladder_BodyOnlyExact(t *testing.T) {
	nr := gitrootLadderRunner(t)
	m := checkDup(t, nr, dedupGitrootBody)
	if m["matchType"] != "exact" || m["symbolName"] != "FindGitRoot" {
		t.Errorf("body-only paste: matchType=%v symbolName=%v; want exact/FindGitRoot", m["matchType"], m["symbolName"])
	}
}

// TestCheckDuplicate_Ladder_VerbatimWithDocComment pins rung 2: pasting the
// function WITH its doc comment — the normal agent behavior — must still hit
// the symbol-level EXACT match, not degrade to a file-level near.
//
// RED (before comment-stripping normalization):
//
//	native_dedup_test.go: doc-commented paste got matchType "near" symbolName <nil>; want exact/FindGitRoot
func TestCheckDuplicate_Ladder_VerbatimWithDocComment(t *testing.T) {
	nr := gitrootLadderRunner(t)
	docStart := strings.Index(dedupGitrootFixture, "// FindGitRoot walks")
	if docStart < 0 {
		t.Fatal("fixture lost its doc comment")
	}
	m := checkDup(t, nr, dedupGitrootFixture[docStart:])
	if m["matchType"] != "exact" {
		t.Fatalf("doc-commented paste got matchType %q (symbolName=%v); want \"exact\" — comment tokens must not pollute the query hash", m["matchType"], m["symbolName"])
	}
	if m["symbolName"] != "FindGitRoot" {
		t.Errorf("symbolName=%v; want FindGitRoot", m["symbolName"])
	}
}

// TestCheckDuplicate_Ladder_RewordedCommentSameIdents pins rung 3: a reworded
// doc comment with the code untouched must reduce to the same normalized body
// — symbol-level EXACT.
//
// RED (before comment-stripping normalization):
//
//	native_dedup_test.go: reworded-comment paste got matchType "none"; want "exact"
func TestCheckDuplicate_Ladder_RewordedCommentSameIdents(t *testing.T) {
	nr := gitrootLadderRunner(t)
	m := checkDup(t, nr, dedupRewordedDoc+dedupGitrootBody)
	if m["matchType"] != "exact" {
		t.Fatalf("reworded-comment paste got matchType %q; want \"exact\" — comment rewording alone must not defeat dedup", m["matchType"])
	}
	if m["symbolName"] != "FindGitRoot" {
		t.Errorf("symbolName=%v; want FindGitRoot", m["symbolName"])
	}
}

// TestCheckDuplicate_Ladder_BenchmarkRenameShape pins rung 4 — the exact
// codeintel-dedup-donmai-001 failure shape: function renamed
// (FindGitRoot→LocateRepoRoot), param renamed (startDir→start), doc comment
// reworded — at its honest post-fix behavior:
//
//  1. Comment stripping moved the shape from token-noise-drowned (the reworded
//     doc alone burned the whole Hamming budget) to a pure rename distance:
//     the query is a symbol-level NEAR at Hamming 4 — one bit past the default
//     threshold of 3 — with the correct filePath+symbolName+line identity.
//  2. The DEFAULT tool path therefore still answers "none". The threshold is
//     deliberately NOT loosened globally to absorb this shape (that would
//     trade a benchmark rung for false positives everywhere); instead the
//     advertisement's none-branch says a "none" on suspected-RENAMED code
//     warrants one targeted grep (eval/codeintel/advertise.go, 4b).
//
// If a future normalization change brings the rename shape within the default
// threshold, assertion 2 flips — restore the "trust the none" advertisement
// wording in the same change.
func TestCheckDuplicate_Ladder_BenchmarkRenameShape(t *testing.T) {
	nr := gitrootLadderRunner(t)
	renamed := strings.ReplaceAll(dedupGitrootBody, "FindGitRoot", "LocateRepoRoot")
	renamed = strings.ReplaceAll(renamed, "startDir", "start")
	query := dedupRewordedDoc + renamed

	// (1) One bit past the default budget: at threshold+1 the symbol-level
	// near fires with full identity. RED before comment stripping: no match at
	// ANY sane threshold — comment tokens pushed the distance far beyond it.
	idx := nr.loadIndex()
	corpus := make([]FileIndex, 0, len(idx.Files))
	for _, fi := range idx.Files {
		corpus = append(corpus, fi)
	}
	matches := FindDuplicateMatches(query, corpus, SimHashDefaultThreshold+1, 1)
	if len(matches) == 0 {
		t.Fatalf("benchmark rename shape found no match even at threshold %d — comment stripping regressed", SimHashDefaultThreshold+1)
	}
	m := matches[0]
	if m.MatchType != "near" || m.SymbolName != "FindGitRoot" || m.FilePath != "gitroot.go" {
		t.Errorf("threshold+1 match = %+v; want symbol-level near on gitroot.go/FindGitRoot", m)
	}
	wantLine := strings.Count(strings.SplitAfter(dedupGitrootFixture, "func FindGitRoot")[0], "\n") + 1
	if m.Line != wantLine {
		t.Errorf("line=%d; want %d", m.Line, wantLine)
	}
	if m.HammingDistance != SimHashDefaultThreshold+1 {
		t.Errorf("hammingDistance=%d; want exactly %d (the measured rename cost — if this drops to <=%d, flip the default-path assertion below and restore the advertisement wording)",
			m.HammingDistance, SimHashDefaultThreshold+1, SimHashDefaultThreshold)
	}

	// (2) The default tool path still answers "none" — the residual limitation
	// the 4b advertisement caveat exists for.
	got := checkDup(t, nr, query)
	if got["matchType"] != "none" {
		t.Errorf("default-threshold path got matchType %q; the 4b none-branch caveat assumes \"none\" — if the engine now catches the rename shape, restore the trust-the-none advertisement instead", got["matchType"])
	}
}

// TestComputeSymbolHashes_MinLinesBoundary pins the symbolHashMinLines
// boundary exactly (a `<` vs `<=` mutation must go red): a 2-line extent is
// excluded, a 3-line extent is included — and the inclusion is observable
// end-to-end through CheckDuplicateNative.
func TestComputeSymbolHashes_MinLinesBoundary(t *testing.T) {
	twoLiner := "func twoLiner(a int) int {\n\treturn a*31 + a }"
	threeLiner := "func threeLiner(widgets []string) int {\n\treturn len(widgets) * 7\n}"
	src := "package mb\n\n" + twoLiner + "\n\n" + threeLiner + "\n"

	ast := (&GoExtractor{}).Extract(src, "mb.go")
	hashes := ComputeSymbolHashes(src, ast.Symbols)
	names := map[string]bool{}
	for _, h := range hashes {
		names[h.Name] = true
	}
	if names["twoLiner"] {
		t.Errorf("2-line extent got a symbol hash; want excluded (< %d lines)", symbolHashMinLines)
	}
	if !names["threeLiner"] {
		t.Errorf("3-line extent got no symbol hash; want included (exactly %d lines)", symbolHashMinLines)
	}

	// End-to-end: the 3-liner pastes back as a symbol-level exact match; the
	// 2-liner does not (whole-file coverage aside, there is no fingerprint).
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "mb.go"), dedupLargeSource()+"\n"+twoLiner+"\n\n"+threeLiner+"\n")
	nr := NewNativeRunner(dir)
	if _, err := nr.BuildIndex(GetRepoMapOptions{}); err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	out, err := nr.CheckDuplicateNative(CheckDuplicateOptions{Content: threeLiner})
	if err != nil {
		t.Fatalf("CheckDuplicateNative(threeLiner): %v", err)
	}
	m := out.(map[string]any)
	if m["matchType"] != "exact" || m["symbolName"] != "threeLiner" {
		t.Errorf("3-line func: matchType=%v symbolName=%v; want exact/threeLiner", m["matchType"], m["symbolName"])
	}
	out, err = nr.CheckDuplicateNative(CheckDuplicateOptions{Content: twoLiner})
	if err != nil {
		t.Fatalf("CheckDuplicateNative(twoLiner): %v", err)
	}
	m = out.(map[string]any)
	if m["matchType"] != "none" {
		t.Errorf("2-line func: matchType=%v; want none (below the %d-line fingerprint floor)", m["matchType"], symbolHashMinLines)
	}
}
