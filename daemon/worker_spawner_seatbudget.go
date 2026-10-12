package daemon

import (
	"fmt"
	"log/slog"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/daemon/seatbudget"
	"github.com/RenseiAI/donmai/sessionshim"
)

// SeatBudget is the spawner-visible per-seat resource budget: the resolved
// share one session's process tree may use. It mirrors seatbudget.Budget
// without importing the enforcement package into the spawner options'
// public shape more than once — the daemon converts at the boundary.
type SeatBudget struct {
	// CPUs is the whole-core seat share.
	CPUs int
	// MemoryMB is the seat memory ceiling in mebibytes. Zero means no cap.
	MemoryMB int
	// IOWeight is the cgroup v2 IO weight. Zero means backend default.
	IOWeight int
	// Mode is auto, enforced, best-effort or none. Empty reads as none
	// (budgeting off).
	Mode string
}

// toSeatBudget converts spawner options into the enforcement budget.
func (b SeatBudget) toSeatBudget() seatbudget.Budget {
	return seatbudget.Budget{
		CPUs:     b.CPUs,
		MemoryMB: b.MemoryMB,
		IOWeight: b.IOWeight,
		Mode:     seatbudget.Mode(b.Mode),
	}
}

// seatBudgetForSpawn resolves the effective seat budget for one spawn: the
// spawner's configured budget with its mode resolved for this OS. ok=false
// means budgeting is off — the seat spawns exactly as before. The share
// lives behind its own RWMutex (not the spawn mutex) because SetSeatBudget
// swaps it from the config watcher while AcceptWork/spawn run unlocked;
// an unlocked struct read there races the write under -race.
func (s *WorkerSpawner) seatBudgetForSpawn() (seatbudget.Budget, bool) {
	s.seatBudgetMu.RLock()
	defer s.seatBudgetMu.RUnlock()
	return s.seatBudgetForSpawnLocked()
}

// seatBudgetForSpawnLocked is seatBudgetForSpawn for callers that hold the
// seat-budget lock (read or write). Numeric limits clamp at the boundary:
// the config path validates, but an embedder composing a spawner directly
// bypasses it — a negative memory cap or IO weight must read as uncapped
// (the renderers already drop non-positive values) rather than travel
// into scope argv or reports as a negative number.
func (s *WorkerSpawner) seatBudgetForSpawnLocked() (seatbudget.Budget, bool) {
	b := s.seatBudget.toSeatBudget()
	if b.Disabled() {
		return seatbudget.Budget{}, false
	}
	if b.CPUs < 1 {
		b.CPUs = 1
	}
	if b.MemoryMB < 0 {
		b.MemoryMB = 0
	}
	if b.IOWeight < 0 {
		b.IOWeight = 0
	}
	return b, true
}

// applySeatBudgetToEnv overlays the seat's cooperative worker caps onto an
// already-composed seat environment. On Linux the hard limits come from the
// cgroup placement (applySeatBudgetToCmd); the env caps ride along so tools
// size their fan-out to the share instead of discovering the quota by
// hitting it. On macOS the env caps ARE the budget. Base-operator values
// win: ComposeEnv never overrides an explicitly set knob.
func applySeatBudgetToEnv(env []string, b seatbudget.Budget, ok bool) []string {
	if !ok {
		return env
	}
	return seatbudget.ComposeEnv(env, b.CPUs)
}

