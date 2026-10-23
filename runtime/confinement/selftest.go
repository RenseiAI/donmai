package confinement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// SelfTestOptions configure one startup self-test.
type SelfTestOptions struct {
	// ProbeCommand starts the probe process: an executable that calls
	// RunProbeFromEnv first thing in main. It runs through the production
	// spawn binding, standing in for the harness.
	ProbeCommand []string
	// ProbeEnv is extra KEY=VALUE environment for the probe.
	ProbeEnv []string
	// ScratchDir holds the probe fixtures: under the host state home, and
	// short, because the fixture binds a socket in its session tmp.
	ScratchDir string
	// Launchers are the spawn paths, one per session mode. Nil uses
	// DefaultLaunchers.
	Launchers map[agent.PromptSessionMode]Launcher
	// Timeout bounds each mode's probe run. Zero means two minutes.
	Timeout time.Duration
	// ProbeHandshakeTimeout bounds how long each probe pass waits for the
	// probe child's handshake (the file the probe writes beside its plan
	// before running any step) before failing the pass with probe_unserved.
	// Zero means DefaultProbeHandshakeTimeout. It is deliberately separate
	// from Timeout: a child that never serves the probe — an executable
	// whose main opened its own interface instead of running it — must fail
	// in seconds, not burn the whole cold-proof budget waiting for results
	// that can never arrive. A child that exits before the bound keeps the
	// existing accounting (its exit code and missing results), not this
	// reason: only a child still running past the bound is unserved.
	ProbeHandshakeTimeout time.Duration
	// CacheDir, when set, keeps a passing self-test record on disk, so the
	// next process on this host reuses it instead of probing again while
	// nothing it proved has changed: the backend and its version (the
	// profile version plus the OS build), the probe set, the harness
	// executable, the probe executable and the host directories. A record
	// that is stale by those, older than CacheTTL, or not passing in both
	// session modes is never reused. It belongs under the host state home,
	// outside every session root. Caching is off when the confiner has a
	// composer callback, whose rules a cached record cannot vouch for.
	CacheDir string
	// CacheTTL bounds how long a cached record is reused. Zero means
	// DefaultSelfTestCacheTTL.
	CacheTTL time.Duration
}

// DefaultSelfTestCacheTTL is how long a cached passing self-test is reused
// when SelfTestOptions.CacheTTL is zero.
const DefaultSelfTestCacheTTL = 24 * time.Hour

// DefaultProbeHandshakeTimeout is how long each probe pass waits for the
// probe child's handshake when SelfTestOptions.ProbeHandshakeTimeout is
// zero. Thirty seconds is orders of magnitude above a healthy probe's
// startup (process spawn plus one file write, even under sandbox-exec on a
// loaded host) and four times below the per-mode proof budget, so an
// executable that never serves the probe fails fast instead of stalling
// every start on the full budget.
const DefaultProbeHandshakeTimeout = 30 * time.Second

// probeHandshakePollInterval is how often a probe pass checks for the
// probe child's handshake file while the launcher runs.
const probeHandshakePollInterval = 50 * time.Millisecond

// ProbeOutcome is one probe's result in one session mode.
type ProbeOutcome struct {
	ID       string                  `json:"id"`
	Class    string                  `json:"class"`
	Mode     agent.PromptSessionMode `json:"mode"`
	Expected string                  `json:"expected"`
	Observed string                  `json:"observed"`
	Pass     bool                    `json:"pass"`
	Detail   string                  `json:"detail,omitempty"`
	// HeldBy names what other than the boundary refuses this probe on this
	// host (heldByCrossDevice), so a pass here is never read as the
	// backend holding it. Empty: only the boundary can refuse it.
	HeldBy string `json:"heldBy,omitempty"`
}

// heldByCrossDevice marks a rename probe whose source and destination sit on
// different filesystems on this host: the kernel refuses it boundary or not.
const heldByCrossDevice = "os:cross_device"

// NotProbedReason is why a probe of the set does not run on this host.
type NotProbedReason string

