package ptyhost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
)

// ---- BenchmarkPTYToFrame ----------------------------------------------------
//
// Measures the T10 plan's latency micro-bench: bytes written to the PTY master
// -> the corresponding Output frame(s) observed on a live Subscribe channel.
// Uses a real Spawn of `cat` (an echo pipe under the PTY) so the measurement
// includes the full path: WriteInput -> kernel PTY -> cat echo -> the
// session's read loop -> onOutput (VT feed + framing + ring/publish) ->
// the subscription pump -> the channel receive. Run with -benchtime=2s -count=3
// and compare the median (see the T10 report for recorded numbers).
func BenchmarkPTYToFrame(b *testing.B) {
	for _, n := range []int{64, 4096, 32768} {
		b.Run(fmt.Sprintf("%dB", n), func(b *testing.B) {
			// Raw mode + echo off: the default canonical line discipline
			// caps an unterminated line at MAX_CANON (1 KiB on darwin) and
			// DISCARDS the excess, so large chunks never reach cat. Raw
			// mode measures the real pipeline: write -> cat -> PTY read ->
			// frame. The stty settle is detected via the session's own
			// termios-backed echoMode (§10 signal).
			s, err := Spawn(Spec{Command: []string{"/bin/sh", "-c", "stty raw -echo; exec cat"}})
			if err != nil {
				b.Fatalf("Spawn: %v", err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				defer cancel()
				_ = s.Stop(ctx)
			}()

			deadline := time.Now().Add(5 * time.Second)
			for {
				scr, _, serr := s.Snapshot()
				if serr != nil {
					b.Fatalf("Snapshot: %v", serr)
				}
				if scr.EchoMode == attachwire.EchoOff {
					break
				}
				if time.Now().After(deadline) {
					b.Fatal("stty raw -echo never took effect")
				}
				time.Sleep(2 * time.Millisecond)
			}

			sub, err := s.Subscribe(0)
			if err != nil {
				b.Fatalf("Subscribe: %v", err)
			}
			defer func() { _ = sub.Close() }()

			data := bytes.Repeat([]byte("a"), n)
			frames := sub.Frames()

			b.ReportAllocs()
			b.SetBytes(int64(n))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.WriteInput(data); err != nil {
					b.Fatalf("WriteInput: %v", err)
				}
				got := 0
				deadline := time.After(10 * time.Second)
				for got < n {
					select {
					case f, ok := <-frames:
						if !ok {
							b.Fatalf("subscription closed early (got %d/%d bytes)", got, n)
						}
						if f.Type == attachwire.TypeOutput {
							got += len(f.Payload)
						}
					case <-deadline:
						b.Fatalf("timed out waiting for echo (got %d/%d bytes)", got, n)
					}
				}
			}
			b.StopTimer()
		})
	}
}

// ---- BenchmarkSnapshotSerialize ---------------------------------------------
//
// Feeds the recorded tmux_vim fixture through a fresh VT once, then repeatedly
// serializes the resulting screen (buildScreen + attachwire.Screen.Encode) — the
// T10 plan's snapshot-serialize micro-bench. Self-contained (does not reuse the
// *testing.T-typed helpers in helpers_test.go) so it needs no changes to
// existing test files.
func BenchmarkSnapshotSerialize(b *testing.B) {
	raw, cols, rows := loadRawFixtureForBench(b, "tmux_vim")
	v := newVTHost(cols, rows, DefaultScrollback, io.Discard, nil)
	v.write(raw)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scr := buildScreen(v.raw(), 1, attachwire.EchoOn, nil)
		enc, err := scr.Encode()
		if err != nil {
			b.Fatalf("Encode: %v", err)
		}
		if len(enc) == 0 {
			b.Fatal("empty snapshot encoding")
		}
	}
}

// loadRawFixtureForBench reads a testdata/<name>.raw fixture and its cols/rows
// from the <name>.json sidecar, without depending on the *testing.T-typed
// loadFixture helper in helpers_test.go.
func loadRawFixtureForBench(b *testing.B, name string) (raw []byte, cols, rows int) {
	b.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name+".raw"))
	if err != nil {
		b.Fatalf("read fixture %s.raw: %v", name, err)
	}
	metaBytes, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		b.Fatalf("read fixture %s.json: %v", name, err)
	}
	var meta struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		b.Fatalf("parse %s.json: %v", name, err)
	}
	cols, rows = meta.Cols, meta.Rows
	if cols == 0 || rows == 0 {
		cols, rows = 80, 24
	}
	return raw, cols, rows
}
