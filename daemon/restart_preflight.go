package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/sessionshim"
)

// ErrRestartPreflightRefused reports that a planned service-manager action did
// not obtain the daemon-owned stop authorization. The daemon remains draining;
// callers must not stop or restart the service.
var ErrRestartPreflightRefused = errors.New("daemon: restart preflight refused")

type restartPreparationState string

const (
	restartPreparationPreparing   restartPreparationState = "preparing"
	restartPreparationPrepared    restartPreparationState = "prepared"
	restartPreparationNotRequired restartPreparationState = "not_required"
	restartPreparationAbandoned   restartPreparationState = "abandoned"
)

type restartPreparation struct {
	id            string
	issuedAt      time.Time
	state         restartPreparationState
	scopeIDs      []string
	covered       map[string][]sessionshim.FencedSession
	requests      map[string]sessionshim.FenceRequest
	acked         map[string]sessionshim.Fence
	persisted     bool
	authorityMode restartPreparationAuthorityMode
}

type restartPreparationAuthorityMode uint8

const (
	restartAuthorityStandalone restartPreparationAuthorityMode = iota
	restartAuthorityLegacyStore
	restartAuthorityExactStore
)

type restartPreparationAudit struct {
	SchemaVersion int                     `json:"schemaVersion"`
	Protocol      string                  `json:"protocol"`
	PreparationID string                  `json:"preparationId"`
	State         restartPreparationState `json:"state"`
	ScopeCount    int                     `json:"scopeCount"`
	UpdatedAt     int64                   `json:"updatedAt"`
}

const restartPreparationStateName = "restart-preflight.state"

// PrepareRestart is the authoritative planned-restart preflight. It accepts no
// caller fence id: the daemon mints one opaque preparation identity, freezes one
// complete correlation snapshot, and retains every per-scope request across
// partial retries.
func (d *Daemon) PrepareRestart(ctx context.Context) (afclient.DaemonRestartPreflightResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	lease, err := d.claimLifecycle(ctx, lifecycleRestartPrepare)
	if err != nil {
		return afclient.DaemonRestartPreflightResponse{}, restartPreflightCause("acquire lifecycle", err)
	}
	defer d.releaseLifecycle(lease)
	return d.prepareRestartWithLease(ctx, lease)
}