// applySeatBudgetToCmd wraps the seat command in its Linux cgroup placement.
// The wrap applies when the seat ASKED for hard enforcement (mode enforced
// or auto on Linux) and the systemd placement is available. "auto" on a
// non-Linux host, or an explicit best-effort mode, never wraps: the seat
// runs on cooperative caps and the report says best-effort. A seat that
// asked for enforcement but has no backend runs unwrapped and the report
// says none with the missing backend named — the spawn must not fail for
// want of a hierarchy, and the report must never claim a confinement the
// seat did not get. name scopes the transient unit per session. userScope
// selects the systemd bus (--user for a per-user daemon service).
//
// NOTE: this is the DIRECT-child lane only. Shim launches never come here:
// they wrap through seatBudgetShimCommandForLaunch, which scopes EVERY shim
// launch (budget or not) in a unit named for the launch incarnation and
// refuses to launch without a backend. The direct lane keeps its budget-only
// wrap, named by ScopeName from the session id alone.
func applySeatBudgetToCmd(command []string, sessionID string, b seatbudget.Budget, ok bool, placement seatbudget.Placement) []string {
	return applySeatBudgetToCmdForBus(command, sessionID, b, ok, placement, false)
}

// applySeatBudgetToCmdForBus is applySeatBudgetToCmd parametrised by the
// systemd bus. Production resolves userScope from the live bus probe;
// tests pin both spellings on any host.
func applySeatBudgetToCmdForBus(command []string, sessionID string, b seatbudget.Budget, ok bool, placement seatbudget.Placement, userScope bool) []string {
	return applySeatBudgetToCmdForBusGOOS(command, sessionID, b, ok, placement, userScope, runtime.GOOS)
}

// applySeatBudgetToCmdForBusGOOS is applySeatBudgetToCmdForBus parametrised
// by GOOS: the default `auto` mode confines on Linux and degrades elsewhere,
// so the wrap decision differs per OS. Production passes runtime.GOOS;
// tests pin both platforms on any host (the auto-mode wrap is the
// documented default and must stay pinned where it confines).
func applySeatBudgetToCmdForBusGOOS(command []string, sessionID string, b seatbudget.Budget, ok bool, placement seatbudget.Placement, userScope bool, goos string) []string {
	if !ok || len(command) == 0 {
		return command
	}
	if !wantsEnforcementForGOOS(b, goos) {
		return command
	}
	if placement != seatbudget.PlacementSystemd {
		return command
	}
	prefix := seatbudget.SystemdScopeArgsForBus(seatbudget.ScopeName(sessionID), b, userScope)
	return append(prefix, command...)
}

// wantsEnforcementForGOOS reports whether the seat asked for hard OS
// enforcement: explicit enforced mode anywhere, or auto on Linux. Kept
// beside the wrap (not on Budget) because it answers the spawner's
// question — "wrap this command?" — while EffectiveMode answers the
// reporter's ("what did the seat get?"). The GOOS parameter lets tests
// pin the default-mode matrix on any host: `auto` is the documented
// default and resolveSeatBudget stores it verbatim, so the branch that
// confines default-configured Linux seats (wrap) versus degrades them
// (best-effort report) needs its own pin — EffectiveModeFor does not
// answer the spawner's wrap question. Production passes runtime.GOOS.
func wantsEnforcementForGOOS(b seatbudget.Budget, goos string) bool {
	if b.Mode == seatbudget.ModeEnforced {
		return true
	}
	return b.Mode == "auto" && goos == "linux"
}

// sessionSeatBudgetReport builds the per-session handle evidence for one
// spawn: the posture the seat actually runs under with the values. A
// disabled budget reports mode "none". A seat that asked for enforcement
// reports enforced ONLY for the systemd wrap — the one placement that
// confines the seat. The quota (not core pinning) is the binding CPU
// limit: identical AllowedCPUs ranges across seats shared 2 cores of 6
// budgeted, and user services never get the cpuset controller delegated,
// so the scope carries quota+weight and the report says quota. A systemd-less Linux host (cgroupfs placement) runs
// the seat unconfined: the launcher performs no direct cgroupfs placement,
// so the report is mode "none" with the missing backend named, never
// enforced. With no backend at all the report is likewise none. Best-effort
// covers explicit best-effort plus auto off Linux.
func sessionSeatBudgetReport(b seatbudget.Budget, ok bool, placement seatbudget.Placement) *SessionSeatBudget {
	return sessionSeatBudgetReportForGOOS(b, ok, placement, runtime.GOOS)
}

