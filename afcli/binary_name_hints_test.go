package afcli

// binary_name_hints_test.go guards against a specific "hint names a command
// that doesn't exist [for you]" defect: a Cobra subcommand's Long/Example
// text that hardcodes the literal binary name "donmai" instead of using the
// `bin` parameter every command factory in this package receives for exactly
// this purpose (see binaryName() in helpers.go).
//
// afcli is designed to be embedded downstream under a DIFFERENT binary name
// via afcli.RegisterCommands(root, Config{BinaryName: "<embedder>", ...}). A
// hardcoded "donmai" example in a command's --help output then tells a user
// of the embedding binary to run a binary they don't have; the closest thing
// on their PATH is either nothing ("donmai: command not found") or, worse, a
// different install of the OSS donmai binary running against the wrong
// context.
//
// This was found live in every leaf of `github` (11 subcommands) and in
// `logs analyze` (1) — both had a correctly-templated top-level Short/Long
// but hardcoded "donmai" in their per-subcommand Example blocks. Fixed by
// threading `bin` through and concatenating it into the Long string instead
// of a static backtick literal.
//
// Scope note: this file only asserts the two fixed leaves, not a blanket
// "no command anywhere may say donmai" sweep. Plenty of Long/Example text
// legitimately references fixed, brand-scoped paths (e.g.
// ~/.config/donmai/...) via the separate runtime/statehome seam — that is a
// different concern (state-directory naming) from "this text tells the user
// to run a command", which is what this test — and the sweep it came from —
// is about. Widening this into a repo-wide "no donmai substring" assertion
// produced dozens of unrelated false positives (state paths, package doc
// comments) when tried; narrow and exact beats broad and noisy here.
import (
	"strings"
	"testing"
)

// embedderBinaryName stands in for a downstream embedder's binary name.
// Deliberately does NOT contain the substring "donmai" (unlike "not-donmai"
// or "non-donmai"), so a plain strings.Contains(text, "donmai") check cannot
// false-positive on the embedder's own name.
const embedderBinaryName = "acme-cli"

// TestEmbeddedBinaryName_GitHubExamplesUseTemplatedBinary is the regression
// test for the exact defect found: every `github` leaf's Example block must
// reference the embedding binary, not a hardcoded "donmai".
func TestEmbeddedBinaryName_GitHubExamplesUseTemplatedBinary(t *testing.T) {
	root := newGitHubCmd(nil, Config{BinaryName: embedderBinaryName})

	leaves := root.Commands()
	if len(leaves) == 0 {
		t.Fatal("newGitHubCmd registered no subcommands — test fixture is stale")
	}
	for _, leaf := range leaves {
		if !strings.Contains(leaf.Long, embedderBinaryName) {
			t.Errorf("github %s: Long text does not mention the embedding binary %q — likely still hardcodes \"donmai\":\n%s",
				leaf.Name(), embedderBinaryName, leaf.Long)
		}
		if strings.Contains(leaf.Long, "donmai") {
			t.Errorf("github %s: Long text hardcodes \"donmai\" instead of the embedding binary %q:\n%s",
				leaf.Name(), embedderBinaryName, leaf.Long)
		}
	}
}

// TestEmbeddedBinaryName_LogsAnalyzeUsesTemplatedBinary mirrors the above
// for `logs analyze`, whose Examples block had the same hardcoding.
func TestEmbeddedBinaryName_LogsAnalyzeUsesTemplatedBinary(t *testing.T) {
	root := newLogsCmd(Config{BinaryName: embedderBinaryName})

	analyze, remaining, err := root.Find([]string{"analyze"})
	if err != nil || len(remaining) > 0 {
		t.Fatalf("could not resolve `logs analyze`: err=%v remaining=%v", err, remaining)
	}

	if !strings.Contains(analyze.Long, embedderBinaryName) {
		t.Errorf("logs analyze: Long text does not mention the embedding binary %q — likely still hardcodes \"donmai\":\n%s",
			embedderBinaryName, analyze.Long)
	}
	// The state-dir path (~/.config/donmai/log-signatures.yaml) is a separate,
	// brand-scoped concern (runtime/statehome) and legitimately stays literal
	// here — only the COMMAND examples are in scope, so check for the exact
	// stale invocation rather than a blanket "no donmai substring" assertion.
	if strings.Contains(analyze.Long, "donmai logs analyze") {
		t.Errorf("logs analyze: Example still invokes \"donmai logs analyze\" instead of %q logs analyze:\n%s",
			embedderBinaryName, analyze.Long)
	}
}
