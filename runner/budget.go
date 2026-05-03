package runner

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/prompt"
)

// BudgetCap names which cap was breached. Surfaces in
// Result.BudgetReport.CapBreached so dashboards can group breaches by
// kind.
type BudgetCap string

// Cap kind constants. Stable wire values — never repurpose; add new
// caps at the bottom.
const (
	CapDuration  BudgetCap = "max-duration-seconds"
	CapSubAgents BudgetCap = "max-sub-agents"
	CapTokens    BudgetCap = "max-tokens"
)

// FailureBudgetExceeded classifies a session that hit a stage-budget
// cap (REN-1485 / REN-1487 Phase 2 acceptance criterion #4). The
// per-cap details live on Result.BudgetReport.
const FailureBudgetExceeded = "budget-exceeded"

// BudgetReport is the per-session enforcement report. Always present
// when the session was dispatched with a non-nil StageBudget; nil
// otherwise (legacy `agent.dispatch_to_queue` work has no budget to
// report on). Surfaced on Result.BudgetReport so the platform's
// WORK_RESULT consumer can render the breach reason without scraping
// log lines.
type BudgetReport struct {
	// Enforced is true when the runner had a non-nil StageBudget to
	// enforce. False when the session was dispatched with no budget
	// (legacy path) — the report is then a no-op observation record.
	Enforced bool `json:"enforced"`

	// Limits captures the configured caps the runner was enforcing.
	// All-zero means "no caps set, proceed unbounded."
	Limits prompt.StageBudget `json:"limits"`

	// ObservedSubAgents counts the Task tool invocations seen across
	// the session.
	ObservedSubAgents int `json:"observedSubAgents"`

	// ObservedTokens is the cumulative input+output token count
	// observed across all turns. Sourced from per-turn ResultEvent.Cost
	// and the final terminal CostData.
	ObservedTokens int64 `json:"observedTokens"`

	// ObservedDurationSeconds is the wall-clock the session ran for at
	// terminal time (or at the breach point).
	ObservedDurationSeconds int `json:"observedDurationSeconds"`

	// CapBreached names which cap tripped. Empty when the session
	// completed within budget.
	CapBreached BudgetCap `json:"capBreached,omitempty"`

	// BreachDetail is the human-readable "<cap> exceeded: observed=X,
	// limit=Y" string. Empty when no breach.
	BreachDetail string `json:"breachDetail,omitempty"`
}

// BudgetEnforcer is a per-session counter + cap checker. Constructed
// once per Run with the session's StageBudget; mutated only via
// Track* methods (concurrency-safe via atomic counters + a sync.Mutex
// for the breach record).
//
// Wall-clock enforcement is delegated to context.WithTimeout — the
// constructor returns a derived ctx that fires when MaxDurationSeconds
// elapses. Token + sub-agent enforcement is observation-driven: every
// agent.Event the runner streams flows through ObserveEvent, which
// returns a non-nil error when the cap is breached. The runner sees
// the error, classifies the failure as budget-exceeded, and stops the
// provider.
//
// When the dispatch carries no StageBudget (legacy path) New returns a
// no-op Enforcer whose Track* methods always return nil — the runner
// can call them unconditionally.
type BudgetEnforcer struct {
	limits  prompt.StageBudget
	enabled bool

	subAgents atomic.Int64
	tokens    atomic.Int64
	startedAt time.Time

	mu     sync.Mutex
	breach *budgetBreach
}

type budgetBreach struct {
	cap    BudgetCap
	detail string
}

// NewBudgetEnforcer constructs an enforcer for the given budget. A nil
// budget produces a disabled enforcer (no caps, no enforcement) so
// callers can use one code path for legacy + stage dispatch.
func NewBudgetEnforcer(b *prompt.StageBudget, now time.Time) *BudgetEnforcer {
	enf := &BudgetEnforcer{startedAt: now}
	if b == nil {
		return enf
	}
	enf.enabled = true
	enf.limits = *b
	return enf
}

// Enabled reports whether the enforcer has any caps to enforce.
func (e *BudgetEnforcer) Enabled() bool { return e.enabled }

// WithDurationCap returns a derived context that automatically
// cancels when the wall-clock budget elapses. Returns the input ctx
// unchanged when no duration cap is configured. The returned cancel
// is always non-nil and safe to defer.
func (e *BudgetEnforcer) WithDurationCap(parent context.Context) (context.Context, context.CancelFunc) {
	if !e.enabled || e.limits.MaxDurationSeconds <= 0 {
		// Use a noop cancel so callers can defer unconditionally.
		ctx, cancel := context.WithCancel(parent)
		return ctx, cancel
	}
	d := time.Duration(e.limits.MaxDurationSeconds) * time.Second
	return context.WithDeadline(parent, e.startedAt.Add(d))
}

