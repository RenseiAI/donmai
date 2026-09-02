package ptyhost

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
)

// recordedWrite is one call the fake PTY writer observed: the byte content
// and the wall-clock instant it was written — exactly what a test needs to
// prove two writes landed as separate segments at least systemInputPacingGap
// apart, without spawning a real PTY or process.
type recordedWrite struct {
	at   time.Time
	data []byte
}

// fakePTYWriter is the ptyWriter fake: it records every write's timestamp and
// bytes, and can be armed to fail the NEXT write exactly once.
type fakePTYWriter struct {
	writes   []recordedWrite
	failNext error
}

func (f *fakePTYWriter) Write(p []byte) (int, error) {
	f.writes = append(f.writes, recordedWrite{at: time.Now(), data: append([]byte(nil), p...)})
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return 0, err
	}
	return len(p), nil
}

// withShrunkPacingGap lowers systemInputPacingGap for the life of one test so
// the suite stays fast, while still exercising the real time.Sleep path the
// production 120ms default also takes — only the DURATION changes, not the
// logic. systemInputPacingGap is a package var (not a const) for exactly
// this: see its doc in systeminput.go.
func withShrunkPacingGap(t *testing.T, gap time.Duration) {
	t.Helper()
	orig := systemInputPacingGap
	systemInputPacingGap = gap
	t.Cleanup(func() { systemInputPacingGap = orig })
}

func TestIsSystemAttributedInput(t *testing.T) {
	tests := []struct {
		name   string
		userID string
		want   bool
	}{
		{name: "the shared system nudge sentinel", userID: attachwire.SystemNudgeUserID, want: true},
		{name: "empty (unstamped never reaches here, but must not match)", userID: "", want: false},
		{name: "an ordinary platform-issued end-user id", userID: "user_01hz3k9xyz", want: false},
		{name: "case mismatch does not match", userID: "SYSTEM:PTY-NUDGE", want: false},
		{name: "a prefix of the sentinel does not match", userID: "system:pty-nud", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSystemAttributedInput([]byte(tc.userID)); got != tc.want {
				t.Errorf("isSystemAttributedInput(%q) = %v, want %v", tc.userID, got, tc.want)
			}
		})
	}
}

func TestIsBareEnter(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{name: "bare CR", data: []byte("\r"), want: true},
		{name: "bare LF", data: []byte("\n"), want: true},
		{name: "CRLF is two bytes, not bare", data: []byte("\r\n"), want: false},
		{name: "empty", data: []byte{}, want: false},
		{name: "a single printable byte", data: []byte("a"), want: false},
		{name: "the nudge clear frame", data: []byte("\x01\x0b"), want: false},
		{name: "a typed line ending in CR", data: []byte("hello\r"), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBareEnter(tc.data); got != tc.want {
				t.Errorf("isBareEnter(%q) = %v, want %v", tc.data, got, tc.want)
			}
		})
	}
}

// TestWriteSystemAttributed_Pacing is item 1: a SYSTEM-attributed write that
// is nothing but a bare CR/LF is held back until systemInputPacingGap has
// elapsed since the previous PTY write, so it lands as its own read() on the
// harness side; anything else (multi-byte data, or a CR/LF long after the
// previous write) is never delayed.
func TestWriteSystemAttributed_Pacing(t *testing.T) {
	withShrunkPacingGap(t, 30*time.Millisecond)

	tests := []struct {
		name string
		// lastWriteAgo is how long before the subtest actually RUNS the
		// previous write is simulated to have happened. It is deliberately
		// NOT a fixed time.Time baked into the table: earlier subtests in
		// this same table sleep for a full gap, and a table-literal
		// time.Now() would go stale by the time a later subtest runs — the
		// timestamp must be computed fresh at execution time instead.
		lastWriteAgo  time.Duration
		zeroLastWrite bool // simulates "no previous write at all" (time.Time{})
		data          []byte
		wantDelayed   bool
	}{
		{name: "bare CR right after a recent write is delayed", lastWriteAgo: 0, data: []byte("\r"), wantDelayed: true},
		{name: "bare LF right after a recent write is delayed exactly like CR", lastWriteAgo: 0, data: []byte("\n"), wantDelayed: true},
		{name: "bare CR long after the previous write is not delayed", lastWriteAgo: time.Hour, data: []byte("\r"), wantDelayed: false},
		{name: "no previous write at all is not delayed", zeroLastWrite: true, data: []byte("\r"), wantDelayed: false},
		{name: "multi-byte text is never delayed even right after a write", lastWriteAgo: 0, data: []byte("hello"), wantDelayed: false},
		{name: "the nudge clear frame is never delayed by pacing", lastWriteAgo: 0, data: []byte("\x01\x0b"), wantDelayed: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fw := &fakePTYWriter{}
			var compose composeTracker
			lastWrite := time.Now().Add(-tc.lastWriteAgo)
			if tc.zeroLastWrite {
				lastWrite = time.Time{}
			}
			start := time.Now()
			n, _, err := writeSystemAttributed(fw, &compose, lastWrite, tc.data)
			if err != nil {
				t.Fatalf("writeSystemAttributed: %v", err)
			}
			if n != len(tc.data) {
				t.Fatalf("n = %d, want %d", n, len(tc.data))
			}
			if len(fw.writes) != 1 {
				t.Fatalf("writes = %d, want 1 (no paste was open)", len(fw.writes))
			}
			if string(fw.writes[0].data) != string(tc.data) {
				t.Fatalf("write = %q, want %q", fw.writes[0].data, tc.data)
			}
			elapsedSinceCall := time.Since(start)
			elapsedSincePrior := fw.writes[0].at.Sub(lastWrite)
			switch {
			case tc.wantDelayed && elapsedSinceCall < systemInputPacingGap:
				t.Errorf("call took %v, want >= %v (should have slept)", elapsedSinceCall, systemInputPacingGap)
			case tc.wantDelayed && elapsedSincePrior < systemInputPacingGap:
				t.Errorf("write landed %v after the previous write, want >= %v", elapsedSincePrior, systemInputPacingGap)
			case !tc.wantDelayed && elapsedSinceCall >= systemInputPacingGap:
				t.Errorf("call took %v, want < %v (should not have slept)", elapsedSinceCall, systemInputPacingGap)
			}
		})
	}
}