// The reasons a probe does not run.
const (
	// NotProbedPlatform: the probe's mechanism does not exist on this OS (a
	// macOS service on Linux), so there is nothing to reach.
	NotProbedPlatform NotProbedReason = "platform"
	// NotProbedHostService: this host runs no such service (no per-user
	// bus), so there is nothing to reach.
	NotProbedHostService NotProbedReason = "host_service_absent"
	// NotProbedKernel: the kernel cannot hold it, so the boundary is weaker
	// on this host than the contract asks — recorded, never passed.
	NotProbedKernel NotProbedReason = "kernel_unsupported"
)

// NotProbed is a probe of the set the self-test did not run on this host,
// and why. It is never counted as held.
type NotProbed struct {
	ID     string                  `json:"id"`
	Mode   agent.PromptSessionMode `json:"mode"`
	Reason NotProbedReason         `json:"reason"`
	Detail string                  `json:"detail,omitempty"`
}

// The observed and expected outcome names.
const (
	outcomeAccepted = "accepted"
	outcomeRefused  = "refused"
	outcomeMissing  = "no_result"
)

// SelfTestRecord names the backend, its version (implementation plus OS
// build), the probe-set version, each probe's outcome and a digest (D1.2).
// A record taken where the scope layer is missing (ScopesAvailable false
// on Linux below ABI 6) is degraded: every probe it ran passed, but the
// boundary it proves is partial, so Degraded names the missing layer and
// Attestation refuses it.
type SelfTestRecord struct {
	Backend          BackendName               `json:"backend"`
	BackendVersion   string                    `json:"backendVersion"`
	ProbeSetVersion  string                    `json:"probeSetVersion"`
	ExecutableDigest string                    `json:"executableDigest,omitempty"`
	SessionModes     []agent.PromptSessionMode `json:"sessionModes"`
	Probes           []ProbeOutcome            `json:"probes"`
	NotProbed        []NotProbed               `json:"notProbed,omitempty"`
	Passed           bool                      `json:"passed"`
	// Degraded names the unenforced layer when the record proves a
	// partial boundary only (the scope layer below Landlock ABI 6).
	// Empty means the record proves the full boundary. It is part of
	// the digest, so a degraded record never verifies as a full one.
	Degraded string    `json:"degraded,omitempty"`
	TestedAt time.Time `json:"testedAt"`
	Digest   string    `json:"digest"`
}

// Failures returns the probes that did not pass.
func (r SelfTestRecord) Failures() []ProbeOutcome {
	var failed []ProbeOutcome
	for _, probe := range r.Probes {
		if !probe.Pass {
			failed = append(failed, probe)
		}
	}
	return failed
}

// Attestation is the per-harness attestation this record supports: the
// session modes whose spawn path passed every probe. A degraded record
// (Degraded set) attests nothing: the probes passed but the boundary is
// partial, so no spawn path is reported as confined.
func (r SelfTestRecord) Attestation() Attestation {
	if r.Degraded != "" {
		return Attestation{}
	}
	return Attestation{
		Backend:         r.Backend,
		BackendVersion:  r.BackendVersion,
		ProbeSetVersion: r.ProbeSetVersion,
		SelfTestDigest:  r.Digest,
		SessionModes:    append([]agent.PromptSessionMode(nil), r.SessionModes...),
		TestedAt:        r.TestedAt.UTC().Format(time.RFC3339),
	}
}

func (r SelfTestRecord) fingerprint() fingerprint {
	return fingerprint{
		backend:          r.Backend,
		backendVersion:   r.BackendVersion,
		probeSetVersion:  r.ProbeSetVersion,
		executableDigest: r.ExecutableDigest,
	}
}

func (r SelfTestRecord) computeDigest() string {
	r.Digest = ""
	raw, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return digestBytes(raw)
}

