// Package upgradeacceptance is the container upgrade acceptance harness for
// headless session-shim adoption.
//
// The acceptance flow (decision record: headless session-shim adoption, test
// plan section) upgrades a live seat across a daemon replacement inside a
// Linux container with a systemd user manager and delegated cgroup v2:
//
//  1. Build two daemon artifacts, N and N+1, where N+1 differs in version
//     string and in one harmless observable behaviour.
//  2. Install N as a user service from the generated unit, dispatch one
//     headless seat whose harness is a scripted fake speaking the pi
//     headless RPC line protocol, and hold the seat on a trigger file.
//  3. Prepare the restart, replace the binary with N+1, restart the unit
//     through systemd.
//  4. Assert the seat survived: worker PID and start identity unchanged,
//     controller generation advanced by exactly one, capacity charged
//     throughout, lease refresh succeeding, and exactly one terminal commit
//     after the trigger is touched.
//
// Today the flow goes red: headless seats are direct-owned children of the
// daemon, so the restart kills the seat. It goes green once the adoption
// slices land (headless shim ownership, credential push, outbox-before-send,
// and the local runtime adopting rather than holding a recovered session).
//
// Layout:
//
//   - testdata/fakeharness — the scripted fake harness binary (pi RPC line
//     protocol, trigger-file gating, session-owned transcript variant).
//   - stubreceiver.go — the stub hosted receiver (lease refresh, step
//     heartbeat, terminal status with exact replay; worker id rotation on
//     re-registration).
//   - failurematrix.go — the D6 failure matrix as table-driven cases.
//   - container/Containerfile + container/upgrade-acceptance.sh — the
//     systemd user-manager image and the N→N+1 driver.
//   - accept_test.go — the in-repo acceptance test driving the production
//     entry points (local runtime intake → dispatch → local receiver) with
//     real binaries.
//
// The container driver and the CI job (.github/workflows/upgrade-acceptance.yml)
// are the record of the red run: they capture the failure cause, and they
// pass once the adoption slices land.
package upgradeacceptance
