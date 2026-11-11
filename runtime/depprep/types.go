package depprep

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Time bounds (ADR D3). These are defaults; the context deadline always wins.
const (
	// DefaultBudget is the whole budget when the context has no deadline.
	DefaultBudget = 5 * time.Minute
	// SnapshotReconcileBound bounds a snapshot restore's reconcile. It
	// applies only with a keeper.
	SnapshotReconcileBound = 30 * time.Second
	// OfflineStoreInstallBound bounds an offline install from a store view
	// before falling through to online. It applies only with a keeper.
	OfflineStoreInstallBound = 90 * time.Second
	// OnlineFloor is the least remaining budget with which the online step
	// starts. With less, Prepare returns deadline_exceeded.
	OnlineFloor = 60 * time.Second
	// Reserve is kept back from every step for killing, cleanup and the
	// report.
	Reserve = 15 * time.Second
)

// Config is fixed once per host process.
type Config struct {
	// Entries supplies the composed kit entries for a leaf. Required.
	Entries EntrySource
	// Scope resolves a subject's credential scope. Nil: one host scope
	// (ADR-2026-10-08 D3). This build consults it only together with a
	// keeper or a credential callback, neither of which it accepts yet.
	Scope ScopeFunc
	// Credentials supplies registry credentials for the fetch phase. Nil:
	// no registry credentials, ever. This build does not deliver
	// credentials; New refuses a non-nil callback.
	Credentials CredentialFunc
	// Keeper is the dependency keeper. Nil: no keeper; exposure is
	// session-only. This build has no keeper; New refuses a non-nil one.
	Keeper Keeper
	// Confiner runs the offline install under the host's confinement
	// backend. Nil: install-time package code never runs (ADR D4).
	Confiner Confiner
}

// Request is one preparation.
type Request struct {
	// Root is the absolute path of the mutable leaf, a clean checkout.
	Root string
	// Subject is who the install is for.
	Subject Subject
	// Toolchain are fixed absolute directories. PATH is these, then
	// /usr/bin:/bin.
	Toolchain []string
	// Scratch is a caller-owned, per-invocation absolute directory outside
	// Root. HOME, TMPDIR, the XDG directories and the per-run caches go
	// under it. The caller discards it.
	Scratch string
	// Network is NetworkAllow or NetworkDeny. It has no default: the zero
	// value is an invalid request.
	Network NetworkPolicy
	// ProxyEnv holds optional explicit proxy settings (HTTP_PROXY,
	// HTTPS_PROXY, NO_PROXY, ALL_PROXY, either case). Nothing is inherited.
	ProxyEnv map[string]string
	// ReadOnly reports the leaf as skipped and installs nothing.
	ReadOnly bool
}

// SubjectKind distinguishes an agent session from a non-agent invocation.
type SubjectKind string

// The subject kinds.
const (
	SubjectSession    SubjectKind = "session"
	SubjectInvocation SubjectKind = "invocation"
)

// Subject is who an install is for (ADR D2).
type Subject struct {
	Kind SubjectKind
	// ID is opaque. It is reported only as a digest.
	ID string
	// Attrs are opaque. They are passed to the ScopeFunc, never logged,
	// reported or emitted.
	Attrs map[string]string
}

// LogValue keeps a subject's id and attributes out of structured logs.
func (s Subject) LogValue() slog.Value {
	return slog.GroupValue(slog.String("kind", string(s.Kind)))
}

// NetworkPolicy is the network posture of a request.
type NetworkPolicy string

// The network policies.
const (
	// NetworkAllow permits the online step.
	NetworkAllow NetworkPolicy = "allow"
	// NetworkDeny allows only snapshot and store paths. Without a keeper,
	// every entry misses with store_miss_offline.
	NetworkDeny NetworkPolicy = "deny"
)

// ScopeFunc resolves a subject's dependency credential scope: an opaque,
// secret-free string derived from what the subject was granted, never from
// the subject kind. An error or an empty scope is unresolved, which gives
// session-only exposure, never an error.
type ScopeFunc func(ctx context.Context, s Subject) (scope string, err error)

