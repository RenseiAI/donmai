package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

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
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.config == nil {
		return fmt.Errorf("daemon config not loaded")
	}

	switch m.Op {
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

	// Idempotent — if already present (matching id), do nothing.
	for _, p := range d.config.Projects {
		if p.ID == params.ID {
			return nil
		}
	}
	d.config.Projects = append(d.config.Projects, ProjectConfig{
		ID:         params.ID,
		Repository: params.Repository,
	})
	return d.persistAndRefreshLocked()
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

	original := d.config.Projects
	filtered := original[:0]
	removed := false
	for _, p := range original {
		if p.ID == params.ID {
			removed = true
			continue
		}
		filtered = append(filtered, p)
	}
	if !removed {
		// Idempotent — id not present, treat as success.
		return nil
	}
	// Build a fresh slice so we don't accidentally share backing storage
	// with the original (defensive against a later mutation racing the
	// spawner snapshot via SetProjects).
	d.config.Projects = append([]ProjectConfig(nil), filtered...)
	return d.persistAndRefreshLocked()
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
// (ResolveMachineCell) stays in the rensei-tui S3 gate. Idempotent: a
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

// persistAndRefreshLocked writes the current config atomically and pushes
// the new project list into the spawner. Caller must hold d.mu.
func (d *Daemon) persistAndRefreshLocked() error {
	if d.opts.ConfigPath == "" {
		return fmt.Errorf("no config path — cannot persist mutation")
	}
	if err := WriteConfig(d.opts.ConfigPath, d.config); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if d.spawner != nil {
		d.spawner.SetProjects(d.config.Projects)
	}
	return nil
}
