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
// Pending marks a durable intent written before execution starts: a crash
// between intent and final outcome must never run a second swap on
// redelivery, so a pending record reloads as an interrupted failure.
type hostUpgradeOutcome struct {
	Applied     bool   `json:"applied"`
	Error       string `json:"error,omitempty"`
	Updated     bool   `json:"updated"`
	Version     string `json:"version,omitempty"`
	Reason      string `json:"reason,omitempty"`
	CompletedAt string `json:"completedAt,omitempty"`
	Pending     bool   `json:"pending,omitempty"`
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

// hostUpgradeSourceConfigured reports whether the production updater has an
// update source to check. The OSS binary ships with no default CDN base
// (UpdateCDNBase is empty): without a source the real updater skips its
// manifest check and reports already-up-to-date without contacting any
// release. Treating that disabled response as applied would falsely ACK
// success for a host that never checked a release, so admission refuses
// without a swap when no source is configured. The source is the
// operator-configured autoUpdate.cdnBase (see effectiveUpdateCDNBase). A
// test seam (runPreparedUpdate) counts as a source because the test provides
// the release implementation.
func (d *Daemon) hostUpgradeSourceConfigured() bool {
	if d.runPreparedUpdate != nil {
		return true
	}
	cfg := d.Config()
	if cfg == nil {
		return false
	}
	return strings.TrimSpace(effectiveUpdateCDNBase(cfg.AutoUpdate)) != ""
}

// hostUpgradeConsentDeadline returns the wall-clock deadline after which the
// request must refuse without a swap: the explicit expiresAt when present,
// otherwise requestedAt plus the maximum actionable age. The execution
// context is bounded by this deadline so a manifest or binary download that
// runs past consent cannot still replace the binary.
func hostUpgradeConsentDeadline(m PendingMutation) (time.Time, bool) {
	if len(bytes.TrimSpace(m.Params)) != 0 {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(m.Params, &raw); err == nil {
			var params hostUpgradeParams
			if err := json.Unmarshal(m.Params, &params); err == nil {
				if strings.TrimSpace(params.ExpiresAt) != "" {
					if deadline, err := time.Parse(time.RFC3339, strings.TrimSpace(params.ExpiresAt)); err == nil {
						return deadline, true
					}
				}
			}
		}
	}
	if strings.TrimSpace(m.RequestedAt) != "" {
		if requested, err := time.Parse(time.RFC3339, strings.TrimSpace(m.RequestedAt)); err == nil {
			return requested.Add(hostUpgradeMaxRequestedAge), true
		}
	}
	return time.Time{}, false
}

// hostUpgradeBoundedContext returns an execution context bounded both by the
// shutdown/cancellation parent and by consent: the deadline is the earlier of
// the consent deadline and now plus the execution bound. A nil deadline
// falls back to the execution bound alone.
func hostUpgradeBoundedContext(m PendingMutation) (context.Context, context.CancelFunc) {
	now := time.Now()
	bound := now.Add(hostUpgradeExecutionBound)
	if deadline, ok := hostUpgradeConsentDeadline(m); ok && deadline.Before(bound) {
		bound = deadline
	}
	return context.WithDeadline(context.Background(), bound)
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

// loadHostUpgradeOutcomes reads the durable outcome file. A missing file is
// not an error (no upgrade has completed yet). Any other read or decode
// failure is an error so the caller can fail closed: a corrupt or
// unreadable file must refuse new swaps until the operator clears it, never
// re-validate to a second swap.
func loadHostUpgradeOutcomes(path string) (map[string]hostUpgradeOutcome, error) {
	outcomes := make(map[string]hostUpgradeOutcome)
	if strings.TrimSpace(path) == "" {
		return outcomes, fmt.Errorf("host.upgrade refused: no durable state path")
	}
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		if os.IsNotExist(err) {
			return outcomes, nil
		}
		return outcomes, fmt.Errorf("host.upgrade refused: durable state unreadable: %w", err)
	}
	var decoded map[string]hostUpgradeOutcome
	if err := json.Unmarshal(data, &decoded); err != nil {
		return outcomes, fmt.Errorf("host.upgrade refused: durable state corrupt: %w", err)
	}
	if decoded == nil {
		return outcomes, fmt.Errorf("host.upgrade refused: durable state corrupt: empty ledger")
	}
	for id, outcome := range decoded {
		if strings.TrimSpace(id) == "" {
			continue
		}
		outcomes[id] = outcome
	}
	return outcomes, nil
}

// persistHostUpgradeOutcome durably records one request outcome before it is
// ever ACKed. The write is atomic (temp + rename) so a crash mid-write
// leaves the previous durable state intact. Every failure is returned so
// the caller can fail closed instead of running without durability.
func persistHostUpgradeOutcome(path, id string, outcome hostUpgradeOutcome) error {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(id) == "" {
		return fmt.Errorf("host.upgrade refused: no durable state path")
	}
	outcomes, err := loadHostUpgradeOutcomes(path)
	if err != nil {
		return err
	}
	outcomes[id] = outcome
	data, err := json.Marshal(outcomes)
	if err != nil {
		return fmt.Errorf("host.upgrade refused: durable state encode failed: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "host-upgrade-state-*.tmp")
	if err != nil {
		return fmt.Errorf("host.upgrade refused: durable state not writable: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("host.upgrade refused: durable state write failed: %w", err)
	}
	// Sync file content before rename so a crash cannot leave a torn write
	// that later reloads as a second swap. Atomic rename alone does not
	// establish crash durability without this flush.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("host.upgrade refused: durable state write failed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("host.upgrade refused: durable state write failed: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("host.upgrade refused: durable state write failed: %w", err)
	}
	// Sync the directory entry so the rename itself is durable: without
	// this a crash between rename and directory flush can lose the outcome
	// and a redelivery would run a second swap.
	if dirFile, err := os.Open(dir); err == nil { //nolint:gosec
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

// probeHostUpgradeDurability verifies the durable outcome file can be read
// and the state directory can be written before a swap starts. A valid
// request that cannot be made durable refuses without a swap instead of
// entering a pending state that a restart would lose into a second swap.
func probeHostUpgradeDurability(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("host.upgrade refused: no durable state path")
	}
	if _, err := loadHostUpgradeOutcomes(path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "host-upgrade-probe-*.tmp")
	if err != nil {
		return fmt.Errorf("host.upgrade refused: durable state not writable: %w", err)
	}
	name := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(name) //nolint:gosec
	return nil
}

// initHostUpgradeState lazily initializes the in-memory upgrade maps and
// loads durable outcomes once per process. A corrupt or unreadable durable
// file fails closed: the daemon refuses new swaps until the operator clears
// it, instead of re-validating to a second swap.
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
		outcomes, err := loadHostUpgradeOutcomes(hostUpgradeStatePath(d.opts.ConfigPath))
		if err != nil {
			d.hostUpgradeDurableBroken = true
			d.hostUpgradeDurableError = err.Error()
		} else {
			for id, outcome := range outcomes {
				// A pending intent without a final outcome means the previous
				// process exited between intent and completion. It may have
				// swapped before exiting, or never started: either way the
				// safe answer is an interrupted failure, never a second swap
				// and never manufactured completion.
				if outcome.Pending {
					completedAt := strings.TrimSpace(outcome.CompletedAt)
					if completedAt == "" {
						completedAt = time.Now().UTC().Format(time.RFC3339)
					}
					outcome = hostUpgradeOutcome{
						Applied:     false,
						Error:       "host.upgrade interrupted: previous attempt did not complete",
						Reason:      "interrupted",
						CompletedAt: completedAt,
					}
				}
				d.hostUpgradeDone[id] = outcome
			}
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
	statePath := hostUpgradeStatePath(d.opts.ConfigPath)
	d.hostUpgradeMu.Lock()
	d.initHostUpgradeStateLocked()
	if outcome, ok := d.hostUpgradeDone[m.ID]; ok {
		if outcome.Pending {
			d.hostUpgradeMu.Unlock()
			return "", nil
		}
		d.hostUpgradeMu.Unlock()
		if outcome.Applied {
			return m.ID, nil
		}
		return "", &HeartbeatMutationFailure{ID: m.ID, Error: outcome.Error}
	}
	if d.hostUpgradeDurableBroken {
		errText := d.hostUpgradeDurableError
		if strings.TrimSpace(errText) == "" {
			errText = "host.upgrade refused: durable state unavailable"
		}
		outcome := hostUpgradeOutcome{Applied: false, Error: errText, CompletedAt: now.UTC().Format(time.RFC3339)}
		d.hostUpgradeDone[m.ID] = outcome
		d.hostUpgradeMu.Unlock()
		return "", &HeartbeatMutationFailure{ID: m.ID, Error: errText}
	}
	if _, ok := d.hostUpgradeInFlight[m.ID]; ok {
		d.hostUpgradeMu.Unlock()
		return "", nil
	}
	// A terminal busy refusal is a defined durable outcome, not a transient
	// skip: if this failure ACK is lost, a redelivery after contention
	// clears must replay the same refusal instead of executing a request
	// already reported refused.
	if len(d.hostUpgradeInFlight) > 0 {
		errText := "host.upgrade already in progress"
		outcome := hostUpgradeOutcome{Applied: false, Error: errText, CompletedAt: now.UTC().Format(time.RFC3339)}
		d.hostUpgradeDone[m.ID] = outcome
		if persistErr := persistHostUpgradeOutcome(statePath, m.ID, outcome); persistErr != nil {
			d.hostUpgradeDurableBroken = true
			d.hostUpgradeDurableError = persistErr.Error()
		}
		d.hostUpgradeMu.Unlock()
		return "", &HeartbeatMutationFailure{ID: m.ID, Error: errText}
	}
	d.lifecycleMu.Lock()
	lifecycleBusy := d.lifecycleOwner != nil
	d.lifecycleMu.Unlock()
	if lifecycleBusy {
		errText := "host.upgrade refused: lifecycle busy"
		outcome := hostUpgradeOutcome{Applied: false, Error: errText, CompletedAt: now.UTC().Format(time.RFC3339)}
		d.hostUpgradeDone[m.ID] = outcome
		if persistErr := persistHostUpgradeOutcome(statePath, m.ID, outcome); persistErr != nil {
			d.hostUpgradeDurableBroken = true
			d.hostUpgradeDurableError = persistErr.Error()
		}
		d.hostUpgradeMu.Unlock()
		return "", &HeartbeatMutationFailure{ID: m.ID, Error: errText}
	}
	if err := parseHostUpgradeParams(m, now); err != nil {
		outcome := hostUpgradeOutcome{Applied: false, Error: err.Error(), CompletedAt: now.UTC().Format(time.RFC3339)}
		d.hostUpgradeDone[m.ID] = outcome
		if persistErr := persistHostUpgradeOutcome(statePath, m.ID, outcome); persistErr != nil {
			d.hostUpgradeDurableBroken = true
			d.hostUpgradeDurableError = persistErr.Error()
		}
		d.hostUpgradeMu.Unlock()
		return "", &HeartbeatMutationFailure{ID: m.ID, Error: err.Error()}
	}
	if !hostUpgradeOptedIn(d.configSnapshotForUpgrade()) {
		err := "host.upgrade refused: machine-owner update consent is not configured (no signed source)"
		outcome := hostUpgradeOutcome{Applied: false, Error: err, CompletedAt: now.UTC().Format(time.RFC3339)}
		d.hostUpgradeDone[m.ID] = outcome
		if persistErr := persistHostUpgradeOutcome(statePath, m.ID, outcome); persistErr != nil {
			d.hostUpgradeDurableBroken = true
			d.hostUpgradeDurableError = persistErr.Error()
		}
		d.hostUpgradeMu.Unlock()
		return "", &HeartbeatMutationFailure{ID: m.ID, Error: err}
	}
	// Without a configured update source the production updater never checks
	// a release: it short-circuits to already-up-to-date with the previously
	// running binary still draining. That disabled response must refuse
	// without a swap, never ACK applied.
	if !d.hostUpgradeSourceConfigured() {
		err := "host.upgrade refused: update source is not configured"
		outcome := hostUpgradeOutcome{Applied: false, Error: err, CompletedAt: now.UTC().Format(time.RFC3339)}
		d.hostUpgradeDone[m.ID] = outcome
		if persistErr := persistHostUpgradeOutcome(statePath, m.ID, outcome); persistErr != nil {
			d.hostUpgradeDurableBroken = true
			d.hostUpgradeDurableError = persistErr.Error()
		}
		d.hostUpgradeMu.Unlock()
		return "", &HeartbeatMutationFailure{ID: m.ID, Error: err}
	}
	if err := probeHostUpgradeDurability(statePath); err != nil {
		outcome := hostUpgradeOutcome{Applied: false, Error: err.Error(), CompletedAt: now.UTC().Format(time.RFC3339)}
		d.hostUpgradeDone[m.ID] = outcome
		_ = persistHostUpgradeOutcome(statePath, m.ID, outcome)
		d.hostUpgradeMu.Unlock()
		return "", &HeartbeatMutationFailure{ID: m.ID, Error: err.Error()}
	}
	// Durable intent before execution: a process exit between admission and
	// the final outcome must not lose request identity into a second swap.
	// The intent reloads as an interrupted failure, never as completion and
	// never as a fresh validation to a second swap.
	pending := hostUpgradeOutcome{Pending: true, CompletedAt: now.UTC().Format(time.RFC3339)}
	if persistErr := persistHostUpgradeOutcome(statePath, m.ID, pending); persistErr != nil {
		errText := persistErr.Error()
		outcome := hostUpgradeOutcome{Applied: false, Error: errText, CompletedAt: now.UTC().Format(time.RFC3339)}
		d.hostUpgradeDone[m.ID] = outcome
		d.hostUpgradeDurableBroken = true
		d.hostUpgradeDurableError = persistErr.Error()
		d.hostUpgradeMu.Unlock()
		return "", &HeartbeatMutationFailure{ID: m.ID, Error: errText}
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
	ctx, cancel := hostUpgradeBoundedContext(m)
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

	// Consent is a deadline, not a one-time admission check: restart
	// preflight can wait past expiresAt while the background context still
	// permits execution. Revalidate before touching the lifecycle and again
	// after preflight so an expired request refuses without a swap.
	if err := parseHostUpgradeParams(m, time.Now()); err != nil {
		d.finishHostUpgrade(m.ID, hostUpgradeOutcome{
			Applied: false, Error: err.Error(),
			CompletedAt: time.Now().UTC().Format(time.RFC3339),
		})
		return
	}
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
		// When consent lapses during the preflight wait the bounded
		// context expires first: report the consent deadline, not the
		// generic preflight wait, so the refusal names expiry.
		if consentErr := parseHostUpgradeParams(m, time.Now()); consentErr != nil {
			d.finishHostUpgrade(m.ID, hostUpgradeOutcome{
				Applied: false, Error: consentErr.Error(),
				CompletedAt: time.Now().UTC().Format(time.RFC3339),
			})
			return
		}
		d.finishHostUpgrade(m.ID, hostUpgradeOutcome{
			Applied: false, Error: "host.upgrade preflight refused: " + err.Error(),
			CompletedAt: time.Now().UTC().Format(time.RFC3339),
		})
		return
	}
	if err := parseHostUpgradeParams(m, time.Now()); err != nil {
		d.releaseLifecycle(lease)
		d.finishHostUpgrade(m.ID, hostUpgradeOutcome{
			Applied: false, Error: err.Error(),
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
	// Consent bounds the swap itself, not just admission and preflight: a
	// download that runs past expiry must not become an applied outcome,
	// even when the release seam ignores context cancellation.
	if consentErr := parseHostUpgradeParams(m, time.Now()); consentErr != nil {
		d.lifecycleMu.Lock()
		if d.ownsLifecycleLocked(lease) && d.stopGen == nil && d.State() == StateUpdating {
			d.setState(StateDraining)
		}
		d.lifecycleMu.Unlock()
		d.releaseLifecycle(lease)
		d.finishHostUpgrade(m.ID, hostUpgradeOutcome{
			Applied: false, Error: consentErr.Error(),
			Reason: reasonOfHostUpgradeResult(result), Version: versionOfHostUpgradeResult(result),
			CompletedAt: time.Now().UTC().Format(time.RFC3339),
		})
		return
	}
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
	// already-up-to-date is a durable no-swap-needed outcome only when a
	// source was actually checked. A disabled updater (no CDN base) reports
	// the same reason without contacting any release; that path refuses
	// instead of ACKing applied.
	if result.Updated || (result.Reason == "already-up-to-date" && d.hostUpgradeSourceConfigured()) {
		d.finishHostUpgrade(m.ID, hostUpgradeOutcome{
			Applied: true, Updated: result.Updated, Version: result.Version, Reason: result.Reason,
			CompletedAt: time.Now().UTC().Format(time.RFC3339),
		})
		return
	}
	if result.Reason == "already-up-to-date" {
		d.finishHostUpgrade(m.ID, hostUpgradeOutcome{
			Applied: false, Error: "host.upgrade refused: update source is not configured",
			Updated: result.Updated, Version: result.Version, Reason: result.Reason,
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
	// Fail closed when the production updater has no source to check: with
	// no configured CDN base the updater would report already-up-to-date
	// without contacting any release, which must never become an applied ACK.
	cdnBase := effectiveUpdateCDNBase(cfg.AutoUpdate)
	if strings.TrimSpace(cdnBase) == "" {
		return &UpdateResult{Updated: false, Version: d.EffectiveVersion(), Reason: "update-source-not-configured"}, fmt.Errorf("host.upgrade refused: update source is not configured")
	}
	updater := NewUpdater(UpdaterOptions{
		CurrentVersion: d.EffectiveVersion(),
		Config:         cfg.AutoUpdate,
		CDNBase:        cdnBase,
		SkipExit:       true,
	})
	return updater.RunUpdate(ctx)
}

// finishHostUpgrade durably records the outcome before it can ever be ACKed.
// A lost response or process exit therefore cannot manufacture completion:
// the next redelivery answers from the recorded outcome. A persist failure
// marks durability broken so later requests refuse instead of risking a
// second swap from a lost record. A persist failure never leaves an
// applied outcome in memory: the attempt is recorded as a failure so a
// redelivery replays failure instead of ACKing success without a durable
// receipt.
func (d *Daemon) finishHostUpgrade(id string, outcome hostUpgradeOutcome) {
	if strings.TrimSpace(outcome.CompletedAt) == "" {
		outcome.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if outcome.Error == "" && !outcome.Applied {
		outcome.Error = "host.upgrade failed without a swap"
	}
	outcome.Pending = false
	// Serialize the read-modify-rename ledger under the shared mutex:
	// admission persists already hold this mutex, so completing outside it
	// lets a concurrent busy refusal overwrite a success (or vice versa)
	// and lose the durable receipt into a second swap after restart.
	d.hostUpgradeMu.Lock()
	d.initHostUpgradeStateLocked()
	persistErr := persistHostUpgradeOutcome(hostUpgradeStatePath(d.opts.ConfigPath), id, outcome)
	if persistErr != nil {
		d.hostUpgradeDurableBroken = true
		d.hostUpgradeDurableError = persistErr.Error()
		// Fail closed: an outcome that cannot be made durable must not be
		// remembered as applied. Redelivery checks Done before the broken
		// guard, so storing success here would emit applied without the
		// claimed durable receipt.
		outcome = hostUpgradeOutcome{
			Applied:     false,
			Error:       persistErr.Error(),
			Updated:     false,
			Version:     outcome.Version,
			Reason:      outcome.Reason,
			CompletedAt: outcome.CompletedAt,
		}
	}
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
