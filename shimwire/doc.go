// Package shimwire is the codec and closed message vocabulary for
// `session-shim-v1`, the local adoption protocol spoken between a per-session
// shim (which owns the harness process group and the PTY) and whichever daemon
// process is currently its controller.
//
// The protocol exists because a daemon restart must not end an interactive
// session. The shim is the durable owner; the daemon is a replaceable
// controller that discovers a live shim after restart and adopts it over this
// wire (ADR-2026-08-17 §D3).
//
// # Why this is a separate wire from attachwire
//
// attachwire is the viewer-facing interactive-attach protocol: it is frozen at
// v1, it is carried by a connection the host dialled OUT, and its host output
// sequence namespace belongs to the PTY-owning process. This package is the
// local, inbound, controller-facing wire. Conflating them would let a daemon
// release renumber host output — exactly the fabricated continuity the ADR
// forbids (§D5). The two namespaces meet in exactly one place: an Output
// message here carries the shim-allocated attachwire host sequence verbatim.
//
// # Framing
//
//	+--------------------+---------+------------------+
//	| length: u32 big-e  | type:u8 | body: bytes...   |
//	+--------------------+---------+------------------+
//
// length covers type+body and is bounded by MaxMessageBytes, so a malformed or
// hostile peer cannot make a reader allocate without limit. Control bodies are
// JSON (strictly decoded — an unknown field is a protocol error, never a silent
// downgrade); the two byte-carrying messages (Output, Input) use a fixed binary
// header so terminal bytes are never base64-inflated through a control encoding.
//
// # Snapshots are bounded at the producer, not by the frame shape
//
// Every message type but one has an inherent size: Output is capped by the PTY
// host at 32 KiB, Input by its caller, and the control bodies are small and
// fixed. A Snapshot is the exception — it carries a serialized screen whose
// scrollback tail is a per-session policy, so a lineage with a long screen
// history produces one that does not fit MaxMessageBytes at all.
//
// A message the framer refuses is a message the SHIM cannot send, and a shim
// that cannot send its resume Snapshot used to close the connection carrying it,
// which cost live sessions their controllers on production hosts. The shim
// therefore bounds an oversized Snapshot before it writes: it re-encodes the
// screen keeping the newest scrollback lines that fit and dropping the oldest
// (sessionshim's boundSnapshotFrame).
//
// This is deliberately NOT a wire change and is gated on no protocol version:
// attachwire.Screen already carries its scrollback as a length-prefixed list, so
// a bounded Snapshot is an ordinary canonical Screen inside an ordinary
// canonical frame, and every receiver — including a released selected-v2 one —
// decodes it exactly as it decodes any other. A receiver-visible truncation
// marker was considered and rejected for the same reason: the Screen decoder
// rejects trailing bytes, so a new field would stop precisely the older
// controllers this protocol exists to keep adopting. The shortening is announced
// in the shim's structured log instead.
//
// The bound is a pure function of the frame and the ceiling, so a retained frame
// bounds to identical wire bytes on every delivery — a re-adoption ring hit
// replays the same bytes for the same host sequence, which is what §D5's
// byte-for-byte rule requires of one sequence.
//
// # Versioning
//
// Compatibility is an advertised min/max RANGE plus a selected version, never
// binary-version equality: a newer daemon MUST be able to adopt an older live
// shim for at least the maximum supported session lifetime (§D3). See Negotiate.
package shimwire
