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
}

// ProbeOutcome is one probe's result in one session mode.
type ProbeOutcome struct {
	ID       string                  `json:"id"`
	Class    string                  `json:"class"`
	Mode     agent.PromptSessionMode `json:"mode"`
	Expected string                  `json:"expected"`
	Observed string                  `json:"observed"`
	Pass     bool                    `json:"pass"`
	Detail   string                  `json:"detail,omitempty"`
}

// The observed and expected outcome names.
const (
	outcomeAccepted = "accepted"
	outcomeRefused  = "refused"
	outcomeMissing  = "no_result"
)

// SelfTestRecord names the backend, its version (implementation plus OS
// build), the probe-set version, each probe's outcome and a digest (D1.2).
type SelfTestRecord struct {
	Backend          BackendName               `json:"backend"`
	BackendVersion   string                    `json:"backendVersion"`
	ProbeSetVersion  string                    `json:"probeSetVersion"`
	ExecutableDigest string                    `json:"executableDigest,omitempty"`
	SessionModes     []agent.PromptSessionMode `json:"sessionModes"`
	Probes           []ProbeOutcome            `json:"probes"`
	Passed           bool                      `json:"passed"`
	TestedAt         time.Time                 `json:"testedAt"`
	Digest           string                    `json:"digest"`
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
// session modes whose spawn path passed every probe.
func (r SelfTestRecord) Attestation() Attestation {
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

// SelfTest proves the backend on this host: it drives a probe process through
// the production spawn binding once per session mode, with every writable
// class, a read-only leaf, protected paths, decoys outside the set and the
// write proxies in reach, and observes what changed. A mode is attested only
// when every probe in it passed. The record is kept for Prepare, passing or
// not; a failure returns the record with a typed error.
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
		return record, err
	}
	launchers := opts.Launchers
	if launchers == nil {
		launchers = DefaultLaunchers()
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
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
		outcomes, err := c.selfTestMode(modeCtx, mode, launcher, opts, filepath.Join(scratch, fmt.Sprintf("m%d", i)), diskImage, diskImageErr)
		cancel()
		if err != nil {
			if _, typed := ReasonOf(err); typed && refusal == nil {
				refusal = err
			}
			record.Probes = append(record.Probes, ProbeOutcome{ID: "mode.setup", Class: "setup", Mode: mode, Expected: outcomeAccepted, Observed: outcomeMissing, Detail: err.Error()})
			continue
		}
		record.Probes = append(record.Probes, outcomes...)
		if allPass(outcomes) {
			record.SessionModes = append(record.SessionModes, mode)
		}
	}
	record.Passed = len(record.SessionModes) == 2 && allPass(record.Probes)
	record.Digest = record.computeDigest()
	c.store(record)
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

func (c *Confiner) selfTestMode(ctx context.Context, mode agent.PromptSessionMode, launcher Launcher, opts SelfTestOptions, dir, diskImage string, diskImageErr error) ([]ProbeOutcome, error) {
	fx, err := newFixture(dir, mode, c.opts.Home, c.opts.StateHome, diskImage, diskImageErr)
	if err != nil {
		return nil, err
	}
	defer fx.cleanup()
	plan, err := c.prepare(fx.spec(mode), "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = plan.Release() }()

	steps := make([]probeStep, 0, len(fx.steps))
	for _, step := range fx.steps {
		steps = append(steps, step.probe)
	}
	planPath := filepath.Join(fx.dir, "plan.json")
	raw, err := json.Marshal(probePlan{ResultPath: probeResultPath(fx.tmp), Steps: steps})
	if err != nil {
		return nil, fmt.Errorf("confinement: encode probe plan: %w", err)
	}
	if err := os.WriteFile(planPath, raw, 0o600); err != nil {
		return nil, fmt.Errorf("confinement: write probe plan: %w", err)
	}
	argv, err := plan.Command(opts.ProbeCommand)
	if err != nil {
		return nil, err
	}
	env := append(plan.Environment(), opts.ProbeEnv...)
	env = append(env, ProbeEnv+"="+planPath)
	exitCode, launchErr := launcher(ctx, argv, env, fx.mut)
	fx.settle()

	results := map[string]stepResult{}
	if raw, err := os.ReadFile(probeResultPath(fx.tmp)); err == nil {
		var decoded []stepResult
		if json.Unmarshal(raw, &decoded) == nil {
			for _, result := range decoded {
				results[result.ID] = result
			}
		}
	}
	outcomes := make([]ProbeOutcome, 0, len(fx.steps))
	for _, step := range fx.steps {
		result, ok := results[step.probe.ID]
		outcome := ProbeOutcome{ID: step.probe.ID, Class: step.class, Mode: mode, Expected: outcomeRefused}
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

// toolOps run an external tool; whether the tool ran says nothing about
// whether it got through, so only their observed effect counts.
var toolOps = map[stepOp]bool{
	opReenter: true, opJobSubmit: true, opAppOpen: true, opLookup: true, opAttach: true, opMount: true,
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
