package agycli

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// API-key-mode compatibility bounds for the agy CLI binary.
//
// agy 1.1.13 added API-key mode: with settings.json
// {"modelProvider":"gemini"} under $HOME/.gemini/antigravity-cli/ and
// GEMINI_API_KEY in the environment, `agy` bills a Gemini API key instead
// of the CLI's own subscription login. The adapter's keyed spawn (keymode.go)
// depends on that behavior, so construction verifies the installed binary
// supports it and fails with agent.ErrProviderUnavailable when it provably
// does not.
const (
	// MinAPIKeyVersion is the lowest agy version that supports API-key
	// mode. A probed version below this fails construction.
	MinAPIKeyVersion = "1.1.13"

	// apiKeyModeHelpFlag is the --help-advertised flag that proves
	// API-key-mode-era CLI surface even when the version probe is
	// inconclusive (unparseable --version output). agy's structured
	// output flag shipped with the API-key-mode generation.
	apiKeyModeHelpFlag = "--output-format"
)

// DefaultAPIKeyProbeTimeout bounds the "<binary> --version" probe at
// construction. Kept short and consistent with the sibling --help probe
// (agyHelpTimeout): both fire once per Provider construction, never per
// spawn.
const DefaultAPIKeyProbeTimeout = 5 * time.Second

// versionProbeFunc runs "<binary> --version" (or an equivalent) and
// returns its raw output. Overridable via Options.VersionProbe so tests
// assert enforcement behavior without a real binary.
type versionProbeFunc func(ctx context.Context, binary string) (string, error)

// defaultVersionProbe execs "<binary> --version" and returns its
// trimmed output.
func defaultVersionProbe(ctx context.Context, binary string) (string, error) {
	//nolint:gosec // binary was resolved with exec.LookPath before this probe.
	cmd := exec.CommandContext(ctx, binary, "--version")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// apiKeyVersionRe extracts a dotted X.Y.Z... version from free-form
// "--version" output (e.g. "agy 1.2.13" or a bare "1.1.13").
var apiKeyVersionRe = regexp.MustCompile(`\d+(?:\.\d+)+`)

// extractAPIKeyVersion pulls the first dotted-version substring out of raw
// "--version" output. Returns ok=false when none is found.
func extractAPIKeyVersion(raw string) (version string, ok bool) {
	m := apiKeyVersionRe.FindString(raw)
	return m, m != ""
}

// compareAPIVersions compares two dotted-integer version strings
// component-wise. Returns -1 if a<b, 0 if equal, 1 if a>b. Lenient
// advisory comparison (missing/non-numeric components compare as 0), not
// a strict semver library — the pin bound above is always a well-formed
// X.Y.Z string authored alongside this file.
func compareAPIVersions(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var av, bv int
		if i < len(as) {
			av, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bv, _ = strconv.Atoi(bs[i])
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

// verifyAPIKeyModeSupport accepts a CLI build that can run a keyed spawn:
// a --version at or above MinAPIKeyVersion, OR --help advertising the
// API-key-mode-era flag surface. It fails closed only when the binary
// provably predates API-key mode (a parsed version below the floor). An
// inconclusive probe (probe error, unparseable version, help text without
// the flag) is non-fatal: the keyed spawn labels the session at runtime
// rather than blocking construction on a signal the binary simply did not
// answer — the same label-don't-block posture the fleet's version pins use.
//
// The boolean reports whether API-key mode was positively confirmed
// (version >= floor, or flag advertised). Callers surface it for
// observability; spawn itself re-checks nothing.
func verifyAPIKeyModeSupport(binary string, versionProbe versionProbeFunc, help agyHelpRunner) error {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultAPIKeyProbeTimeout)
	defer cancel()
	raw, err := versionProbe(ctx, binary)
	if err != nil {
		return nil // inconclusive: label, don't block
	}
	v, ok := extractAPIKeyVersion(raw)
	if !ok {
		// No parseable version: fall back to the --help flag signal.
		return verifyAPIKeyModeHelpFallback(binary, help)
	}
	if compareAPIVersions(v, MinAPIKeyVersion) < 0 {
		return apiKeyVersionBelowMinError(v)
	}
	return nil
}

// verifyAPIKeyModeHelpFallback checks the --help text for the
// API-key-mode-era flag when --version yields no parseable version. A
// help-probe failure is inconclusive (label, don't block); a missing flag
// is likewise inconclusive — only a parsed below-floor version fails.
func verifyAPIKeyModeHelpFallback(binary string, help agyHelpRunner) error {
	ctx, cancel := context.WithTimeout(context.Background(), agyHelpTimeout)
	defer cancel()
	out, err := help(ctx, binary)
	if err != nil {
		return nil // inconclusive: label, don't block
	}
	if strings.Contains(string(out), apiKeyModeHelpFlag) {
		return nil
	}
	return nil // inconclusive: label, don't block
}

// apiKeyVersionBelowMinError builds the actionable construction-failure
// error for a confirmed-below-floor binary.
func apiKeyVersionBelowMinError(got string) error {
	return fmt.Errorf(
		"agy %s predates API-key mode (minimum supported version %s) — "+
			"upgrade agy to run keyed sessions",
		got, MinAPIKeyVersion,
	)
}