// sessionSeatBudgetReportForGOOS is sessionSeatBudgetReport parametrised by
// GOOS: the default `auto` mode reports enforced on Linux (where it wraps)
// and best-effort elsewhere. Production passes runtime.GOOS; tests pin the
// wrap/report agreement on any host — a wrap without an enforced report
// (or vice versa) is the false-confinement shape this package exists to
// prevent.
func sessionSeatBudgetReportForGOOS(b seatbudget.Budget, ok bool, placement seatbudget.Placement, goos string) *SessionSeatBudget {
	if !ok {
		return &SessionSeatBudget{Mode: string(seatbudget.ModeNone)}
	}
	if wantsEnforcementForGOOS(b, goos) {
		detail := "cpus " + seatbudget.CPUSet(b.CPUs) + ", quota " + seatbudget.CPUQuotaPercent(b.CPUs)
		if mem := seatbudget.MemoryBytes(b.MemoryMB); mem != "" {
			detail += ", memory " + mem + " bytes"
		}
		if placement == seatbudget.PlacementSystemd {
			return &SessionSeatBudget{
				Mode:     string(seatbudget.ModeEnforced),
				CPUs:     b.CPUs,
				MemoryMB: b.MemoryMB,
				Detail:   detail + " via transient systemd scope",
			}
		}
		return &SessionSeatBudget{Mode: string(seatbudget.ModeNone), Detail: seatBudgetNoBackendDetailFor(placement)}
	}
	return &SessionSeatBudget{
		Mode:     string(seatbudget.ModeBestEffort),
		CPUs:     b.CPUs,
		MemoryMB: b.MemoryMB,
		Detail:   seatbudget.DescribeBestEffort(b.CPUs, len(seatbudget.WorkerCapKeys())),
	}
}

// shimSeatBudget resolves the spawner's budget for the shim launch path:
// the same effective budget the direct path wraps at spawn. ok=false means
// budgeting is off — the shim still launches inside its own scope (see
// seatBudgetShimCommandForLaunch), just without limit properties. d may be
// nil in tests that construct a spawner without a daemon.
func (d *Daemon) shimSeatBudget() (seatbudget.Budget, bool) {
	if d == nil || d.spawner == nil {
		return seatbudget.Budget{}, false
	}
	return d.spawner.seatBudgetForSpawn()
}

// seatBudgetShimScopeName derives the transient scope unit name for one shim
// launch: a fixed-length digest of the lifecycle identity plus the launch's
// process epoch. Two launches of one session (original and relaunch after a
// restart) are different scopes; two sessions sharing a session-id prefix
// never alias. The epoch suffix keeps the incarnation greppable after the
// digest.
func seatBudgetShimScopeName(orgID, sessionID string, processEpoch uint64) string {
	return seatbudget.ScopeNameForIncarnation(orgID, sessionID, processEpoch)
}

// seatBudgetShimCommandForLaunch wraps the worker command for one shim launch
// in its own transient scope named scope. On Linux every shim launch wraps,
// budget or not: the scope is what keeps the seat out of the daemon unit's
// control-group kill. The scope carries the budget's limits only when the
// budget asks for hard enforcement — the same rule the direct lane applies
// (wantsEnforcementForGOOS) — so a best-effort budget runs on its cooperative
// worker caps inside a limit-free scope, exactly as its report says. ok=false
// refuses the launch: Linux without the systemd placement, where an unscoped
// shim would die with its daemon. Off Linux no scope exists and the bare
// command runs. goos is injected so tests pin the platform gate on any host.
func seatBudgetShimCommandForLaunch(command []string, scope string, b seatbudget.Budget, ok bool, placement seatbudget.Placement, userScope bool, goos string) ([]string, bool) {
	if goos != "linux" {
		return command, true
	}
	if placement != seatbudget.PlacementSystemd {
		return nil, false
	}
	limits := seatbudget.Budget{}
	if ok && wantsEnforcementForGOOS(b, goos) {
		limits = b
	}
	prefix := seatbudget.SystemdScopeArgsForBus(scope, limits, userScope)
	return append(prefix, command...), true
}

