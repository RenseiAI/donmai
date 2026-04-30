package daemon

import (
	"bufio"
	"io"
)

// pumpLines streams lines from r through the writer with a worker-tagged
// prefix. It returns when r reaches EOF or errors.
func pumpLines(r io.Reader, workerID string, w PrefixedWriter) {
	if r == nil {
		return
	}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		w.WriteWorkerLine(workerID, scanner.Text())
	}
}

// drain reads r to completion and discards the bytes.
func drain(r io.Reader) {
	if r == nil {
		return
	}
	_, _ = io.Copy(io.Discard, r)
}
