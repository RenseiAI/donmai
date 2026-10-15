// Package seatstate reports one seat's state on three independent axes.
//
// A seat is one accepted session's whole process tree: the worker the daemon
// spawns plus everything that worker starts. The three axes are:
//
//   - Session: is the harness process present? present | detached | exited |
//     absent. Present means the process identity is live; detached means the
//     seat's process group survives without its supervisor binding; exited
//     means the process identity is proved gone; absent means no record of
//     the seat exists at all.
//   - Activity: what is the harness doing? working | idle_at_prompt |
//     unknown. One arbiter (DecideActivity) computes it from ranked evidence
//     and records which rung decided. An inconclusive probe reports unknown —
//     never a guess.
//   - Resumability: can a new incarnation continue this seat's work?
//     resumable | fresh_only | unknown. Resumable requires positive evidence
//     that the context artifact exists and is readable; absent evidence
//     reports unknown, never a guess.
//
// Needs-input is deliberately NOT an axis: it is a count plus a reason
// (NeedsInput), orthogonal to what the harness is doing. A working harness
// can hold queued input, and an idle harness can hold none.
package seatstate
