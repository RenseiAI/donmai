package sessionshim

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
)

// The closed set of environment keys a controller uses to hand a freshly
// launched worker process everything it needs to BECOME a shim
// (ADR-2026-08-17 §D1/§D11 step 2).
//
// Env is the carrier rather than a flag or a config file for one reason: the
// daemon already composes the worker's environment and nothing else about the
// spawn contract changes, so shim ownership can be switched on for interactive
// sessions without a second launch protocol to keep in sync.
//
// Every value here is non-secret by construction — a lifecycle identity, a
// directory, and four durations. That is the same bound §D6 puts on the
// discovery record, and it is deliberate: these values are visible in the
// process table, so anything secret would be leaked by the carrier itself.
const (
	// EnvOwnership is the gate. Anything other than "1" leaves the worker on the
	// pre-shim direct-ownership path, byte for byte.
	EnvOwnership = "DONMAI_SESSION_SHIM"
	// EnvOrgID and EnvSessionID carry the sole lifecycle identity (§D2).
	EnvOrgID     = "DONMAI_SESSION_SHIM_ORG_ID"
	EnvSessionID = "DONMAI_SESSION_SHIM_SESSION_ID"
	// EnvRegistryDir is the discovery directory, resolved by the controller
	// through its injected state-directory seam so no path is compiled in here.
	EnvRegistryDir = "DONMAI_SESSION_SHIM_REGISTRY_DIR"
	// EnvProcessEpoch is the monotonic per-session value for this incarnation.
	EnvProcessEpoch = "DONMAI_SESSION_SHIM_PROCESS_EPOCH"
	// The §D8 orphan bounds, in milliseconds. The shim re-validates the
	// inequality itself rather than trusting the launcher: a controller and a
	// shim that disagree about the bound is exactly the configuration drift that
	// produces double execution.
	EnvOrphanDeadlineMS    = "DONMAI_SESSION_SHIM_ORPHAN_DEADLINE_MS"
	EnvTerminationGraceMS  = "DONMAI_SESSION_SHIM_TERMINATION_GRACE_MS"
	EnvPropagationMarginMS = "DONMAI_SESSION_SHIM_PROPAGATION_MARGIN_MS"
	EnvExternalReleaseMS   = "DONMAI_SESSION_SHIM_EXTERNAL_RELEASE_MS"
	// EnvRingBytes optionally overrides the shim-owned output ring budget, in
	// payload bytes. It exists for one reason: an acceptance run has to be able
	// to make the ring actually EVICT, and the eviction path is only reachable
	// by exceeding the budget. The production budget is 8 MiB, which the
	// acceptance seam's own volume misses by more than two orders of magnitude,
	// so without this the lane silently proves nothing.
	//
	// It is absent from the environment unless a controller sets it, and the
	// only controller path that sets it is gated on the acceptance-control
	// token file. A launch without it is byte-for-byte the released contract.
	EnvRingBytes = "DONMAI_SESSION_SHIM_RING_BYTES"
)

// ErrNoLaunch reports that the environment does not select shim ownership.
var ErrNoLaunch = errors.New("sessionshim: no shim launch in the environment")

// Launch is the decoded shim-ownership instruction a worker process received
// from whichever controller spawned it.
type Launch struct {
	Identity     Identity
	RegistryDir  string
	Orphan       OrphanPolicy
	ProcessEpoch uint64
	// RingBytes optionally overrides the shim-owned output ring budget, in
	// payload bytes. Zero means the worker's own spec decides, which is the
	// released behaviour.
	RingBytes int
}

// Env renders the launch as the environment overlay a controller adds to the
// worker's spawn environment.
//
// It is the ONLY producer, paired with the only consumer (LaunchFromEnv), so the
// key set cannot drift between the two halves of the contract — a mismatch there
// would surface as a worker that silently stays on the direct-ownership path
// while the controller waits for a discovery record that never arrives.
func (l Launch) Env() map[string]string {
	env := map[string]string{
		EnvOwnership:           "1",
		EnvOrgID:               l.Identity.OrgID,
		EnvSessionID:           l.Identity.SessionID,
		EnvRegistryDir:         l.RegistryDir,
		EnvProcessEpoch:        strconv.FormatUint(l.ProcessEpoch, 10),
		EnvOrphanDeadlineMS:    millis(l.Orphan.Deadline),
		EnvTerminationGraceMS:  millis(l.Orphan.TerminationGrace),
		EnvPropagationMarginMS: millis(l.Orphan.PropagationMargin),
		EnvExternalReleaseMS:   millis(l.Orphan.ExternalReleaseThreshold),
	}
	if l.RingBytes > 0 {
		env[EnvRingBytes] = strconv.Itoa(l.RingBytes)
	}
	return env
}

func millis(d time.Duration) string {
	return strconv.FormatInt(int64(d/time.Millisecond), 10)
}

// EnvKeys returns every key in the launch contract.
//
// Callers use it to scrub the contract out of environments it must not reach —
// notably the harness child, which is a session's WORKLOAD and has no business
// knowing where its own supervisor's registry lives.
func EnvKeys() []string {
	return []string{
		EnvOwnership, EnvOrgID, EnvSessionID, EnvRegistryDir, EnvProcessEpoch,
		EnvOrphanDeadlineMS, EnvTerminationGraceMS, EnvPropagationMarginMS,
		EnvExternalReleaseMS,
	}
}

// IsEnvKey reports whether key belongs to the launch contract.
func IsEnvKey(key string) bool {
	for _, k := range EnvKeys() {
		if k == key {
			return true
		}
	}
	return false
}

