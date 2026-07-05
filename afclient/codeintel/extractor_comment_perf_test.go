package codeintel

import (
	"strings"
	"testing"
	"time"
)

// TestGoExtractor_LargeCommentBlockIsLinear guards against quadratic-time
// doc-comment accumulation. Rebuilding the accumulated comment with `s += ...`
// on every consecutive // line is O(n^2) and let a small plaintext file hang
// indexing for many seconds (the adversarial review measured ~14.6s for a
// 100k-line comment block; a 2M-line variant did not finish in 2 minutes).
//
// RED (before the strings.Builder fix): 100k consecutive // lines took multiple
// seconds, blowing the bound below.
func TestGoExtractor_LargeCommentBlockIsLinear(t *testing.T) {
	const n = 100_000
	var b strings.Builder
	b.WriteString("package huge\n")
	for i := 0; i < n; i++ {
		b.WriteString("// comment line filler filler filler filler filler filler\n")
	}
	b.WriteString("func After() {}\n")
	src := b.String()

	start := time.Now()
	ast := (&GoExtractor{}).Extract(src, "huge.go")
	elapsed := time.Since(start)

	// Linear extraction of 100k lines is single-digit milliseconds; the O(n^2)
	// bug is multiple seconds. 3s cleanly separates the two on any machine.
	if elapsed > 3*time.Second {
		t.Errorf("Go extraction of %d comment lines took %v — quadratic comment accumulation", n, elapsed)
	}
	// Sanity: the trailing symbol is still extracted with its doc attached.
	if len(ast.Symbols) != 1 || ast.Symbols[0].Name != "After" {
		t.Fatalf("expected 1 symbol After; got %+v", ast.Symbols)
	}
}

// TestRustExtractor_LargeDocBlockIsLinear is the Rust analogue over /// doc
// comments (extractor_rust.go had the identical `currentDoc += ...` pattern).
func TestRustExtractor_LargeDocBlockIsLinear(t *testing.T) {
	const n = 100_000
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("/// doc line filler filler filler filler filler filler\n")
	}
	b.WriteString("pub fn after() {}\n")
	src := b.String()

	start := time.Now()
	ast := (&RustExtractor{}).Extract(src, "huge.rs")
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Errorf("Rust extraction of %d doc lines took %v — quadratic doc accumulation", n, elapsed)
	}
	if len(ast.Symbols) != 1 || ast.Symbols[0].Name != "after" {
		t.Fatalf("expected 1 symbol after; got %+v", ast.Symbols)
	}
}
