package depprep

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/internal/kit"
	"github.com/RenseiAI/donmai/internal/semverrange"
)

// ErrInvalidConfig marks a Config that New refuses.
var ErrInvalidConfig = errors.New("depprep: invalid config")

// Step names, as reported in Error.Step and PhaseReport.Name.
const (
	stepVersion = "version"
	stepFetch   = "fetch"
	stepInstall = "install"
)

// Preparer prepares leaves. It holds no per-request state and is safe for
// concurrent use.
type Preparer struct {
	cfg  Config
	exec procRunner
	now  func() time.Time
}

// New validates cfg once per host process.
func New(cfg Config) (*Preparer, error) {
	if cfg.Entries == nil {
		return nil, fmt.Errorf("%w: an entry source is required", ErrInvalidConfig)
	}
	if cfg.Keeper != nil {
		return nil, fmt.Errorf("%w: this build has no dependency keeper; Keeper must be nil", ErrInvalidConfig)
	}
	if cfg.Credentials != nil {
		return nil, fmt.Errorf("%w: this build does not deliver registry credentials; Credentials must be nil", ErrInvalidConfig)
	}
	return &Preparer{cfg: cfg, exec: osRunner{}, now: time.Now}, nil
}

// Prepare installs a leaf's dependencies through the kit entries that select
// it. It never waits for a keeper fill. The context deadline is the whole
// budget; with none, DefaultBudget applies. The returned Report records what
// was attempted, with a nil error and alongside a non-nil one; a non-nil
// error is always a *Error.
func (p *Preparer) Prepare(ctx context.Context, req Request) (*Report, error) {
	start := p.now()
	rep := &Report{
		SubjectKind: req.Subject.Kind,
		Network:     req.Network,
		Record:      InstallRecord{Schema: InstallRecordSchema, SubjectKind: req.Subject.Kind},
	}
	if req.Subject.ID != "" {
		sum := sha256.Sum256([]byte(req.Subject.ID))
		rep.SubjectDigest = hex.EncodeToString(sum[:])
	}
	err := p.prepare(ctx, req, rep)
	rep.Duration = p.now().Sub(start)
	if err != nil {
		rep.Outcome = OutcomeFailed
		return rep, err
	}
	return rep, nil
}

