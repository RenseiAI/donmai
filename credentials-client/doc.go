// Package credentials is the agent-side credential loader library.
//
// An agent process imports this package once and obtains a single
// [Loader] that abstracts over two delivery modes:
//
//   - Daemon mode: the parent process (a local daemon, a CI shim, a test
//     harness) has set the RENSEI_CREDENTIAL_SOCKET environment variable
//     to the path of a unix socket speaking the HELLO/INITIAL/UPDATE/BYE
//     line-delimited JSON protocol. The loader connects, sends a HELLO
//     frame naming the session id, receives an INITIAL snapshot, and
//     reads UPDATE frames in the background.
//
//   - Standalone mode: the socket env var is unset (or the daemon-mode
//     handshake failed). The loader snapshots os.Environ() into an
//     internal map and never receives UPDATE deltas.
//
// In both modes the Get / All / Subscribe surface is identical so an
// agent never needs to branch on mode.
//
// # Blocklist
//
// The loader applies [AgentEnvBlocklist] from
// internal/credentials at read time. Any name appearing on that list
// returns ("", false) from Get, is omitted from All, and is stripped
// from UPDATE deltas before subscribers see them. This is a
// defence-in-depth layer; the daemon also filters on emission.
//
// # Wire protocol (daemon mode)
//
// Single connection, line-delimited JSON.
//
// Client → server (one frame):
//
//	{"type":"HELLO","sessionId":"<id>"}
//
// Server → client (one frame, then zero or more UPDATE frames):
//
//	{"type":"INITIAL","env":{"FOO":"bar",...}}
//	{"type":"UPDATE","delta":{"FOO":"bar2"},"rotatedAt":"2026-05-17T18:00:00Z"}
//
// Either side may send a final frame:
//
//	{"type":"BYE","reason":"<optional>"}
//
// # Failure mode
//
// Any daemon-mode failure (env var unset, dial error, handshake
// timeout, malformed INITIAL) collapses to standalone mode. A single
// info-level log line is emitted; no credential values appear in logs.
// Standalone mode is the also fallback for unit-test environments and
// for OSS agents that run without a daemon at all.
package credentials
