// Package seatbudget implements the daemon's per-seat resource budget.
//
// A seat is one accepted session's whole process tree: the worker the daemon
// spawns (`donmai agent run`) plus everything that worker starts — the
// harness child, the model-invoked tool subprocesses, and the build/test
// fan-out those tools start. Without a budget one seat's heavy command
// saturates the host: build tools fan out one worker per core
// (Turbopack/Next builds, vitest, `go test`, `make -j`), starving the other
// seats and the operator's foreground work.
//
// The budget is configured per host (see daemon.Config.SeatBudget) with
// defaults derived from host cores and memory divided by the max concurrent
// seat count. The defaults preserve single-seat throughput: on a host whose
// seat limit is one, the budget is the whole machine minus the system
// reservation, so a lone seat is never throttled.
//
// Enforcement differs by platform:
//
//   - Linux: hard enforcement with cgroups v2. The seat's process tree is
//     confined to its CPU set and memory limit (AllowedCPUs-style pinning,
//     CPU quota, memory high/max). See cgroup.go.
//   - macOS: best effort. There is no core affinity on Apple Silicon, so the
//     seat gets worker-cap environment (GOMAXPROCS plus the build/test
//     worker knobs each tool actually reads) composed with the installed
//     service process-priority mode. See coop.go.
//
// Every seat reports its budget as enforced | best-effort | none with the
// values, on host status and on the session result. Additive fields only.
package seatbudget