func (d *Daemon) prepareRestartWithLease(ctx context.Context, lease *lifecycleLease) (afclient.DaemonRestartPreflightResponse, error) {
	d.lifecycleMu.Lock()
	if !d.ownsLifecycleLocked(lease) {
		d.lifecycleMu.Unlock()
		return afclient.DaemonRestartPreflightResponse{}, restartPreflightError("lifecycle ownership changed")
	}
	if d.stopGen != nil {
		state := d.State()
		d.lifecycleMu.Unlock()
		return afclient.DaemonRestartPreflightResponse{}, restartPreflightError("terminal stop has begun (state %q)", state)
	}
	switch d.State() {
	case StateRunning, StatePaused, StateDraining:
	default:
		state := d.State()
		d.lifecycleMu.Unlock()
		return afclient.DaemonRestartPreflightResponse{}, restartPreflightError("current state %q", state)
	}
	d.setState(StateDraining)
	spawner := d.spawner
	d.lifecycleMu.Unlock()

	if spawner != nil {
		if err := spawner.pauseAndWaitSpawnReservations(ctx); err != nil {
			return afclient.DaemonRestartPreflightResponse{}, restartPreflightCause("settle session admissions", err)
		}
		if active, _ := spawner.ActiveSessionCounts(); active != 0 {
			return afclient.DaemonRestartPreflightResponse{}, restartPreflightError(
				"%d direct-owned session(s) remain outside session-shim coverage", active,
			)
		}
	}

	preparation, err := d.restartPreparationSnapshot()
	if err != nil {
		return afclient.DaemonRestartPreflightResponse{}, err
	}
	if err := d.consumeSessionShimAcceptanceFenceRefusal(preparation); err != nil {
		return afclient.DaemonRestartPreflightResponse{}, restartPreflightCause("acceptance fence acknowledgement", err)
	}
	switch preparation.state {
	case restartPreparationPrepared:
		if err := d.validatePreparedRestartPermission(preparation, d.restartPreparationNow()); err != nil {
			return afclient.DaemonRestartPreflightResponse{}, restartPreflightCause("revalidate cached prepared authorization", err)
		}
		return restartPreparationResponse(preparation), nil
	case restartPreparationNotRequired:
		if err := d.validateNotRequiredRestartPermission(preparation); err != nil {
			return afclient.DaemonRestartPreflightResponse{}, restartPreflightCause("revalidate cached not-required authorization", err)
		}
		return restartPreparationResponse(preparation), nil
	}
	if !preparation.persisted {
		if err := d.persistRestartPreparation(preparation, restartPreparationPreparing); err != nil {
			return afclient.DaemonRestartPreflightResponse{}, restartPreflightCause("persist local preparation", err)
		}
		preparation.persisted = true
	}
	if len(preparation.scopeIDs) == 0 {
		if err := d.persistRestartPreparation(preparation, restartPreparationNotRequired); err != nil {
			return afclient.DaemonRestartPreflightResponse{}, restartPreflightCause("persist not-required preparation", err)
		}
		if err := d.validateNotRequiredRestartPermission(preparation); err != nil {
			return afclient.DaemonRestartPreflightResponse{}, restartPreflightCause("validate not-required authorization after persistence", err)
		}
		preparation.state = restartPreparationNotRequired
		return restartPreparationResponse(preparation), nil
	}

	for _, orgID := range preparation.scopeIDs {
		if _, ok := preparation.acked[orgID]; ok {
			continue
		}
		fence, err := d.acknowledgeRestartPreparationScope(ctx, preparation, orgID)
		if err != nil {
			return afclient.DaemonRestartPreflightResponse{}, restartPreflightCause("organization "+fmt.Sprintf("%q", orgID), err)
		}
		preparation.acked[orgID] = fence
		d.shims.mu.Lock()
		d.shims.fences[orgID] = fence
		if len(preparation.scopeIDs) == 1 {
			fenceCopy := fence
			d.shims.fence = &fenceCopy
		}
		d.shims.mu.Unlock()
	}
	if err := d.persistRestartPreparation(preparation, restartPreparationPrepared); err != nil {
		return afclient.DaemonRestartPreflightResponse{}, restartPreflightCause("persist prepared authorization", err)
	}
	if err := d.validatePreparedRestartPermission(preparation, d.restartPreparationNow()); err != nil {
		return afclient.DaemonRestartPreflightResponse{}, restartPreflightCause("validate prepared authorization after persistence", err)
	}
	preparation.state = restartPreparationPrepared
	return restartPreparationResponse(preparation), nil
}

func (d *Daemon) restartPreparationSnapshot() (*restartPreparation, error) {
	d.shims.mu.Lock()
	if existing := d.shims.restart; existing != nil && existing.state != restartPreparationAbandoned {
		d.shims.mu.Unlock()
		return existing, nil
	}
	d.shims.mu.Unlock()

	covered := d.sessionShimFenceSnapshot()
	if err := d.verifyRestartRegistryCoverage(covered); err != nil {
		return nil, restartPreflightCause("registry coverage", err)
	}
	byOrg := make(map[string][]sessionshim.FencedSession)
	for _, session := range covered {
		byOrg[session.OrgID] = append(byOrg[session.OrgID], session)
	}
	scopeIDs := make([]string, 0, len(byOrg))
	for orgID := range byOrg {
		scopeIDs = append(scopeIDs, orgID)
	}
	sort.Strings(scopeIDs)

	id, err := d.newRestartPreparationID()
	if err != nil {
		return nil, restartPreflightCause("mint preparation identity", err)
	}
	preparation := &restartPreparation{
		id:            id,
		issuedAt:      d.restartPreparationNow(),
		state:         restartPreparationPreparing,
		scopeIDs:      scopeIDs,
		covered:       byOrg,
		requests:      make(map[string]sessionshim.FenceRequest),
		acked:         make(map[string]sessionshim.Fence),
		authorityMode: d.restartPreparationAuthorityMode(),
	}
	d.shims.mu.Lock()
	if existing := d.shims.restart; existing != nil && existing.state != restartPreparationAbandoned {
		preparation = existing
	} else {
		d.shims.restart = preparation
	}
	d.shims.mu.Unlock()
	return preparation, nil
}