// shimSeatLaunchRecord states what one shim launch gives its seat: the scope
// unit (Linux only; empty where no scope exists), and the posture and limits
// the direct lane would report for the same budget on the same placement —
// enforced only where the scope carries the limits, best-effort for the
// cooperative caps, none when budgeting is off. The launching daemon writes it
// beside the discovery record before the shim starts, so an adopting daemon
// can fall back to it when the live scope cannot be read.
func shimSeatLaunchRecord(id sessionshim.Identity, processEpoch uint64, scope string, b seatbudget.Budget, ok bool, goos string, now time.Time) sessionshim.SeatLaunch {
	placement := seatbudget.PlacementNone
	if scope != "" {
		placement = seatbudget.PlacementSystemd
	}
	rep := sessionSeatBudgetReportForGOOS(b, ok, placement, goos)
	ioWeight := 0
	if rep.Mode == string(seatbudget.ModeEnforced) && b.IOWeight > 0 {
		ioWeight = b.IOWeight
	}
	return sessionshim.NewSeatLaunch(id, processEpoch, scope, rep.Mode, rep.CPUs, rep.MemoryMB, ioWeight, now)
}

// shimScopeProbe is the host facts the shim launch gate and the seat
// read-back consult. The zero value is production: runtime.GOOS, the live
// placement (presence plus creation-permission) and bus probes, the
// probe's own refusal line, and the systemctl read-back. A test sets the
// fields it needs on its own daemon, so suites that run in parallel never
// share a stub.
type shimScopeProbe struct {
	goos        string
	placement   func() seatbudget.Placement
	userScope   func() bool
	scopeReason func() string
	readLimits  func(scope string, userScope bool) (seatbudget.SeatLimits, bool)
	readBatch   func(scopes map[string]string, userScope bool) map[string]seatbudget.SeatLimits
	stopScope   func(scope string, userScope bool) error
	ownBusClass func() string
}

func (p shimScopeProbe) hostGOOS() string {
	if p.goos != "" {
		return p.goos
	}
	return runtime.GOOS
}

func (p shimScopeProbe) hostPlacement() seatbudget.Placement {
	if p.placement != nil {
		return p.placement()
	}
	return seatBudgetLaunchPlacement()
}

func (p shimScopeProbe) hostUserScope() bool {
	if p.userScope != nil {
		return p.userScope()
	}
	return seatBudgetLaunchUserScope()
}

// hostScopeReason reports why the scope placement is unavailable: the
// manager's own refusal line when the probe was denied, empty when the
// backend is merely absent. The launch gate folds it into the refusal so
// the accept error names the reason instead of only the placement.
func (p shimScopeProbe) hostScopeReason() string {
	if p.scopeReason != nil {
		return p.scopeReason()
	}
	return seatbudget.ScopeRefusalReason()
}

func (p shimScopeProbe) readSeatLimits(scope string, userScope bool) (seatbudget.SeatLimits, bool) {
	if p.readLimits != nil {
		return p.readLimits(scope, userScope)
	}
	return seatbudget.ReadSeatLimits(scope, userScope)
}

// readSeatLimitsBatch reads many seats' limits with one systemctl call. A
// nil readBatch (older tests that stub only readLimits) falls back to one
// read per seat through readSeatLimits, so every existing stub keeps
// working; production always sets readBatch.
func (p shimScopeProbe) readSeatLimitsBatch(scopes map[string]string, userScope bool) map[string]seatbudget.SeatLimits {
	if p.readBatch != nil {
		return p.readBatch(scopes, userScope)
	}
	out := make(map[string]seatbudget.SeatLimits, len(scopes))
	for key, scope := range scopes {
		if limits, ok := p.readSeatLimits(scope, userScope); ok {
			out[key] = limits
		}
	}
	return out
}

