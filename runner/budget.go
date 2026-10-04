package runner

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
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
// cap before its work was delivered. The per-cap details live on
// Result.BudgetReport and Result.BudgetBreach.
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
	// A nil sub-agent cap (absent) means "not enforced"; an
	// explicit zero means "no sub-agents allowed".
	Limits prompt.StageBudget `json:"limits"`

	// ObservedSubAgents counts the sub-agent delegation tool
	// invocations seen across the session.
	ObservedSubAgents int `json:"observedSubAgents"`

	// ObservedTokens is the cumulative input+output token count
	// observed across all turns: every ResultEvent.Cost, counting each
	// token once even when a harness reports a running total (see
	// BudgetEnforcer.resultIncrementLocked), plus the per-call usage the harness
	// reported after its last ResultEvent — the model calls of a turn the
	// runner stopped at the cap, which no ResultEvent will reconcile. It
	// equals the input+output tokens of Result.Cost.
	ObservedTokens int64 `json:"observedTokens"`

	// ObservedDurationSeconds is the wall-clock the session ran for at
	// terminal time (or at the breach point).
	ObservedDurationSeconds int `json:"observedDurationSeconds"`

	// CapBreached names which cap tripped. Empty when the session
	// stayed within budget. Set on a completed session too, when the cap
	// stopped it after its work was delivered.
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
// the error, stops the provider, and ends the session: completed with
// the breach recorded when the work was already delivered, otherwise
// failed as budget-exceeded.
//
// The token meter runs during a turn, not only at its end: it counts the
// usage a harness reports for each model call (a non-aggregate
// LlmCallEvent) as the call completes, and reconciles it against the
// turn's ResultEvent cost. So a turn that runs past the cap is seen at the
// model call that crossed it, and the runner stops the session at the next
// turn boundary instead of when the turn ends. The meter also marks the
// point — four fifths of MaxTokens — at which the runner asks the agent to
// wrap up (wrapUpDue).
//
// The enforcer is also the session's usage meter: it meters every event
// whether or not a budget is set, and the runner reports its total as
// Result.Cost, so the reported cost and the budget meter never disagree.
//
// When the dispatch carries no StageBudget (legacy path) New returns an
// Enforcer with no caps whose ObserveEvent always returns nil — the runner
// can call it unconditionally.
type BudgetEnforcer struct {
	limits  prompt.StageBudget
	enabled bool

	subAgents atomic.Int64
	startedAt time.Time

	// midTurnWrapUp is set when the session's harness takes a message into
	// a turn that is still running (a steer channel), so the wrap-up
	// request can reach the agent before its next model call instead of
	// waiting for the turn to end. Set once by the runner before the stream
	// starts.
	midTurnWrapUp bool

	mu     sync.Mutex
	breach *budgetBreach

	// Usage meter, guarded by mu. metered is the reconciled usage: the sum
	// of every ResultEvent cost increment (resultIncrementLocked). pending is the
	// harness-reported per-call usage (non-aggregate LlmCallEvents) since
	// the last ResultEvent that carried a cost — its NumTurns counts those
	// calls; lastResult is that ResultEvent's cost.
	metered    agent.CostData
	pending    agent.CostData
	lastResult agent.CostData

	// wrapUp is the wrap-up request's state, guarded by mu.
	wrapUp wrapUpState
}

// wrapUpState is where a session stands with the wrap-up request.
type wrapUpState int