// CredentialFunc returns registry credentials for one scope and manager. They
// reach only the fetch phase, as a per-run mode-0600 manager configuration
// file under Scratch named through the entry's credentials.config_env, and
// are removed afterwards.
type CredentialFunc func(ctx context.Context, scope, manager string) (Registry, error)

// Registry is the credential material for one fetch: the content of the
// manager configuration file the fetch phase reads (for the npm family, user
// configuration lines). Its formatting methods never print the content.
type Registry struct {
	Config []byte
}

// String redacts the configuration.
func (r Registry) String() string {
	if len(r.Config) == 0 {
		return "Registry{}"
	}
	return "Registry{[redacted]}"
}

// GoString redacts the configuration.
func (r Registry) GoString() string { return r.String() }

// Format redacts the configuration under every verb.
func (r Registry) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(r.String())) }

// LogValue redacts the configuration.
func (r Registry) LogValue() slog.Value { return slog.StringValue(r.String()) }

// Keeper is the dependency keeper of ADR-2026-10-08 as Prepare sees it:
// snapshot restore, offline install through the scope's store view, and a
// background fill on a miss. Its methods are defined by the keeper slices,
// which plug in behind this field without changing Prepare's signature,
// codes or fallback order. Until then the interface is sealed and
// Config.Keeper must be nil.
type Keeper interface {
	sealedKeeper()
}

// Confiner runs one install-phase process under the host's executor
// confinement backend (ADR D4 rule 3): the writable set is exactly
// ConfineRequest.Writable, there is no network where the backend can deny
// it, and there are no credentials. Which lifecycle scripts run is the
// repository's own manager configuration; the confiner neither widens nor
// narrows it.
type Confiner interface {
	Confine(ctx context.Context, req ConfineRequest) (ConfinedCommand, error)
}

// ConfineRequest describes the process to confine.
type ConfineRequest struct {
	// Argv is the process to run. Argv[0] is an absolute path.
	Argv []string
	// Env is the constructed environment, KEY=VALUE.
	Env []string
	// Dir is the working directory: the leaf.
	Dir string
	// Writable is the writable set: the leaf, the per-run cache and the
	// scratch directories.
	Writable []string
}

// ConfinedCommand is what Prepare runs instead of the request. Prepare still
// runs it in its own process group under the step deadline.
type ConfinedCommand struct {
	Argv []string
	Env  []string
	// Release, when set, is called once the process has exited.
	Release func()
}

// Outcome is the overall result of a Prepare.
type Outcome string

// The outcomes.
const (
	OutcomeReady         Outcome = "ready"
	OutcomeNotApplicable Outcome = "not_applicable"
	OutcomeSkipped       Outcome = "skipped"
	OutcomeFailed        Outcome = "failed"
)

// Path is the path one entry's install took.
type Path string

// The paths.
const (
	PathSnapshot Path = "snapshot"
	PathStore    Path = "store"
	PathOnline   Path = "online"
	PathSkipped  Path = "skipped"
	PathFailed   Path = "failed"
)

// Exposure is how a store reaches the install (ADR-2026-10-08 D5).
type Exposure string

// The exposures. Without a keeper only ExposureSessionOnly occurs.
const (
	ExposureOverlay     Exposure = "overlay"
	ExposureSeeded      Exposure = "seeded"
	ExposureView        Exposure = "view"
	ExposureProxy       Exposure = "proxy"
	ExposureSessionOnly Exposure = "session-only"
)

// MissReason is the closed miss reason (ADR-2026-10-08 D10, extended by
// ADR D8). An unknown value is malformed.
type MissReason string

// The miss reasons.
const (
	MissNoEntry                MissReason = "no_entry"
	MissManagerAmbiguous       MissReason = "manager_ambiguous"
	MissScopeUnresolved        MissReason = "scope_unresolved"
	MissKeeperDisabled         MissReason = "keeper_disabled"
	MissExposureUnavailable    MissReason = "exposure_unavailable"
	MissNoSnapshot             MissReason = "no_snapshot"
	MissStorePathUnstable      MissReason = "store_path_unstable"
	MissReconcileFailed        MissReason = "reconcile_failed"
	MissNotCovered             MissReason = "not_covered"
	MissFetchRequiresBuild     MissReason = "fetch_requires_build"
	MissStoreMissOffline       MissReason = "store_miss_offline"
	MissBudgetExhausted        MissReason = "budget_exhausted"
	MissRequiresRepositoryCode MissReason = "requires_repository_code"
	MissConfinementUnavailable MissReason = "confinement_unavailable"
	MissToolchainMismatch      MissReason = "toolchain_mismatch"
)