func (p *Preparer) prepare(ctx context.Context, req Request, rep *Report) *Error {
	if err := validateRequest(req); err != nil {
		return &Error{Code: CodeInvalidRequest, Detail: err.Error()}
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = p.now().Add(DefaultBudget)
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	if ctx.Err() != nil {
		return &Error{Code: CodeDeadlineExceeded, Detail: "the budget ran out before preparation started", Err: ctx.Err()}
	}

	entries, err := p.cfg.Entries.Entries(ctx, Leaf{Root: req.Root, OS: hostOS()})
	if err != nil {
		return &Error{Code: CodeEntriesUnavailable, Detail: "entry source failed", Err: err}
	}
	for _, e := range entries {
		if verr := validateEntry(e); verr != nil {
			return &Error{Code: CodeEntriesUnavailable, Manager: e.Manager, Detail: verr.Error()}
		}
	}
	pin := readLeafPin(req.Root)
	groups, gerr := groupEligible(req.Root, entries, pin)
	if gerr != nil {
		rep.MissReason = gerr.MissReason
		return gerr
	}
	if len(groups) == 0 {
		// No entry selects the leaf: run nothing, never fall back to a
		// built-in command list. What that means is the caller's call.
		rep.Outcome = OutcomeNotApplicable
		rep.MissReason = MissNoEntry
		return nil
	}
	if req.ReadOnly {
		for _, g := range groups {
			er := EntryReport{Manager: g.manager, Ecosystem: g.entries[0].Ecosystem, Path: PathSkipped, Network: req.Network}
			if pin.manager == g.manager {
				er.Pin, er.PinVerified = pin.version, pin.verified
			}
			rep.Entries = append(rep.Entries, er)
		}
		rep.Outcome = OutcomeSkipped
		return nil
	}

	dirs, err := newRunDirs(req.Scratch)
	if err != nil {
		return &Error{Code: CodeInvalidRequest, Detail: "scratch is not writable", Err: err}
	}
	a := &attempt{p: p, req: req, deadline: deadline, dirs: dirs, base: baseEnv(req, dirs), path: pathDirs(req.Toolchain)}

	var failure *Error
	var installed []string
	for i, g := range groups {
		if failure != nil {
			rep.Entries = append(rep.Entries, EntryReport{Manager: g.manager, Ecosystem: g.entries[0].Ecosystem, Path: PathSkipped, Network: req.Network})
			continue
		}
		er, entry, ferr := a.prepareGroup(ctx, i, g, pin)
		rep.Entries = append(rep.Entries, er)
		installed = append(installed, entry.Installed...)
		if ferr != nil {
			failure = ferr
			continue
		}
		rep.Record.Entries = append(rep.Record.Entries, InstallRecordEntry{
			Manager: er.Manager, Version: er.Version,
			KitID: er.KitID, KitDigestKind: er.KitDigestKind, KitDigest: er.KitDigest,
			Exposure: er.Exposure, Path: er.Path, MissReason: er.MissReason,
			LockfileDigest: lockfileDigest(req.Root, leafHolds(req.Root, entry.Lockfiles)),
			InstalledPaths: er.InstalledPaths, PackageCode: er.PackageCode,
		})
	}
	rep.Leaf = a.leafCheck(ctx, installed)
	if failure != nil {
		return failure
	}
	rep.Outcome = OutcomeReady
	return nil
}

// attempt is one Prepare's state once it has entries to run.
type attempt struct {
	p        *Preparer
	req      Request
	deadline time.Time
	dirs     *runDirs
	base     envSet
	path     []string

	// The leaf's git status before the first manager process, taken
	// lazily so a refusal that runs nothing runs no git either.
	leafTaken bool
	leafOK    bool
	before    leafStatus
}

// snapshotLeaf records the leaf's status once, before the first process.
func (a *attempt) snapshotLeaf(ctx context.Context) {
	if a.leafTaken {
		return
	}
	a.leafTaken = true
	a.before, a.leafOK = gitStatus(ctx, a.p.exec, a.req.Root, a.base, a.path)
}

// leafCheck compares the leaf after the install with the snapshot (ADR D4
// rule 5). It gets its own short bound out of the reserve, which exists for
// exactly this kind of cleanup.
func (a *attempt) leafCheck(ctx context.Context, installed []string) LeafCheck {
	if !a.leafTaken {
		return LeafCheck{}
	}
	if !a.leafOK {
		return LeafCheck{Status: LeafCheckUnavailable}
	}
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), Reserve/3)
	defer cancel()
	after, ok := gitStatus(checkCtx, a.p.exec, a.req.Root, a.base, a.path)
	if !ok {
		return LeafCheck{Status: LeafCheckUnavailable}
	}
	check := LeafCheck{Status: LeafCheckClean, Undeclared: undeclaredPaths(a.before, after, installed)}
	if len(check.Undeclared) > 0 {
		check.Status = LeafCheckDirty
	}
	return check
}