func (d *Daemon) verifyRestartRegistryCoverage(covered []sessionshim.FencedSession) error {
	registry, err := sessionshim.NewRegistry(d.sessionShimConfig().RegistryDir)
	if err != nil {
		return err
	}
	entries, err := registry.Scan()
	if err != nil {
		return err
	}
	coveredSet := make(map[restartRegistryCorrelation]struct{}, len(covered))
	for _, session := range covered {
		coveredSet[restartCorrelationKey(session)] = struct{}{}
	}
	for _, entry := range entries {
		if entry.Err != nil {
			return fmt.Errorf("unclassified registry entry %q: %w", entry.Name, entry.Err)
		}
		candidate := sessionshim.FencedSession{
			OrgID: entry.Record.OrgID, SessionID: entry.Record.SessionID,
			ShimID: entry.Record.ShimID, ProcessEpoch: entry.Record.ProcessEpoch,
		}
		if _, ok := coveredSet[restartCorrelationKey(candidate)]; !ok {
			return fmt.Errorf("live registry correlation %s/%s/%s/%d is not in the frozen daemon snapshot",
				candidate.OrgID, candidate.SessionID, candidate.ShimID, candidate.ProcessEpoch)
		}
	}
	return nil
}

type restartRegistryCorrelation struct {
	orgID, sessionID, shimID string
	processEpoch             uint64
}

func restartCorrelationKey(session sessionshim.FencedSession) restartRegistryCorrelation {
	return restartRegistryCorrelation{
		orgID: session.OrgID, sessionID: session.SessionID,
		shimID: session.ShimID, processEpoch: session.ProcessEpoch,
	}
}

func (d *Daemon) acknowledgeRestartPreparationScope(ctx context.Context, preparation *restartPreparation, orgID string) (sessionshim.Fence, error) {
	cfg := d.sessionShimConfig()
	policy := sessionshim.FencePolicy{RestartBudget: cfg.RestartBudget, Orphan: cfg.Orphan}
	covered := preparation.covered[orgID]
	request, ok := preparation.requests[orgID]
	if !ok {
		hostID, err := d.sessionShimHostID(ctx, orgID)
		if err != nil {
			return sessionshim.Fence{}, fmt.Errorf("resolve host identity: %w", err)
		}
		request, err = sessionshim.NewExactFenceRequest(preparation.id, hostID, covered, policy, preparation.issuedAt)
		if err != nil {
			return sessionshim.Fence{}, err
		}
		preparation.requests[orgID] = sessionshim.CloneFenceRequest(request)
	}
	if cfg.ExactFenceStore != nil {
		return sessionshim.AcknowledgeExactFence(ctx, cfg.ExactFenceStore, request)
	}
	// The v0.67 semantic store remains source/runtime compatible. The daemon's
	// fixed preparation time and frozen coverage make every retry equivalent;
	// hosted exact-byte compositions use ExactFenceStore above.
	return sessionshim.RequestFence(ctx, cfg.FenceStore, preparation.id, request.Fence.HostID, covered, policy, preparation.issuedAt)
}

