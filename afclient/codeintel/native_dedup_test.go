package codeintel

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
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
