// Package sanitize implements the §9 terminal-escape sanitization allowlist of
// the interactive-attach-v1 wire protocol — the security-critical filter every
// host-produced terminal byte MUST pass before it reaches a viewer.
//
// # Normative source
//
// The authoritative specification is
// donmai-architecture/protocol/interactive-attach-v1.md (revision v1.0-draft3),
// section 9 ("Terminal-escape sanitization allowlist"), with the escape-safe
// snapshot rule in §12.1 and the sanitization checklist item in Appendix A.
// Every disposition in this package is governed by the §9 allowlist table;
// where code and spec disagree, the spec wins.
//
// # Defense in depth (frozen)
//
// Enforcement is IDENTICAL at the relay and at every viewer (web/xterm.js,
// iOS/libghostty). A viewer MUST NOT assume the relay sanitized, and the relay
// MUST NOT assume the viewer will — both apply this filter. This Go package is
// the reference implementation; testdata/corpus.json (see ConformanceCorpus) is
// the shared, language-neutral conformance fixture that the relay, the web
// viewer, and the iOS viewer are all required to pass byte-for-byte.
//
// # Statefulness (frozen)
//
// The sanitizer is a stateful VT/escape parser that carries partial-sequence
// state ACROSS Output-frame boundaries within a leg. A fresh Sanitizer is used
// per leg. An escape sequence split across frames is classified exactly as if
// it had arrived contiguously: "ESC ] 5 2 ;" at the tail of one Write plus its
// payload and terminator on the next is OSC 52 and is stripped. A dangling
// incomplete OSC/DCS/APC/PM/SOS introducer at a Write boundary is held pending —
// never passed through — until its terminator arrives or the held bytes reach
// DefaultHoldMaxBytes (the spec's sanitizerHoldMaxBytes, value v1-draft), at
// which point the entire held sequence is stripped at the cap and the parser
// resynchronizes safely. Per-frame stateless filtering is non-conformant: it
// passes exactly the split-sequence bypass this rule exists to close.
//
// # The governing invariant (frozen)
//
// A viewer emulator is a display-only mirror. It NEVER emits input in response
// to output bytes. Every escape sequence whose terminal-standard effect is "the
// terminal writes a reply back on its input" (cursor-position reports, device
// attributes, status reports, color/mode queries) is answered only by the
// host-side headless VT; this sanitizer strips the trigger so no viewer ever
// replies. That single rule closes the output-triggers-input injection class.
//
// # Scope
//
// This package filters viewer-bound terminal bytes: Output frame payloads and
// any escape-bearing bytes a viewer synthesizes from a Snapshot. It also
// provides UIString, the frozen "length-capped, control-char-stripped"
// treatment for every protocol string a viewer renders as UI text (Marker
// labels, error.message, presence display names, the neutralized title chip).
//
// The package is pure Go, standard-library only, and carries no transport or
// relay logic.
package sanitize