// ObserveEvent updates the running counters from an agent.Event and
// returns a non-nil *BudgetExceededError when the event tripped a cap.
// Callers should treat the error as a clean cancellation: stop the
// provider, classify the failure, write WORK_RESULT.
//
// The enforcer continues to track counters after a breach — the
// caller may receive the same error again on subsequent events. This
// is intentional: the runner's first response is to cancel the stream
// context, but events buffered by the provider may still flow until
// the channel closes.
func (e *BudgetEnforcer) ObserveEvent(ev agent.Event) *BudgetExceededError {
	if !e.enabled {
		return nil
	}

	switch v := ev.(type) {
	case agent.ToolUseEvent:
		// Sub-agent count = number of Task tool invocations. The
		// match is case-insensitive + suffix-tolerant so MCP-namespaced
		// task tools (e.g. `mcp__af__Task`) still count.
		if isTaskTool(v.ToolName) {
			n := e.subAgents.Add(1)
			if cap := e.limits.MaxSubAgents; cap > 0 && n > int64(cap) {
				return e.recordBreach(CapSubAgents,
					fmt.Sprintf("max-sub-agents exceeded: observed=%d limit=%d", n, cap))
			}
		}
	case agent.ResultEvent:
		// Per-turn cost may arrive on intermediate ResultEvents (some
		// providers emit one per turn) or on the terminal one. We sum
		// whatever the provider gives us; the platform's roll-up is
		// authoritative downstream.
		if v.Cost != nil {
			delta := v.Cost.InputTokens + v.Cost.OutputTokens
			if delta > 0 {
				n := e.tokens.Add(delta)
				if cap := e.limits.MaxTokens; cap > 0 && n > cap {
					return e.recordBreach(CapTokens,
						fmt.Sprintf("max-tokens exceeded: observed=%d limit=%d", n, cap))
				}
			}
		}
	}

	return nil
}

// CheckDuration returns a non-nil *BudgetExceededError when the
// wall-clock cap has been breached. The runner calls this from the
// post-loop classification path so a context-deadline-exceeded surfaces
// as the canonical budget breach reason instead of FailureTimeout.
// Returns nil when no cap is configured or the cap has not yet
// tripped.
func (e *BudgetEnforcer) CheckDuration(now time.Time) *BudgetExceededError {
	if !e.enabled || e.limits.MaxDurationSeconds <= 0 {
		return nil
	}
	elapsed := now.Sub(e.startedAt)
	limit := time.Duration(e.limits.MaxDurationSeconds) * time.Second
	if elapsed >= limit {
		return e.recordBreach(CapDuration,
			fmt.Sprintf("max-duration-seconds exceeded: observed=%ds limit=%ds",
				int(elapsed.Seconds()), e.limits.MaxDurationSeconds))
	}
	return nil
}

// Report returns the per-session enforcement record. Safe to call at
// any point; values reflect the latest observations + breach state.
// Always returns a non-nil report — the runner attaches it to
// Result.BudgetReport unconditionally so dashboards can show "no
// budget enforced" sessions distinctly from "budget OK" ones.
func (e *BudgetEnforcer) Report(now time.Time) *BudgetReport {
	rep := &BudgetReport{
		Enforced:                e.enabled,
		Limits:                  e.limits,
		ObservedSubAgents:       int(e.subAgents.Load()),
		ObservedTokens:          e.tokens.Load(),
		ObservedDurationSeconds: int(now.Sub(e.startedAt).Seconds()),
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.breach != nil {
		rep.CapBreached = e.breach.cap
		rep.BreachDetail = e.breach.detail
	}
	return rep
}

// recordBreach is the internal write path for the first breach.
// Subsequent breach attempts are no-ops on the recorded state but
// still return the BudgetExceededError so the caller can see the
// signal regardless of order.
func (e *BudgetEnforcer) recordBreach(cap BudgetCap, detail string) *BudgetExceededError {
	e.mu.Lock()
	if e.breach == nil {
		e.breach = &budgetBreach{cap: cap, detail: detail}
	}
	e.mu.Unlock()
	return &BudgetExceededError{Cap: cap, Detail: detail}
}

// BudgetExceededError is returned by ObserveEvent / CheckDuration when
// a cap has been tripped. The runner classifies the failure as
// FailureBudgetExceeded and stops the provider cleanly.
type BudgetExceededError struct {
	Cap    BudgetCap
	Detail string
}

// Error satisfies the error interface.
func (e *BudgetExceededError) Error() string {
	return fmt.Sprintf("budget exceeded: %s (%s)", e.Cap, e.Detail)
}

// IsBudgetExceeded reports whether err wraps a *BudgetExceededError.
// Convenience helper for the runner's classification fork.
func IsBudgetExceeded(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*BudgetExceededError)
	return ok
}

// isTaskTool reports whether the tool name represents Claude's Task
// (sub-agent) tool. Match is case-insensitive + tolerates MCP-style
// namespace prefixes (e.g. "mcp__af__Task", "task", "Task").
func isTaskTool(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "task" {
		return true
	}
	if strings.HasSuffix(n, "__task") {
		return true
	}
	return false
}
