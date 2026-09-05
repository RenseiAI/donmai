// Package stubagent is the deterministic fake agent that runs INSIDE the
// stub harness's interactive PTY child.
//
// # Why a child process rather than a scripted in-process handle
//
// The parent package (provider/harness/stub) already ships a scripted,
// in-process agent.Provider: it fabricates agent.Event values without
// spawning anything. That is the right tool for unit-testing the runner's
// event handling, and the wrong tool for exercising the daemon, because
// nothing about it is a real session — no PTY, no process group, no
// signal delivery, no shim adoption, no exit status.
//
// This package is the other half. It is a real program the harness spawns
// through the shared provider/harness/ptycli driver, exactly as claude,
// codex, pi and shell are spawned: same ptyhost.Spawn plumbing, same
// session-shim adoption when the controller asks this process to be a
// shim, same SIGTERM -> grace -> SIGKILL stop escalation, same coarse
// Init/Result event mapping. What it does NOT have is a model — its
// behaviour is a scenario script, so an integration environment can drive
// every session-lifecycle branch (clean exit, failing exit, hang,
// stop-ignoring, chatty output, agent-to-agent traffic) with no provider
// credential, no network, and no wall-clock dependence on a model's mood.
//
// # The scenario
//
// Run executes a Scenario: an ordered list of steps, plus a policy that
// says how the program answers a cooperative stop. See Scenario for the
// JSON form and Load for how one is resolved from the environment.
//
// A scenario is deterministic: given the same Scenario value and the same
// input, the program emits byte-identical output. Nothing consults the
// clock for content, and the identifiers on emitted agent-to-agent lines
// are derived from (Name, Seed, step index) rather than drawn at random.
//
// # Environment
//
// The scenario is read from the process environment, because that is the
// one channel every spawn path in this repo already carries end to end
// (agent.Spec.Env -> ptyhost.Spec.Env -> the child). See Load for the
// variable names and their precedence.
package stubagent
