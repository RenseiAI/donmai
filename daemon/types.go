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
//	rensei-architecture/004-sandbox-capability-matrix.md §Local daemon mode
//	rensei-architecture/011-local-daemon-fleet.md
//
// This is the public package surface — downstream binaries can import it
// directly to embed the daemon runtime under their own command tree. The
// afcli package re-exports the runtime as the `daemon run` subcommand.
//
// This package is the Go port of agentfactory/packages/daemon/src (REN-1408).
// The TS package @renseiai/daemon is deprecated; final removal is scheduled
// for cycle 6 after the smoke harness has soaked for 7 nights.
package daemon

import "time"

// Version is the daemon binary version. Kept in sync with agentfactory-tui's
// release tags. Reported in DaemonStatus and in the registration payload.
//
// 0.4.0-dev: in-tree daemon now talks to the real platform endpoints
// (REN-1422 — POST /api/workers/register + POST /api/workers/<id>/heartbeat
// with runtime JWT). Replaces the 0.3.10-sidecar bash heartbeat shim that
// shipped for the 2026-05-01 demo.
//
// 0.5.5: stage-driven SDLC Phase 2 daemon side (REN-1485 / REN-1487):
// new QueuedWork stage fields (StagePrompt, StageID, StageBudget,
// StageLifecycle, StageSourceEventID) decoded + forwarded onto
// SessionDetail; sub-agent budget enforcement via runner.BudgetEnforcer
// (max-duration / max-sub-agents / max-tokens with WORK_RESULT
// budget-exceeded surfacing); REN-1481 runtime-token refresh probe
// (probes /api/workers/<id>/refresh-token before falling back to a
// full re-register so the workerId stays stable when the platform
// side ships the companion handler).
//
// 0.7.1: Wave 12 — kit lifecycle honest end-to-end. Sigstore bundle-
// mode verifier (sigstore-go) on /api/daemon/kits/<id>/verify-signature
// replaces the always-KitTrustUnsigned stub. Git-source kit install
// (go-git/v5) on /api/daemon/kits/<id>/install replaces the 501 stub.
// Trust modes (permissive / signed-by-allowlist / attested) configurable
// via daemon.yaml; trustOverride: "allowed-this-once" audit-logged
// per REN-1314.
const Version = "0.7.1"

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
	SessionID          string            `json:"sessionId"`
	Repository         string            `json:"repository"`
	Ref                string            `json:"ref"`
	Resources          *SessionResources `json:"resources,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	MaxDurationSeconds int               `json:"maxDurationSeconds,omitempty"`
}

// SessionResources is the optional resource request on a SessionSpec.
type SessionResources struct {
	VCpu     int `json:"vCpu,omitempty"`
	MemoryMB int `json:"memoryMb,omitempty"`
}

// SessionHandle is the daemon-side handle for an in-flight session.
type SessionHandle struct {
	SessionID  string       `json:"sessionId"`
	PID        int          `json:"pid"`
	AcceptedAt string       `json:"acceptedAt"`
	State      SessionState `json:"state"`
}

// ── Heartbeat payload ──────────────────────────────────────────────────────

// HeartbeatPayload is the body sent on POST /v1/daemon/heartbeat.
type HeartbeatPayload struct {
	WorkerID       string             `json:"workerId"`
	Hostname       string             `json:"hostname"`
	Status         RegistrationStatus `json:"status"`
	ActiveSessions int                `json:"activeSessions"`
	MaxSessions    int                `json:"maxSessions"`
	Region         string             `json:"region,omitempty"`
	SentAt         string             `json:"sentAt"`
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