const (
	// wrapUpNotDue: the meter has not reached the wrap-up point.
	wrapUpNotDue wrapUpState = iota
	// wrapUpPending: the meter reached it; the request is not delivered yet.
	wrapUpPending
	// wrapUpDelivered: the agent has been asked to wrap up.
	wrapUpDelivered
)

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
//
// It can only ever TIGHTEN parent: context.WithDeadline never extends a
// parent's deadline, so the effective cap is min(parent, budget). A budget
// longer than the run context's own bound is therefore silently truncated to
// it — which is why a caller holding a dispatched budget must also size
// [Options.MaxSessionDuration] from that budget rather than relying on this
// enforcer to widen anything.
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
// The token cap is checked against the live meter (observedLocked) on
// every per-call usage event and every ResultEvent, so it can trip in the
// middle of a turn. The runner then lets the turn reach its next boundary
// before it stops the provider (consumeEvents).
//
// The enforcer continues to track counters after a breach — the
// caller may receive the same error again on subsequent events. This
// is intentional: the runner's first response is to cancel the stream
// context, but events buffered by the provider may still flow until
// the channel closes.
func (e *BudgetEnforcer) ObserveEvent(ev agent.Event) *BudgetExceededError {
	switch v := ev.(type) {
	case agent.ToolUseEvent:
		// Sub-agent count = number of sub-agent delegation tool
		// invocations ("Task" or "Agent"). The match is
		// case-insensitive + suffix-tolerant so MCP-namespaced
		// tools (e.g. `mcp__af__Task`) still count. An explicit
		// zero cap means no sub-agents are allowed — the first
		// counted call breaches — while an absent (nil) cap is
		// not enforced.
		if isSubAgentTool(v.ToolName) {
			n := e.subAgents.Add(1)
			if limit := e.limits.MaxSubAgents; e.enabled && limit != nil && n > int64(*limit) {
				return e.recordBreach(CapSubAgents,
					fmt.Sprintf("max-sub-agents exceeded: observed=%d limit=%d", n, *limit))
			}
		}
	case agent.LlmCallEvent:
		// Per-call usage is the live meter between ResultEvents, and the
		// evidence resultIncrementLocked needs to recognize a running total.
		// Aggregate/synthetic events are derived FROM a ResultEvent
		// (runtime/span), so they are neither new usage nor evidence.
		if !v.Synthetic && v.UsageSource != agent.LlmUsageAggregate {
			e.mu.Lock()
			e.pending.InputTokens += v.InputTokens
			e.pending.OutputTokens += v.OutputTokens
			e.pending.CachedInputTokens += v.CachedInputTokens
			e.pending.NumTurns++
			breach := e.checkTokensLocked()
			e.mu.Unlock()
			return breach
		}
	case agent.ResultEvent:
		// A run can see several ResultEvents: a steering or memory-inject
		// turn re-consumes the stream after the first terminal.
		e.mu.Lock()
		var breach *BudgetExceededError
		if e.resultIncrementLocked(v.Cost) > 0 {
			breach = e.checkTokensLocked()
		}
		e.mu.Unlock()
		return breach
	}

	return nil
}

// checkTokensLocked checks the live token meter against the token cap:
// it marks the wrap-up point the first time the meter reaches it, and
// returns the breach while the meter is over the cap. Called with e.mu
// held, after the meter moved.
func (e *BudgetEnforcer) checkTokensLocked() *BudgetExceededError {
	limit := e.limits.MaxTokens
	if !e.enabled || limit <= 0 {
		return nil
	}
	n := e.observedLocked()
	if e.wrapUp == wrapUpNotDue && n >= wrapUpTokens(limit) {
		e.wrapUp = wrapUpPending
	}
	if n <= limit {
		return nil
	}
	detail := fmt.Sprintf("max-tokens exceeded: observed=%d limit=%d", n, limit)
	if e.breach == nil {
		e.breach = &budgetBreach{cap: CapTokens, detail: detail}
	}
	return &BudgetExceededError{Cap: CapTokens, Detail: detail}
}

// wrapUpTokens is the token count at which the runner asks the agent to
// wrap up: four fifths of the token cap, leaving the last fifth to commit,
// push, open the pull request and write the turn result.
func wrapUpTokens(limit int64) int64 {
	return limit - limit/5
}

// observedLocked is the live token meter: the reconciled usage plus the
// per-call usage reported since the last ResultEvent. Called with e.mu held.
func (e *BudgetEnforcer) observedLocked() int64 {
	return e.metered.InputTokens + e.metered.OutputTokens + e.pending.InputTokens + e.pending.OutputTokens
}

// resultIncrementLocked returns how many NEW tokens a ResultEvent's cost adds
// to the session, and adds the new usage to the meter. Harnesses differ in
// what that cost means once a run has more than one ResultEvent: some report
// the usage since the previous one, others (pi, codex, gemini) report the
// handle's running total, so a second ResultEvent repeats every token of the
// first. Summing running totals double-counted — a steered pi session that
// spent 1.46M tokens was measured at 2.78M, recording a breach of a 1.5M cap
// it never reached.
//
// The two are told apart on evidence, not on the harness's name: when the
// per-call usage the harness reported since the previous ResultEvent is
// exactly the difference between this total and that one, the new total
// provably repeats the earlier usage, and only the difference is new. In
// every other case the whole cost is counted, exactly as before this rule
// existed, so a harness without per-call usage — or with per-call usage that
// does not reconcile — keeps its previous accounting and is never
// under-counted. A counter that restarts (a resumed handle) reconciles
// against zero, not against the previous handle, so it is counted whole too.
//
// The same increment — whole, or the difference field by field — is added to
// every field of the meter (tokens, cached tokens, dollars, turns), so the
// cost the runner reports counts each turn once, just as the token cap does.
// Called with e.mu held.
func (e *BudgetEnforcer) resultIncrementLocked(cost *agent.CostData) int64 {
	if cost == nil {
		// No cost to reconcile: keep the per-call evidence for the next
		// ResultEvent that carries one.
		return 0
	}
	total := cost.InputTokens + cost.OutputTokens
	previous := e.lastResult.InputTokens + e.lastResult.OutputTokens
	callTokens := e.pending.InputTokens + e.pending.OutputTokens
	increment := *cost
	if previous > 0 && e.pending.NumTurns > 0 && total == previous+callTokens {
		increment = costDifference(*cost, e.lastResult)
	}
	e.metered = addCost(e.metered, increment)
	e.pending, e.lastResult = agent.CostData{}, *cost
	return increment.InputTokens + increment.OutputTokens
}

