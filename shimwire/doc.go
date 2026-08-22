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
// # Versioning
//
// Compatibility is an advertised min/max RANGE plus a selected version, never
// binary-version equality: a newer daemon MUST be able to adopt an older live
// shim for at least the maximum supported session lifetime (§D3). See Negotiate.
package shimwire
