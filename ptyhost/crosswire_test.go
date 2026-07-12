package ptyhost

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
)

// TestCrossPackageFrameRoundTrip is the T10 cross-package invariant test: it
// pushes >=1 MiB of the recorded tmux_vim fixture bytes (looped) through a REAL
// ptyhost session (a child process cat-ing the fixture file under the PTY, so
// the bytes traverse the full kernel-PTY -> read loop -> VT feed -> framing
// pipeline) and asserts, for every host-produced frame observed on a live
// subscription:
//
//  1. the frame round-trips attachwire.EncodeFrame -> DecodeFrame identically
//     (§2 canonical encoding: type/seq/rel_time/payload all preserved);
//  2. host seqs are strictly contiguous (§4);
//
// and that a Snapshot taken MID-STREAM (while Output frames are still
// flowing) is byte-stable through the §12.1 codec: encode -> decode ->
// re-encode yields identical bytes, and the same holds one level up through
// the frozen §3.1 SnapshotEnvelope.
func TestCrossPackageFrameRoundTrip(t *testing.T) {
	const fixture = "tmux_vim"
	rawFixture, err := os.ReadFile(filepath.Join("testdata", fixture+".raw"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// Loop the fixture until >= 1 MiB has been produced.
	loops := (1<<20)/len(rawFixture) + 1
	wantBytes := int64(loops * len(rawFixture))

	// The child cats the fixture file `loops` times. The PTY is put into raw
	// mode by the child (stty raw) so the kernel line discipline does not
	// rewrite the fixture's bytes (ONLCR would turn every \n into \r\n and
	// break the exact byte count); if stty fails we still only assert on
	// what actually arrived, so the invariants below are unaffected.
	//
	// The `cat </dev/tty >/dev/null &` background reader is load-bearing:
	// each fixture loop carries four terminal QUERY sequences (DA1, DA2,
	// OSC 10, OSC 11) that the host VT answers by writing replies to the PTY
	// master (§12 — its designed duty). A child that never reads stdin lets
	// those replies fill the kernel slave input queue (~1 KiB), after which
	// the VT's reply write blocks WHILE onOutput holds s.mu, wedging the
	// entire session — see TestQueryRepliesWedgeNonReadingChild for the
	// documented reproducer of that bug. The explicit </dev/tty is required:
	// POSIX shells point a background job's stdin at /dev/null when job
	// control is off, so a bare `cat >/dev/null &` would read nothing.
	script := "stty raw 2>/dev/null; cat </dev/tty >/dev/null & i=0; while [ $i -lt " + strconv.Itoa(loops) + " ]; do cat testdata/" + fixture + ".raw; i=$((i+1)); done"
	s, err := Spawn(Spec{Command: []string{"sh", "-c", script}})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	sub, err := s.Subscribe(0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Close() }()

	var (
		total        int64
		lastSeq      uint64
		lastRel      uint64
		frameCount   int
		tookSnapshot bool
	)
	deadline := time.After(60 * time.Second)
loop:
	for {
		select {
		case f, ok := <-sub.Frames():
			if !ok {
				break loop
			}
			frameCount++

			// (1) §2 frame codec round-trip identity for EVERY emitted frame.
			re, err := attachwire.DecodeFrame(attachwire.EncodeFrame(f))
			if err != nil {
				t.Fatalf("frame seq=%d type=%v: DecodeFrame(EncodeFrame(f)) failed: %v", f.Seq, f.Type, err)
			}
			if re.Type != f.Type || re.Seq != f.Seq || re.RelTime != f.RelTime || !bytes.Equal(re.Payload, f.Payload) {
				t.Fatalf("frame seq=%d type=%v not round-trip identical:\n got %#v\nwant %#v", f.Seq, f.Type, re, f)
			}

			// (2) §4 host seq contiguity + §2 rel_time monotonicity.
			if lastSeq != 0 && f.Seq != lastSeq+1 {
				t.Fatalf("host seq gap: got %d after %d", f.Seq, lastSeq)
			}
			if f.RelTime < lastRel {
				t.Fatalf("rel_time went backwards: %d after %d (seq %d)", f.RelTime, lastRel, f.Seq)
			}
			lastSeq, lastRel = f.Seq, f.RelTime

			if f.Type == attachwire.TypeOutput {
				total += int64(len(f.Payload))
			}
			if f.Type == attachwire.TypeExit {
				break loop
			}

			// (3) mid-stream Snapshot byte-stability, taken once roughly half
			// the volume has flowed (stream is still live).
			if !tookSnapshot && total > wantBytes/2 {
				tookSnapshot = true
				assertSnapshotByteStable(t, s)
			}
		case <-deadline:
			t.Fatalf("timed out; received %d/%d bytes in %d frames", total, wantBytes, frameCount)
		}
	}

	if total < 1<<20 {
		t.Errorf("only %d Output bytes traversed the pipeline, want >= 1 MiB", total)
	}
	if !tookSnapshot {
		t.Error("mid-stream snapshot was never taken (stream ended before the half-volume trigger)")
	}
	t.Logf("cross-package invariant held over %d frames / %d Output bytes", frameCount, total)
}

// assertSnapshotByteStable takes a read-only Snapshot from the live session and
// proves §12.1 byte stability: Screen encode -> DecodeScreen -> re-encode is
// byte-identical, and the §3.1 SnapshotEnvelope wrap decodes back to identical
// fields and re-encodes byte-identically.
func assertSnapshotByteStable(t *testing.T, s *Session) {
	t.Helper()

	scr, atSeq, err := s.Snapshot()
	if err != nil {
		t.Fatalf("mid-stream Snapshot: %v", err)
	}

	enc1, err := scr.Encode()
	if err != nil {
		t.Fatalf("mid-stream screen Encode: %v", err)
	}
	dec, err := attachwire.DecodeScreen(enc1)
	if err != nil {
		t.Fatalf("mid-stream screen DecodeScreen (escape-safety violated): %v", err)
	}
	enc2, err := dec.Encode()
	if err != nil {
		t.Fatalf("mid-stream screen re-Encode: %v", err)
	}
	if !bytes.Equal(enc1, enc2) {
		t.Fatalf("mid-stream snapshot not byte-stable: encode->decode->encode differs (%d vs %d bytes)", len(enc1), len(enc2))
	}

	env := attachwire.SnapshotEnvelope{
		AtSeq:      uint64(atSeq),
		SnapFormat: attachwire.SnapFormatScreen,
		Snap:       enc1,
	}
	envEnc1 := env.Encode()
	envDec, err := attachwire.DecodeSnapshotEnvelope(envEnc1)
	if err != nil {
		t.Fatalf("mid-stream envelope decode: %v", err)
	}
	if envDec.AtSeq != env.AtSeq || envDec.SnapFormat != env.SnapFormat || !bytes.Equal(envDec.Snap, env.Snap) {
		t.Fatalf("mid-stream envelope fields changed across decode")
	}
	if !bytes.Equal(envDec.Encode(), envEnc1) {
		t.Fatalf("mid-stream envelope not byte-stable: encode->decode->encode differs")
	}
}
