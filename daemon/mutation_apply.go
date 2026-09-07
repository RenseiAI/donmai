package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/codesurvival"
	"github.com/RenseiAI/donmai/internal/interview"
	"github.com/RenseiAI/donmai/kgextract"
	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/runner/access"
)

// applyMutations is the daemon-side handler for the heartbeat response's
// pendingMutations[]. Each mutation either succeeds (id ends up in applied)
// or fails (id + error end up in failures). Both go back in the next
// heartbeat as ACK so the platform can resolve the daemon_mutations rows.
//
// Phase 2c of 2026-05-18-daemon-config-sync-DESIGN.md.
//
// Concurrency: the heartbeat goroutine is the only caller, so within a
// single beat we process mutations strictly serially. We still take the
// daemon mu when reading/writing config and the spawner mu via SetProjects
// — internal lock discipline matches the rest of the daemon.
//
// Atomicity: each yaml write goes through daemon/config.go's WriteConfig
// which does write-temp + os.Rename. If the rename succeeds we update
// in-memory config and spawner. If the rename fails we leave both alone
// and ACK the mutation as failed — the platform will see the error and
// the daemon's yaml stays consistent.
//
// Idempotency: project.add on an id already present is a no-op success.
// project.remove on an id not present is a no-op success. The platform
// re-queues mutations on the next beat if it doesn't see an ACK, so
// this idempotency lets the daemon's failure modes (crash between apply
// and ACK send) self-heal without operator intervention.

// applyMutationsLock guards against re-entrant apply calls — the
// heartbeat goroutine is the sole caller in production but a future
// debug command (or test) could trigger one in parallel. Mutating
// daemon.yaml twice concurrently would race os.Rename.
var applyMutationsLock sync.Mutex

// applyPendingMutations is the OnPendingMutations callback the daemon
// wires into HeartbeatService. It is invoked once per heartbeat that
// carries any pending mutations and returns the lists the service
// then buffers for the next outbound beat.
func (d *Daemon) applyPendingMutations(_ context.Context, mutations []PendingMutation) (
	applied []string,
	failures []HeartbeatMutationFailure,
) {
	if len(mutations) == 0 {
		return nil, nil
	}
	applyMutationsLock.Lock()
	defer applyMutationsLock.Unlock()

	for _, m := range mutations {
		if err := d.applyOneMutation(m); err != nil {
			slog.Warn("[daemon-sync] mutation failed",
				"id", m.ID, "op", m.Op, "err", err.Error())
			failures = append(failures, HeartbeatMutationFailure{
				ID:    m.ID,
				Error: err.Error(),
			})
			continue
		}
		slog.Info("[daemon-sync] mutation applied",
			"id", m.ID, "op", m.Op)
		applied = append(applied, m.ID)
	}
	return applied, failures
}

