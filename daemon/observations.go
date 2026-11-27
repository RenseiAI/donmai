package daemon

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/observation"
)

// quotaSlot is the quota state of one config home a harness reads its
// login from. configHome "" is the harness's default home.
type quotaSlot struct {
	configHome string
	accounts   []agent.UsageAccount
}

// subscriptionProviders maps a quota harness onto the subscription
// provider that issues its login.
var subscriptionProviders = map[string]string{
	agent.UsageHarnessCodex:  observation.ProviderOpenAIChatGPT,
	agent.UsageHarnessClaude: observation.ProviderAnthropicClaude,
	agent.UsageHarnessMuse:   observation.ProviderMuse,
}

// ObservationReport composes the heartbeat observations field: a full
// subscription-login report over every quota slot. Nil when the daemon
// has no quota state, mirroring the quota key; nil also when an item
// fails to encode, since a partial full report would mark items gone.
func (d *Daemon) ObservationReport() *observation.Report {
	if d == nil {
		return nil
	}
	slots := append([]quotaSlot{{accounts: d.quota.snapshot()}}, d.extraQuotaSlots()...)
	report, err := subscriptionLoginReport(slots, time.Now(), false)
	if err != nil {
		slog.Warn("daemon: subscription-login observations omitted", "err", err)
		return nil
	}
	return report
}

// extraQuotaSlots is the seam for logins read from config homes other
// than each harness's default. No extra homes are configured yet.
func (d *Daemon) extraQuotaSlots() []quotaSlot { return nil }

// subscriptionLoginReport builds one item per (harness, slot). It returns
// nil, nil when no slot holds quota state. timedOut marks a scan whose
// probes overran: every item then reports readiness unknown.
func subscriptionLoginReport(slots []quotaSlot, now time.Time, timedOut bool) (*observation.Report, error) {
	var items []observation.Item
	for _, slot := range slots {
		for _, account := range slot.accounts {
			item, err := subscriptionLoginItem(slot.configHome, account, now, timedOut)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		return nil, nil
	}
	report, err := observation.NewReport(now, true, []string{observation.KindSubscriptionLogin}, items)
	if err != nil {
		return nil, fmt.Errorf("subscription-login report: %w", err)
	}
	return report, nil
}

// subscriptionLoginItem maps one quota account onto a subscription-login
// item. A probed account id keys the item by account; otherwise, and for
// muse, whose fixed id names a host session rather than an account, the
// item is host-scoped. The raw id never leaves this function unhashed.
func subscriptionLoginItem(configHome string, account agent.UsageAccount, now time.Time, timedOut bool) (observation.Item, error) {
	harness := account.Provider
	provider := subscriptionProviders[harness]
	slot := observation.SlotFor(harness, configHome)
	header := observation.Header{
		Slot:      slot,
		Identity:  observation.IdentityHostScoped,
		ItemKey:   observation.HostScopedItemKey(observation.KindSubscriptionLogin, harness, slot),
		Readiness: observation.Readiness{State: observation.ReadinessUnknown, CheckedAt: now.UTC().Format(time.RFC3339)},
	}
	if account.ID != "" && harness != agent.UsageHarnessMuse {
		header.Identity = observation.IdentityAccount
		header.ItemKey = observation.SubscriptionLoginItemKey(provider, account.ID, "")
	}
	if check := account.AuthCheck; check != nil {
		header.Readiness = observation.Readiness{State: observation.ReadinessReady, CheckedAt: check.CheckedAt}
		if !check.OK {
			header.Readiness.State = observation.ReadinessUnavailable
			header.Readiness.Reasons = []string{"login_refused"}
		}
	}
	if timedOut {
		header.Readiness = observation.Readiness{
			State: observation.ReadinessUnknown, Reasons: []string{"scan_timeout"}, CheckedAt: now.UTC().Format(time.RFC3339),
		}
	}
	plan := strings.ToLower(account.Plan)
	if plan != "" {
		header.Definition = provider + ":" + plan
	}
	attrs := observation.SubscriptionLogin{
		Harness: harness, Provider: provider, Plan: plan,
		Billing: observation.Billing{Mode: observation.BillingUnknown},
	}
	if windows := account.Limits.Windows; len(windows) > 0 {
		allowance := observation.Allowance{ID: account.LimitID, Unit: observation.UnitPercent}
		if allowance.ID == "" {
			allowance.ID = harness
		}
		for _, w := range windows {
			out := observation.Window{ID: w.ID, Kind: string(w.Kind), Used: w.UsedPercent, ResetsAt: w.ResetsAt}
			if w.WindowDurationMins != nil && *w.WindowDurationMins > 0 {
				d := *w.WindowDurationMins
				out.WindowDurationMins = &d
			}
			if w.UsedPercent < 100 {
				attrs.Billing.Mode = observation.BillingAllowance
			}
			allowance.Windows = append(allowance.Windows, out)
		}
		attrs.Allowances = []observation.Allowance{allowance}
	}
	item, err := observation.Encode(header, attrs)
	if err != nil {
		return observation.Item{}, fmt.Errorf("subscription-login %s item: %w", harness, err)
	}
	return item, nil
}