// TestWriteSystemAttributed_PasteGuard is item 2: a dangling bracketed-paste
// region is closed with CSI 201~ before a SYSTEM-attributed write, so the
// write's own bytes never land inside someone else's abandoned paste. A
// closed (or never-opened) region is a no-op — only the caller's write
// happens.
func TestWriteSystemAttributed_PasteGuard(t *testing.T) {
	withShrunkPacingGap(t, 10*time.Millisecond) // keep the "no pacing expected" branches fast

	t.Run("open paste is closed before the clear frame, then the frame lands", func(t *testing.T) {
		fw := &fakePTYWriter{}
		var compose composeTracker
		compose.feed([]byte("\x1b[200~")) // a viewer's paste-start with no matching 201~ yet
		if !compose.pasteOpen() {
			t.Fatal("setup: composeTracker should report an open paste region")
		}

		n, _, err := writeSystemAttributed(fw, &compose, time.Now().Add(-time.Hour), []byte("\x01\x0b"))
		if err != nil {
			t.Fatalf("writeSystemAttributed: %v", err)
		}
		if n != 2 {
			t.Fatalf("n = %d, want 2 (the clear frame's own byte count)", n)
		}
		if len(fw.writes) != 2 {
			t.Fatalf("writes = %d, want 2 (paste-close, then the clear frame)", len(fw.writes))
		}
		if string(fw.writes[0].data) != "\x1b[201~" {
			t.Errorf("first write = %q, want the paste-close sequence", fw.writes[0].data)
		}
		if string(fw.writes[1].data) != "\x01\x0b" {
			t.Errorf("second write = %q, want the caller's clear frame", fw.writes[1].data)
		}
		if compose.pasteOpen() {
			t.Error("paste should be closed after the guarded write")
		}
	})

	t.Run("no open paste means no spurious close write", func(t *testing.T) {
		fw := &fakePTYWriter{}
		var compose composeTracker
		n, _, err := writeSystemAttributed(fw, &compose, time.Now().Add(-time.Hour), []byte("hi"))
		if err != nil {
			t.Fatalf("writeSystemAttributed: %v", err)
		}
		if n != 2 {
			t.Fatalf("n = %d, want 2", n)
		}
		if len(fw.writes) != 1 {
			t.Fatalf("writes = %d, want 1 (no paste was ever open)", len(fw.writes))
		}
	})

	t.Run("a bare Enter after a paste-close is still paced off the close write", func(t *testing.T) {
		withShrunkPacingGap(t, 30*time.Millisecond)
		fw := &fakePTYWriter{}
		var compose composeTracker
		compose.feed([]byte("\x1b[200~"))

		_, _, err := writeSystemAttributed(fw, &compose, time.Now().Add(-time.Hour), []byte("\r"))
		if err != nil {
			t.Fatalf("writeSystemAttributed: %v", err)
		}
		if len(fw.writes) != 2 {
			t.Fatalf("writes = %d, want 2 (paste-close, then the delayed CR)", len(fw.writes))
		}
		if elapsed := fw.writes[1].at.Sub(fw.writes[0].at); elapsed < systemInputPacingGap {
			t.Errorf("CR landed %v after the paste-close write, want >= %v", elapsed, systemInputPacingGap)
		}
	})
}

// TestWriteSystemAttributed_WriteErrorIsWrappedAndComposeUntouched proves a
// failed write neither updates the compose model nor swallows the
// underlying error.
func TestWriteSystemAttributed_WriteErrorIsWrappedAndComposeUntouched(t *testing.T) {
	boom := errors.New("boom")
	fw := &fakePTYWriter{failNext: boom}
	var compose composeTracker

	_, _, err := writeSystemAttributed(fw, &compose, time.Now().Add(-time.Hour), []byte("x"))
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
	if !strings.Contains(err.Error(), "ptyhost:") {
		t.Errorf("err = %q, want a ptyhost-prefixed message", err.Error())
	}
	if compose.pending() {
		t.Error("compose must not observe a failed write")
	}
}
