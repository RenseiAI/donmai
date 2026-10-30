package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// hostUpgradeOp is the single approved host-upgrade mutation operation
// delivered over the established heartbeat pendingMutations channel.
//
// The platform owns the serialized request contract (mutation id, requestedAt
// and the consented/expiresAt params below); the daemon owns execution and
// the reported outcome. The request never carries a release URL, signing
// trust or executable content: the swap always runs through the local
// AutoUpdate configuration (CDN base, channel, signers, trust root) via
// Daemon.Update, which preserves the independent machine-owner opt-in and
// the restart preflight.
const hostUpgradeOp = "host.upgrade"

// hostUpgradeExecutionBound caps one hosted upgrade attempt. It keeps a
// delayed update from holding the lifecycle lease (and the draining state)
// indefinitely and gives Shutdown a bound without waiting on the network.
const hostUpgradeExecutionBound = 5 * time.Minute

// hostUpgradeMaxRequestedAge bounds how long a requested upgrade stays
// actionable when the request carries no explicit expiresAt. A stale request
// that sat queued for longer than this refuses without a swap instead of
// updating a host the operator may no longer intend to move.
const hostUpgradeMaxRequestedAge = 24 * time.Hour

// hostUpgradeOutcome is the durable per-request outcome. Applied is true only
// when the attempt reached its defined durable point (binary swapped or host
// already current); every refusal or failed attempt records Applied false
// with a human-readable Error. CompletedAt is when the attempt finished.
type hostUpgradeOutcome struct {
	Applied     bool   `json:"applied"`
	Error       string `json:"error,omitempty"`
	Updated     bool   `json:"updated"`
	Version     string `json:"version,omitempty"`
	Reason      string `json:"reason,omitempty"`
	CompletedAt string `json:"completedAt,omitempty"`
}

// hostUpgradeForbiddenParams are request keys the daemon never honors. Any
// request carrying one refuses without a swap: the release URL, signing
// trust and executable content always come from local configuration and the
// signed release, never from the remote request.
var hostUpgradeForbiddenParams = map[string]struct{}{
	"url": {}, "downloadurl": {}, "binaryurl": {}, "binary": {},
	"executable": {}, "content": {}, "payload": {}, "script": {},
	"command": {}, "signers": {}, "trustroot": {}, "trustrootpath": {},
	"signature": {}, "bundle": {}, "sha256": {}, "hash": {},
	"cdn": {}, "cdnbase": {}, "endpoint": {}, "channel": {},
}

