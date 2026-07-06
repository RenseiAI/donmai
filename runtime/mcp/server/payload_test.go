package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secondDocLine is a sentinel that exists ONLY on the second line of the
// fixture symbol's doc block, so payload assertions can tell the compact
// (first-line-only) projection apart from the full includeDoc projection.
const secondDocLine = "SECOND-DOC-LINE-SENTINEL"

// fixtureRepoMultilineDoc writes a repo whose one symbol carries a multi-line
// documentation block — the shape whose truncation the WS1 compact projection
// promises by default.
func fixtureRepoMultilineDoc(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := `package pay

// ProcessPayment handles a payment end to end.
// ` + secondDocLine + ` it validates, authorizes, captures and settles.
func ProcessPayment(id string) error { return nil }
`
	if err := os.WriteFile(filepath.Join(dir, "pay.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dir
}

// callToolText invokes one tool and returns the single text payload, failing
// the test on any protocol or tool error.
func callToolText(t *testing.T, s *Server, tool string, args string) string {
	t.Helper()
	res, rerr := s.callTool(context.Background(), tool, json.RawMessage(args))
	if rerr != nil {
		t.Fatalf("%s protocol error: %v", tool, rerr)
	}
	if res.IsError {
		t.Fatalf("%s returned isError: %+v", tool, res.Content)
	}
	if len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("%s: want a single text content item, got %+v", tool, res.Content)
	}
	return res.Content[0].Text
}

// TestCallTool_SearchSymbols_CompactByDefault_FullWithIncludeDoc pins the
// agent-facing payload shape through the REAL MCP tool surface: the default
// result must carry only the doc's first line (the 2nd line is the dominant
// per-hit token cost), and {"includeDoc":true} must restore the full block.
func TestCallTool_SearchSymbols_CompactByDefault_FullWithIncludeDoc(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, fixtureRepoMultilineDoc(t))

	compact := callToolText(t, s, ToolSearchSymbols, `{"query":"ProcessPayment"}`)
	if !strings.Contains(compact, "ProcessPayment") {
		t.Fatalf("compact result should name the symbol: %s", compact)
	}
	if strings.Contains(compact, secondDocLine) {
		t.Errorf("default search_symbols payload must NOT contain the doc's 2nd line:\n%s", compact)
	}

	full := callToolText(t, s, ToolSearchSymbols, `{"query":"ProcessPayment","includeDoc":true}`)
	if !strings.Contains(full, secondDocLine) {
		t.Errorf("includeDoc search_symbols payload MUST contain the doc's 2nd line:\n%s", full)
	}
}

// TestCallTool_SearchCode_CompactByDefault_FullWithIncludeDoc: same payload
// contract on the BM25 search_code path.
func TestCallTool_SearchCode_CompactByDefault_FullWithIncludeDoc(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, fixtureRepoMultilineDoc(t))

	compact := callToolText(t, s, ToolSearchCode, `{"query":"ProcessPayment"}`)
	if !strings.Contains(compact, "ProcessPayment") {
		t.Fatalf("compact result should name the symbol: %s", compact)
	}
	if strings.Contains(compact, secondDocLine) {
		t.Errorf("default search_code payload must NOT contain the doc's 2nd line:\n%s", compact)
	}

	full := callToolText(t, s, ToolSearchCode, `{"query":"ProcessPayment","includeDoc":true}`)
	if !strings.Contains(full, secondDocLine) {
		t.Errorf("includeDoc search_code payload MUST contain the doc's 2nd line:\n%s", full)
	}
}
