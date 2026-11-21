package observation

import (
	"errors"
	"fmt"
	"math"
)

// KindSubscriptionLogin is kind 1: a subscription login a harness signs in with.
const KindSubscriptionLogin = "subscription-login"

// Closed vocabularies of the subscription-login kind. Harness ids and
// window kinds are the agent package's UsageHarness* and UsageWindow*.
var (
	harnesses   = []string{"codex", "claude", "muse"}
	windowKinds = []string{"session", "weekly", "monthly", "other"}
)

// Subscription providers, allowance units and billing modes.
const (
	ProviderOpenAIChatGPT   = "openai-chatgpt"
	ProviderAnthropicClaude = "anthropic-claude"
	ProviderMuse            = "muse"

	UnitPercent = "percent"
	UnitTokens  = "tokens"

	BillingAllowance  = "allowance"
	BillingCredits    = "credits"
	BillingOverage    = "overage"
	BillingAPIMetered = "api-metered"
	BillingUnknown    = "unknown"
)

// SubscriptionLogin is the closed attribute schema of a subscription login.
// The account id is not an attribute: it reaches the wire only hashed, as
// the item's itemKey.
type SubscriptionLogin struct {
	Harness    string      `json:"harness" disclosure:"enum"`
	Provider   string      `json:"provider" disclosure:"enum"`
	Plan       string      `json:"plan,omitempty" disclosure:"public"`
	Allowances []Allowance `json:"allowances" disclosure:"public"`
	Billing    Billing     `json:"billing" disclosure:"enum"`
}

// Allowance is one allowance the account holds, with its windows.
type Allowance struct {
	ID      string   `json:"id"`
	Unit    string   `json:"unit"`
	Windows []Window `json:"windows"`
}

// Window is one rolling window of an allowance. Used is a percentage for
// a percent allowance; Limit is set only for a tokens allowance.
type Window struct {
	ID                 string   `json:"id"`
	Kind               string   `json:"kind"`
	Used               float64  `json:"used"`
	Limit              *float64 `json:"limit,omitempty"`
	ResetsAt           string   `json:"resetsAt,omitempty"`
	WindowDurationMins *int     `json:"windowDurationMins,omitempty"`
}

// Billing is what happens past the included allowance.
type Billing struct {
	Mode           string   `json:"mode"`
	CreditsBalance *float64 `json:"creditsBalance,omitempty"`
}

func (SubscriptionLogin) kindID() string { return KindSubscriptionLogin }

func finiteNonNegative(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

func (s SubscriptionLogin) normalize() (Attributes, error) {
	switch {
	case !oneOf(s.Harness, harnesses...):
		return nil, fmt.Errorf("harness %q is not allowed", s.Harness)
	case !oneOf(s.Provider, ProviderOpenAIChatGPT, ProviderAnthropicClaude, ProviderMuse):
		return nil, fmt.Errorf("provider %q is not allowed", s.Provider)
	case s.Plan != "" && !slugPattern.MatchString(s.Plan):
		return nil, errors.New("plan is not a lowercase slug")
	case !oneOf(s.Billing.Mode, BillingAllowance, BillingCredits, BillingOverage, BillingAPIMetered, BillingUnknown):
		return nil, fmt.Errorf("billing mode %q is not allowed", s.Billing.Mode)
	case s.Billing.CreditsBalance != nil && !finiteNonNegative(*s.Billing.CreditsBalance):
		return nil, errors.New("credits balance is not a finite non-negative number")
	}
	out := s
	out.Allowances = make([]Allowance, 0, len(s.Allowances))
	for _, a := range s.Allowances {
		if !slugPattern.MatchString(a.ID) || !oneOf(a.Unit, UnitPercent, UnitTokens) {
			return nil, fmt.Errorf("allowance %q has a bad id or unit", a.ID)
		}
		windows := make([]Window, 0, len(a.Windows))
		for _, w := range a.Windows {
			if err := w.validate(a.Unit); err != nil {
				return nil, fmt.Errorf("allowance %q: %w", a.ID, err)
			}
			windows = append(windows, w)
		}
		a.Windows = windows
		out.Allowances = append(out.Allowances, a)
	}
	return out, nil
}

func (w Window) validate(unit string) error {
	switch {
	case !slugPattern.MatchString(w.ID):
		return errors.New("window id is not a lowercase slug")
	case !oneOf(w.Kind, windowKinds...):
		return fmt.Errorf("window kind %q is not allowed", w.Kind)
	case !finiteNonNegative(w.Used) || (unit == UnitPercent && w.Used > 100):
		return errors.New("window used is out of range")
	case w.Limit != nil && (unit == UnitPercent || !finiteNonNegative(*w.Limit)):
		return errors.New("window limit is set on a percent window or out of range")
	case w.ResetsAt != "" && !validTime(w.ResetsAt):
		return errors.New("window resetsAt is not RFC3339")
	case w.WindowDurationMins != nil && *w.WindowDurationMins <= 0:
		return errors.New("window duration is not positive")
	}
	return nil
}
