// Package ptyhost is the OSS PTY session host for the interactive-attach-v1
// wire protocol (donmai-architecture/protocol/interactive-attach-v1.md; owning
// ADR ADR-2026-07-12-interactive-pty-session-host.md).
//
// A Session spawns a command under a pseudo-terminal, owns the master fd, feeds
// every read chunk through a headless VT (the snapshot authority, §12), and
// publishes host-produced, sequence-bearing frames (Output, applied-Resize
// echo, Marker, Snapshot, Exit) to a bounded ring buffer and any number of live
// subscriptions. It is the host half of the protocol: it produces the host
// output sequence (§4), answers terminal queries locally to the PTY master
// (§12), serializes the current screen into a snapFormat-0x01 Snapshot (§12.1),
// and enforces the flush-before-Exit teardown ordering (§12.2).
//
// # Boundary
//
// This package is transport-free by construction: it opens NO inbound listener
// of any kind — no sockets, no HTTP or TLS servers. The relay attach path lives
// elsewhere; the only attach surface this package exposes is AttachLocal, an
// in-process viewer/driver used for the OSS-standalone case (§5, §12
// local-attach scope). All viewer-bound Output bytes on that surface pass the
// §9 sanitizer (attachwire/sanitize); the host→relay leg (produced here) carries
// raw PTY bytes (§3.1). The one exception is the parallel on-disk cast (§16,
// recorder.go): because it is a persistent artifact rather than a live,
// ephemeral stream, its "o" events pass through their own dedicated §9
// sanitizer instance before being written, so the cast is never a byte-exact
// copy of the relay leg.
//
// # Concurrency
//
// A Session is safe for concurrent use. The VT is fed from a single goroutine
// (the read loop) and every snapshot is taken under the same mutex that guards
// feeding (§12 single-feeder discipline). Host frame sequence allocation,
// ring/subscription fan-out, and the recorder are all serialized under that one
// mutex so frame ordering is correct by construction (§4).
//
// The package depends only on the framing library (github.com/RenseiAI/donmai/
// attachwire and .../sanitize), the VT (github.com/charmbracelet/x/vt) behind a
// small internal interface so it is swappable, creack/pty, and golang.org/x/sys.
package ptyhost
