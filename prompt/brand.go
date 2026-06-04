package prompt

import (
	"strings"

	"github.com/RenseiAI/donmai/runtime/statehome"
)

// Brand carries the display and CLI tokens the prompt templates interpolate so
// the rendered system/user prompts name the binary the agent is actually
// running under — never a hardcoded vendor brand.
//
// The OSS default (statehome brand "donmai") renders BrandDisplay="Donmai" and
// BrandCLI="donmai"; the closed rensei binary, which calls
// statehome.SetBrand("rensei") at process init, renders BrandDisplay="Rensei"
// and BrandCLI="rensei". The platform contract is therefore byte-identical to
// the pre-brand-seam templates: "autonomous Rensei agent" / "rensei linear".
type Brand struct {
	// BrandDisplay is the human-facing brand name used in prose
	// (e.g. "autonomous {Display} agent"). Title-cased.
	BrandDisplay string

	// BrandCLI is the binary/command name used in command examples
	// (e.g. "`{CLI} linear`"). Lowercase, matches the on-disk binary.
	BrandCLI string
}

// ResolveBrand derives the active [Brand] from the process-global statehome
// seam. The CLI token is the statehome brand verbatim (it IS the binary name
// — "donmai" / "rensei"); the display token title-cases its first rune.
// Resolving at call time (rather than caching) keeps the builder's zero value
// useful and honours an embedder that sets the brand before first dispatch.
//
// It is exported so other prompt-surface producers (e.g. the runner's
// mid-session steering message) can name the active binary's CLI consistently
// with the rendered templates, from a single source of truth.
func ResolveBrand() Brand {
	cli := strings.TrimSpace(statehome.Brand())
	if cli == "" {
		cli = statehome.DefaultBrand
	}
	return Brand{
		BrandDisplay: titleBrand(cli),
		BrandCLI:     cli,
	}
}

// titleBrand upper-cases the first rune of the brand token to form the display
// name ("donmai" -> "Donmai", "rensei" -> "Rensei"). It operates on the first
// rune only — brand tokens are single lowercase words by repo convention, so a
// full strings.Title (deprecated, and word-splitting) is neither needed nor
// wanted.
func titleBrand(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = []rune(strings.ToUpper(string(r[0])))[0]
	return string(r)
}
