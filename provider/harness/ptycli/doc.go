// Package ptycli is the shared interactive PTY spawn-mode driver (W4;
// interactive-attach-v1 protocol, donmai-architecture/protocol/
// interactive-attach-v1.md; owning ADR
// ADR-2026-07-12-interactive-pty-session-host.md).
//
// It plays the same role for the interactive spawn mode that
// provider/harness/clijsonl plays for the headless CLI-JSONL loop: one
// shared Spawn + Handle so every harness that declares
// agent.HarnessCaps.SupportsInteractivePTY (today: claude, codex, shell, pi,
// stub — tomorrow, whichever P8 harness flips the flag) drives the exact same
// ptyhost.Spawn plumbing and the exact same coarse agent.Handle event
// mapping, instead of re-implementing it per harness.
//
// # Event semantics (program decision D4)
//
// Interactive mode is deliberately coarse at the agent.Event layer: the
// byte-accurate PTY stream (agent.InteractiveSession) is the product, not
// structured events. A Handle from this package emits exactly one InitEvent
// once the PTY child is up (SessionID stays empty — no harness's interactive
// TUI exposes a provider-native session id on its own stdout; that identity,
// when one exists, belongs to the interactive-attach-v1 session layer the
// composing layer owns, not to this package) and exactly one terminal
// ResultEvent when the PTY child exits (exit code 0 → success, anything else
// → a failure ResultEvent carrying the exit/signal detail). No
// AssistantTextEvent/ToolUseEvent/etc — those require parsing a
// harness-native structured wire format that an interactive TUI does not
// emit.
//
// # Suspend/resume coexistence (P5-WS6 seam)
//
// This package does not wire PreToolUse suspend itself — that is a
// composing-layer concern. What it exposes is the seam: Handle.EmitMarker is
// a passthrough to the underlying ptyhost.Session.EmitMarker, so the
// composing layer can call
// handle.EmitMarker(agent.MarkerApprovalPending) / MarkerApprovalResolved
// (agent/interactive.go) without a type assertion to
// agent.InteractiveCapable first. A suspend is a defined, attach-alive
// state (the PTY, ring, VT, recorder, and every attach stay live) — never a
// wedge.
package ptycli