// addCost returns the field-by-field sum of a and b.
func addCost(a, b agent.CostData) agent.CostData {
	return agent.CostData{
		InputTokens:       a.InputTokens + b.InputTokens,
		OutputTokens:      a.OutputTokens + b.OutputTokens,
		CachedInputTokens: a.CachedInputTokens + b.CachedInputTokens,
		TotalCostUsd:      a.TotalCostUsd + b.TotalCostUsd,
		NumTurns:          a.NumTurns + b.NumTurns,
	}
}

// costDifference returns what a running total adds over the previous one,
// field by field; a field that did not grow adds nothing.
func costDifference(total, previous agent.CostData) agent.CostData {
	return agent.CostData{
		InputTokens:       max(total.InputTokens-previous.InputTokens, 0),
		OutputTokens:      max(total.OutputTokens-previous.OutputTokens, 0),
		CachedInputTokens: max(total.CachedInputTokens-previous.CachedInputTokens, 0),
		TotalCostUsd:      max(total.TotalCostUsd-previous.TotalCostUsd, 0),
		NumTurns:          max(total.NumTurns-previous.NumTurns, 0),
	}
}

// usageSnapshot is the session's cumulative usage total as the meter
// counted it: every ResultEvent increment plus the per-call usage reported
// since the last one. The zero value means "nothing metered yet". Its
// input+output tokens equal Report's ObservedTokens.
func (e *BudgetEnforcer) usageSnapshot() (in, out, cached int64, costUsd float64) {
	cost := e.cost()
	if cost == nil {
		return 0, 0, 0, 0
	}
	return cost.InputTokens, cost.OutputTokens, cost.CachedInputTokens, cost.TotalCostUsd
}

// cost is the session's usage as the meter counted it: every ResultEvent
// increment plus the per-call usage reported since the last one (which has
// tokens and calls but no dollar amount). nil when nothing was metered. Its
// input+output tokens equal Report's ObservedTokens.
func (e *BudgetEnforcer) cost() *agent.CostData {
	e.mu.Lock()
	defer e.mu.Unlock()
	total := addCost(e.metered, e.pending)
	if total == (agent.CostData{}) {
		return nil
	}
	return &total
}

// wrapUpDue reports whether the session reached the wrap-up point and the
// agent has not yet been asked to wrap up.
func (e *BudgetEnforcer) wrapUpDue() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.wrapUp == wrapUpPending
}

// wrapUpSent records that the wrap-up request reached the agent.
func (e *BudgetEnforcer) wrapUpSent() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.wrapUp = wrapUpDelivered
}

// breached returns the first recorded breach as an error, or nil while the
// session is within its budget.
func (e *BudgetEnforcer) breached() *BudgetExceededError {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.breach == nil {
		return nil
	}
	return &BudgetExceededError{Cap: e.breach.cap, Detail: e.breach.detail}
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
		ObservedDurationSeconds: int(now.Sub(e.startedAt).Seconds()),
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rep.ObservedTokens = e.observedLocked()
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
func (e *BudgetEnforcer) recordBreach(breached BudgetCap, detail string) *BudgetExceededError {
	e.mu.Lock()
	if e.breach == nil {
		e.breach = &budgetBreach{cap: breached, detail: detail}
	}
	e.mu.Unlock()
	return &BudgetExceededError{Cap: breached, Detail: detail}
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

// isSubAgentTool reports whether the tool name represents a sub-agent
// delegation tool ("Task" or "Agent"). Match is case-insensitive +
// tolerates MCP-style namespace prefixes (e.g. "mcp__af__Task",
// "mcp__af__Agent", "task", "Agent").
func isSubAgentTool(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "task" || n == "agent" {
		return true
	}
	if strings.HasSuffix(n, "__task") || strings.HasSuffix(n, "__agent") {
		return true
	}
	return false
}
