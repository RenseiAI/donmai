// Package interview holds the canonical wire-type constants for the
// Interactive Project Interviews feature. These constants are the Go
// mirror of platform/src/lib/interview/wire-types.ts — kept in sync
// manually using the same discipline as internal/credentials/blocklist.go
// and the platform's AGENT_ENV_BLOCKLIST.
//
// Source of truth: platform/src/lib/interview/wire-types.ts and
// runs/2026-06-02-interactive-interviews/01-CONTRACT-FREEZE.md §7.
//
// Every producer and consumer in donmai MUST import from this package
// rather than hard-coding string literals, so a rename is a single-
// package change.
package interview

import "fmt"

// InterviewRunMode is the QueuedWork.Mode value that activates the
// non-terminating interactive interview loop in the runner.
// Matches INTERVIEW_RUN_MODE in wire-types.ts.
const InterviewRunMode = "interview"

// InjectKindMemory is the Kind value for a standard agent-memory
// inject (default / back-compat). An empty Kind is treated as
// InjectKindMemory by all consumers.
// Matches INJECT_KIND_MEMORY in wire-types.ts.
const InjectKindMemory = "memory"

// InjectKindUser is the Kind value for a user-turn inject arriving
// from an interactive interview session.
// Matches INJECT_KIND_USER in wire-types.ts.
const InjectKindUser = "user"

// InterviewCompleteSentinel is the sentinel text the agent emits
// (inside its streamed assistant text) to signal that the interview
// has completed all phases and is ready to hand off to SDLC.
// The runner watches for this string to exit the interview loop.
// Matches INTERVIEW_COMPLETE_SENTINEL in wire-types.ts.
const InterviewCompleteSentinel = "<!-- INTERVIEW_COMPLETE -->"

// InterviewCompletedEvent is the CloudEvent type emitted by
// POST /api/interview/[id]/complete. It triggers the rensei.wait
// signal gate that continues the SDLC workflow.
// Matches INTERVIEW_COMPLETED_EVENT in wire-types.ts.
const InterviewCompletedEvent = "com.rensei.interview.completed"

// InterviewSignalFilterKey is the key in the gate's signalFilter
// map whose value is the interviewId. Used by the platform to match
// the incoming CloudEvent to the waiting workflow instance.
// Matches INTERVIEW_SIGNAL_FILTER_KEY in wire-types.ts.
const InterviewSignalFilterKey = "interviewId"

// TokenChannel returns the Redis pub/sub channel name for
// token-delta frames for the given interview id.
// Matches interviewTokenChannel(id) in wire-types.ts.
//
// Channel format: "interview:{interviewId}:token-deltas"
func TokenChannel(id string) string {
	return fmt.Sprintf("interview:%s:token-deltas", id)
}
