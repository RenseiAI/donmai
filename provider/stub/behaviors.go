package stub

// Behavior names a pre-scripted event sequence the stub Handle emits.
//
// Behavior values are read from Spec.Env["RENSEI_STUB_MODE"] (the
// F.1.1 §3.3 knob) or Spec.ProviderConfig["stub.behavior"] (the typed
// v0.5.0 knob). When neither is set the provider falls back to
// BehaviorSucceedWithPR.
type Behavior string

// Behavior constants.
//
// The first six map 1:1 to F.1.1 §3.3 modes. The last two
// (BehaviorMidStreamError, BehaviorInjectTest) are F.2.2 additions
// requested by the runner unit tests in F.2.5.
const (
	// BehaviorSucceedWithPR emits the canonical successful event
	// sequence terminating in ResultEvent{Success: true}. The smoke
	// harness asserts on this byte-exact sequence (F.1.1 §3.3).
	BehaviorSucceedWithPR Behavior = "succeed-with-pr"

	// BehaviorFailOnClone emits an InitEvent followed by an
	// ErrorEvent with code "clone_failed", then closes. Exercises
	// the runner's spawn-failure / non-terminal-error path.
	BehaviorFailOnClone Behavior = "fail-on-clone"

	// BehaviorHangThenTimeout emits InitEvent + SystemEvent then
	// blocks on context cancellation. Exercises the runner's
	// MaxDuration / timeout path.
	BehaviorHangThenTimeout Behavior = "hang-then-timeout"

	// BehaviorSilentFail emits an InitEvent and closes the channel
	// without a terminal ResultEvent. Exercises the runner's
	// synthetic-error path (F.1.1 §3.3 silent-fail mode).
	BehaviorSilentFail Behavior = "silent-fail"

	// BehaviorSlowTool emits a ToolUseEvent followed by N
	// ToolProgressEvent ticks before a ToolResultEvent. The progress
	// tick count comes from Spec.ProviderConfig["stub.progressTicks"]
	// (default 3).
	BehaviorSlowTool Behavior = "slow-tool"

	// BehaviorCostOverrun emits a successful Result with a very
	// large TotalCostUsd to exercise cost-cap warnings. F.1.1 §3.3.
	BehaviorCostOverrun Behavior = "cost-overrun"

	// BehaviorMidStreamError emits Init → System → AssistantText →
	// ErrorEvent → close. Exercises mid-session provider crash.
	BehaviorMidStreamError Behavior = "mid-stream-error"

	// BehaviorInjectTest emits Init → System then blocks until the
	// first Handle.Inject call. Each Inject appends an
	// AssistantTextEvent (echoing the injected text) and the first
	// inject also emits the terminal ResultEvent and closes the
	// channel. Exercises the SupportsMessageInjection path.
	BehaviorInjectTest Behavior = "inject-test"
)

// behaviorEnvKey is the legacy F.1.1 §3.3 environment knob.
const behaviorEnvKey = "RENSEI_STUB_MODE"

// behaviorConfigKey is the typed v0.5.0 ProviderConfig knob.
const behaviorConfigKey = "stub.behavior"

// progressTicksConfigKey selects the BehaviorSlowTool tick count.
const progressTicksConfigKey = "stub.progressTicks"

// defaultProgressTicks is the BehaviorSlowTool tick count when no
// override is set.
const defaultProgressTicks = 3

// IsKnown reports whether the supplied behavior name is recognized.
// Unknown behavior names fall back to BehaviorSucceedWithPR at
// Spawn time so misconfigured tests do not silently hang.
func IsKnown(b Behavior) bool {
	switch b {
	case BehaviorSucceedWithPR,
		BehaviorFailOnClone,
		BehaviorHangThenTimeout,
		BehaviorSilentFail,
		BehaviorSlowTool,
		BehaviorCostOverrun,
		BehaviorMidStreamError,
		BehaviorInjectTest:
		return true
	default:
		return false
	}
}