func (d *Daemon) applyOneMutation(m PendingMutation) error {
	if isSessionMutationOp(m.Op) {
		return d.applySessionMutation(m)
	}
	// pool.deleted is handled before the config lock is taken: it touches no
	// yaml and it writes the host-status snapshot through setLastHostStatus,
	// which takes d.mu itself (Go mutexes are not reentrant).
	if m.Op == "pool.deleted" {
		return d.applyPoolDeleted(m)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.config == nil {
		return fmt.Errorf("daemon config not loaded")
	}

	switch m.Op {
	case "project.enable":
		return d.applyProjectEnableLocked(m)
	case "project.disable":
		return d.applyProjectDisableLocked(m)
	case "project.add":
		return d.applyProjectAddLocked(m)
	case "project.remove":
		return d.applyProjectRemoveLocked(m)
	case "modelAccess.set":
		return d.applyModelAccessSetLocked(m)
	case "modelAccess.clear":
		return d.applyModelAccessClearLocked(m)
	default:
		// Unknown op — newer platform than daemon. ACK as failed so the
		// platform can stop re-queueing.
		return fmt.Errorf("unsupported mutation op %q (upgrade daemon?)", m.Op)
	}
}

// ApplySessionMutations applies only runtime session mutations and ignores
// config mutations. Downstream embedders with additional worker identities can
// use it as HeartbeatOptions.OnPendingMutations while keeping config ownership
// with each identity's own config pipeline.
func (d *Daemon) ApplySessionMutations(_ context.Context, mutations []PendingMutation) (
	applied []string,
	failures []HeartbeatMutationFailure,
) {
	for _, m := range mutations {
		if !isSessionMutationOp(m.Op) {
			continue
		}
		if err := d.applySessionMutation(m); err != nil {
			failures = append(failures, HeartbeatMutationFailure{ID: m.ID, Error: err.Error()})
			continue
		}
		applied = append(applied, m.ID)
	}
	return applied, failures
}

// isSessionMutationOp reports whether op is a runtime SESSION mutation — one
// that acts on a live session rather than on daemon config.
//
// Single-sourcing the set matters because it is consumed twice, by
// applyOneMutation and by ApplySessionMutations, and those two had already
// drifted apart once by construction: a new session verb added to the first
// would have been silently ignored by the second, leaving embedders that own
// their own config pipeline unable to remediate a session at all.
func isSessionMutationOp(op string) bool {
	switch op {
	case "session.kill", "session.wake", "session.restart-harness":
		return true
	default:
		return false
	}
}

// applySessionMutation dispatches the runtime session verbs.
//
// session.kill terminates. session.wake and session.restart-harness are the
// bounded, escalating remediation for a wedged seat and are the opposite of a
// kill: they retain the shim, the identity and the worktree, and write only
// content-free keystrokes (mutation_session_wake.go).
func (d *Daemon) applySessionMutation(m PendingMutation) error {
	switch m.Op {
	case "session.kill":
		return d.applySessionKill(m)
	case "session.wake":
		return d.applySessionWake(m)
	case "session.restart-harness":
		return d.applySessionRestartHarness(m)
	default:
		return fmt.Errorf("unsupported session mutation op %q", m.Op)
	}
}

func (d *Daemon) applySessionKill(m PendingMutation) error {
	var params struct {
		SessionID string `json:"sessionId"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(m.Params, &params); err != nil {
		return fmt.Errorf("session.kill decode params: %w", err)
	}
	params.SessionID = strings.TrimSpace(params.SessionID)
	if params.SessionID == "" {
		return fmt.Errorf("session.kill requires sessionId")
	}
	if d.spawner == nil {
		return fmt.Errorf("session.kill: worker spawner is not initialized")
	}
	if err := d.spawner.ForceKillSession(params.SessionID); err != nil {
		return fmt.Errorf("session.kill: %w", err)
	}
	return nil
}

// applyPoolDeleted handles the `pool.deleted` mutation: the capacity pool this
// host was bound to has been deleted, and the op carries the pool id, a
// human-readable reason, and the ids of other pools the host could be bound to
// instead.
//
// What the daemon does: record the state (so the claim gate suspends new
// claims immediately, without waiting for the next beat's hostStatus to say
// the same thing), log it once with the candidates for the operator, and ACK
// applied. In-flight sessions are untouched — a deleted pool means "take on
// nothing new", never "abandon what you are running".
//
// What the daemon deliberately does NOT do: re-bind itself to one of the
// candidate pools. Pool membership is assigned server-side; the registration
// wire (RegisterRequest) has no pool-id field, so there is no request this
// daemon could send that would move it to another pool. Re-binding would mean
// inventing a client-side flow the server does not implement — exactly the
// half-working client the layering rules forbid (`011-local-daemon-fleet.md`
// keeps pool membership on the control-plane side of the daemon contract, and
// its drain semantics already establish the correct local response to "this
// host may not take new work": stop claiming, finish what is running).
// The candidates are therefore reported, not acted on: an operator re-binds
// the host, and the next heartbeat's hostStatus flips back to ok, which
// resumes claiming automatically.
//
// ACKing applied (rather than failed) is the point of the change: the op is
// understood and fully handled locally, and a failed ACK would leave the
// control plane re-queueing a mutation forever.
func (d *Daemon) applyPoolDeleted(m PendingMutation) error {
	var params struct {
		PoolID           string   `json:"poolId"`
		CandidatePoolIDs []string `json:"candidatePoolIds"`
		Reason           string   `json:"reason"`
	}
	if len(m.Params) > 0 {
		if err := json.Unmarshal(m.Params, &params); err != nil {
			return fmt.Errorf("pool.deleted decode params: %w", err)
		}
	}
	params.PoolID = strings.TrimSpace(params.PoolID)
	if params.PoolID == "" {
		return fmt.Errorf("pool.deleted requires poolId")
	}
	candidates := make([]string, 0, len(params.CandidatePoolIDs))
	for _, id := range params.CandidatePoolIDs {
		if id = strings.TrimSpace(id); id != "" {
			candidates = append(candidates, id)
		}
	}
	action := strings.TrimSpace(params.Reason)
	if action == "" {
		action = "This pool was deleted. Re-bind this host to another pool to resume claiming work."
	}
	// Same shape the heartbeat's hostStatus carries, so one snapshot serves
	// both the claim gate and the operator-facing status surface.
	d.setLastHostStatus(HostStatusDetail{
		Status:            "pool_deleted",
		RecommendedAction: action,
		CandidatePoolIDs:  candidates,
	})
	slog.Warn("[daemon-sync] capacity pool deleted — suspending new-work claims",
		"poolId", params.PoolID,
		"candidatePoolIds", strings.Join(candidates, ","),
		"reason", action,
		"inFlightSessions", "unaffected")
	return nil
}

func (d *Daemon) applyProjectAddLocked(m PendingMutation) error {
	var params struct {
		ID         string `json:"id"`
		Repository string `json:"repository"`
	}
	if err := json.Unmarshal(m.Params, &params); err != nil {
		return fmt.Errorf("decode params: %w", err)
	}
	if params.ID == "" || params.Repository == "" {
		return fmt.Errorf("project.add requires id + repository")
	}
	migrated := migrateProjectAdmissionV2(d.config)

	// Legacy compatibility: project.add creates one repository resource and
	// enables its project. Multiple repositories may share the same project ID.
	repositoryExists := false
	for _, repository := range d.config.Repositories {
		if repository.ProjectID == params.ID && normalizeRepositorySource(repository.Source) == normalizeRepositorySource(params.Repository) {
			repositoryExists = true
			break
		}
	}
	if !repositoryExists {
		d.config.Repositories = append(d.config.Repositories, RepositoryConfig{
			ID:        legacyRepositoryID(params.ID, params.Repository),
			ProjectID: params.ID,
			Source:    params.Repository,
		})
	}
	enabled := enableProjectID(&d.config.EnabledProjectIDs, params.ID)
	if repositoryExists && !enabled && !migrated {
		return nil
	}
	return d.persistProjectAdmissionAndRefreshLocked(migrated)
}

func (d *Daemon) applyProjectRemoveLocked(m PendingMutation) error {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(m.Params, &params); err != nil {
		return fmt.Errorf("decode params: %w", err)
	}
	if params.ID == "" {
		return fmt.Errorf("project.remove requires id")
	}
	migrated := migrateProjectAdmissionV2(d.config)

	// Legacy compatibility: project.remove removes every repository resource
	// for the ID and disables admission. New callers should use
	// project.disable when repository configuration must be retained.
	original := d.config.Repositories
	filtered := original[:0]
	removed := false
	for _, repository := range original {
		if repository.ProjectID == params.ID {
			removed = true
			continue
		}
		filtered = append(filtered, repository)
	}
	disabled := disableProjectID(&d.config.EnabledProjectIDs, params.ID)
	if !removed && !disabled && !migrated {
		// Idempotent — id not present, treat as success.
		return nil
	}
	// Build a fresh slice so we don't accidentally share backing storage
	// with the original (defensive against a later mutation racing the
	// spawner snapshot via SetProjects).
	d.config.Repositories = append([]RepositoryConfig(nil), filtered...)
	return d.persistProjectAdmissionAndRefreshLocked(migrated)
}

func (d *Daemon) applyProjectEnableLocked(m PendingMutation) error {
	id, err := mutationProjectID(m)
	if err != nil {
		return fmt.Errorf("project.enable: %w", err)
	}
	migrated := migrateProjectAdmissionV2(d.config)
	if !enableProjectID(&d.config.EnabledProjectIDs, id) && !migrated {
		return nil
	}
	return d.persistProjectAdmissionAndRefreshLocked(migrated)
}

func (d *Daemon) applyProjectDisableLocked(m PendingMutation) error {
	id, err := mutationProjectID(m)
	if err != nil {
		return fmt.Errorf("project.disable: %w", err)
	}
	migrated := migrateProjectAdmissionV2(d.config)
	if !disableProjectID(&d.config.EnabledProjectIDs, id) && !migrated {
		return nil
	}
	return d.persistProjectAdmissionAndRefreshLocked(migrated)
}

func mutationProjectID(m PendingMutation) (string, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(m.Params, &params); err != nil {
		return "", fmt.Errorf("decode params: %w", err)
	}
	params.ID = strings.TrimSpace(params.ID)
	if params.ID == "" {
		return "", fmt.Errorf("requires id")
	}
	return params.ID, nil
}

func enableProjectID(ids *[]string, id string) bool {
	for _, existing := range *ids {
		if existing == id {
			return false
		}
	}
	*ids = normalizeProjectIDs(append(*ids, id))
	return true
}

func disableProjectID(ids *[]string, id string) bool {
	filtered := make([]string, 0, len(*ids))
	removed := false
	for _, existing := range *ids {
		if existing == id {
			removed = true
			continue
		}
		filtered = append(filtered, existing)
	}
	*ids = filtered
	return removed
}

// knownWorkloadKey reports whether a modelAccess.set workload key belongs to
// the shared workload vocabulary: the agent work types (runner.AllWorkTypes),
// the non-agent batch work types (code-survival-scan, kg-extraction), and the
// mode-derived "interview" workload. The embedder derives the enforcement-time
// lookup key from exactly these sources (explicit Workload | WorkType | Mode),
// so a key outside the vocabulary can never match a session — the strict block
// the operator thought they configured would be stored verbatim and silently
// fall back to the Default block / platform ceiling at enforcement time.
func knownWorkloadKey(key string) bool {
	if runner.IsKnownWorkType(key) {
		return true
	}
	switch key {
	case codesurvival.WorkTypeCodeSurvivalScan,
		kgextract.WorkTypeKGExtraction,
		interview.InterviewRunMode:
		return true
	}
	return false
}

// knownWorkloadKeys lists the full workload vocabulary for error messages.
func knownWorkloadKeys() []string {
	keys := append([]string(nil), runner.AllWorkTypes...)
	return append(keys,
		codesurvival.WorkTypeCodeSurvivalScan,
		kgextract.WorkTypeKGExtraction,
		interview.InterviewRunMode,
	)
}

// applyModelAccessSetLocked writes (or overwrites) a per-machine /
// per-workload model-access policy block into Config.ModelAccess
// (P3 / ADR-2026-06-06 §4.2). It decodes {workload?, policy}:
//   - workload == "" => write into .Default
//   - workload != "" => write into .Workloads[workload]
//
// POLICY ONLY: only the AccessPolicy ({matrix, authOrder?}) crosses this
// channel — never credentials (keys ride the separate snapshot socket,
// §4.4). This step is pure plumbing: it STORES the block; enforcement
// (ResolveMachineCell) stays in the downstream gate. Idempotent: a
// repeated set with the same params overwrites to the same value.
// Caller must hold d.mu (applyOneMutation acquires it).
func (d *Daemon) applyModelAccessSetLocked(m PendingMutation) error {
	var params struct {
		Workload string              `json:"workload"`
		Policy   access.AccessPolicy `json:"policy"`
	}
	if err := json.Unmarshal(m.Params, &params); err != nil {
		return fmt.Errorf("decode params: %w", err)
	}
	// Reject (NACK) workload keys outside the shared vocabulary instead of
	// storing them: a typo'd key never matches at enforcement time, so the
	// "strict" block would silently fall back to the ceiling. Failing the
	// mutation surfaces the typo platform-side via the failure ACK.
	// modelAccess.clear stays unvalidated so legacy bad keys remain removable.
	if params.Workload != "" && !knownWorkloadKey(params.Workload) {
		return fmt.Errorf("unknown workload %q: not in the shared workload vocabulary (known keys: %s)",
			params.Workload, strings.Join(knownWorkloadKeys(), ", "))
	}

	if d.config.ModelAccess == nil {
		d.config.ModelAccess = &access.ModelAccessConfig{}
	}
	if params.Workload == "" {
		d.config.ModelAccess.Default = params.Policy
	} else {
		if d.config.ModelAccess.Workloads == nil {
			d.config.ModelAccess.Workloads = make(map[string]access.AccessPolicy)
		}
		d.config.ModelAccess.Workloads[params.Workload] = params.Policy
	}
	return d.persistAndRefreshLocked()
}

// applyModelAccessClearLocked removes a per-machine / per-workload
// model-access policy block (P3 / ADR-2026-06-06 §4.2). It decodes
// {workload}:
//   - workload != "" => delete .Workloads[workload] (fall back to .Default,
//     then to the platform org/project ceiling)
//   - workload == "" => clear the entire ModelAccess block (revert to the
//     nil-block identity: effective = platformAllowed)
//
// Idempotent: clear of an absent workload (or an already-nil block) is a
// no-op success — it still persists so the ACK lifecycle and on-disk state
// stay consistent, mirroring applyProjectRemoveLocked's no-op-success
// contract. Caller must hold d.mu.
func (d *Daemon) applyModelAccessClearLocked(m PendingMutation) error {
	var params struct {
		Workload string `json:"workload"`
	}
	if err := json.Unmarshal(m.Params, &params); err != nil {
		return fmt.Errorf("decode params: %w", err)
	}

	if d.config.ModelAccess == nil {
		// Nothing to clear — already at the nil-block identity. No-op success.
		return nil
	}
	if params.Workload == "" {
		// Clear the whole block — back to the nil-block identity.
		d.config.ModelAccess = nil
		return d.persistAndRefreshLocked()
	}
	if _, ok := d.config.ModelAccess.Workloads[params.Workload]; !ok {
		// Idempotent — workload override not present, treat as success.
		return nil
	}
	delete(d.config.ModelAccess.Workloads, params.Workload)
	return d.persistAndRefreshLocked()
}

func (d *Daemon) persistProjectAdmissionAndRefreshLocked(migrated bool) error {
	if migrated {
		if err := backupLegacyProjectConfig(d.opts.ConfigPath); err != nil {
			return fmt.Errorf("back up legacy project config: %w", err)
		}
	}
	return d.persistAndRefreshLocked()
}

func backupLegacyProjectConfig(path string) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open config directory %q: %w", dir, err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(base)
	if err != nil {
		return fmt.Errorf("read %q: %w", path, err)
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	backupName := base + ".v1-backup-" + stamp
	backupPath := filepath.Join(dir, backupName)
	if err := root.WriteFile(backupName, data, 0o600); err != nil {
		return fmt.Errorf("write %q: %w", backupPath, err)
	}
	return nil
}

// persistAndRefreshLocked writes the current config atomically and pushes
// the new project list into the spawner. Caller must hold d.mu.
func (d *Daemon) persistAndRefreshLocked() error {
	if d.opts.ConfigPath == "" {
		return fmt.Errorf("no config path — cannot persist mutation")
	}
	syncLegacyProjectProjection(d.config)
	normalizeProjectContract(d.config)
	syncLegacyProjectProjection(d.config)
	if err := WriteConfig(d.opts.ConfigPath, d.config); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if d.spawner != nil {
		d.spawner.SetProjectConfiguration(d.config.EffectiveProjectConfigs(), d.config.EffectiveEnabledProjectIDs())
		d.spawner.SetProjectAdmissionMode(d.config.EffectiveProjectAdmissionMode())
	}
	return nil
}
