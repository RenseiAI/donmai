// Package pi implements the agent.Provider contract against a
// `pi --mode rpc` JSONL-over-stdio subprocess.
//
// Architecture (one child per session — NOT the codex "one app-server, N
// threads" model):
//
//	pi.Provider
//	  └── Spawn(spec) ─▶ child process: `pi --mode rpc` (cmd.Dir = spec.Cwd)
//	        ├── JSONL commands  in  (prompt / steer / follow_up / abort / …)
//	        └── JSONL events    out (agent_start / message_* / tool_* / agent_end / …)
//
// pi's own per-process session model makes multiplexing a non-feature (the
// experimental `pi-orchestrator` is deliberately not used); one child per
// session buys worktree/credential isolation for free. This mirrors the
// codex/ subprocess-RPC shape (rpc.go ≈ jsonrpc.go, handle.go ≈ handle.go,
// policy.go generalizes approval.go) but over pi's simpler LF-delimited
// JSONL framing rather than JSON-RPC 2.0.
//
// # Wire-shape provenance (READ THIS)
//
// pi was NOT installed on the authoring host (`pi --version` absent), so
// every pi command/event shape in this package is transcribed from the
// design doc (runs/2026-07-21-open-harness-strategy/09-design-pi-adapter.md)
// and its research/pi.md source, NOT verified against a running binary. The
// donmai-smokes step20 lane (harness/pi_install.go, pinned binary in CI) is
// the authority that validates these shapes end-to-end; until it accrues
// green history the pi matrix cells stay Stability:"experimental",
// Smoked:false and the runner registry does NOT enable pi for non-stub
// dispatch (DEC-2/DEC-3). The pinned version lives in probe.go
// (PinnedVersion) and the generated matrix binaryPins section.
//
// # The trust boundary (§5 of the design — load-bearing, quoted verbatim)
//
// pi runs tools with the full permissions of the spawning user and ships NO
// permission system, no sandbox, no MCP. Any Rensei-side trust boundary must
// be built and owned entirely by Rensei. This package builds it as an
// in-process policy boundary with three layers, all fail-closed:
//
//  1. Tool constraint via extension overrides (extension.go): the embedded,
//     pinned donmai policy extension (compiled INTO the donmai binary via an
//     embed directive, never fetched) overrides EVERY built-in tool — read,
//     write, edit, bash and the read-only variants. Each overridden tool
//     serializes
//     the intended call (tool, args, resolved paths, cwd), raises a
//     structured extension_ui_request over stdio, blocks until the Go side
//     answers allow/deny, and on allow delegates to the original tool; on
//     deny returns a tool-error string carrying the reason so the model sees
//     WHY (mirroring codex ApprovalDecision.Reason).
//
//  2. Go-side adjudication + handshake (policy.go, extension.go): policy.go
//     is the codex approval.go engine generalized — built-in safety-deny
//     regexes first, then path containment (writes/edits outside Spec.Cwd
//     denied unless an AllowPattern covers them; reads outside cwd denied by
//     default for autonomous sessions), then Spec allow/deny patterns in the
//     Claude grammar, then DefaultDecision. On load the extension emits a
//     donmai.handshake carrying its SHA; the provider verifies the SHA
//     against the embedded payload and replies with a nonce carried on every
//     subsequent round-trip. NO handshake within the timeout ⇒ no prompt is
//     ever sent, spawn fails closed ("policy extension failed to load").
//     Handshake SHA mismatch ⇒ session killed. This closes both the "pi
//     loaded a stale/different extension" hole and the "session ran with no
//     policy at all" hole.
//
//  3. Integrity monitors (handle.go, fail-closed at runtime): an
//     extension_error referencing the donmai extension aborts the session
//     (ErrorEvent{Code:"policy_extension_failed"}) rather than continuing
//     unguarded; a tool_execution_start for a built-in tool WITHOUT a
//     preceding adjudication round-trip for the same call id is a policy
//     bypass ⇒ session aborted; the child env is allowlist-composed so a
//     fleet box's personal ~/.pi credentials and blocklisted host secrets
//     are never visible to fleet sessions.
//
// What this deliberately does NOT claim: OS-level sandboxing. The policy
// extension is an in-process boundary — a hostile MODEL OUTPUT is contained
// (it can only call overridden tools), but a hostile TOOL EXECUTION still
// runs as the user. OS/sandbox-family enforcement stays the sandbox provider
// family's job (E2B/container cells), unchanged. Do not mistake this
// extension for a sandbox.
//
// # Package layout
//
//   - doc.go            — this charter + the §5 trust-boundary statement
//   - manifest.go       — HarnessManifest (§3) + Capabilities projection
//   - pi.go             — Provider: New (probe + version pin), Spawn, Resume, Shutdown
//   - probe.go          — version-pin bounds + construction-time enforcement
//   - rpc.go            — JSONL client: LF-framed command writer / event reader
//   - handle.go         — per-session Handle: event pump, Inject (steer/follow_up), Stop
//   - event_mapping.go  — pi event union → agent.Event (§4)
//   - policy.go         — trust-boundary adjudicator (generalized codex approval.go)
//   - extension.go      — materialize + SHA-verify the embedded policy extension (§5.2)
//   - spec_translation.go — agent.Spec → argv/env/config
package pi
