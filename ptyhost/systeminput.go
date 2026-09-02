package ptyhost

import (
	"fmt"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
)

// systemInputPacingGap is the keystroke-scale minimum spacing this host
// enforces between whatever was last written to the PTY master and a
// SYSTEM-authority write that is nothing but a bare CR/LF.
//
// An operator "nudge" is delivered upstream as a small paced burst of
// SYSTEM-authority Input frames (a clear, the line, then a standalone
// Enter), but that pacing is enforced at the SENDER's send time only: if a
// leg between the sender and this host stalls even briefly, the queued
// frames arrive back-to-back and this host's next PTY write happens right on
// top of the one before it. A harness's own read() can then coalesce the two
// into one chunk — exactly the shape a TUI's paste-detection heuristic fires
// on, which inserts a bare Enter as a literal newline instead of submitting
// the line (the whole point of the nudge). This is the LAST-HOP guarantee:
// it does not matter how the frames queued upstream, only that this host
// never hands the harness a bare Enter in the same read() as whatever came
// immediately before it.
//
// A package var, not a const, so a test can shrink it well below the
// wall-clock 120ms default and still exercise the real delay path (see
// systeminput_test.go) without slowing the suite.
var systemInputPacingGap = 120 * time.Millisecond

// pasteCloseFrame closes a dangling bracketed-paste region (ptyhost/compose.go
// tracks CSI 200~/201~) so a SYSTEM-authority write's own bytes never land
// inside someone else's abandoned paste, where every byte — including a
// clear-line control or an Enter — is swallowed as literal pasted text
// instead of being interpreted.
var pasteCloseFrame = []byte("\x1b[201~")

// isSystemAttributedInput reports whether userID names the SYSTEM-authority
// sender this host special-cases at the last hop, using the ONE constant the
// relay and this wire library share (attachwire.SystemNudgeUserID) rather
// than a locally hardcoded literal. Any other relay-stamped userID is
// ordinary (human) input and is never delayed or paste-guarded.
func isSystemAttributedInput(userID []byte) bool {
	return string(userID) == attachwire.SystemNudgeUserID
}

// isBareEnter reports whether data is exactly one CR or LF byte — the shape a
// standalone Enter keystroke produces, and the only shape
// systemInputPacingGap ever delays. Multi-byte data (a typed line, a paste,
// an escape sequence) is never delayed even when SYSTEM-attributed.
func isBareEnter(data []byte) bool {
	return len(data) == 1 && (data[0] == '\r' || data[0] == '\n')
}

// ptyWriter is the narrow write surface a system-attributed write needs.
// *os.File (the real PTY master) satisfies it in production;
// systeminput_test.go drives a recording fake against it directly, with no
// PTY or child process involved, so the pacing/paste-guard DECISION logic is
// unit-tested independently of process spawning.
type ptyWriter interface {
	Write(p []byte) (int, error)
}

// writeSystemAttributed applies the two last-hop guarantees described on
// systemInputPacingGap and pasteCloseFrame, then writes data to w verbatim
// and feeds it to compose exactly like an ordinary PTY write. It returns the
// byte count of data's own write (the paste-close write, when it happens, is
// not attributed to the caller's payload) and the timestamp the caller
// should remember as its next lastWrite.
//
// Callers MUST already have confirmed userID is system-attributed
// (isSystemAttributedInput) and MUST serialize every PTY writer against this
// one exactly like Session.writeMu does — this function holds no lock of its
// own, which is what lets it be driven directly by a single-goroutine test.
func writeSystemAttributed(
	w ptyWriter,
	compose *composeTracker,
	lastWrite time.Time,
	data []byte,
) (n int, newLastWrite time.Time, err error) {
	newLastWrite = lastWrite
	if compose.pasteOpen() {
		if _, werr := w.Write(pasteCloseFrame); werr != nil {
			return 0, newLastWrite, fmt.Errorf("ptyhost: close dangling bracketed paste before system input: %w", werr)
		}
		compose.feed(pasteCloseFrame)
		newLastWrite = time.Now()
	}

	if isBareEnter(data) {
		if elapsed := time.Since(newLastWrite); elapsed < systemInputPacingGap {
			time.Sleep(systemInputPacingGap - elapsed)
		}
	}

	n, err = w.Write(data)
	if n > 0 && n <= len(data) {
		compose.feed(data[:n])
	}
	newLastWrite = time.Now()
	if err != nil {
		return n, newLastWrite, fmt.Errorf("ptyhost: write system-attributed input: %w", err)
	}
	return n, newLastWrite, nil
}