// stopSeatScope releases one finished seat's transient scope. The zero value
// is production; a test that stubs the stop asserts on the disposed unit.
func (p shimScopeProbe) stopSeatScope(scope string, userScope bool) error {
	if p.stopScope != nil {
		return p.stopScope(scope, userScope)
	}
	return seatbudget.StopSeatScope(scope, userScope)
}

// hostOwnBusClass reports which manager owns this daemon's own cgroup: the
// user manager or the system manager. The zero value reads the live path;
// a test pins it without touching /proc.
func (p shimScopeProbe) hostOwnBusClass() string {
	if p.ownBusClass != nil {
		return p.ownBusClass()
	}
	return seatbudget.DetectOwnBusCgroupClass(seatbudget.SelfCgroupPath)
}

// checkShimScopeBus is the pre-spawn bus half of the launch gate: it picks
// the bus from the manager owning this daemon's own cgroup and refuses the
// launch when that manager cannot host a scope. A system-installed daemon
// with no user manager reads class "system": the scope is created on the
// system bus, and the launch is refused only when that bus is unusable.
// A daemon under a per-user manager reads class "user" and needs the user
// bus. An unreadable cgroup ("unknown") keeps the cached placement
// verdict: the probe already answered, and an unknown own-cgroup must not
// overrule it.
func (d *Daemon) checkShimScopeBus(placement seatbudget.Placement) error {
	if placement != seatbudget.PlacementSystemd {
		return nil
	}
	wantUserBus := d.shimScope.hostUserScope()
	if d.shimScope.hostGOOS() != "linux" {
		return nil
	}
	class := d.shimScope.hostOwnBusClass()
	switch class {
	case "user":
		if !wantUserBus {
			return fmt.Errorf("session shim: this daemon runs under a per-user manager but its scope would be created on the system bus: refusing the launch before spawn")
		}
	case "system":
		if wantUserBus {
			return fmt.Errorf("session shim: this daemon runs as a system install with no user manager: the user bus is unreachable, refusing the launch before spawn")
		}
	}
	return nil
}

// shimSeatReportKey names one launch incarnation: the lifecycle identity, the
// process epoch, and the shim id (random per shim process, so a relaunch at
// the same epoch never reads its predecessor's cached report).
type shimSeatReportKey struct {
	id     sessionshim.Identity
	epoch  uint64
	shimID string
}

// shimSeatReportCache holds one report per launch incarnation. Adoption never
// changes a running seat's limits, so a seat is read back once rather than
// on every status poll — the read execs systemctl, which must not run per
// poll, and never under the shim-state lock. The zero value is ready to use.
type shimSeatReportCache struct {
	mu      sync.Mutex
	reports map[shimSeatReportKey]SessionSeatBudget
}

func (c *shimSeatReportCache) get(key shimSeatReportKey) (SessionSeatBudget, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rep, ok := c.reports[key]
	return rep, ok
}

func (c *shimSeatReportCache) put(key shimSeatReportKey, rep SessionSeatBudget) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reports == nil {
		c.reports = make(map[shimSeatReportKey]SessionSeatBudget)
	}
	c.reports[key] = rep
}

// retain drops every cached report whose incarnation is no longer adopted or
// quarantined, so the cache never outgrows the live set.
func (c *shimSeatReportCache) retain(live map[shimSeatReportKey]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.reports {
		if _, ok := live[key]; !ok {
			delete(c.reports, key)
		}
	}
}

// shimSeatReport is the handle evidence for one adopted or quarantined
// shim-owned seat, read once per launch incarnation and cached (see
// shimSeatReportCache).
func (d *Daemon) shimSeatReport(registry *sessionshim.Registry, key shimSeatReportKey) *SessionSeatBudget {
	if rep, ok := d.shimSeatReports.get(key); ok {
		return &rep
	}
	rep := d.readShimSeatReport(registry, key.id, key.epoch)
	d.shimSeatReports.put(key, *rep)
	return rep
}

