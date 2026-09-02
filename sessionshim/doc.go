// Package sessionshim implements per-session shim ownership and daemon
// adoption (ADR-2026-08-17).
//
// # The problem
//
// An interactive session used to be exactly as durable as the daemon that
// launched it: the daemon's worker path built a ptyhost.Session in-process, so
// the daemon's service lifetime WAS the session host's lifetime. An ordinary
// package upgrade therefore killed every live terminal.
//
// # The shape of the fix
//
// One small, version-stable SHIM process per interactive session owns the
// harness process group, the PTY master, the output sequence, the bounded
// replay ring, and the final exit observation. The daemon becomes a replaceable
// CONTROLLER: it creates a shim for a new session, or discovers and adopts an
// existing one over shimwire after a restart. This is a real ownership move,
// not a keepalive wrapper — the daemon holds no fd, no exec.Cmd, and no
// *ptyhost.Session (§D1).
//
// # The five invariants that make it safe
//
//  1. IDENTITY. (OrgID, SessionID) is the sole lifecycle identity. shim id,
//     process epoch, PID, socket path, and controller generation are
//     correlation or fencing values; none can create, release, terminalize, or
//     re-key a session (§D2).
//  2. ADOPTION BEFORE ADVERTISEMENT. A starting daemon classifies every registry
//     entry, adopts every compatible live shim, and accounts for every
//     quarantined one BEFORE it advertises ready capacity (§D4). Advertising
//     first would let it claim work against slots that are already occupied.
//  3. SINGLE CONTROLLER. Every adoption advances a monotonic generation which
//     every mutating frame must carry, so an old daemon can never regain input,
//     resize, or stop authority after a newer one adopts (§D4).
//  4. QUARANTINE, NOT KILL. An incompatible, malformed, ambiguous, or
//     unauthenticated shim is quarantined — refused authority, but never killed,
//     and always counted against capacity and shown in diagnostics (§D7).
//     Killing would make the compatibility path exactly as destructive as the
//     restart it exists to survive.
//  5. EXPIRY IS NOT PROOF OF DEATH. Neither an orphan deadline nor a restart
//     fence expiry releases a claim on time alone. Release needs a terminal
//     receipt from an adopted live owner, or a durable tombstone proving the
//     harness group was reaped (§D8/§D10).
//
// # A transmissible frame is never a verdict on a carrier
//
// A corollary of invariant 4 that had to be learned twice on production hosts:
// no frame this shim can actually send, and no momentary consumer lag, may cost
// a live harness its supervision. Two paths used to do exactly that, and both
// are now bounded rather than fatal.
//
// The qualifier is real, not hedging. A frame that CANNOT be made to fit still
// ends the connection: ErrSnapshotUnboundable (a live grid larger than the wire
// with no scrollback left to drop) and an oversized non-Snapshot host frame both
// return an error from the write path, and pumpOutput closes the controller on
// it exactly as it always has. Those are producer bugs or impossible geometries,
// not the ordinary long-history Snapshot this bound exists for.
//
//   - A Snapshot too large for one shimwire message is bounded at the shim
//     before it is written, by dropping the oldest scrollback lines until it
//     fits (snapshotbound.go). It used to be refused by the framer, which closed
//     the connection carrying it — on the resume Snapshot, at adoption, which is
//     the least survivable moment there is.
//   - A consumer at the controller's in-flight budget gets BACK-PRESSURE: the
//     socket reader stalls, which stalls the shim's output pump, and the shim's
//     ring does the job it exists for — evict and declare an explicit Gap (§D5).
//     Reaching the budget used to drop the connection outright. It still fails
//     closed past a bounded stall deadline, because the reader is the only
//     goroutine that can deliver a durable heartbeat receipt and a consumer that
//     has STOPPED must not park it forever. Behind is not stopped.
//
// # Boundary
//
// Everything here is OSS and brand-neutral. The restart fence is an OPTIONAL
// composing callback (FenceStore): an OSS-only daemon has no remote reaper, so
// it needs no fence, and it still gets the local bounded-orphan rule. No hosted
// control plane is assumed, named, or required anywhere in this package.
package sessionshim