// applyScopeVerdict marks a passing record degraded where the scope probe
// says the layer is missing: the probes passed but the boundary they prove
// is partial, so the full-boundary attestation stays empty while the seat
// still runs (the mount tree, the process namespace and the filesystem rules
// hold; only the scope layer is missing). The session modes stay on the
// record so Prepare still starts seats from it; Attestation refuses it as a
// full boundary and the status surface carries the degraded reason. An
// enforced layer, or a record that did not pass, is left alone. SelfTest
// calls it on its production record; tests drive it directly with both
// verdicts, without a Linux kernel.
func (r *SelfTestRecord) applyScopeVerdict(ok bool, why string) {
	if !r.Passed || ok || why == "" {
		return
	}
	r.Degraded = why
}

// SelfTest proves the backend on this host: it drives a probe process through
// the production spawn binding once per session mode, with every writable
// class, a read-only leaf, protected paths, decoys outside the set and the
// write proxies in reach, and observes what changed. A second pass per mode
// renders the same world under the workarea read scope and proves the read
// allowlist: reads inside it succeed, reads and listings outside it fail. A
// mode is attested only when every probe in it passed — and where the scope
// layer is missing (Linux below ABI 6) the passing record is marked
// degraded instead of attested: the mount tree, the process namespace and
// the filesystem rules hold, but signals and abstract sockets outside stay
// reachable. The record is kept for Prepare, passing or not; a failure
// returns the record with a typed error.
func (c *Confiner) SelfTest(ctx context.Context, opts SelfTestOptions) (SelfTestRecord, error) {
	if len(opts.ProbeCommand) == 0 {
		return SelfTestRecord{}, errors.New("confinement: self-test needs a probe command")
	}
	if !filepath.IsAbs(opts.ScratchDir) {
		return SelfTestRecord{}, errors.New("confinement: self-test scratch directory must be absolute")
	}
	if !filepath.IsAbs(c.opts.Home) || !filepath.IsAbs(c.opts.StateHome) {
		return SelfTestRecord{}, errors.New("confinement: self-test needs the operator home and host state home")
	}
	current, err := c.fingerprint()
	if err != nil {
		c.store(SelfTestRecord{TestedAt: time.Now().UTC()})
		return SelfTestRecord{}, err
	}
	record := SelfTestRecord{
		Backend:          current.backend,
		BackendVersion:   current.backendVersion,
		ProbeSetVersion:  ProbeSetVersion,
		ExecutableDigest: current.executableDigest,
		TestedAt:         time.Now().UTC(),
	}
	if err := c.opts.Backend.Check(); err != nil {
		record.Digest = record.computeDigest()
		c.store(record)
		// A Check failure is an outcome too: it supersedes any earlier pass
		// so the status path reports unproven instead of attesting from it.
		c.recordLatest(record)
		return record, err
	}
	cache, cacheOK := c.selfTestCache(opts, current)
	if cacheOK {
		if cached, hit := cache.load(time.Now()); hit {
			c.store(cached)
			return cached, nil
		}
	}
	launchers := opts.Launchers
	if launchers == nil {
		launchers = DefaultLaunchers()
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	// Resolve the handshake bound once, here, so every pass and the decoy
	// wait on the same value. opts is this call's own copy; the cache key
	// above was already computed from the caller's values, and the bound is
	// not part of what a cached record proves.
	if opts.ProbeHandshakeTimeout <= 0 {
		opts.ProbeHandshakeTimeout = DefaultProbeHandshakeTimeout
	}
	if err := os.MkdirAll(opts.ScratchDir, 0o700); err != nil {
		return SelfTestRecord{}, fmt.Errorf("confinement: self-test scratch: %w", err)
	}
	scratch, err := os.MkdirTemp(opts.ScratchDir, "st")
	if err != nil {
		return SelfTestRecord{}, fmt.Errorf("confinement: self-test scratch: %w", err)
	}
	defer func() { _ = removeAllForce(scratch) }()
	diskImage, diskImageErr := createProbeDiskImage(ctx, scratch)

	var refusal error
	for i, mode := range []agent.PromptSessionMode{agent.PromptModeAutonomous, agent.PromptModeHumanControlled} {
		launcher := launchers[mode]
		if launcher == nil {
			record.Probes = append(record.Probes, ProbeOutcome{ID: "mode.launcher", Class: "setup", Mode: mode, Expected: outcomeAccepted, Observed: outcomeMissing, Detail: "no launcher for this session mode"})
			continue
		}
		modeCtx, cancel := context.WithTimeout(ctx, timeout)
		outcomes, notProbed, err := c.selfTestMode(modeCtx, mode, launcher, opts, filepath.Join(scratch, fmt.Sprintf("m%d", i)), diskImage, diskImageErr)
		cancel()
		if err != nil {
			if _, typed := ReasonOf(err); typed && refusal == nil {
				refusal = err
			}
			record.Probes = append(record.Probes, ProbeOutcome{ID: "mode.setup", Class: "setup", Mode: mode, Expected: outcomeAccepted, Observed: outcomeMissing, Detail: err.Error()})
			continue
		}
		record.Probes = append(record.Probes, outcomes...)
		record.NotProbed = append(record.NotProbed, notProbed...)
		if allPass(outcomes) {
			record.SessionModes = append(record.SessionModes, mode)
		}
	}
	record.Passed = len(record.SessionModes) == 2 && allPass(record.Probes)
	if record.Passed {
		// A passing record on a kernel without the scope layer proves a
		// partial boundary only: every probe passed, but the layer that
		// holds signals and abstract sockets is missing. Mark it so
		// Attestation refuses it and operators see it, rather than
		// reporting the host as confined.
		record.applyScopeVerdict(ScopesAvailable())
	}
	record.Digest = record.computeDigest()
	c.store(record)
	// The latest outcome supersedes any earlier pass: record it where the
	// status path reads it back, passing or not, so a failure after a pass
	// reports unproven instead of attesting from the stale pass.
	c.recordLatest(record)
	// A passing, whole record is proof seats start from: keep it where
	// the status path reads it back, so the operator surface reports
	// what the seats proved rather than what the daemon guessed. A
	// degraded passing record is proof of a partial boundary: it is kept
	// as the latest outcome above, and the cache below reuses it as
	// degraded on a host whose scope layer is still missing.
	if record.Passed && record.Degraded == "" && refusal == nil {
		c.recordProven(record)
	}
	// A degraded passing record is reusable as degraded where the scope
	// layer is still missing (keyed by the scope verdict in load), so a
	// sub-floor host does not re-run the full self-test on every spawn.
	if cacheOK && record.Passed && refusal == nil {
		cache.save(record)
	}
	if refusal != nil {
		return record, refusal
	}
	if !record.Passed {
		failed := record.Failures()
		names := make([]string, 0, len(failed))
		for _, probe := range failed {
			names = append(names, string(probe.Mode)+"/"+probe.ID)
		}
		sort.Strings(names)
		if len(names) > 12 {
			names = append(names[:12], fmt.Sprintf("and %d more", len(failed)-12))
		}
		return record, refuse(ReasonSelfTestFailed, "%d probe(s) failed: %s", len(failed), strings.Join(names, ", "))
	}
	return record, nil
}

func (c *Confiner) store(record SelfTestRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.selfTest = &record
}

func (c *Confiner) selfTestMode(ctx context.Context, mode agent.PromptSessionMode, launcher Launcher, opts SelfTestOptions, dir, diskImage string, diskImageErr error) ([]ProbeOutcome, []NotProbed, error) {
	fx, err := newFixture(dir, mode, fixtureHost{
		home: c.opts.Home, stateHome: c.opts.StateHome,
		diskImage: diskImage, diskImageErr: diskImageErr,
		loopbackOpen:   leavesLoopbackEgressOpen(c.opts.Backend),
		probeCommand:   opts.ProbeCommand,
		probeHandshake: opts.ProbeHandshakeTimeout,
	})
	if err != nil {
		return nil, nil, err
	}
	defer fx.cleanup()
	// The read-scope pass runs first: it renders the world under the read
	// scope and changes nothing the write pass judges, while a write pass
	// against a backend that holds nothing can leave the set unrepresentable
	// (a hard link out of it) for any later rendering. Its plan sits in the
	// session tmp, inside the read allowlist.
	readOutcomes, err := c.probePass(ctx, passInput{
		mode: mode, launcher: launcher, opts: opts, fx: fx,
		spec: fx.readSpec(mode), steps: fx.readSteps,
		planPath: filepath.Join(fx.tmp, "read-plan.json"), resultPath: filepath.Join(fx.tmp, "read-result.json"),
	})
	if err != nil {
		return nil, nil, err
	}
	// The write and widening probes run with reads open, so a read deny
	// cannot stand in for the write rule a probe is judging. The plan sits
	// in the session tmp: the probe reads it from inside the boundary, and
	// a backend that hides everything outside the writable set (a tmpfs
	// root) would hide a plan beside the scratch directory.
	outcomes, err := c.probePass(ctx, passInput{
		mode: mode, launcher: launcher, opts: opts, fx: fx,
		spec: fx.spec(mode), steps: fx.steps,
		planPath: filepath.Join(fx.tmp, "plan.json"), resultPath: probeResultPath(fx.tmp),
		settle: fx.settle,
	})
	if err != nil {
		return nil, nil, err
	}
	notProbed := make([]NotProbed, 0, len(fx.notProbed))
	for _, entry := range fx.notProbed {
		entry.Mode = mode
		notProbed = append(notProbed, entry)
	}
	return append(outcomes, readOutcomes...), notProbed, nil
}

// passInput is one probe pass: a spec rendered through the production
// spawn binding, the steps the probe runs under it, and where the plan and
// the results go.
type passInput struct {
	mode                 agent.PromptSessionMode
	launcher             Launcher
	opts                 SelfTestOptions
	fx                   *fixture
	spec                 Spec
	steps                []harnessStep
	planPath, resultPath string
	// settle runs after the probe exits and before any effect is judged.
	settle func()
}

func (c *Confiner) probePass(ctx context.Context, in passInput) ([]ProbeOutcome, error) {
	plan, err := c.prepare(in.spec, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = plan.Release() }()

	steps := make([]probeStep, 0, len(in.steps))
	for _, step := range in.steps {
		steps = append(steps, step.probe)
	}
	raw, err := json.Marshal(probePlan{ResultPath: in.resultPath, Steps: steps})
	if err != nil {
		return nil, fmt.Errorf("confinement: encode probe plan: %w", err)
	}
	if err := os.WriteFile(in.planPath, raw, 0o600); err != nil {
		return nil, fmt.Errorf("confinement: write probe plan: %w", err)
	}
	argv, err := plan.Command(in.opts.ProbeCommand)
	if err != nil {
		return nil, err
	}
	env := append(plan.Environment(), in.opts.ProbeEnv...)
	env = append(env, ProbeEnv+"="+in.planPath)
	exitCode, launchErr, unserved := c.launchProbe(ctx, in, argv, env)
	if unserved != nil {
		return nil, unserved
	}
	if in.settle != nil {
		in.settle()
	}

	results := map[string]stepResult{}
	if raw, err := os.ReadFile(in.resultPath); err == nil {
		var decoded []stepResult
		if json.Unmarshal(raw, &decoded) == nil {
			for _, result := range decoded {
				results[result.ID] = result
			}
		}
	}
	outcomes := make([]ProbeOutcome, 0, len(in.steps))
	for _, step := range in.steps {
		result, ok := results[step.probe.ID]
		outcome := ProbeOutcome{ID: step.probe.ID, Class: step.class, Mode: in.mode, Expected: outcomeRefused, HeldBy: step.heldBy}
		if step.expectAccepted {
			outcome.Expected = outcomeAccepted
		}
		if !ok {
			outcome.Observed = outcomeMissing
			outcome.Detail = fmt.Sprintf("the probe left no result (exit %d", exitCode)
			if launchErr != nil {
				outcome.Detail += ", " + errnoText(launchErr)
			}
			outcome.Detail += ")"
			outcomes = append(outcomes, outcome)
			continue
		}
		if step.setupErr != "" {
			outcome.Observed = outcomeMissing
			outcome.Detail = step.setupErr
			outcomes = append(outcomes, outcome)
			continue
		}
		effect := step.effect(result)
		if !toolOps[step.probe.Op] && result.Err == "" {
			// A call that returned success did what it was asked, whatever
			// a later probe did to the path since.
			effect = true
		}
		if effect {
			outcome.Observed = outcomeAccepted
		} else {
			outcome.Observed = outcomeRefused
		}
		if step.expectAccepted {
			outcome.Pass = effect && result.Err == ""
		} else {
			outcome.Pass = !effect
		}
		if !outcome.Pass {
			outcome.Detail = describeResult(result)
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

// launchOutcome is one launcher run: the child's exit code, or the launch
// error when the child never ran to completion.
type launchOutcome struct {
	exit int
	err  error
}

// launchProbe runs one probe pass's child and waits for its handshake. It
// returns the child's exit code and launch error for the existing
// result accounting, or a typed probe_unserved refusal when the child is
// still running past the handshake bound without serving the probe.
//
// A child that exits before the bound keeps the existing accounting (its
// exit code and missing results): only a child still running past the
// bound is unserved. That distinction keeps a broken probe command a
// judged failure while an executable whose main never reaches the probe —
// an embedder interface opened under the PTY instead — fails fast instead
// of burning the whole proof budget. The refusal surfaces as the mode's
// setup outcome with the typed reason, so the self-test returns promptly
// with no attestation instead of stalling.
func (c *Confiner) launchProbe(ctx context.Context, in passInput, argv, env []string) (exitCode int, launchErr error, unserved error) {
	bound := in.opts.ProbeHandshakeTimeout
	if bound <= 0 {
		bound = DefaultProbeHandshakeTimeout
	}
	handshake := probeHandshakePath(in.planPath)
	// A crashed earlier pass must not satisfy this one: the handshake is
	// per plan path, and the plan is rewritten every pass, but remove a
	// leftover anyway so only this child can answer.
	_ = os.Remove(handshake)

	launchCtx, stopLaunch := context.WithCancel(ctx)
	defer stopLaunch()
	launched := make(chan launchOutcome, 1)
	go func() {
		exit, err := in.launcher(launchCtx, argv, env, in.fx.mut)
		launched <- launchOutcome{exit: exit, err: err}
	}()

	deadline := time.Now().Add(bound)
	for {
		if _, err := os.Stat(handshake); err == nil {
			break
		}
		select {
		case out := <-launched:
			// The child is gone before serving: account for it below
			// exactly as before, exit code and missing results.
			return out.exit, out.err, nil
		case <-time.After(probeHandshakePollInterval):
		}
		if time.Now().After(deadline) {
			break
		}
	}
	if _, err := os.Stat(handshake); err == nil {
		// Served: wait out the child under the proof budget. The launch
		// context is a child of the mode context, so its deadline still
		// bounds this wait; stop it explicitly so a wedged launcher
		// cannot outlive the return.
		select {
		case out := <-launched:
			return out.exit, out.err, nil
		case <-ctx.Done():
			stopLaunch()
			out := <-launched
			return out.exit, out.err, nil
		}
	}
	// Still running past the bound without serving: stop it and fail fast.
	// Nothing ran, so there is nothing to settle and no result to judge.
	stopLaunch()
	<-launched
	return -1, nil, refuse(ReasonProbeUnserved,
		"the probe child emitted no handshake within %s; the executable likely does not serve the confinement probe from main", bound)
}

// toolOps run an external tool; whether the tool ran says nothing about
// whether it got through, so only their observed effect counts.
var toolOps = map[stepOp]bool{
	opReenter: true, opJobSubmit: true, opAppOpen: true, opLookup: true, opAttach: true, opMount: true,
	opPrefWrite: true, opNestedBoundary: true, opNestedRemount: true,
}

func describeResult(result stepResult) string {
	switch {
	case result.Err != "":
		return result.Err
	case result.Exit != 0:
		return fmt.Sprintf("exit %d", result.Exit)
	default:
		return "the operation succeeded"
	}
}

func allPass(outcomes []ProbeOutcome) bool {
	for _, outcome := range outcomes {
		if !outcome.Pass {
			return false
		}
	}
	return len(outcomes) > 0
}