// LaunchFromEnv decodes a launch instruction from lookup (normally os.Getenv).
//
// It returns ErrNoLaunch when the gate is unset, which is the ordinary case and
// not a failure. When the gate IS set, every remaining field is required and a
// malformed one is an error rather than a default: a worker that quietly fell
// back to direct ownership after being told to be a shim would leave the
// controller adopting nothing while believing the session was shim-backed.
func LaunchFromEnv(lookup func(string) string) (Launch, error) {
	if lookup == nil || lookup(EnvOwnership) != "1" {
		return Launch{}, ErrNoLaunch
	}
	l := Launch{
		Identity:    Identity{OrgID: lookup(EnvOrgID), SessionID: lookup(EnvSessionID)},
		RegistryDir: lookup(EnvRegistryDir),
	}
	if err := l.Identity.Validate(); err != nil {
		return Launch{}, err
	}
	if l.RegistryDir == "" {
		return Launch{}, fmt.Errorf("sessionshim: %s is empty", EnvRegistryDir)
	}
	epoch, err := parseUint(lookup(EnvProcessEpoch), EnvProcessEpoch)
	if err != nil {
		return Launch{}, err
	}
	l.ProcessEpoch = epoch

	// maxLaunchDurationMS bounds every duration in the contract. It is generous
	// (a day) and it is a BOUND: the values arrive as text from a process
	// environment, and an unbounded parse would let a typo become a duration that
	// overflows into a negative deadline — an orphan rule that fires immediately
	// or never, decided by arithmetic nobody checked.
	const maxLaunchDurationMS = 24 * 60 * 60 * 1000

	durations := []struct {
		key string
		dst *time.Duration
	}{
		{EnvOrphanDeadlineMS, &l.Orphan.Deadline},
		{EnvTerminationGraceMS, &l.Orphan.TerminationGrace},
		{EnvPropagationMarginMS, &l.Orphan.PropagationMargin},
		{EnvExternalReleaseMS, &l.Orphan.ExternalReleaseThreshold},
	}
	for _, d := range durations {
		ms, err := parseUint(lookup(d.key), d.key)
		if err != nil {
			return Launch{}, err
		}
		if ms > maxLaunchDurationMS {
			return Launch{}, fmt.Errorf("sessionshim: %s is %d ms, max %d", d.key, ms, maxLaunchDurationMS)
		}
		*d.dst = time.Duration(ms) * time.Millisecond //nolint:gosec // G115: bounded above by maxLaunchDurationMS
	}
	// The ring override is the one OPTIONAL key in the contract: absent means the
	// worker's own spec decides, which is the released behaviour. Present and
	// malformed is still an error — a controller that meant to shrink the ring
	// and silently got 8 MiB would make an acceptance run prove nothing.
	if raw := lookup(EnvRingBytes); raw != "" {
		// maxLaunchRingBytes bounds the parse for the same reason the durations
		// are bounded: this arrives as text from a process environment.
		const maxLaunchRingBytes = 1 << 30
		ring, err := parseUint(raw, EnvRingBytes)
		if err != nil {
			return Launch{}, err
		}
		if ring == 0 || ring > maxLaunchRingBytes {
			return Launch{}, fmt.Errorf("sessionshim: %s is %d bytes, want 1..%d", EnvRingBytes, ring, maxLaunchRingBytes)
		}
		l.RingBytes = int(ring) //nolint:gosec // G115: bounded above by maxLaunchRingBytes
	}
	// Re-validate rather than trust. The launcher already checked this at its own
	// startup, but the two processes are separately configurable and §D8 makes
	// the inequality a precondition for ADMITTING a session, not a courtesy the
	// spawner performs on the shim's behalf.
	if err := l.Orphan.Validate(); err != nil {
		return Launch{}, err
	}
	return l, nil
}

func parseUint(v, key string) (uint64, error) {
	if v == "" {
		return 0, fmt.Errorf("sessionshim: %s is empty", key)
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("sessionshim: %s is not a non-negative integer: %w", key, err)
	}
	return n, nil
}

// StartFromEnv is the worker-side half of the launch contract: it turns a
// decoded Launch plus a PTY spec into a running shim.
//
// The workarea is taken from the caller rather than the environment because it
// is verified at adoption against what the CONTROLLER believes (§D7); reading it
// from the same env block the controller wrote would make that check compare a
// value against itself.
func StartFromEnv(l Launch, spec ptyhost.Spec, workareaPath string) (*Shim, error) {
	return StartFromEnvWithRoot(l, spec, workareaPath, "")
}

// StartFromEnvWithRoot is the additive session-root-v1 launch path. The legacy
// helper above keeps old call sites and discovery bytes unchanged.
func StartFromEnvWithRoot(l Launch, spec ptyhost.Spec, workareaPath, workareaRoot string) (*Shim, error) {
	registry, err := NewRegistry(l.RegistryDir)
	if err != nil {
		return nil, err
	}
	if l.RingBytes > 0 {
		// The launch is authoritative over the worker's own spec here: the
		// controller is the only party that knows this session is an acceptance
		// session, and the ring budget has to be settled before the first frame.
		spec.RingBytes = l.RingBytes
	}
	return Start(Options{
		Identity:     l.Identity,
		Registry:     registry,
		Spec:         spec,
		WorkareaPath: workareaPath,
		WorkareaRoot: workareaRoot,
		Orphan:       l.Orphan,
		ProcessEpoch: l.ProcessEpoch,
	})
}
