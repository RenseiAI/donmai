// Package stub is the deterministic fake harness. It has two spawn modes,
// and the difference between them is the point.
//
// # Headless mode (Spec.Interactive == nil)
//
// The original mode: a fully in-process implementation of agent.Provider for
// tests, the F.4 smoke harness, and any caller that needs a real Provider
// with no external runtime dependencies. It calls out to no binary, network
// endpoint, or filesystem, and fabricates a pre-scripted agent.Event
// sequence. That is the right shape for unit-testing the runner's event
// handling and the wrong shape for exercising the daemon, because nothing
// about it is a real session — no PTY, no process group, no signal delivery,
// no shim adoption, no exit status.
//
// # Interactive mode (Spec.Interactive != nil)
//
// The other half: a real PTY child running the deterministic fake AGENT
// (provider/harness/stub/stubagent), spawned through the SHARED
// provider/harness/ptycli driver — the same call claude, codex, pi and shell
// make. Same ptyhost plumbing, same session-shim adoption, same
// SIGTERM -> grace -> SIGKILL stop escalation, same coarse Init/Result event
// mapping. What it lacks is a model: its behaviour is a scenario script, so
// an integration environment can drive every session-lifecycle branch with no
// provider credential and no network. See interactive.go and the package
// README for the scenario format and the fault knobs.
//
// The remainder of this comment describes the headless mode.
//
// Each Spawn returns a Handle that emits a pre-scripted
// sequence of agent.Event values driven by a behavior name read from
// either Spec.Env["DONMAI_STUB_MODE"] (the legacy F.1.1 §3.3 knob) or
// Spec.ProviderConfig["stub.behavior"] (the v0.5.0 typed-config knob).
// When neither is set the provider runs in BehaviorSucceedWithPR mode.
//
// # Behavior matrix
//
// The behavior name selects exactly one event-sequence script. F.1.1
// §3.3 defines the canonical mode list; this package implements them
// verbatim plus two extras (mid-stream-error, inject-test) requested by
// the F.2.2 dispatch.
//
//	BehaviorSucceedWithPR  : Init → System → AssistantText → ToolUse → ToolResult → AssistantText(WORK_RESULT:passed) → Result(success)
//	BehaviorFailOnClone    : Init → ErrorEvent(code=clone_failed) → close
//	BehaviorHangThenTimeout: Init → System → wait on ctx → close on cancel
//	BehaviorSilentFail     : Init → close (no terminal Result, exercises synthetic-error path)
//	BehaviorSlowTool       : Init → ToolUse → N×ToolProgress → ToolResult → Result
//	BehaviorCostOverrun    : Init → AssistantText → Result(success, cost=$999.99)
//	BehaviorMidStreamError : Init → System → AssistantText → ErrorEvent → close
//	BehaviorInjectTest     : Init → System → block on Inject → first inject → Result → close
//
// # Capabilities
//
// All optional flags default to true so the runner exercises every
// gating path when wired against the stub. Tests may override by
// constructing a Provider via New(WithCapabilities(...)).
//
// # Why this package exists
//
// F.4 ships a smoke harness that drives the runner end-to-end without
// invoking the claude or codex CLI. The smoke harness asserts on the
// byte-exact event sequence emitted by BehaviorSucceedWithPR per F.1.1
// §3.3. The runner unit tests in F.2.5 also use this provider to drive
// every failure-mode branch deterministically.
//
// # Adding a new behavior
//
//  1. Add the constant to the Behavior block in behaviors.go.
//  2. Add a script function (e.g. scriptMyBehavior) that emits Events
//     onto the channel and returns when done.
//  3. Wire the constant into the switch in (*provider).run.
//  4. Add a row to the unit_test.go table.
//  5. Update the behavior matrix in this doc and in README.md, and
//     mirror the change to F.1.1 §3.3 if the new behavior is part of
//     the locked smoke contract.
package stub
