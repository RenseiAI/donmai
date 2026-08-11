package ptyhost

import (
	"bufio"
	"encoding/json"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/attachwire/sanitize"
)

// recorder writes a parallel asciinema v2 cast of the session (§16): a header
// line followed by [time, code, data] event lines. It shares the process-spawn
// rel_time anchor with the wire (§2), so the cast and the live stream stay
// aligned. All methods are called under the Session mutex (write-through).
//
// Output ("o") events are NOT a verbatim copy of the PTY bytes: they pass
// through this recorder's own §9 escape-sequence sanitizer (attachwire/
// sanitize) before being written to disk, because the cast is a persistent
// artifact rather than a live, ephemeral stream. This is a SEPARATE sanitizer
// instance from any viewer/relay leg — Sanitizer is stateful and must never be
// shared across legs (see ptyhost/local.go's AttachLocal for the analogous
// viewer-side instance). resize/marker/header events carry no PTY bytes and
// are unaffected.
type recorder struct {
	mu  sync.Mutex
	f   *os.File
	w   *bufio.Writer
	san *sanitize.Sanitizer
}

// castHeader is the asciinema v2 header object.
type castHeader struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp int64             `json:"timestamp"`
	Env       map[string]string `json:"env,omitempty"`
}

// newRecorder creates the cast file and writes the header. A nil recorder (empty
// path) is a valid no-op used everywhere below.
func newRecorder(path string, cols, rows int, termEnv, shell string) (*recorder, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Create(path) //nolint:gosec // path is caller-controlled session config
	if err != nil {
		return nil, err
	}
	r := &recorder{f: f, w: bufio.NewWriter(f), san: sanitize.New()}
	hdr := castHeader{
		Version:   2,
		Width:     cols,
		Height:    rows,
		Timestamp: time.Now().Unix(),
		Env:       map[string]string{"TERM": termEnv, "SHELL": shell},
	}
	b, _ := json.Marshal(hdr)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.w.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return nil, err
	}
	return r, nil
}

// writeEvent writes one [time, code, data] line. relMicros is microseconds since
// spawn; the cast records seconds as a float.
func (r *recorder) writeEvent(relMicros uint64, code, data string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeEventLocked(relMicros, code, data)
}

// writeEventLocked is writeEvent's body, assuming r.mu is already held. Split
// out so output() can run the sanitizer and append the resulting event inside
// one critical section — the sanitizer is stateful, so its Write call and the
// resulting event's position in the file must never race a concurrent event.
func (r *recorder) writeEventLocked(relMicros uint64, code, data string) {
	// Build the JSON array manually so the float time keeps full precision and
	// data is JSON-escaped.
	secs := float64(relMicros) / 1e6
	dataJSON, _ := json.Marshal(data)
	line := make([]byte, 0, recorderCapHint(len(dataJSON)))
	line = append(line, '[')
	line = strconv.AppendFloat(line, secs, 'f', 6, 64)
	line = append(line, ',', '"')
	line = append(line, code...)
	line = append(line, '"', ',')
	line = append(line, dataJSON...)
	line = append(line, ']', '\n')
	_, _ = r.w.Write(line)
}

// output records terminal output ("o"). data is raw PTY bytes; they pass
// through this recorder's dedicated §9 sanitizer before being written, so a
// disallowed escape/OSC sequence (e.g. an OSC 52 clipboard write) never lands
// in the on-disk cast even though it rides the live host→relay leg unaltered
// (that leg is sanitized only at the viewer, per ptyhost/doc.go's Boundary
// section). The sanitize call and the resulting writeEventLocked happen under
// one mu.Lock so the stateful sanitizer can never be raced by a concurrent
// resize/marker/output call.
func (r *recorder) output(relMicros uint64, data []byte) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	sanitized := r.san.Write(data)
	r.writeEventLocked(relMicros, "o", string(sanitized))
}

// resize records an applied resize ("r") as "COLSxROWS" (cast v2.1).
func (r *recorder) resize(relMicros uint64, cols, rows uint64) {
	r.writeEvent(relMicros, "r", strconv.FormatUint(cols, 10)+"x"+strconv.FormatUint(rows, 10))
}

// marker records an annotation ("m").
func (r *recorder) marker(relMicros uint64, label string) {
	r.writeEvent(relMicros, "m", label)
}

// close flushes and best-effort fsyncs the cast file.
func (r *recorder) close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.w != nil {
		_ = r.w.Flush()
	}
	if r.f != nil {
		_ = r.f.Sync()
		_ = r.f.Close()
	}
}