// shimSeatReportsBatch is the batched twin of shimSeatReport: every
// uncached seat in keys is read with one systemctl call, then cached per
// incarnation exactly as the single path caches. Cached seats never reach
// the bus. liveOK maps a key to whether its scope was observed; the caller
// threads it into the record fallback the same way readShimSeatReport
// does for a single seat.
func (d *Daemon) shimSeatReportsBatch(registry *sessionshim.Registry, keys []shimSeatReportKey) map[shimSeatReportKey]*SessionSeatBudget {
	out := make(map[shimSeatReportKey]*SessionSeatBudget, len(keys))
	var scopes map[string]string
	var order []shimSeatReportKey
	for _, key := range keys {
		if rep, ok := d.shimSeatReports.get(key); ok {
			cached := rep
			out[key] = &cached
			continue
		}
		scope := d.shimSeatScopeForReport(registry, key.id, key.epoch)
		if scope == "" {
			rep := d.readShimSeatReport(registry, key.id, key.epoch)
			d.shimSeatReports.put(key, *rep)
			out[key] = rep
			continue
		}
		if scopes == nil {
			scopes = make(map[string]string)
		}
		tag := string(key.id.OrgID) + "\x1f" + string(key.id.SessionID) + "\x1f" + strconv.FormatUint(key.epoch, 10) + "\x1f" + key.shimID
		scopes[tag] = scope
		order = append(order, key)
	}
	var live map[string]seatbudget.SeatLimits
	if len(scopes) > 0 {
		live = d.shimScope.readSeatLimitsBatch(scopes, d.shimScope.hostUserScope())
	}
	for _, key := range order {
		tag := string(key.id.OrgID) + "\x1f" + string(key.id.SessionID) + "\x1f" + strconv.FormatUint(key.epoch, 10) + "\x1f" + key.shimID
		limits, ok := live[tag]
		rep := d.reportShimSeatFromLive(registry, key.id, key.epoch, limits, ok)
		d.shimSeatReports.put(key, *rep)
		out[key] = rep
	}
	return out
}

// shimSeatScopeForReport resolves the only scope the read-back may query
// for one seat — the unit the launch identity names — without reading it.
// Empty means no scope exists to read (invalid identity), and the caller
// falls back to the record path that reports none.
func (d *Daemon) shimSeatScopeForReport(registry *sessionshim.Registry, id sessionshim.Identity, epoch uint64) string {
	derived := ""
	if id.Validate() == nil {
		derived = seatBudgetShimScopeName(id.OrgID, id.SessionID, epoch)
	}
	if derived == "" {
		return ""
	}
	if registry != nil {
		if rec, found, err := registry.SeatLaunch(id, epoch); err == nil && found && rec.Scope != "" && rec.Scope != derived {
			return derived
		}
	}
	return derived
}

// reportShimSeatFromLive builds one seat's handle evidence from an observed
// (or unobserved) scope plus its launch record: the same rule
// readShimSeatReport applies to a single seat, with the live outcome
// supplied by the batched call instead of a per-seat exec.
func (d *Daemon) reportShimSeatFromLive(registry *sessionshim.Registry, id sessionshim.Identity, epoch uint64, live seatbudget.SeatLimits, liveOK bool) *SessionSeatBudget {
	derived := ""
	if id.Validate() == nil {
		derived = seatBudgetShimScopeName(id.OrgID, id.SessionID, epoch)
	}
	var seat seatbudget.LaunchedSeat
	if registry != nil {
		rec, found, err := registry.SeatLaunch(id, epoch)
		if err != nil {
			slog.Warn("session shim: seat launch record unreadable; reporting from the live scope only",
				"session", id.String(), "error", err)
		}
		if found && rec.Scope != "" && rec.Scope != derived {
			slog.Warn("session shim: seat launch record names a scope its launch identity does not; reporting from the live scope only",
				"session", id.String(), "recorded_scope", rec.Scope, "scope", derived)
			found = false
		}
		if found {
			seat = seatbudget.LaunchedSeat{
				Scope:    rec.Scope,
				Mode:     seatbudget.Mode(rec.Mode),
				CPUs:     rec.CPUs,
				MemoryMB: rec.MemoryMB,
				IOWeight: rec.IOWeight,
			}
		}
	}
	scope := seat.Scope
	if scope == "" && seat.Mode == "" {
		scope = derived
	}
	if liveOK {
		seat.Scope = scope
	}
	rep := seatbudget.ReportSeatLimits(seat, live, liveOK)
	return &SessionSeatBudget{Mode: string(rep.Mode), CPUs: rep.CPUs, MemoryMB: rep.MemoryMB, Detail: rep.Detail}
}