func (d *Daemon) validatePreparedRestartPermission(preparation *restartPreparation, now time.Time) error {
	if preparation == nil || len(preparation.scopeIDs) == 0 {
		return errors.New("prepared authorization has no authority scopes")
	}
	if len(preparation.acked) != len(preparation.scopeIDs) {
		return fmt.Errorf("prepared acknowledgement count is %d, want %d", len(preparation.acked), len(preparation.scopeIDs))
	}
	d.shims.mu.RLock()
	currentFences := make(map[string]sessionshim.Fence, len(preparation.scopeIDs))
	for _, orgID := range preparation.scopeIDs {
		if fence, ok := d.shims.fences[orgID]; ok {
			currentFences[orgID] = fence
		}
	}
	d.shims.mu.RUnlock()
	for _, orgID := range preparation.scopeIDs {
		request, requestOK := preparation.requests[orgID]
		ack, ackOK := preparation.acked[orgID]
		current, currentOK := currentFences[orgID]
		if !requestOK || !ackOK || !currentOK {
			return fmt.Errorf("organization %q is missing frozen request or acknowledgement", orgID)
		}
		if ack.State != sessionshim.FenceHeld || current.State != sessionshim.FenceHeld {
			return fmt.Errorf("organization %q fence state is no longer held", orgID)
		}
		if ack.HoldUntilUnixNano <= now.UnixNano() || current.HoldUntilUnixNano <= now.UnixNano() {
			return fmt.Errorf("organization %q fence hold expired", orgID)
		}
		switch preparation.authorityMode {
		case restartAuthorityExactStore:
			expectedBytes, err := json.Marshal(request.Fence)
			if err != nil || !bytes.Equal(expectedBytes, request.RequestBytes) {
				return fmt.Errorf("organization %q frozen request bytes changed", orgID)
			}
			if !sameRestartFenceSemantic(ack, request.Fence) || !sameRestartFenceSemantic(current, ack) || current.DurableRevision != ack.DurableRevision {
				return fmt.Errorf("organization %q exact acknowledgement changed after preparation", orgID)
			}
			if strings.TrimSpace(ack.DurableRevision) == "" || strings.TrimSpace(current.DurableRevision) == "" {
				return fmt.Errorf("organization %q acknowledgement has no durable revision", orgID)
			}
		case restartAuthorityLegacyStore:
			if ack.FenceID != request.Fence.FenceID || !sameFencedSessions(ack.Sessions, request.Fence.Sessions) ||
				current.FenceID != ack.FenceID || !sameFencedSessions(current.Sessions, ack.Sessions) ||
				current.DurableRevision != ack.DurableRevision {
				return fmt.Errorf("organization %q legacy acknowledgement changed after preparation", orgID)
			}
		case restartAuthorityStandalone:
			expectedBytes, err := json.Marshal(request.Fence)
			if err != nil || !bytes.Equal(expectedBytes, request.RequestBytes) ||
				!sameRestartFenceSemantic(ack, request.Fence) || !sameRestartFenceSemantic(current, ack) ||
				ack.DurableRevision != "" || current.DurableRevision != "" {
				return fmt.Errorf("organization %q standalone held intent changed after preparation", orgID)
			}
		default:
			return fmt.Errorf("organization %q has unknown restart authority mode", orgID)
		}
	}
	return nil
}

func sameRestartFenceSemantic(a, b sessionshim.Fence) bool {
	return a.FenceID == b.FenceID && a.HostID == b.HostID &&
		a.IssuedAtUnixNano == b.IssuedAtUnixNano && a.HoldUntilUnixNano == b.HoldUntilUnixNano &&
		a.State == b.State && sameFencedSessions(a.Sessions, b.Sessions)
}

func (d *Daemon) restartPreparationAuthorityMode() restartPreparationAuthorityMode {
	cfg := d.sessionShimConfig()
	switch {
	case cfg.ExactFenceStore != nil:
		return restartAuthorityExactStore
	case cfg.FenceStore != nil:
		return restartAuthorityLegacyStore
	default:
		return restartAuthorityStandalone
	}
}

func (d *Daemon) validateNotRequiredRestartPermission(preparation *restartPreparation) error {
	if preparation == nil || len(preparation.scopeIDs) != 0 || len(preparation.acked) != 0 || len(preparation.requests) != 0 {
		return errors.New("not-required authorization contains an authority scope")
	}
	covered := d.sessionShimFenceSnapshot()
	if len(covered) != 0 {
		return fmt.Errorf("not-required authorization no longer has an empty shim snapshot (%d correlations)", len(covered))
	}
	return d.verifyRestartRegistryCoverage(covered)
}

