// Package credentialsclientts hosts a Go test that enforces parity between
// the canonical Go AgentEnvBlocklist in
// `internal/credentials/blocklist.go` and the TypeScript duplicate shipped
// in `clients/credentials-client-ts/src/blocklist.ts`.
//
// This is a cross-language drift guard. The Go canonical is the single
// source of truth; this test reads the TS file as raw text and asserts
// every canonical entry appears as a string literal in the TS array. CI
// fails on divergence so a future PR that updates one side without the
// other cannot ship.
//
// Sync obligation: when adding/removing entries from
// `internal/credentials/blocklist.go`'s AgentEnvBlocklist, update
// `clients/credentials-client-ts/src/blocklist.ts` in the same PR.
//
// Part of FOLLOWUP-17 (credentials-gap) — Wave A, A4.
package credentialsclientts

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/internal/credentials"
)

// tsBlocklistPath returns the absolute path to the TS blocklist file,
// resolved relative to this test file so the test works regardless of the
// caller's CWD (e.g. `go test ./...` from the module root, or `go test .`
// from within this directory).
func tsBlocklistPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	// thisFile == .../donmai/clients/credentials-client-ts/blocklist_parity_test.go
	dir := filepath.Dir(thisFile)
	return filepath.Join(dir, "src", "blocklist.ts")
}

// stringLiteralRE matches single- or double-quoted string literals.
// We use this to extract every quoted token from the TS file's
// AGENT_ENV_BLOCKLIST array so the comparison is robust against
// reordering, whitespace, comments, and quote-style changes.
var stringLiteralRE = regexp.MustCompile(`['"]([A-Z][A-Z0-9_]*)['"]`)

// TestBlocklistParity_TSClient asserts the TypeScript blocklist file
// contains exactly the same entries as the canonical Go blocklist.
//
// Order-insensitive, count-sensitive. Comparison is on uppercase env-var
// names (the regex filters to `[A-Z][A-Z0-9_]*` so log lines like
// `'sync obligation'` in JSDoc comments don't false-positive).
func TestBlocklistParity_TSClient(t *testing.T) {
	t.Parallel()

	tsPath := tsBlocklistPath(t)
	raw, err := os.ReadFile(tsPath)
	if err != nil {
		t.Fatalf("read TS blocklist %q: %v", tsPath, err)
	}

	// Narrow to the AGENT_ENV_BLOCKLIST array literal. The TS source
	// declares `AGENT_ENV_BLOCKLIST: readonly string[] = Object.freeze([...]);`
	// so we anchor on `Object.freeze(` and read until the matching `])`.
	// If a future refactor changes the wrapper, the START_MARKER and
	// END_MARKER constants are the only knobs to update.
	src := string(raw)
	const startMarker = "Object.freeze(["
	const endMarker = "])"

	mention := strings.Index(src, "AGENT_ENV_BLOCKLIST")
	if mention < 0 {
		t.Fatalf("TS blocklist file %q does not mention AGENT_ENV_BLOCKLIST", tsPath)
	}
	startIdx := strings.Index(src[mention:], startMarker)
	if startIdx < 0 {
		t.Fatalf("TS blocklist file %q: no %q after AGENT_ENV_BLOCKLIST (refactor?)",
			tsPath, startMarker)
	}
	startIdx += mention + len(startMarker)
	endIdx := strings.Index(src[startIdx:], endMarker)
	if endIdx < 0 {
		t.Fatalf("TS blocklist file %q: no %q closing the array (refactor?)",
			tsPath, endMarker)
	}
	arrayBody := src[startIdx : startIdx+endIdx]

	matches := stringLiteralRE.FindAllStringSubmatch(arrayBody, -1)
	got := make([]string, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		k := m[1]
		if _, dup := seen[k]; dup {
			t.Errorf("TS blocklist contains duplicate entry %q", k)
			continue
		}
		seen[k] = struct{}{}
		got = append(got, k)
	}

	want := append([]string{}, credentials.AgentEnvBlocklist...)

	sort.Strings(got)
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("TS blocklist parity: count mismatch\n  got  (%d): %v\n  want (%d): %v\n  source: %s",
			len(got), got, len(want), want, tsPath)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("TS blocklist parity: entry %d differs\n  got:  %q\n  want: %q\n  full got:  %v\n  full want: %v",
				i, got[i], want[i], got, want)
		}
	}
}