// prepareGroup selects one entry of a manager group and runs the
// keeper-absent online path for it: fetch, then offline install.
func (a *attempt) prepareGroup(ctx context.Context, index int, g managerGroup, pin leafPin) (EntryReport, Entry, *Error) {
	p, req, deadline, dirs, base, path := a.p, a.req, a.deadline, a.dirs, a.base, a.path
	er := EntryReport{
		Manager:   g.manager,
		Ecosystem: g.entries[0].Ecosystem,
		Exposure:  ExposureSessionOnly,
		Path:      PathFailed,
		Network:   req.Network,
	}
	pinned := pin.manager == g.manager && pin.version != ""
	if pin.manager == g.manager {
		er.Pin, er.PinVerified = pin.version, pin.verified
	}
	// entry is the selected entry once selection succeeds. A failure after
	// that still returns it, so the leaf check knows its installed paths.
	var entry Entry
	fail := func(e *Error) (EntryReport, Entry, *Error) {
		if e.Manager == "" {
			e.Manager = g.manager
		}
		if er.MissReason == "" {
			er.MissReason = e.MissReason
		}
		return er, entry, e
	}

	stepCtx, cancel := context.WithDeadline(ctx, deadline.Add(-Reserve))
	defer cancel()

	// Pick the entry. A pinned leaf selects by its pin without running
	// anything: obtaining a pinned manager is a download, which belongs to
	// the online step. An unpinned leaf asks the toolchain's manager.
	ranVersion := false
	switch {
	case pinned:
		selected, serr := g.selectByVersion(pin.version)
		if serr != nil {
			return fail(serr)
		}
		entry = selected
	case g.needsVersionToSelect():
		probe := g.entries[0]
		if probe.VersionCommand == "" {
			return fail(&Error{
				Code: CodeToolchainUnavailable, Step: stepVersion,
				Detail: "entries select by version but declare no version command",
			})
		}
		version, verr := a.runVersion(stepCtx, probe, base, &er)
		if verr != nil {
			return fail(verr)
		}
		ranVersion = true
		selected, serr := g.selectByVersion(version)
		if serr != nil {
			return fail(serr)
		}
		entry = selected
	default:
		entry = g.entries[0]
	}
	er.Versions = entry.Versions
	er.KitID, er.KitDigestKind, er.KitDigest = entry.KitID, entry.KitDigestKind, entry.KitDigest

	// Package code never runs unconfined (ADR D4 rule 4).
	if p.cfg.Confiner == nil && entry.NoCode == nil {
		er.MissReason = MissConfinementUnavailable
		return fail(&Error{
			Code: CodeEntriesUnavailable, MissReason: MissConfinementUnavailable,
			Detail: "the entry declares no no_code settings and no confiner is configured",
		})
	}
	// Without a keeper there is no snapshot and no store: every offline
	// path misses.
	if req.Network == NetworkDeny {
		er.MissReason = MissStoreMissOffline
		return fail(&Error{
			Code: CodeNetworkDenied, MissReason: MissStoreMissOffline,
			Detail: "no offline path without a keeper",
		})
	}
	fetch, _ := parseCommand(entry.Commands.Fetch)
	install, _ := parseCommand(entry.Commands.Install)
	programs := []string{fetch.program, install.program}
	if entry.VersionCommand != "" && !ranVersion {
		vc, _ := parseCommand(entry.VersionCommand)
		programs = append(programs, vc.program)
	}
	for _, prog := range programs {
		if _, ok := lookProgram(prog, path); !ok {
			return fail(&Error{
				Code:   CodeToolchainUnavailable,
				Detail: fmt.Sprintf("%s is in no toolchain directory", prog),
			})
		}
	}
	// The online floor: do not start the online step with less remaining.
	if remaining := deadline.Sub(p.now()); remaining < OnlineFloor || ctx.Err() != nil {
		return fail(&Error{
			Code:   CodeDeadlineExceeded,
			Detail: fmt.Sprintf("%s remaining is under the %s online floor", remaining.Truncate(time.Second), OnlineFloor),
		})
	}

	if !pinned && entry.VersionCommand != "" && !ranVersion {
		if _, verr := a.runVersion(stepCtx, entry, base, &er); verr != nil {
			return fail(verr)
		}
	}

	store, err := storeEnv(entry, index, dirs)
	if err != nil {
		return fail(&Error{Code: CodeInvalidRequest, Detail: "scratch is not writable", Err: err})
	}
	noCode := map[string]string(nil)
	if entry.NoCode != nil {
		noCode = entry.NoCode.Env
	}

	// Phase 1: fetch. No package code, no repository hooks. The only phase
	// that may ever see registry credentials.
	fetchEnv := base.with(store, entry.ImportEnv, noCode, fetch.assign)
	if ferr := a.runPhase(stepCtx, stepFetch, fetch, fetchEnv, nil, &er); ferr != nil {
		return fail(ferr)
	}

	// A pinned manager has now provisioned itself; record what it runs and
	// hold it to the selected range.
	if pinned && entry.VersionCommand != "" {
		version, verr := a.runVersion(stepCtx, entry, base.with(store), &er)
		if verr != nil {
			return fail(verr)
		}
		if entry.Versions != "" {
			if r, rerr := semverrange.ParseRange(entry.Versions); rerr == nil {
				v, perr := semverrange.ParseVersion(version)
				if perr != nil || !r.Contains(v) {
					er.MissReason = MissToolchainMismatch
					return fail(&Error{
						Code: CodeToolchainMismatch, MissReason: MissToolchainMismatch,
						Detail: "the provisioned manager version is outside the entry's versions range",
					})
				}
			}
		}
	} else if pinned {
		er.Version = pin.version
	}

	// Phase 2: offline install from the per-run cache. No credentials.
	// Unconfined, package code stays off.
	installLayers := []map[string]string{store, entry.ImportEnv}
	if p.cfg.Confiner == nil {
		installLayers = append(installLayers, noCode)
		er.PackageCode = PackageCodeSkipped
	} else {
		er.PackageCode = PackageCodeConfined
	}
	installLayers = append(installLayers, install.assign)
	installEnv := base.with(installLayers...)
	writable := []string{req.Root, dirs.root}
	if ierr := a.runPhase(stepCtx, stepInstall, install, installEnv, writable, &er); ierr != nil {
		return fail(ierr)
	}

	er.Path = PathOnline
	er.MissReason = ""
	er.InstalledPaths = leafHolds(req.Root, entry.Installed)
	return er, entry, nil
}

