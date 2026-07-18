// Package daemon implements the long-running rensei-daemon runtime in Go.
//
// The daemon is a single-machine, multi-project supervisor that:
//   - Registers itself with the orchestrator (dial-out) and exchanges a one-time
//     rsp_live_* token for a scoped JWT.
//   - Sends a periodic heartbeat to the orchestrator.
//   - Accepts inbound work specs (sessions) and spawns worker child processes.
//   - Exposes an HTTP control API on 127.0.0.1:7734 for the af / rensei CLI.
//   - Optionally self-updates by drain → fetch → verify → swap → restart.
//
// Architecture reference:
//
//	donmai-architecture/004-sandbox-capability-matrix.md §Local daemon mode
//	donmai-architecture/011-local-daemon-fleet.md
//
// This is the public package surface — downstream binaries can import it
// directly to embed the daemon runtime under their own command tree. The
// afcli package re-exports the runtime as the `daemon run` subcommand.
//
// This package is the Go port of donmai-libraries/packages/daemon/src.
// The TS package @renseiai/daemon is deprecated; final removal is scheduled
// for cycle 6 after the smoke harness has soaked for 7 nights.
package daemon

import (
	"time"

	"github.com/RenseiAI/donmai/runner/access"
)

// Version is the daemon binary version reported in DaemonStatus and in
// the registration payload.
//
// Now a `var` (was `const`) so the binary's main can override it via
// `-ldflags "-X github.com/RenseiAI/donmai/daemon.Version=$VERSION"`
// at build time, OR a downstream embedder (e.g. rensei-tui's daemon
// run command) can pass its own version via `Options.Version` at
// daemon construction. The const form pinned the value to whatever
// donmai's source had at vendor time, which left the
// `rensei-daemon-run` HTTP /api/daemon/status endpoint reporting an
// outdated string forever — confusing operators who saw e.g. `0.7.1`
// even after upgrading both binaries past it.
//
// Default is `"dev"` so an unreleased build (or a vendored copy that
// forgot to inject) is obvious in status output.
var Version = "dev"

// DefaultHTTPPort is the port the daemon's control HTTP server binds to.
// Keep in sync with afclient.DefaultDaemonConfig (port 7734).
const DefaultHTTPPort = 7734

// DefaultHTTPHost is the bind address for the control HTTP server.
const DefaultHTTPHost = "127.0.0.1"

// CapacityRefreshInterval is how often the daemon re-emits its capacity
// snapshot. Mirrors the TS CAPACITY_REFRESH_INTERVAL_MS = 60_000.
const CapacityRefreshInterval = 60 * time.Second

// HeartbeatDefaultInterval is the fallback heartbeat cadence when the
// orchestrator does not return one in RegisterResponse. The TS path uses 30s
// as the fallback; we keep that here, but `15s` is the canonical SLO target.
const HeartbeatDefaultInterval = 30 * time.Second

// ExitCodeRestart is the exit code the daemon uses to signal the supervisor
// "restart requested" after a successful binary swap. The launchd plist /
// systemd unit treats code 3 as a clean restart, not a crash.
const ExitCodeRestart = 3

// ── Lifecycle state ────────────────────────────────────────────────────────

// State is the lifecycle state of a Daemon instance.
type State string

// Lifecycle state constants.
const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StatePaused   State = "paused"
	StateDraining State = "draining"
	StateUpdating State = "updating"
)

// RegistrationStatus is the worker-status string sent to the orchestrator in
// the heartbeat payload. Mirrors the TS DaemonRegistrationStatus.
type RegistrationStatus string

// Registration status constants.
const (
	RegistrationIdle     RegistrationStatus = "idle"
	RegistrationBusy     RegistrationStatus = "busy"
	RegistrationDraining RegistrationStatus = "draining"
)

// ── Session types ──────────────────────────────────────────────────────────

// SessionState is the lifecycle of a single worker child process spawned for
// an accepted session.
type SessionState string

// Session state constants.
const (
	SessionStarting   SessionState = "starting"
	SessionRunning    SessionState = "running"
	SessionCompleted  SessionState = "completed"
	SessionFailed     SessionState = "failed"
	SessionTerminated SessionState = "terminated"
)

