package codeintel

import (
	"encoding/json"
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

// writeExactRepo writes n single-func packages each defining the SAME exported
// function name, so the exact-match short-circuit has n exact candidates spread
// across files p00.go, p01.go, … (filePath order is the expected tie-break).
func writeExactRepo(t *testing.T, n int, funcName string) string {
	t.Helper()
	dir := t.TempDir()
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("package p%02d\n\nfunc %s() int {\n\treturn %d\n}\n", i, funcName, i)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("p%02d.go", i)), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestSearchSymbols_ExactMatchesAllReturnedInOrder is the RED-FIRST test for
// the exact-short-circuit completeness fix: when several definitions share the
// query name, ALL of them must be returned (up to symbolExactMaxResults), in
// deterministic (name, filePath, line) order — never an arbitrary 3-of-n
// subset drawn from map iteration order.
func TestSearchSymbols_ExactMatchesAllReturnedInOrder(t *testing.T) {
	dir := writeExactRepo(t, 4, "Extract")
	nr := NewNativeRunner(dir)

	out, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "Extract"})
	if err != nil {
		t.Fatalf("SearchSymbolsNative: %v", err)
	}
	results := out.([]map[string]any)
	if len(results) != 4 {
		t.Fatalf("4 exact matches must all be returned, got %d", len(results))
	}
	var prevPath string
	for i, r := range results {
		sym, ok := r["symbol"].(map[string]any)
		if !ok {
			t.Fatalf("result %d is not a symbol hit: %v", i, r)
		}
		if sym["name"] != "Extract" {
			t.Errorf("result %d name = %v, want Extract", i, sym["name"])
		}
		path, _ := sym["filePath"].(string)
		if path <= prevPath {
			t.Errorf("results not in ascending filePath order: %q after %q", path, prevPath)
		}
		prevPath = path
	}
}

// TestSearchSymbols_ExactOverCapAppendsSentinel: more exact matches than the
// hard cap (20) returns exactly the cap plus a trailing sentinel element
// carrying the omitted count — existing parsers ignore it (no "symbol" /
// "filePath" keys) but an agent sees the truncation instead of silence.
func TestSearchSymbols_ExactOverCapAppendsSentinel(t *testing.T) {
	dir := writeExactRepo(t, 25, "New")
	nr := NewNativeRunner(dir)

	out, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "New"})
	if err != nil {
		t.Fatalf("SearchSymbolsNative: %v", err)
	}
	results := out.([]map[string]any)
	if len(results) != symbolExactMaxResults+1 {
		t.Fatalf("want %d hits + 1 sentinel, got %d elements", symbolExactMaxResults, len(results))
	}
	for i, r := range results[:symbolExactMaxResults] {
		if _, ok := r["symbol"]; !ok {
			t.Errorf("element %d should be a real hit, got %v", i, r)
		}
	}
	sentinel := results[len(results)-1]
	if got, _ := sentinel["truncatedExactMatches"].(int); got != 5 {
		t.Errorf("sentinel truncatedExactMatches = %v, want 5", sentinel["truncatedExactMatches"])
	}
	if hint, _ := sentinel["hint"].(string); !strings.Contains(hint, "maxResults") {
		t.Errorf("sentinel hint should tell the caller to raise maxResults, got %v", sentinel["hint"])
	}
	if _, hasSym := sentinel["symbol"]; hasSym {
		t.Errorf("sentinel must not look like a hit: %v", sentinel)
	}

	// An explicit MaxResults above the cap surfaces everything, no sentinel.
	out, err = nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "New", MaxResults: 25})
	if err != nil {
		t.Fatalf("SearchSymbolsNative: %v", err)
	}
	if results := out.([]map[string]any); len(results) != 25 {
		t.Errorf("MaxResults=25 should return all 25 exact hits with no sentinel, got %d", len(results))
	}

	// An explicit LOWER MaxResults is honored, with the sentinel reporting the rest.
	out, err = nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "New", MaxResults: 3})
	if err != nil {
		t.Fatalf("SearchSymbolsNative: %v", err)
	}
	results = out.([]map[string]any)
	if len(results) != 4 {
		t.Fatalf("MaxResults=3 should return 3 hits + sentinel, got %d elements", len(results))
	}
	if got, _ := results[3]["truncatedExactMatches"].(int); got != 22 {
		t.Errorf("sentinel truncatedExactMatches = %v, want 22", results[3]["truncatedExactMatches"])
	}
}

// TestSearchSymbols_ExactResultsDeterministic runs the same exact-name search
// repeatedly and requires byte-identical output: candidates come from map
// iteration, so only a fully-ordering comparator makes the result (and any
// truncation cut) reproducible.
func TestSearchSymbols_ExactResultsDeterministic(t *testing.T) {
	dir := writeExactRepo(t, 4, "Extract")
	nr := NewNativeRunner(dir)

	var first string
	for i := 0; i < 5; i++ {
		out, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "Extract"})
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("marshal run %d: %v", i, err)
		}
		if i == 0 {
			first = string(b)
			continue
		}
		if string(b) != first {
			t.Fatalf("run %d output differs:\n got %s\nwant %s", i, b, first)
		}
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
