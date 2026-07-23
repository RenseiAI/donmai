package opencode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// defaultGetenv wraps os.Getenv so tests can inject a fake without
// touching the global process environment.
func defaultGetenv(key string) string { return os.Getenv(key) }

// Version-pin bounds for the opencode CLI binary
// (07-design-opencode-spawn.md §8). opencode ships roughly twice a day,
// independently of donmai releases, so the adapter pins a version and
// enforces it at construction time (below) rather than trusting
// whatever happens to be on $PATH.
//
// These constants are the SINGLE SOURCE OF TRUTH for both the
// probe-time enforcement in New() and the generated matrix's
// binaryPins section (matrix/cells.go references these constants
// directly rather than re-typing the numbers, so the two can never
// drift from each other).
const (
	// MinVersion is the lowest opencode version the adapter's flag
	// surface (D-1..D-4 fixes: --auto not --dangerously-skip-permissions,
	// port 4096, terminal-event fix) is known to work against. A probed
	// version below this fails construction with
	// agent.ErrProviderUnavailable.
	MinVersion = "1.17.18"

	// PinnedVersion is the exact version donmai-smokes' CI installer
	// (harness/opencode_install.go) installs and the version the
	// generated matrix's binaryPins section labels the opencode cells
	// against.
	PinnedVersion = "1.17.18"

	// VerifiedAgainst is the highest opencode version anyone has
	// actually run this adapter against. A probed version above this is
	// allowed to spawn (DEC-2: label, don't block) but the provider
	// emits a SystemEvent{Subtype:"unverified_harness_version"} once per
	// session so operators can see the drift.
	VerifiedAgainst = "1.17.18"
)

// unverifiedVersionSubtype is the SystemEvent.Subtype emitted once per
// session when the probed binary version could not be confirmed to fall
// within [MinVersion, VerifiedAgainst] (07 §8).
const unverifiedVersionSubtype = "unverified_harness_version"

// DefaultVersionProbeTimeout bounds the "<binary> --version" probe at
// construction. Kept short and separate from DefaultProbeTimeout (the
// HTTP-server-mode liveness check) since the two probes fire in
// mutually exclusive modes.
const DefaultVersionProbeTimeout = 3 * time.Second

// versionProbeFunc runs "<binary> --version" (or an equivalent) and
// returns its raw output. Overridable via Options.VersionProbe so tests
// assert enforcement behavior without a real fake-CLI script.
type versionProbeFunc func(ctx context.Context, binary string) (string, error)

// defaultVersionProbe execs "<binary> --version" and returns its
// trimmed stdout.
func defaultVersionProbe(ctx context.Context, binary string) (string, error) {
	// nolint:gosec // G204: binary is the same resolved-from-PATH path
	// New() already uses to exec `opencode run`; --version is a
	// read-only, argument-free query.
	cmd := exec.CommandContext(ctx, binary, "--version")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// versionRe extracts a dotted X.Y.Z... version from free-form
// "--version" output (e.g. "opencode 1.17.18" or a bare "1.17.18").
var versionRe = regexp.MustCompile(`\d+(?:\.\d+)+`)

// extractVersion pulls the first dotted-version substring out of raw
// "--version" output. Returns ok=false when none is found.
func extractVersion(raw string) (version string, ok bool) {
	m := versionRe.FindString(raw)
	return m, m != ""
}

// compareVersions compares two dotted-integer version strings
// (e.g. "1.17.18") component-wise. Returns -1 if a<b, 0 if equal, 1 if
// a>b. This is a lenient advisory comparison (missing/non-numeric
// components compare as 0), not a strict semver library — the pin
// bounds above are always well-formed X.Y.Z strings authored alongside
// this file.
func compareVersions(a, b string) int {
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

// checkVersionPin runs probe against binary and classifies the result:
//
//   - a version confirmed below MinVersion is a hard failure (err != nil,
//     wrapping agent.ErrProviderUnavailable at the call site);
//   - a version confirmed above VerifiedAgainst, OR a version this
//     function could not determine at all (probe error, unparseable
//     output) — is "unverified": non-fatal, but the caller should label
//     the session (DEC-2: an untested/unpinned tier is labeled, not
//     blocked, unless it fails a hard floor);
//   - a version within [MinVersion, VerifiedAgainst] is verified.
//
// probeErr is returned (non-nil) only for the hard-failure case; a nil
// probeErr with unverified=true means "proceed, but label the session."
func checkVersionPin(ctx context.Context, probe versionProbeFunc, binary string) (unverified bool, probeErr error) {
	raw, err := probe(ctx, binary)
	if err != nil {
		// Could not run --version at all (missing binary, unsupported
		// flag, timeout). Non-fatal: label the session as unverified
		// rather than blocking a runner whose binary otherwise works.
		return true, nil
	}
	v, ok := extractVersion(raw)
	if !ok {
		return true, nil
	}
	if compareVersions(v, MinVersion) < 0 {
		return false, versionBelowMinError(v)
	}
	if compareVersions(v, VerifiedAgainst) > 0 {
		return true, nil
	}
	return false, nil
}

// versionBelowMinError builds the actionable construction-failure error
// for a confirmed-below-MinVersion binary.
func versionBelowMinError(got string) error {
	return fmt.Errorf(
		"%w: opencode %s is below the minimum supported version %s (pinned %s) — "+
			"upgrade with `npm i -g opencode-ai@%s`",
		agent.ErrProviderUnavailable, got, MinVersion, PinnedVersion, PinnedVersion,
	)
}
