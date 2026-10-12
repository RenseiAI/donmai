package muse

import (
	"encoding/json"
	"io"
	"testing"
)

// pipePair is a closeable pipe pair for the hand-rolled client tests.
func newPipePair(t *testing.T) (*io.PipeReader, *io.PipeWriter) {
	t.Helper()
	r, w := io.Pipe()
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	return r, w
}

func jsonDecoder(r *io.PipeReader) func(*map[string]any) error {
	decoder := json.NewDecoder(r)
	return func(v *map[string]any) error { return decoder.Decode(v) }
}

func writeJSON(t *testing.T, w *io.PipeWriter, body any) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_, _ = w.Write(append(buf, '\n'))
}
