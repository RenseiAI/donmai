package ptyhost

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRecorderCastRoundTrip records a session and parses the asciinema v2 cast
// back: the header carries the geometry + env, and the event stream contains "o"
// (output), "r" (resize), and "m" (marker) events with monotonic non-decreasing
// times (§16).
func TestRecorderCastRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.cast")
	s := mustSpawn(t, Spec{
		Command:    []string{"sh", "-c", "printf hello; sleep 2"},
		Cols:       80,
		Rows:       24,
		RecordPath: path,
	})
	if err := s.EmitMarker("approval-pending"); err != nil {
		t.Fatalf("EmitMarker: %v", err)
	}
	if err := s.Resize(90, 30, 0, 0); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	waitDone(t, s, 10*time.Second)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cast: %v", err)
	}
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	if len(lines) < 2 {
		t.Fatalf("cast has %d lines, want header + events", len(lines))
	}

	var hdr castHeader
	if err := json.Unmarshal(lines[0], &hdr); err != nil {
		t.Fatalf("parse header: %v", err)
	}
	if hdr.Version != 2 || hdr.Width != 80 || hdr.Height != 24 {
		t.Errorf("header = %+v, want version 2 / 80x24", hdr)
	}
	if hdr.Env["TERM"] == "" {
		t.Error("header env missing TERM")
	}

	kinds := map[string]int{}
	var last float64
	for _, ln := range lines[1:] {
		var ev []json.RawMessage
		if err := json.Unmarshal(ln, &ev); err != nil || len(ev) != 3 {
			t.Fatalf("bad event line %q: %v", ln, err)
		}
		var ts float64
		var code string
		if err := json.Unmarshal(ev[0], &ts); err != nil {
			t.Fatalf("bad event time %q: %v", ev[0], err)
		}
		if err := json.Unmarshal(ev[1], &code); err != nil {
			t.Fatalf("bad event code %q: %v", ev[1], err)
		}
		if ts < last {
			t.Errorf("event time %v < previous %v (non-monotonic)", ts, last)
		}
		last = ts
		kinds[code]++
	}
	for _, k := range []string{"o", "r", "m"} {
		if kinds[k] == 0 {
			t.Errorf("cast missing %q events (kinds=%v)", k, kinds)
		}
	}
}
