package codeintel

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCompactRepo builds a temp repo with one documented function whose doc
// block is multi-line and long, plus several prefix-siblings, so the compact
// projection and result caps are observable.
func writeCompactRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	longDoc := strings.Repeat("this documentation line is deliberately padded with words ", 5)
	goSrc := `package pay

// ProcessPayment handles a payment end to end.
// ` + longDoc + `
// It validates, authorizes, captures and settles.
func ProcessPayment(id string) error {
	return nil
}

// ProcessPaymentBatch handles many payments.
func ProcessPaymentBatch(ids []string) error {
	return nil
}

// ProcessPaymentRetry retries a payment.
func ProcessPaymentRetry(id string) error {
	return nil
}

// ProcessPaymentAudit audits a payment.
func ProcessPaymentAudit(id string) error {
	return nil
}

// ProcessPaymentRefund refunds a payment.
func ProcessPaymentRefund(id string) error {
	return nil
}

// ProcessPaymentVoid voids a payment.
func ProcessPaymentVoid(id string) error {
	return nil
}

// ProcessPaymentSettle settles a payment.
func ProcessPaymentSettle(id string) error {
	return nil
}
`
	if err := os.WriteFile(filepath.Join(dir, "pay.go"), []byte(goSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestSearchSymbols_CompactDefault verifies the default search-symbols
// projection: a compact map per hit carrying name/kind/filePath/line/signature
// and a documentation string truncated to its first line, never the full
// multi-line doc block.
func TestSearchSymbols_CompactDefault(t *testing.T) {
	dir := writeCompactRepo(t)
	nr := NewNativeRunner(dir)

	out, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "ProcessPayment"})
	if err != nil {
		t.Fatalf("SearchSymbolsNative: %v", err)
	}
	results, ok := out.([]map[string]any)
	if !ok {
		t.Fatalf("expected []map result, got %T", out)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}

	sym, ok := results[0]["symbol"].(map[string]any)
	if !ok {
		t.Fatalf("compact default: symbol should be map[string]any, got %T", results[0]["symbol"])
	}
	for _, key := range []string{"name", "kind", "filePath", "line", "signature"} {
		if _, ok := sym[key]; !ok {
			t.Errorf("compact symbol missing %q: %v", key, sym)
		}
	}
	doc, _ := sym["documentation"].(string)
	if doc == "" {
		t.Fatal("compact symbol should keep a one-line documentation")
	}
	if strings.Contains(doc, "\n") {
		t.Errorf("compact documentation must be single-line, got %q", doc)
	}
	if got := len([]rune(doc)); got > compactDocMaxLen+1 { // +1 for the ellipsis rune
		t.Errorf("compact documentation length %d exceeds cap %d", got, compactDocMaxLen)
	}
}

// TestSearchSymbols_IncludeDocReturnsFullSymbol verifies the opt-in escape
// hatch: IncludeDoc restores the full CodeSymbol shape with the complete
// multi-line documentation block.
func TestSearchSymbols_IncludeDocReturnsFullSymbol(t *testing.T) {
	dir := writeCompactRepo(t)
	nr := NewNativeRunner(dir)

	out, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "ProcessPayment", IncludeDoc: true})
	if err != nil {
		t.Fatalf("SearchSymbolsNative: %v", err)
	}
	results := out.([]map[string]any)
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	sym, ok := results[0]["symbol"].(CodeSymbol)
	if !ok {
		t.Fatalf("IncludeDoc: symbol should be CodeSymbol, got %T", results[0]["symbol"])
	}
	if !strings.Contains(sym.Documentation, "\n") {
		t.Errorf("IncludeDoc should return the full multi-line documentation, got %q", sym.Documentation)
	}
}

// TestSearchSymbols_ExactShortCircuit verifies that when an exact-name match
// exists, only the exact match(es) are returned — prefix/fuzzy siblings are
// suppressed.
func TestSearchSymbols_ExactShortCircuit(t *testing.T) {
	dir := writeCompactRepo(t)
	nr := NewNativeRunner(dir)

	out, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "ProcessPayment"})
	if err != nil {
		t.Fatalf("SearchSymbolsNative: %v", err)
	}
	results := out.([]map[string]any)
	if len(results) != 1 {
		t.Fatalf("exact short-circuit: want only the exact ProcessPayment hit, got %d results", len(results))
	}
	if mt := results[0]["matchType"]; mt != "exact" {
		t.Errorf("matchType: got %v, want exact", mt)
	}
	sym := results[0]["symbol"].(map[string]any)
	if sym["name"] != "ProcessPayment" {
		t.Errorf("name: got %v, want ProcessPayment", sym["name"])
	}
}

