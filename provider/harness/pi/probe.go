package pi

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// Version-pin bounds for the pi CLI binary (09-design-pi-adapter.md §8). pi
// ships multiple releases/day (v0.80.10 at research time), independently of
// donmai releases, so the adapter pins a version and enforces it at
// construction time rather than trusting whatever is on $PATH — the same
// probe-time-enforcement posture opencode/probe.go established (07 §8).
//
// These constants are the SINGLE SOURCE OF TRUTH for both the probe-time
// enforcement in New() and the generated matrix's binaryPins section
// (matrix/cells.go reads pi.MinVersion/pi.PinnedVersion/pi.VerifiedAgainst
// directly rather than re-typing the numbers, so the two can never drift).
//
// UNVERIFIED-LOCALLY: pi was not installed on the authoring host
// (`pi --version` was absent), so PinnedVersion is the design doc's
// researched version, not a version this adapter was actually run against.
// VerifiedAgainst is intentionally set EQUAL to MinVersion (not to a higher
// "we ran it" number) to encode that honesty: until the donmai-smokes step20
// lane installs and exercises the pinned binary, no pi version has been
// verified, and any probed binary will be labeled unverified per DEC-2.
const (
	// MinVersion is the lowest pi version the adapter's command/event
	// surface is designed against. A probed version below this fails
	// construction with agent.ErrProviderUnavailable.
	MinVersion = "0.80.10"

	// PinnedVersion is the exact version donmai-smokes' CI installer
	// (harness/pi_install.go) installs and the version the generated
	// matrix's binaryPins section labels the pi cells against.
	//
	// npm: @earendil-works/pi-coding-agent@0.80.10 (09 §8).
	PinnedVersion = "0.80.10"

	// VerifiedAgainst is the highest pi version this adapter has actually
	// been run against. It stays EQUAL to MinVersion until the smokes lane
	// proves a real run — every probed binary is therefore labeled
	// unverified (DEC-2: label, don't block) rather than silently trusted.
	VerifiedAgainst = "0.80.10"
)

// unverifiedVersionSubtype is the SystemEvent.Subtype emitted once per
// session when the probed binary version could not be confirmed to fall
// within [MinVersion, VerifiedAgainst] (09 §8). Matches opencode's subtype
// so downstream observers key on one string across harnesses.
const unverifiedVersionSubtype = "unverified_harness_version"

// DefaultVersionProbeTimeout bounds the "pi --version" probe at construction.
const DefaultVersionProbeTimeout = 3 * time.Second

// versionProbeFunc runs "<binary> --version" (or equivalent) and returns its
// raw output. Overridable via Options.VersionProbe so tests assert
// enforcement behavior without a real pi binary on PATH.
type versionProbeFunc func(ctx context.Context, binary string) (string, error)

// defaultVersionProbe execs "<binary> --version" and returns trimmed stdout.
func defaultVersionProbe(ctx context.Context, binary string) (string, error) {
	// nolint:gosec // G204: binary is the resolved-from-PATH path New() also
	// uses to exec `pi --mode rpc`; --version is a read-only query.
	out, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// versionRe extracts a dotted X.Y.Z... version from free-form --version
// output (e.g. "pi 0.80.10" or a bare "0.80.10").
var versionRe = regexp.MustCompile(`\d+(?:\.\d+)+`)

// extractVersion pulls the first dotted-version substring out of raw output.
func extractVersion(raw string) (version string, ok bool) {
	m := versionRe.FindString(raw)
	return m, m != ""
}

// compareVersions compares two dotted-integer version strings component-wise.
// Returns -1 if a<b, 0 if equal, 1 if a>b. Lenient advisory comparison
// (missing/non-numeric components compare as 0), matching opencode's helper —
// the pin bounds above are always well-formed X.Y.Z strings.
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
//   - a version confirmed below MinVersion is a hard failure (probeErr != nil);
//   - a version confirmed above VerifiedAgainst, OR one this function could
//     not determine at all (probe error, unparseable output), is "unverified":
//     non-fatal, but the caller should label the session (DEC-2);
//   - a version within [MinVersion, VerifiedAgainst] is verified.
func checkVersionPin(ctx context.Context, probe versionProbeFunc, binary string) (unverified bool, probeErr error) {
	raw, err := probe(ctx, binary)
	if err != nil {
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

// versionBelowMinError builds the actionable construction-failure error for a
// confirmed-below-MinVersion binary.
func versionBelowMinError(got string) error {
	return fmt.Errorf(
		"%w: pi %s is below the minimum supported version %s (pinned %s) — "+
			"upgrade with `npm i -g @earendil-works/pi-coding-agent@%s`",
		agent.ErrProviderUnavailable, got, MinVersion, PinnedVersion, PinnedVersion,
	)
}