func (d *Daemon) restartPreparationNow() time.Time {
	d.shims.mu.RLock()
	now := d.shims.restartNow
	d.shims.mu.RUnlock()
	if now != nil {
		return now()
	}
	return time.Now()
}

func restartPreparationResponse(preparation *restartPreparation) afclient.DaemonRestartPreflightResponse {
	state := afclient.DaemonRestartPrepared
	if preparation.state == restartPreparationNotRequired {
		state = afclient.DaemonRestartNotRequired
	}
	return afclient.DaemonRestartPreflightResponse{
		Protocol: afclient.DaemonRestartPreflightProtocol, State: state,
		PreparationID: preparation.id, ScopeCount: len(preparation.scopeIDs),
	}
}

func restartPreflightError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRestartPreflightRefused, fmt.Sprintf(format, args...))
}

func restartPreflightCause(operation string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrRestartPreflightRefused, operation, err)
}

func (d *Daemon) newRestartPreparationID() (string, error) {
	d.shims.mu.RLock()
	generate := d.shims.restartID
	d.shims.mu.RUnlock()
	if generate != nil {
		return generate()
	}
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return "rp_" + hex.EncodeToString(bytes[:]), nil
}

func (d *Daemon) persistRestartPreparation(preparation *restartPreparation, state restartPreparationState) error {
	record := restartPreparationAudit{
		SchemaVersion: 1, Protocol: afclient.DaemonRestartPreflightProtocol,
		PreparationID: preparation.id, State: state, ScopeCount: len(preparation.scopeIDs),
		UpdatedAt: d.restartPreparationNow().UnixNano(),
	}
	d.shims.mu.RLock()
	write := d.shims.restartStateWriter
	d.shims.mu.RUnlock()
	if write != nil {
		return write(record)
	}
	return writeRestartPreparationAudit(d.sessionShimConfig().RegistryDir, record)
}

func (d *Daemon) abandonRestartPreparation(_ context.Context) error {
	d.shims.mu.RLock()
	preparation := d.shims.restart
	d.shims.mu.RUnlock()
	if preparation == nil || preparation.state == restartPreparationAbandoned {
		return nil
	}
	if err := d.persistRestartPreparation(preparation, restartPreparationAbandoned); err != nil {
		return err
	}
	d.shims.mu.Lock()
	if d.shims.restart == preparation {
		preparation.state = restartPreparationAbandoned
	}
	d.shims.mu.Unlock()
	return nil
}

func writeRestartPreparationAudit(registryDir string, record restartPreparationAudit) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode restart preflight audit: %w", err)
	}
	if len(data) > 4096 {
		return errors.New("restart preflight audit exceeds 4096 bytes")
	}
	if err := os.MkdirAll(registryDir, sessionshim.RegistryDirMode); err != nil {
		return fmt.Errorf("create restart preflight audit directory: %w", err)
	}
	if err := os.Chmod(registryDir, sessionshim.RegistryDirMode); err != nil {
		return fmt.Errorf("tighten restart preflight audit directory: %w", err)
	}
	root, err := os.OpenRoot(registryDir)
	if err != nil {
		return fmt.Errorf("open restart preflight audit directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("restart preflight audit nonce: %w", err)
	}
	tmpName := ".restart-preflight-" + hex.EncodeToString(nonce[:]) + ".tmp"
	file, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, sessionshim.RecordFileMode)
	if err != nil {
		return fmt.Errorf("create restart preflight audit: %w", err)
	}
	defer func() { _ = root.Remove(tmpName) }()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write restart preflight audit: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync restart preflight audit: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close restart preflight audit: %w", err)
	}
	if err := root.Rename(tmpName, restartPreparationStateName); err != nil {
		return fmt.Errorf("publish restart preflight audit: %w", err)
	}
	dir, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open restart preflight audit directory for sync: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("sync restart preflight audit directory: %w", err)
	}
	return dir.Close()
}
