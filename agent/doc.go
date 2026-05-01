// Package agent declares the provider-agnostic contract for the
// agentfactory-tui multi-provider agent-runner subsystem.
//
// This package is the verbatim Go translation of the legacy TypeScript
// type contract at:
//
//	../agentfactory/packages/core/src/providers/types.ts
//
// It defines the types every provider implementation (claude, codex, stub,
// future spring-ai/a2a/gemini/ollama/opencode/jules/amp) and the runner
// orchestrator depend on. The package is pure types + interfaces with zero
// behavior, no I/O, no business logic, and no dependencies beyond the Go
// standard library and log/slog.
//
// # Public Package
//
// This package is exported at the top level of the module:
//
//	github.com/RenseiAI/agentfactory-tui/agent
//
// It is importable by downstream consumers including rensei-tui without
// requiring an agentfactory-tui dependency cascade. F.1.1 §1 ratifies this
// boundary.
//
// # Wire Format Compatibility
//
// JSON tags use camelCase to match the legacy TS shape verbatim. The JSON
// shape is consumed by readers of QueuedWork.resolvedProfile across the
// fleet. If you change a wire shape (add/remove/rename a JSON field),
// update the F.1.1 design doc at:
//
//	../runs/2026-05-01-wave-6-fleet-iteration/F1.1-runner-contract.md
//
// in the same change so the contract stays single-sourced.
//
// # The Nine Capabilities
//
// Providers declare a Capabilities struct describing which optional
// features they support. The runner gates behavior off these flags
// rather than try-catching unsupported operations. The named capability
// constants (CapMessageInjection, CapSessionResume, ...) align 1:1 with
// the Capabilities struct fields and are used with IsSupported() helper.
//
// # Sealed Event Variants
//
// The Event interface uses the sealed-interface variant pattern: each of
// the 8 event variants (InitEvent, SystemEvent, AssistantTextEvent,
// ToolUseEvent, ToolResultEvent, ToolProgressEvent, ResultEvent, ErrorEvent)
// implements both Kind() EventKind and the unexported isAgentEvent()
// marker. New variants must be added to this package — external packages
// cannot satisfy the interface, which keeps the discriminated union
// closed. For polymorphic JSON decoding use UnmarshalEvent.
package agent
