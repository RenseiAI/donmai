package daemon

import (
	"errors"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/observation"
)

// Observation refusal code and its typed reasons.
const (
	observationUnavailableCode    = "observation_unavailable"
	observationReasonDisabled     = "disabled"
	observationReasonGuardReached = "guard_reached"
	observationReasonExhausted    = "exhausted"
	observationReasonBilling      = "billing_boundary"
)

// observationUnavailableError refuses a claim no observed item can serve.
type observationUnavailableError struct {
	kind, reason string
	until        time.Time
}

func (e *observationUnavailableError) Error() string {
	return "observation unavailable: " + e.kind + " " + e.reason
}

// observationUnavailableReport is the typed NACK detail.
type observationUnavailableReport struct {
	Code   string `json:"code"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	Until  string `json:"until,omitempty"`
}

func observationUnavailableReportForError(err error) *observationUnavailableReport {
	var refused *observationUnavailableError
	if !errors.As(err, &refused) {
		return nil
	}
	out := &observationUnavailableReport{Code: observationUnavailableCode, Kind: refused.kind, Reason: refused.reason}
	if !refused.until.IsZero() {
		out.Until = refused.until.UTC().Format(time.RFC3339)
	}
	return out
}

// selectableObservationSlots is the selection seam: the daemon runs each
// harness from its default config home only, so the default slot is the
// only one it can select. Another slot needs its config home carried into
// the session's harness env before it can be offered here.
func (d *Daemon) selectableObservationSlots(string) []string {
	return []string{observation.DefaultSlot}
}

// ObservationClaimAdmission decides, at claim time, whether host-session
// work for spec's harness may run under registration's pushed policy. It
// selects the first viable slot; with none it refuses with the first
// slot's typed reason. A registration with no pushed policy, or work that
// is not host-session, passes through. Running sessions are never touched.
func (d *Daemon) ObservationClaimAdmission(registration string, spec SessionSpec, now time.Time) error {
	if d == nil || spec.AuthMode != string(agent.AuthHostSession) || spec.Harness == "" {
		return nil
	}
	var account *agent.UsageAccount
	accounts := d.QuotaSnapshot()
	for i := range accounts {
		if accounts[i].Provider == spec.Harness {
			account = &accounts[i]
		}
	}
	var refusal error
	for _, slot := range d.selectableObservationSlots(spec.Harness) {
		eff, pushed := d.effectiveObservationPolicy(registration, observationSlotKey{observation.KindSubscriptionLogin, slot, spec.Harness})
		if !pushed {
			return nil
		}
		err := subscriptionLoginViability(eff, account, now)
		if err == nil {
			return nil
		}
		if refusal == nil {
			refusal = err
		}
	}
	return refusal
}

// subscriptionLoginViability applies one login's policy to its windows.
// A window blocks while used >= 100 - guardPercent and its reset is still
// ahead (a missing reset counts as ahead). A window under 100 blocks as
// guard_reached. When only exhausted windows block, the login is past its
// allowance: with another window still open the billing state is
// allowance and the reason is exhausted; with none open the state is
// unknown, which counts as past a billing boundary and is refused.
// Missing or stale usage admits, like the quota claim gate.
func subscriptionLoginViability(eff effectiveObservationPolicy, account *agent.UsageAccount, now time.Time) error {
	refuse := func(reason string, until time.Time) error {
		return &observationUnavailableError{kind: observation.KindSubscriptionLogin, reason: reason, until: until}
	}
	if !eff.Enabled {
		return refuse(observationReasonDisabled, time.Time{})
	}
	if account == nil || !quotaSnapshotFresh(account.Limits.CheckedAt, now) {
		return nil
	}
	guardLine := 100 - float64(eff.GuardPercent)
	var until time.Time
	guarded, exhausted, open := false, false, false
	for _, w := range account.Limits.Windows {
		if w.UsedPercent < 100 {
			open = true
		}
		reset, ok := parseQuotaInstant(w.ResetsAt)
		if w.UsedPercent < guardLine || (ok && !reset.After(now)) {
			continue
		}
		if ok && reset.After(until) {
			until = reset
		}
		if w.UsedPercent < 100 {
			guarded = true
		} else {
			exhausted = true
		}
	}
	switch {
	case guarded:
		return refuse(observationReasonGuardReached, until)
	case exhausted && open:
		return refuse(observationReasonExhausted, until)
	case exhausted:
		return refuse(observationReasonBilling, until)
	}
	return nil
}