// TestSearchSymbols_ExactShortCircuitCap verifies that multiple exact matches
// are capped at 3 by default and that an explicit MaxResults raises the cap.
func TestSearchSymbols_ExactShortCircuitCap(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		src := fmt.Sprintf("package p%d\n\nfunc New() int {\n\treturn %d\n}\n", i, i)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("p%d.go", i)), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	nr := NewNativeRunner(dir)

	out, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "New"})
	if err != nil {
		t.Fatalf("SearchSymbolsNative: %v", err)
	}
	if results := out.([]map[string]any); len(results) != 3 {
		t.Errorf("exact matches should cap at 3 by default, got %d", len(results))
	}

	out, err = nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "New", MaxResults: 5})
	if err != nil {
		t.Fatalf("SearchSymbolsNative: %v", err)
	}
	if results := out.([]map[string]any); len(results) != 5 {
		t.Errorf("explicit MaxResults should raise the exact cap, got %d", len(results))
	}
}

// TestSearchSymbols_DefaultCapFive verifies the non-exact default cap dropped
// from 20 to 5, still overridable via MaxResults.
func TestSearchSymbols_DefaultCapFive(t *testing.T) {
	dir := writeCompactRepo(t)
	nr := NewNativeRunner(dir)

	// "ProcessPaymentR" prefix-matches Retry/Refund and fuzzy-matches others —
	// no exact match, so the default cap applies. Use a broader prefix that
	// yields >5 candidates with no exact hit.
	out, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "processpaymen"})
	if err != nil {
		t.Fatalf("SearchSymbolsNative: %v", err)
	}
	results := out.([]map[string]any)
	if len(results) != 5 {
		t.Errorf("default cap: got %d results, want 5", len(results))
	}

	out, err = nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "processpaymen", MaxResults: 7})
	if err != nil {
		t.Fatalf("SearchSymbolsNative: %v", err)
	}
	if results := out.([]map[string]any); len(results) != 7 {
		t.Errorf("MaxResults=7: got %d results, want 7", len(results))
	}
}

// TestSearchCode_CompactDefaultAndIncludeDoc verifies the same compact
// projection contract on the BM25 search-code path.
func TestSearchCode_CompactDefaultAndIncludeDoc(t *testing.T) {
	dir := writeCompactRepo(t)
	nr := NewNativeRunner(dir)

	out, err := nr.SearchCodeNative(SearchCodeOptions{Query: "ProcessPayment"})
	if err != nil {
		t.Fatalf("SearchCodeNative: %v", err)
	}
	results := out.([]map[string]any)
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	sym, ok := results[0]["symbol"].(map[string]any)
	if !ok {
		t.Fatalf("compact default: symbol should be map[string]any, got %T", results[0]["symbol"])
	}
	if doc, _ := sym["documentation"].(string); strings.Contains(doc, "\n") {
		t.Errorf("compact documentation must be single-line, got %q", doc)
	}

	out, err = nr.SearchCodeNative(SearchCodeOptions{Query: "ProcessPayment", IncludeDoc: true})
	if err != nil {
		t.Fatalf("SearchCodeNative IncludeDoc: %v", err)
	}
	results = out.([]map[string]any)
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	if _, ok := results[0]["symbol"].(CodeSymbol); !ok {
		t.Fatalf("IncludeDoc: symbol should be CodeSymbol, got %T", results[0]["symbol"])
	}
}

// TestFirstDocLine pins the truncation contract: first line only, capped at
// compactDocMaxLen runes with an ellipsis when cut.
func TestFirstDocLine(t *testing.T) {
	long := strings.Repeat("x", compactDocMaxLen+40)
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"single short line", "does a thing", "does a thing"},
		{"multi-line keeps first", "line one\nline two\nline three", "line one"},
		{"long line truncated", long, strings.Repeat("x", compactDocMaxLen) + "…"},
		{"leading whitespace trimmed", "  padded  \nnext", "padded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstDocLine(tt.in); got != tt.want {
				t.Errorf("firstDocLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
