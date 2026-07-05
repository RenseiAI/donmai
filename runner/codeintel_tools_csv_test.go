package runner

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/prompt"
)

// csvNames splits a --tools CSV into its component tool names ("" -> nil).
func csvNames(csv string) []string {
	if csv == "" {
		return nil
	}
	return strings.Split(csv, ",")
}

// TestCodeIntelToolsCSV_DropsUnknownNames is the regression guard for the
// cross-lane divergence: codeIntelToolsCSV (the stdio `--tools` flag) and
// effectiveCodeIntelTools (the FQ allow-list + prompt partial) MUST agree on
// unknown-name handling. The CSV is fed to the server's validateTools, which
// rejects any unknown name all-or-nothing at startup — so a single typo in the
// requested subset would take the ENTIRE code-intel server down while the
// prompt still advertised its tools.
//
// RED (before the fix): codeIntelToolsCSV joined ci.Tools VERBATIM, so an
// unknown name reached --tools and killed the server, while the FQ/prompt lanes
// filtered it out.
func TestCodeIntelToolsCSV_DropsUnknownNames(t *testing.T) {
	t.Parallel()

	// The set of real tool names the server will accept.
	known := map[string]struct{}{}
	for _, tm := range codeIntelTools {
		known[tm.tool] = struct{}{}
	}

	cases := []struct {
		name  string
		tools []string
		// wantCSV is the exact expected --tools value ("" == omit flag).
		wantCSV string
	}{
		{
			name:    "known+unknown drops the unknown",
			tools:   []string{"af_code_search_symbols", "af_code_typo"},
			wantCSV: "af_code_search_symbols",
		},
		{
			// All-unknown collapses to the full set (matching
			// effectiveCodeIntelTools), so the server defaults to all six and the
			// prompt's advertised six are all live — the CSV omits --tools rather
			// than passing the bogus name that would kill the server.
			name:    "only-unknown omits --tools (server default = all six)",
			tools:   []string{"af_code_typo"},
			wantCSV: "",
		},
		{
			name:    "known subset is preserved in canonical order",
			tools:   []string{"af_code_search_code", "af_code_search_symbols"},
			wantCSV: "af_code_search_symbols,af_code_search_code",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ci := &prompt.CodeIntelWork{Repo: "owner/repo", Tools: tc.tools}

			csv := codeIntelToolsCSV(ci)
			if csv != tc.wantCSV {
				t.Errorf("codeIntelToolsCSV = %q, want %q", csv, tc.wantCSV)
			}

			// Invariant: every name the server would receive on --tools must be a
			// real tool name, otherwise validateTools fails startup all-or-nothing.
			for _, n := range csvNames(csv) {
				if _, ok := known[n]; !ok {
					t.Errorf("--tools carries unknown name %q — server startup would fail all-or-nothing", n)
				}
			}

			// Invariant: the CSV lane and the FQ allow-list lane must expose the
			// SAME tool set, so the server never starts with a different subset
			// than the prompt advertises.
			fq := codeIntelFQToolNames(ci)
			fqTools := map[string]struct{}{}
			for _, name := range fq {
				fqTools[strings.TrimPrefix(name, codeIntelFQPrefix)] = struct{}{}
			}
			csvSet := csvNames(csv)
			// When the CSV is empty the server exposes all six; the FQ lane must
			// then also carry all six.
			if csv == "" {
				if len(fq) != len(codeIntelTools) {
					t.Errorf("empty --tools implies all six, but FQ lane advertises %d: %v", len(fq), fq)
				}
			} else {
				if len(csvSet) != len(fqTools) {
					t.Errorf("CSV lane (%v) and FQ lane (%v) expose different-sized sets", csvSet, fq)
				}
				for _, n := range csvSet {
					if _, ok := fqTools[n]; !ok {
						t.Errorf("--tools carries %q which the FQ allow-list does not advertise", n)
					}
				}
			}
		})
	}
}
