package prompt

import (
	"errors"
	"regexp"
	"strings"

	"github.com/RenseiAI/donmai/runtime/statehome"
)

const defaultCLIExecutableName = "donmai"

var (
	cliExecutableNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	errCLIExecutableName     = errors.New("must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}")
)

// ResolveCLIExecutableName validates one process-owned command basename.
// Empty preserves the standalone Donmai default. Explicit values are exact:
// they are never trimmed, normalized, or derived from filesystem identity.
func ResolveCLIExecutableName(configured string) (string, error) {
	if configured == "" {
		return defaultCLIExecutableName, nil
	}
	if !cliExecutableNamePattern.MatchString(configured) {
		return "", errCLIExecutableName
	}
	return configured, nil
}

// Brand carries the display and CLI tokens the prompt templates interpolate so
// the rendered system/user prompts name the binary the agent is actually
// running under — never a hardcoded vendor brand.
//
// The OSS default renders BrandDisplay="Donmai" and BrandCLI="donmai".
// Embedders may set a distinct display brand through statehome and supply the
// stable process executable separately through [Builder.WithCLIExecutableName].
type Brand struct {
	// BrandDisplay is the human-facing brand name used in prose
	// (e.g. "autonomous {Display} agent"). Title-cased.
	BrandDisplay string

	// BrandCLI is the binary/command name used in command examples
	// (e.g. "`{CLI} linear`"). Lowercase, matches the on-disk binary.
	BrandCLI string
}

// ResolveBrand derives the display brand from the process-global statehome
// seam. Its CLI token is the standalone literal "donmai"; embedders provide
// their stable executable identity explicitly through [Builder.WithCLIExecutableName].
// This keeps named filesystem instances out of command instructions.
//
// It remains exported for callers that need the standalone display/CLI pair.
func ResolveBrand() Brand {
	return resolveBrandWithCLI(defaultCLIExecutableName)
}

func resolveBrandWithCLI(cli string) Brand {
	display := strings.TrimSpace(statehome.Brand())
	if display == "" {
		display = statehome.DefaultBrand
	}
	if cli == "" {
		cli = defaultCLIExecutableName
	}
	return Brand{
		BrandDisplay: titleBrand(display),
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