// PackageCode records whether install-time package code could run.
type PackageCode string

// The package-code postures.
const (
	// PackageCodeSkipped: every phase ran with the entry's no_code settings.
	PackageCodeSkipped PackageCode = "skipped"
	// PackageCodeConfined: the offline install ran under the Confiner with
	// the repository's own script allowlist.
	PackageCodeConfined PackageCode = "confined"
)

// Report is what Prepare attempted. It is returned with a nil error and
// alongside a non-nil one, and carries no credential, scope value or subject
// attribute.
type Report struct {
	Outcome Outcome
	// MissReason is set when the outcome has one (not_applicable: no_entry).
	MissReason  MissReason
	SubjectKind SubjectKind
	// SubjectDigest is the hex sha256 of the subject id, when there is one.
	SubjectDigest string
	Network       NetworkPolicy
	Entries       []EntryReport
	// Leaf is the leaf hygiene check (ADR D4 rule 5).
	Leaf LeafCheck
	// Record is the secret-free install record. Prepare writes it nowhere:
	// for an invocation it lives only here (ADR D4 rule 5); a session
	// caller writes it into its root's reserved metadata.
	Record   InstallRecord
	Duration time.Duration
}

// EntryReport is one selected entry's result.
type EntryReport struct {
	Manager   string
	Ecosystem string
	// Version is the manager version the version command reported, or the
	// leaf's pin when the command was not run.
	Version string
	// Pin is the leaf's declared manager version, when it declares one.
	Pin string
	// PinVerified reports that the pin carries a hash the manager checks.
	// A pin without one is recorded as unverified, not refused.
	PinVerified   bool
	Versions      string
	KitID         string
	KitDigestKind string
	KitDigest     string
	Exposure      Exposure
	Path          Path
	MissReason    MissReason
	Phases        []PhaseReport
	// InstalledPaths are the snapshot.installed paths present afterwards.
	InstalledPaths []string
	PackageCode    PackageCode
	Network        NetworkPolicy
	// Diagnostics is a bounded tail of the failing step's output.
	Diagnostics string
}

// PhaseReport is one process Prepare ran.
type PhaseReport struct {
	// Name is "version", "fetch" or "install".
	Name     string
	Duration time.Duration
	ExitCode int
}

// LeafCheckStatus says whether the leaf hygiene check ran.
type LeafCheckStatus string

// The leaf check statuses.
const (
	LeafCheckNotRun      LeafCheckStatus = ""
	LeafCheckClean       LeafCheckStatus = "clean"
	LeafCheckDirty       LeafCheckStatus = "dirty"
	LeafCheckUnavailable LeafCheckStatus = "unavailable"
)

// LeafCheck reports the paths the install created in the leaf. Every one
// must be inside an entry's snapshot.installed and ignored by git; any other
// is named in Undeclared, which a caller's write check also sees.
type LeafCheck struct {
	Status     LeafCheckStatus
	Undeclared []string
}

// InstallRecordSchema identifies the install record's shape.
const InstallRecordSchema = "donmai.dev/dependency-install-record/v1"

// InstallRecord is the secret-free install record (ADR-2026-10-08 D6.3).
type InstallRecord struct {
	Schema      string
	SubjectKind SubjectKind
	Entries     []InstallRecordEntry
}

// InstallRecordEntry is one entry's record.
type InstallRecordEntry struct {
	Manager       string
	Version       string
	KitID         string
	KitDigestKind string
	KitDigest     string
	Exposure      Exposure
	Path          Path
	MissReason    MissReason
	// LockfileDigest is the hex sha256 over the entry's present lockfiles,
	// by path.
	LockfileDigest string
	InstalledPaths []string
	PackageCode    PackageCode
}