// readShimSeatReport builds one shim-owned seat's handle evidence: the limits
// read back from its live scope, falling back to the launch record the
// launching daemon wrote beside the discovery record, and never this daemon's
// current configuration, which may have changed since the launch. Without a
// launch record (a seat launched before records existed, or a registry that
// cannot be read) the scope name is still derived from the launch identity,
// so a seat this release launched reads back even if its record was lost.
//
// The only scope ever read is the one the launch identity names. The launching
// daemon always records exactly that scope, so a record naming any other unit
// is corrupt: it is set aside, never followed, because a well-formed name of a
// different seat's scope would attribute that seat's limits to this one.
func (d *Daemon) readShimSeatReport(registry *sessionshim.Registry, id sessionshim.Identity, epoch uint64) *SessionSeatBudget {
	derived := ""
	if id.Validate() == nil {
		derived = seatBudgetShimScopeName(id.OrgID, id.SessionID, epoch)
	}
	var seat seatbudget.LaunchedSeat
	if registry != nil {
		rec, found, err := registry.SeatLaunch(id, epoch)
		if err != nil {
			slog.Warn("session shim: seat launch record unreadable; reporting from the live scope only",
				"session", id.String(), "error", err)
		}
		if found && rec.Scope != "" && rec.Scope != derived {
			slog.Warn("session shim: seat launch record names a scope its launch identity does not; reporting from the live scope only",
				"session", id.String(), "recorded_scope", rec.Scope, "scope", derived)
			found = false
		}
		if found {
			seat = seatbudget.LaunchedSeat{
				Scope:    rec.Scope,
				Mode:     seatbudget.Mode(rec.Mode),
				CPUs:     rec.CPUs,
				MemoryMB: rec.MemoryMB,
				IOWeight: rec.IOWeight,
			}
		}
	}
	scope := seat.Scope
	if scope == "" && seat.Mode == "" {
		scope = derived
	}
	var live seatbudget.SeatLimits
	liveOK := false
	if scope != "" {
		live, liveOK = d.shimScope.readSeatLimits(scope, d.shimScope.hostUserScope())
	}
	if liveOK {
		seat.Scope = scope
	}
	rep := seatbudget.ReportSeatLimits(seat, live, liveOK)
	return &SessionSeatBudget{Mode: string(rep.Mode), CPUs: rep.CPUs, MemoryMB: rep.MemoryMB, Detail: rep.Detail}
}

// wrapSeatCommandForTest is the production entry point's seam: tests drive
// AcceptWork (the real spawn path) and assert on the wrapped command via
// WorkerCommand capture, not this helper directly.
//
// seatBudgetLaunchPlacement and seatBudgetLaunchUserScope resolve the Linux
// enforcement placement for a fresh direct spawn, and are the production
// default behind shimScopeProbe for a shim launch. They default to the live
// probes; a test that overrides them must restore the defaults (no parallel
// use while stubbed) — a shim test sets its daemon's shimScope instead.
var seatBudgetLaunchPlacement = seatbudget.HostPlacement

var seatBudgetLaunchUserScope = seatbudget.UserScopeForBus
