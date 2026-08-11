package ptyhost

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire/sanitize"
)

// corpusEntry returns the named fixture from the shared §9 conformance
// corpus (attachwire/sanitize), so the sanitized-recording expectation below
// is authoritative rather than a hand-copied byte sequence that could drift
// from the sanitizer's own reference behavior.
func corpusEntry(t *testing.T, name string) sanitize.Entry {
	t.Helper()
	entries, err := sanitize.ConformanceCorpus()
	if err != nil {
		t.Fatalf("sanitize.ConformanceCorpus: %v", err)
	}
	for _, e := range entries {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("corpus fixture %q not found", name)
	return sanitize.Entry{}
}

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

// TestRecorderSanitizesOutput proves the recorder passes "o" (output) event
// bytes through the §9 escape-sequence sanitizer before persisting them to
// the on-disk cast, using the shared attachwire/sanitize conformance corpus
// as the authoritative expectation: a disallowed OSC 52 clipboard-write
// sequence (osc52_strip) must not survive into the cast, while plain text and
// an allowed SGR color sequence (sgr_pass) must survive byte-for-byte.
// resize/marker/header events carry no PTY bytes and are unaffected — not
// exercised here (TestRecorderCastRoundTrip already covers their shape).
func TestRecorderSanitizesOutput(t *testing.T) {
	osc52 := corpusEntry(t, "osc52_strip")
	osc52In, err := osc52.InputBytes()
	if err != nil {
		t.Fatalf("osc52_strip InputBytes: %v", err)
	}
	sgr := corpusEntry(t, "sgr_pass")
	sgrIn, err := sgr.InputBytes()
	if err != nil {
		t.Fatalf("sgr_pass InputBytes: %v", err)
	}
	sgrWant, err := sgr.ExpectedOutputBytes()
	if err != nil {
		t.Fatalf("sgr_pass ExpectedOutputBytes: %v", err)
	}

	path := filepath.Join(t.TempDir(), "sanitized.cast")
	r, err := newRecorder(path, 80, 24, "xterm", "sh")
	if err != nil {
		t.Fatalf("newRecorder: %v", err)
	}

	var raw []byte
	raw = append(raw, "hello "...)
	raw = append(raw, sgrIn...)
	raw = append(raw, " world"...)
	raw = append(raw, osc52In...)
	raw = append(raw, "done"...)
	r.output(0, raw)
	r.close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cast: %v", err)
	}
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	if len(lines) < 2 {
		t.Fatalf("cast has %d lines, want header + at least one event", len(lines))
	}

	var got []byte
	for _, ln := range lines[1:] {
		var ev []json.RawMessage
		if err := json.Unmarshal(ln, &ev); err != nil || len(ev) != 3 {
			t.Fatalf("bad event line %q: %v", ln, err)
		}
		var code string
		if err := json.Unmarshal(ev[1], &code); err != nil {
			t.Fatalf("bad event code %q: %v", ev[1], err)
		}
		if code != "o" {
			continue
		}
		var chunk string
		if err := json.Unmarshal(ev[2], &chunk); err != nil {
			t.Fatalf("bad event data %q: %v", ev[2], err)
		}
		got = append(got, chunk...)
	}

	if bytes.Contains(got, osc52In) {
		t.Errorf("cast retained the disallowed OSC 52 sequence verbatim: %q", got)
	}
	if !bytes.Contains(got, []byte("hello ")) || !bytes.Contains(got, []byte(" world")) || !bytes.Contains(got, []byte("done")) {
		t.Errorf("cast lost plain text: %q", got)
	}
	if !bytes.Contains(got, sgrWant) {
		t.Errorf("cast dropped an allowed SGR sequence: got %q, want it to contain %q", got, sgrWant)
	}
}