// SessionSpec is an inbound work specification dispatched by the orchestrator.
// Subset of SandboxSpec from 004 relevant to the daemon's session-dispatch
// path.
type SessionSpec struct {
	SessionID string `json:"sessionId"`
	// ProjectID is the authoritative project-admission identity. New
	// dispatchers must set it explicitly; repository-only dispatch remains
	// supported for legacy OSS coordinators.
	ProjectID string `json:"projectId,omitempty"`
	// RepositoryID is the stable selected repository-resource identity.
	RepositoryID string `json:"repositoryId,omitempty"`
	Repository   string `json:"repository"`
	// RequiresRepository distinguishes repository-free work from work that
	// must select a repository resource or configured primary.
	RequiresRepository bool              `json:"requiresRepository,omitempty"`
	Ref                string            `json:"ref"`
	Resources          *SessionResources `json:"resources,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	MaxDurationSeconds int               `json:"maxDurationSeconds,omitempty"`
	// ProjectName is the allowlist-resolved ProjectConfig.ID for this session,
	// populated by PollItemToSessionSpec when the work item matches a daemon
	// allowlist entry. Empty when no allowlist entry matched (spec accepted on
	// the fallback path). Embedders can read this in OnPreSpawn to scope
	// per-session credential snapshots without a redundant allowlist lookup.
	ProjectName string `json:"projectName,omitempty"`

	// ── P3 narrow-only gate inputs (ADR-2026-06-06 §5.3) ─────────────────
	//
	// Copied through by PollItemToSessionSpec from the platform-stamped
	// PollWorkItem / ResolvedProfile so the embedder's existing OnPreSpawn
	// closure can read everything access.ResolveMachineCell needs (plus
	// d.Config().ModelAccess) WITHOUT changing the OnPreSpawn signature.
	// The daemon does NOT enforce — enforcement is the rensei-tui S3 gate.
	// All additive + omitempty; every field is absent on a pre-P3 work item
	// (=> the gate sees a nil ceiling / identity and the SessionSpec is
	// byte-identical for the existing fields).

	// PlatformAllowed is the CLOSED set the platform already narrowed
	// (org∩project) — the immutable CEILING the machine gate may only
	// subtract from. Same set the platform stamps; carried faithfully.
	PlatformAllowed []access.AuthMode `json:"platformAllowed,omitempty"`

	// AuthMode is the platform's resolved auth-mode pick (selectAuthMode);
	// the gate honors it iff it survives the machine ∩ ceiling intersection.
	AuthMode string `json:"authMode,omitempty"`

	// WorkType is the workflow discriminant ("development", "qa", "research",
	// "kg-extraction", ...). One input to the embedder's workload derivation.
	WorkType string `json:"workType,omitempty"`

	// Mode is the run-mode discriminant ("" = headless, "interview" =
	// interactive). The other input to workload derivation.
	Mode string `json:"mode,omitempty"`

	// Company is the endpoint company key (e.g. "anthropic") — the matrix
	// company-row key the gate resolves the machine cell against.
	Company string `json:"company,omitempty"`

	// Model is the platform-resolved model id (e.g. "claude-sonnet-4-5") —
	// the most-specific matrix key in the model > company > '*' resolution.
	Model string `json:"model,omitempty"`

	// Workload is the explicit workload key for the per-workload narrowing
	// block (e.g. "kg-extraction"); "" => the Default block. Carried when
	// the platform stamps it explicitly; otherwise the embedder derives it
	// from WorkType/Mode.
	Workload string `json:"workload,omitempty"`
}

// SessionResources is the optional resource request on a SessionSpec.
type SessionResources struct {
	VCpu     int `json:"vCpu,omitempty"`
	MemoryMB int `json:"memoryMb,omitempty"`
}

// SessionHandle is the daemon-side handle for an in-flight session.
//
// Wire shape (camelCase JSON) returned by GET /api/daemon/sessions. The
// list endpoint is the live index of what is running on this host; the
// enrichment fields below (WorktreePath / ProjectName / Repository) make
// it self-sufficient for a local reader (e.g. the host-watch fleet
// dashboard) that wants to locate each session's on-disk worktree
// (and thus its `.agent/events.jsonl` + `.agent/state.json`) WITHOUT a
// per-session GET /api/daemon/sessions/<id> round-trip.
//
// All three enrichment fields are additive + omitempty — a pre-enrichment
// client sees a byte-identical handle for the original four fields, and a
// new client tolerates their absence (empty string). See
// ADR-2026-06-13-daemon-sessionhandle-enrichment (amends
// ADR-2026-05-07-daemon-http-control-api).
type SessionHandle struct {
	SessionID  string       `json:"sessionId"`
	PID        int          `json:"pid"`
	AcceptedAt string       `json:"acceptedAt"`
	State      SessionState `json:"state"`

	// WorktreePath is the absolute on-disk path of the per-session
	// worktree the spawned worker operates in
	// (<WorktreeParentDir>/<sessionID>). A local reader joins this with
	// state.AgentDirName to reach <path>/.agent/events.jsonl and
	// <path>/.agent/state.json. Empty when the daemon cannot resolve the
	// worktree parent (no state dir).
	WorktreePath string `json:"worktreePath,omitempty"`

	// ProjectName is the allowlist-resolved project identifier
	// (ProjectConfig.ID) this session was dispatched under. Mirrors
	// SessionSpec.ProjectName. Empty when no allowlist entry matched.
	ProjectName string `json:"projectName,omitempty"`

	// Repository is the git URL (or owner/name slug) the session operates
	// on, as carried on the inbound SessionSpec. Lets a local reader scope
	// the fleet view to the sessions for one repo (the CWD's repo) without
	// a per-session detail call.
	Repository string `json:"repository,omitempty"`
}

// ── Heartbeat payload ──────────────────────────────────────────────────────

// HeartbeatPayload is the body sent on POST /v1/daemon/heartbeat.
type HeartbeatPayload struct {
	WorkerID       string             `json:"workerId"`
	Hostname       string             `json:"hostname"`
	Status         RegistrationStatus `json:"status"`
	ActiveSessions int                `json:"activeSessions"`
	// ActiveInteractiveSessions is the interactive ("interview" run-mode)
	// subset of ActiveSessions. A *int so nil ("not classified by this
	// embedder") stays distinct from a genuine 0. The corresponding wire key
	// on heartbeatRequestBody is `activeInteractiveCount` (heartbeat.go).
	ActiveInteractiveSessions *int   `json:"activeInteractiveSessions,omitempty"`
	MaxSessions               int    `json:"maxSessions"`
	Region                    string `json:"region,omitempty"`
	SentAt                    string `json:"sentAt"`

	// AllowlistHash is the SHA-256 of the daemon's current project
	// allowlist (see allowlist_report.go). Sent on every beat so the
	// platform can detect drift cheaply. Empty string when the daemon
	// has no projects configured.
	//
	// Phase 1d of 2026-05-18-daemon-config-sync-DESIGN.md.
	AllowlistHash string `json:"allowlistHash,omitempty"`

	// Allowlist is the full structured allowlist payload. Included only
	// when AllowlistHash changes from the platform's last-known value
	// (the daemon caches its previously-reported hash and includes the
	// list only on first beat or on change). Steady-state overhead per
	// beat is the 64-byte hash + ~8 bytes of JSON framing.
	Allowlist []ProjectAllowlistEntry `json:"allowlist,omitempty"`

	// Load carries the per-beat CPU/memory utilisation sample (0–100).
	// Populated from HeartbeatOptions.GetLoad when it returns ok; nil (and
	// thus omitted from the wire body) otherwise. The platform's heartbeat
	// route parses load.{cpu,memory} into worker_hosts.last_cpu_pct /
	// last_mem_pct (item 8). Pointer + omitempty so an absent sample is
	// distinguishable from a genuine {cpu:0,memory:0}.
	Load *heartbeatLoadFields `json:"load,omitempty"`
}

// ── Auto-update channel/schedule ───────────────────────────────────────────

// UpdateChannel is the release channel for the auto-updater.
type UpdateChannel string

// Update channel constants.
const (
	ChannelStable UpdateChannel = "stable"
	ChannelBeta   UpdateChannel = "beta"
	ChannelMain   UpdateChannel = "main"
)

// UpdateSchedule is the cadence the supervisor wakes the daemon to check.
type UpdateSchedule string

// Update schedule constants.
const (
	ScheduleNightly   UpdateSchedule = "nightly"
	ScheduleOnRelease UpdateSchedule = "on-release"
	ScheduleManual    UpdateSchedule = "manual"
)

// CloneStrategy controls how the daemon clones a project repo for new
// workarea pool members.
type CloneStrategy string

// Clone strategy constants.
const (
	CloneShallow   CloneStrategy = "shallow"
	CloneFull      CloneStrategy = "full"
	CloneReference CloneStrategy = "reference-clone"
)