// hostUpgradeParams is the approved request shape. Consented must be an
// explicit true; ExpiresAt, when present, is an RFC3339 deadline after which
// the request refuses without a swap. Reason is display-only.
type hostUpgradeParams struct {
	Consented *bool  `json:"consented"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// parseHostUpgradeParams validates the approved request contract. It returns
// nil when the request is actionable: explicit consent, no forbidden content,
// and neither the explicit expiry nor the platform RequestedAt age has
// passed. Every refusal explains itself for the failure ACK.
func parseHostUpgradeParams(m PendingMutation, now time.Time) error {
	if len(bytes.TrimSpace(m.Params)) == 0 {
		return fmt.Errorf("host.upgrade requires params with consented:true")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(m.Params, &raw); err != nil {
		return fmt.Errorf("host.upgrade decode params: %w", err)
	}
	for key := range raw {
		if _, forbidden := hostUpgradeForbiddenParams[strings.ToLower(key)]; forbidden {
			return fmt.Errorf("host.upgrade param %q is never taken from the remote request", key)
		}
	}
	var params hostUpgradeParams
	if err := json.Unmarshal(m.Params, &params); err != nil {
		return fmt.Errorf("host.upgrade decode params: %w", err)
	}
	if params.Consented == nil || !*params.Consented {
		return fmt.Errorf("host.upgrade requires consented:true")
	}
	if strings.TrimSpace(params.ExpiresAt) != "" {
		deadline, err := time.Parse(time.RFC3339, strings.TrimSpace(params.ExpiresAt))
		if err != nil {
			return fmt.Errorf("host.upgrade expiresAt is malformed: %w", err)
		}
		if !now.Before(deadline) {
			return fmt.Errorf("host.upgrade request expired at %s", params.ExpiresAt)
		}
		return nil
	}
	if strings.TrimSpace(m.RequestedAt) != "" {
		requested, err := time.Parse(time.RFC3339, strings.TrimSpace(m.RequestedAt))
		if err != nil {
			return fmt.Errorf("host.upgrade requestedAt is malformed: %w", err)
		}
		if now.Sub(requested) > hostUpgradeMaxRequestedAge {
			return fmt.Errorf("host.upgrade request is stale (requestedAt %s)", m.RequestedAt)
		}
	}
	return nil
}

// hostUpgradeOptedIn reports the independent machine-owner opt-in: the host
// has a configured signed source. With no signers the updater is fail-closed
// and refuses every swap, so a remote request alone can never authorize one.
func hostUpgradeOptedIn(cfg *Config) bool {
	return cfg != nil && len(cfg.AutoUpdate.Signers) > 0
}

// hostUpgradeStatePath is the durable outcome file. It lives beside
// daemon.yaml so a process exit between swap and ACK cannot manufacture
// completion: the next boot reloads it and answers a redelivery from the
// recorded outcome instead of running a second swap.
func hostUpgradeStatePath(configPath string) string {
	if strings.TrimSpace(configPath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), "host-upgrade-state.json")
}

// loadHostUpgradeOutcomes reads the durable outcome file. Missing file is not
// an error (no upgrade has completed yet); a corrupt file fails closed with
// no outcomes so a redelivery re-validates instead of trusting garbage.
func loadHostUpgradeOutcomes(path string) map[string]hostUpgradeOutcome {
	outcomes := make(map[string]hostUpgradeOutcome)
	if strings.TrimSpace(path) == "" {
		return outcomes
	}
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return outcomes
	}
	var decoded map[string]hostUpgradeOutcome
	if err := json.Unmarshal(data, &decoded); err != nil {
		return outcomes
	}
	for id, outcome := range decoded {
		if strings.TrimSpace(id) == "" {
			continue
		}
		outcomes[id] = outcome
	}
	return outcomes
}

// persistHostUpgradeOutcome durably records one request outcome before it is
// ever ACKed. The write is atomic (temp + rename) so a crash mid-write
// leaves the previous durable state intact.
func persistHostUpgradeOutcome(path, id string, outcome hostUpgradeOutcome) {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(id) == "" {
		return
	}
	outcomes := loadHostUpgradeOutcomes(path)
	outcomes[id] = outcome
	data, err := json.Marshal(outcomes)
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "host-upgrade-state-*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	_ = os.Rename(tmpName, path)
}

// initHostUpgradeState lazily initializes the in-memory upgrade maps and
// loads durable outcomes once per process.
func (d *Daemon) initHostUpgradeStateLocked() {
	if d.hostUpgradeDone == nil {
		d.hostUpgradeDone = make(map[string]hostUpgradeOutcome)
	}
	if d.hostUpgradeInFlight == nil {
		d.hostUpgradeInFlight = make(map[string]struct{})
	}
	if d.hostUpgradeCancels == nil {
		d.hostUpgradeCancels = make(map[string]context.CancelFunc)
	}
	if !d.hostUpgradeLoaded {
		for id, outcome := range loadHostUpgradeOutcomes(hostUpgradeStatePath(d.opts.ConfigPath)) {
			d.hostUpgradeDone[id] = outcome
		}
		d.hostUpgradeLoaded = true
	}
}

// handleHostUpgradeMutation is the heartbeat-goroutine entry point for the
// approved operation. It never blocks on the lifecycle and never holds the
// daemon config lock: validation and bookkeeping complete synchronously, and
// a valid request hands a bounded, cancellable execution to a background
// goroutine outside every lock the heartbeat needs to keep progressing.
//
// Return contract mirrors applyPendingMutations: appliedID or failure is
// buffered for the next beat's ACK, while empty both means "no ACK yet" —
// the platform will redeliver and the redelivery will observe the in-flight
// or completed outcome.
func (d *Daemon) handleHostUpgradeMutation(m PendingMutation) (appliedID string, failure *HeartbeatMutationFailure) {
	if m.Op != hostUpgradeOp {
		return "", &HeartbeatMutationFailure{ID: m.ID, Error: fmt.Sprintf("unsupported mutation op %q", m.Op)}
	}
	if strings.TrimSpace(m.ID) == "" {
		return "", &HeartbeatMutationFailure{ID: m.ID, Error: "host.upgrade requires an id"}
	}
	now := time.Now()
	d.hostUpgradeMu.Lock()
	d.initHostUpgradeStateLocked()
	if outcome, ok := d.hostUpgradeDone[m.ID]; ok {
		d.hostUpgradeMu.Unlock()
		if outcome.Applied {
			return m.ID, nil
		}
		return "", &HeartbeatMutationFailure{ID: m.ID, Error: outcome.Error}
	}
	if _, ok := d.hostUpgradeInFlight[m.ID]; ok {
		d.hostUpgradeMu.Unlock()
		return "", nil
	}
	if len(d.hostUpgradeInFlight) > 0 {
		d.hostUpgradeMu.Unlock()
		return "", &HeartbeatMutationFailure{ID: m.ID, Error: "host.upgrade already in progress"}
	}
	d.lifecycleMu.Lock()
	lifecycleBusy := d.lifecycleOwner != nil
	d.lifecycleMu.Unlock()
	if lifecycleBusy {
		d.hostUpgradeMu.Unlock()
		return "", &HeartbeatMutationFailure{ID: m.ID, Error: "host.upgrade refused: lifecycle busy"}
	}
	if err := parseHostUpgradeParams(m, now); err != nil {
		outcome := hostUpgradeOutcome{Applied: false, Error: err.Error(), CompletedAt: now.UTC().Format(time.RFC3339)}
		d.hostUpgradeDone[m.ID] = outcome
		persistHostUpgradeOutcome(hostUpgradeStatePath(d.opts.ConfigPath), m.ID, outcome)
		d.hostUpgradeMu.Unlock()
		return "", &HeartbeatMutationFailure{ID: m.ID, Error: err.Error()}
	}
	if !hostUpgradeOptedIn(d.configSnapshotForUpgrade()) {
		err := "host.upgrade refused: machine-owner update consent is not configured (no signed source)"
		outcome := hostUpgradeOutcome{Applied: false, Error: err, CompletedAt: now.UTC().Format(time.RFC3339)}
		d.hostUpgradeDone[m.ID] = outcome
		persistHostUpgradeOutcome(hostUpgradeStatePath(d.opts.ConfigPath), m.ID, outcome)
		d.hostUpgradeMu.Unlock()
		return "", &HeartbeatMutationFailure{ID: m.ID, Error: err}
	}
	d.hostUpgradeInFlight[m.ID] = struct{}{}
	d.hostUpgradeMu.Unlock()

	go d.runHostUpgrade(m)

	return "", nil
}

// configSnapshotForUpgrade reads the current config without holding d.mu
// across validation. It holds only the read lock for the copy.
func (d *Daemon) configSnapshotForUpgrade() *Config {
	return d.Config()
}

// runHostUpgrade executes one consented request outside every heartbeat and
// config lock. It shares the single lifecycle authority with the scheduled
// and manual updaters via tryClaimLifecycle: a contended lease fails the
// attempt instead of queueing a second swap behind the holder.
// cancelHostUpgrades cancels every in-flight hosted upgrade attempt. Stop
// calls it before waiting on the lifecycle lease so a delayed update cannot
// hold shutdown past its own bound.
func (d *Daemon) cancelHostUpgrades() {
	d.hostUpgradeMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(d.hostUpgradeCancels))
	for _, cancel := range d.hostUpgradeCancels {
		cancels = append(cancels, cancel)
	}
	d.hostUpgradeMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (d *Daemon) runHostUpgrade(m PendingMutation) {
	ctx, cancel := context.WithTimeout(context.Background(), hostUpgradeExecutionBound)
	d.hostUpgradeMu.Lock()
	d.initHostUpgradeStateLocked()
	d.hostUpgradeCancels[m.ID] = cancel
	d.hostUpgradeMu.Unlock()
	defer func() {
		cancel()
		d.hostUpgradeMu.Lock()
		delete(d.hostUpgradeCancels, m.ID)
		d.hostUpgradeMu.Unlock()
	}()

	lease, ok := d.tryClaimLifecycle(lifecycleUpdate)
	if !ok {
		d.finishHostUpgrade(m.ID, hostUpgradeOutcome{
			Applied: false, Error: "host.upgrade refused: lifecycle busy",
			CompletedAt: time.Now().UTC().Format(time.RFC3339),
		})
		return
	}
	preparation, err := d.prepareRestartWithLease(ctx, lease)
	_ = preparation
	if err != nil {
		d.releaseLifecycle(lease)
		d.finishHostUpgrade(m.ID, hostUpgradeOutcome{
			Applied: false, Error: "host.upgrade preflight refused: " + err.Error(),
			CompletedAt: time.Now().UTC().Format(time.RFC3339),
		})
		return
	}
	d.lifecycleMu.Lock()
	owned := d.ownsLifecycleLocked(lease) && d.stopGen == nil && d.State() == StateDraining
	if owned {
		d.setState(StateUpdating)
	}
	d.lifecycleMu.Unlock()
	if !owned {
		d.releaseLifecycle(lease)
		d.finishHostUpgrade(m.ID, hostUpgradeOutcome{
			Applied: false, Error: "host.upgrade refused: lifecycle lost before update",
			CompletedAt: time.Now().UTC().Format(time.RFC3339),
		})
		return
	}

	result, runErr := d.executeHostUpgrade(ctx)
	d.lifecycleMu.Lock()
	if d.ownsLifecycleLocked(lease) && d.stopGen == nil && d.State() == StateUpdating {
		d.setState(StateDraining)
	}
	d.lifecycleMu.Unlock()
	d.releaseLifecycle(lease)

	if runErr != nil {
		d.finishHostUpgrade(m.ID, hostUpgradeOutcome{
			Applied: false, Error: "host.upgrade failed: " + runErr.Error(),
			Reason: reasonOfHostUpgradeResult(result), Version: versionOfHostUpgradeResult(result),
			CompletedAt: time.Now().UTC().Format(time.RFC3339),
		})
		return
	}
	if result == nil {
		d.finishHostUpgrade(m.ID, hostUpgradeOutcome{
			Applied: false, Error: "host.upgrade failed: empty update result",
			CompletedAt: time.Now().UTC().Format(time.RFC3339),
		})
		return
	}
	if result.Updated || result.Reason == "already-up-to-date" {
		d.finishHostUpgrade(m.ID, hostUpgradeOutcome{
			Applied: true, Updated: result.Updated, Version: result.Version, Reason: result.Reason,
			CompletedAt: time.Now().UTC().Format(time.RFC3339),
		})
		return
	}
	errText := "host.upgrade refused: " + result.Reason
	if strings.TrimSpace(result.Reason) == "" {
		errText = "host.upgrade failed without a swap"
	}
	d.finishHostUpgrade(m.ID, hostUpgradeOutcome{
		Applied: false, Error: errText, Updated: result.Updated, Version: result.Version, Reason: result.Reason,
		CompletedAt: time.Now().UTC().Format(time.RFC3339),
	})
}

// executeHostUpgrade routes the attempt through the existing update entry
// point. The test seam (runPreparedUpdate) and the production updater share
// this call so the heartbeat path cannot diverge from the manual path.
func (d *Daemon) executeHostUpgrade(ctx context.Context) (*UpdateResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := d.Config()
	if cfg == nil {
		return nil, fmt.Errorf("no config loaded")
	}
	if d.runPreparedUpdate != nil {
		return d.runPreparedUpdate(ctx, cfg.AutoUpdate, d.EffectiveVersion())
	}
	updater := NewUpdater(UpdaterOptions{
		CurrentVersion: d.EffectiveVersion(),
		Config:         cfg.AutoUpdate,
		SkipExit:       true,
	})
	return updater.RunUpdate(ctx)
}

// finishHostUpgrade durably records the outcome before it can ever be ACKed.
// A lost response or process exit therefore cannot manufacture completion:
// the next redelivery answers from the recorded outcome.
func (d *Daemon) finishHostUpgrade(id string, outcome hostUpgradeOutcome) {
	if strings.TrimSpace(outcome.CompletedAt) == "" {
		outcome.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if outcome.Error == "" && !outcome.Applied {
		outcome.Error = "host.upgrade failed without a swap"
	}
	persistHostUpgradeOutcome(hostUpgradeStatePath(d.opts.ConfigPath), id, outcome)
	d.hostUpgradeMu.Lock()
	d.initHostUpgradeStateLocked()
	delete(d.hostUpgradeInFlight, id)
	d.hostUpgradeDone[id] = outcome
	d.hostUpgradeMu.Unlock()
}

func reasonOfHostUpgradeResult(result *UpdateResult) string {
	if result == nil {
		return ""
	}
	return result.Reason
}

func versionOfHostUpgradeResult(result *UpdateResult) string {
	if result == nil {
		return ""
	}
	return result.Version
}
