package ptyhost

import (
	"bufio"
	"encoding/json"
	"os"
	"strconv"
	"sync"
	"time"
)

// recorder writes a parallel asciinema v2 cast of the session (§16): a header
// line followed by [time, code, data] event lines. It shares the process-spawn
// rel_time anchor with the wire (§2), so the cast and the live stream stay
// aligned. All methods are called under the Session mutex (write-through).
type recorder struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
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
	r := &recorder{f: f, w: bufio.NewWriter(f)}
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

// output records terminal output ("o").
func (r *recorder) output(relMicros uint64, data []byte) {
	r.writeEvent(relMicros, "o", string(data))
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