// runVersion runs an entry's version command with package code off and
// returns the version it printed.
func (a *attempt) runVersion(ctx context.Context, e Entry, env envSet, er *EntryReport) (string, *Error) {
	vc, err := parseCommand(e.VersionCommand)
	if err != nil {
		return "", &Error{Code: CodeEntriesUnavailable, Step: stepVersion, Detail: err.Error()}
	}
	noCode := map[string]string(nil)
	if e.NoCode != nil {
		noCode = e.NoCode.Env
	}
	res, perr := a.runProcess(ctx, stepVersion, vc, env.with(e.ImportEnv, noCode, vc.assign), nil, er)
	if perr != nil {
		if perr.Code == CodeInstallFailed {
			perr.Code = CodeToolchainUnavailable
			perr.Detail = "the version command failed"
		}
		return "", perr
	}
	version := reportedVersion(res.output)
	if version == "" {
		return "", &Error{Code: CodeToolchainUnavailable, Step: stepVersion, Detail: "the version command printed nothing"}
	}
	er.Version = version
	return version, nil
}

// reportedVersion extracts the first semantic version a version command
// printed, or its trimmed first line when there is none.
func reportedVersion(out []byte) string {
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	for _, field := range strings.Fields(line) {
		candidate := strings.TrimPrefix(strings.TrimPrefix(field, "go"), "v")
		if v, err := semverrange.ParseVersion(candidate); err == nil {
			return v.String()
		}
	}
	if len(line) > 128 {
		line = line[:128]
	}
	return line
}

// runPhase runs one fetch or install process.
func (a *attempt) runPhase(ctx context.Context, step string, c command, env envSet, writable []string, er *EntryReport) *Error {
	_, err := a.runProcess(ctx, step, c, env, writable, er)
	return err
}

// runProcess resolves, optionally confines, runs and records one process.
// writable non-nil marks the install phase, the only one a Confiner wraps.
func (a *attempt) runProcess(ctx context.Context, step string, c command, env envSet, writable []string, er *EntryReport) (procResult, *Error) {
	p, dir := a.p, a.req.Root
	program, ok := lookProgram(c.program, a.path)
	if !ok {
		return procResult{}, &Error{
			Code: CodeToolchainUnavailable, Step: step,
			Detail: fmt.Sprintf("%s is in no toolchain directory", c.program),
		}
	}
	if ctx.Err() != nil {
		return procResult{}, &Error{Code: CodeDeadlineExceeded, Step: step, Err: ctx.Err()}
	}
	pr := proc{argv: append([]string{program}, c.args...), env: env.list(), dir: dir}
	if writable != nil && p.cfg.Confiner != nil {
		confined, err := p.cfg.Confiner.Confine(ctx, ConfineRequest{Argv: pr.argv, Env: pr.env, Dir: dir, Writable: writable})
		if err != nil {
			return procResult{}, &Error{Code: CodeInstallFailed, Step: step, Detail: "confinement refused the install", Err: err}
		}
		if confined.Release != nil {
			defer confined.Release()
		}
		pr.argv, pr.env = confined.Argv, confined.Env
	}
	a.snapshotLeaf(ctx)
	started := p.now()
	res, err := p.exec.run(ctx, pr)
	er.Phases = append(er.Phases, PhaseReport{Name: step, Duration: p.now().Sub(started), ExitCode: res.exit})
	switch {
	case ctx.Err() != nil:
		er.Diagnostics = string(res.output)
		return res, &Error{Code: CodeDeadlineExceeded, Step: step, Err: ctx.Err()}
	case err != nil:
		er.Diagnostics = string(res.output)
		return res, &Error{Code: CodeInstallFailed, Step: step, Detail: "the process did not run", Err: err}
	case res.exit != 0:
		er.Diagnostics = string(res.output)
		return res, &Error{Code: CodeInstallFailed, Step: step, Detail: fmt.Sprintf("exit status %d", res.exit)}
	}
	return res, nil
}

// hostOS is the kit platform key of the running host.
func hostOS() string {
	switch runtime.GOOS {
	case "darwin":
		return kit.OSMacOS
	case "windows":
		return kit.OSWindows
	default:
		return kit.OSLinux
	}
}
