package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/RenseiAI/donmai/observation"
)

// observationsPolicyOp is the host mutation that pushes a registration's
// observed-item policy.
const observationsPolicyOp = "observations.policy"

// primaryObservationRegistration keys the policy the daemon's own
// heartbeat lane receives. It stays fixed when the worker id rotates.
const primaryObservationRegistration = ""

// ObservationsConfig is the local narrowing block of daemon.yaml.
type ObservationsConfig struct {
	Items []LocalObservationItem `yaml:"items,omitempty" json:"items,omitempty"`
}

// LocalObservationItem narrows one item, matched by (kind, slot, harness).
// GuardPercent only ever raises the pushed guard.
type LocalObservationItem struct {
	Kind         string `yaml:"kind" json:"kind"`
	Slot         string `yaml:"slot" json:"slot"`
	Harness      string `yaml:"harness" json:"harness"`
	Disabled     bool   `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	GuardPercent *int   `yaml:"guardPercent,omitempty" json:"guardPercent,omitempty"`
}

// observationSlotKey matches a policy item to an observed item. It never
// uses itemKey: a control plane keeps only its own keyed hash of it.
type observationSlotKey struct{ kind, slot, harness string }

// observationPolicyItem is one item of the pushed policy payload.
type observationPolicyItem struct {
	Kind                   string `json:"kind"`
	Slot                   string `json:"slot"`
	Harness                string `json:"harness"`
	Enabled                bool   `json:"enabled"`
	GuardPercent           int    `json:"guardPercent"`
	AllowCredits           bool   `json:"allowCredits"`
	AllowOverage           bool   `json:"allowOverage"`
	AllowAPIBilledFallback bool   `json:"allowApiBilledFallback"`
}

// effectiveObservationPolicy is the pushed item after local narrowing.
// An item the pushed policy omits is disabled.
type effectiveObservationPolicy = observationPolicyItem

// observationPolicies holds each registration's pushed policy. A
// registration with no entry has had none pushed: no observation gate.
type observationPolicies struct {
	mu             sync.RWMutex
	byRegistration map[string]map[observationSlotKey]observationPolicyItem
}

// decodeObservationPolicy validates a policy payload into its item map.
func decodeObservationPolicy(raw json.RawMessage) (map[observationSlotKey]observationPolicyItem, error) {
	var payload struct {
		Version int                     `json:"version"`
		Items   []observationPolicyItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if payload.Version != 1 {
		return nil, fmt.Errorf("unsupported policy version %d", payload.Version)
	}
	out := make(map[observationSlotKey]observationPolicyItem, len(payload.Items))
	for _, it := range payload.Items {
		key := observationSlotKey{it.Kind, it.Slot, it.Harness}
		switch {
		case it.Kind != observation.KindSubscriptionLogin:
			return nil, fmt.Errorf("unknown kind %q", it.Kind)
		case it.Slot != observation.DefaultSlot && !isHex16(it.Slot):
			return nil, fmt.Errorf("slot %q is neither default nor 16 lowercase hex", it.Slot)
		case subscriptionProviders[it.Harness] == "":
			return nil, fmt.Errorf("unknown harness %q", it.Harness)
		case it.GuardPercent < 0 || it.GuardPercent > 100:
			return nil, fmt.Errorf("guardPercent %d is outside 0-100", it.GuardPercent)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("item %s/%s/%s is repeated", it.Kind, it.Slot, it.Harness)
		}
		out[key] = it
	}
	return out, nil
}

func isHex16(s string) bool {
	return len(s) == 16 && !slices.ContainsFunc([]byte(s), func(c byte) bool { return (c < '0' || c > '9') && (c < 'a' || c > 'f') })
}

// applyObservationsPolicy replaces registration's whole policy.
func (d *Daemon) applyObservationsPolicy(registration string, m PendingMutation) error {
	items, err := decodeObservationPolicy(m.Params)
	if err != nil {
		return fmt.Errorf("%s: %w", observationsPolicyOp, err)
	}
	// The push is enforced even when it cannot be persisted; the failed
	// ack makes the control plane push again.
	persistErr := d.persistObservationPolicy(registration, m.Params)
	d.setObservationPolicy(registration, items)
	if persistErr != nil {
		return fmt.Errorf("%s: %w", observationsPolicyOp, persistErr)
	}
	return nil
}

func (d *Daemon) setObservationPolicy(registration string, items map[observationSlotKey]observationPolicyItem) {
	p := &d.observationPolicies
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byRegistration == nil {
		p.byRegistration = map[string]map[observationSlotKey]observationPolicyItem{}
	}
	p.byRegistration[registration] = items
}

// effectiveObservationPolicy returns registration's policy for key after
// local narrowing; pushed is false when the registration has none.
func (d *Daemon) effectiveObservationPolicy(registration string, key observationSlotKey) (eff effectiveObservationPolicy, pushed bool) {
	d.observationPolicies.mu.RLock()
	items, pushed := d.observationPolicies.byRegistration[registration]
	eff = items[key]
	d.observationPolicies.mu.RUnlock()
	if !pushed {
		return eff, false
	}
	if local, ok := d.localObservationItem(key); ok {
		eff.Enabled = eff.Enabled && !local.Disabled
		if local.GuardPercent != nil && *local.GuardPercent > eff.GuardPercent {
			eff.GuardPercent = min(*local.GuardPercent, 100)
		}
	}
	return eff, true
}

// localObservationItem finds the daemon.yaml narrowing entry for key.
func (d *Daemon) localObservationItem(key observationSlotKey) (LocalObservationItem, bool) {
	cfg := d.Config()
	if cfg == nil || cfg.Observations == nil {
		return LocalObservationItem{}, false
	}
	for _, it := range cfg.Observations.Items {
		if (observationSlotKey{it.Kind, it.Slot, it.Harness}) == key {
			return it, true
		}
	}
	return LocalObservationItem{}, false
}

// ApplyObservationMutations returns the observations.* mutation handler
// for one registration. Embedders with additional worker identities wire
// it into that identity's heartbeat lane, so each registration's pushed
// policy stays its own; other ops are ignored. Pair it with
// DrainDeferredScanAcksFor(registration) as that lane's DrainDeferredAcks,
// so each registration's scan completes only on its own beat with its own
// report.
func (d *Daemon) ApplyObservationMutations(registration string) func(context.Context, []PendingMutation) ([]string, []HeartbeatMutationFailure) {
	return func(_ context.Context, mutations []PendingMutation) (applied []string, failures []HeartbeatMutationFailure) {
		for _, m := range mutations {
			var err error
			switch m.Op {
			case observationsPolicyOp:
				err = d.applyObservationsPolicy(registration, m)
			case observationsScanOp:
				err = d.applyObservationsScan(m, registration)
			default:
				continue
			}
			if err != nil {
				// A queued scan is not acknowledged in this beat: like the
				// daemon's own lane, absorb it so the platform redelivers
				// and the redelivery observes the completed outcome.
				if errors.Is(err, errObservationScanDeferred) {
					continue
				}
				failures = append(failures, HeartbeatMutationFailure{ID: m.ID, Error: err.Error()})
				continue
			}
			applied = append(applied, m.ID)
		}
		return applied, failures
	}
}
