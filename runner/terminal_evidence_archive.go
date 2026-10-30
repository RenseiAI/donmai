package runner

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// terminalEvidenceMaxBytes bounds one archived evidence file. Beyond it the
// file is skipped: a bounded local copy, never an unbounded read.
const terminalEvidenceMaxBytes = 64 << 20

// readTerminalEvidenceChunk reads one opened evidence file with a bounded
// probe: up to limit+1 bytes. The extra byte distinguishes a file that
// grew past the bound after Stat (the read observes limit+1 bytes, which
// the caller refuses) from one that fits (the read observes at most limit
// bytes, accepted). Without the probe byte a file that grew mid-read
// would archive as a truncated success alongside a manifest that promises
// the complete descriptor.
func readTerminalEvidenceChunk(f *os.File, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(f, limit+1))
}

// copyTerminalEvidenceContents best-effort copies every declared terminal
// evidence file's verified bytes into the teardown's archive directory dir,
// which archiveTerminalEvidence already created for the evidence manifest.
// The manifest (descriptor paths) is the durable record and is written
// first; these copies are a convenience alongside it. Every file is opened
// and verified exactly as a fail-closed preservation would be, but a file
// that cannot be safely copied is skipped with a log line: the copy never
// changes the manifest, never fails the teardown, and never keeps the
// workarea. Callers rely on archiveTerminalEvidence's status gate (only
// failed and stopped sessions) and its empty-descriptor no-op, so this
// function only skips what is unsafe to read.
func (r *Runner) copyTerminalEvidenceContents(sessionID string, res *Result, dir string) {
	if res == nil || dir == "" {
		return
	}
	for i, desc := range res.terminalTranscripts {
		if desc.Path == "" {
			continue
		}
		raw, ok := r.readVerifiedEvidenceFile(desc.Path)
		if !ok {
			r.logger.Warn("terminal evidence copy skipped; manifest still records the descriptor",
				"sessionId", sessionID, "index", i)
			continue
		}
		base := rescueSegment(filepath.Base(desc.Path))
		dest := filepath.Join(dir, fmt.Sprintf("evidence-%02d-%s", i, base))
		if !r.writeVerifiedEvidenceCopy(dest, raw) {
			r.logger.Warn("terminal evidence copy skipped; manifest still records the descriptor",
				"sessionId", sessionID, "index", i)
		}
	}
}

// readVerifiedEvidenceFile opens one declared evidence file and returns its
// verified bytes. The pre-open Lstat is only a fast-path hint; the post-open
// Stat is authoritative: the descriptor or one of its parent directories
// could have been swapped between the hint and Open. O_NOFOLLOW stops only
// a swapped trailing symlink; a swapped parent directory redirects the open
// through a symlink to an outside file with the same basename, which opens
// as a regular file. The SameFile identity comparison closes that seam:
// any file that is not the Lstat-observed file is refused. The LimitReader
// probe additionally caps a file that grew in that window, so the read
// never allocates beyond the bound plus one probe byte. Any refusal
// returns ok=false and the caller skips the file.
func (r *Runner) readVerifiedEvidenceFile(path string) (raw []byte, ok bool) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	// Reject oversized files before reading so the size check bounds
	// allocation: an unbounded ReadFile would load the whole transcript
	// into memory before the limit could apply.
	if fi.Size() > terminalEvidenceMaxBytes {
		return nil, false
	}
	// Open without following a trailing symlink and without blocking
	// on a FIFO: a path swapped to a symlink between Lstat and Open
	// fails here with ELOOP instead of archiving its target, and a
	// path swapped to a FIFO opens without blocking so the
	// regular-file check below can reject it.
	openFile := r.terminalEvidenceOpenFile
	if openFile == nil {
		openFile = openTerminalEvidenceFile
	}
	f, err := openFile(path)
	if err != nil {
		return nil, false
	}
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || st.Size() > terminalEvidenceMaxBytes || !os.SameFile(fi, st) {
		_ = f.Close()
		return nil, false
	}
	readSource := r.terminalEvidenceReadSource
	if readSource == nil {
		readSource = func(f *os.File) ([]byte, error) {
			return readTerminalEvidenceChunk(f, terminalEvidenceMaxBytes)
		}
	}
	raw, err = readSource(f)
	_ = f.Close()
	if err != nil {
		return nil, false
	}
	if int64(len(raw)) > terminalEvidenceMaxBytes {
		return nil, false
	}
	return raw, true
}

// writeVerifiedEvidenceCopy writes one verified evidence copy and reads it
// back. A write failure or a readback mismatch removes the destination so
// no partial or corrupted copy is mistaken for evidence, and returns false
// so the caller skips the file. Best-effort throughout: the return only
// decides whether this file is skipped, never the teardown.
func (r *Runner) writeVerifiedEvidenceCopy(dest string, raw []byte) bool {
	writeFile := r.terminalEvidenceWriteFile
	if writeFile == nil {
		writeFile = os.WriteFile
	}
	readFile := r.terminalEvidenceReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	//nolint:gosec // G703: dest is runner-owned under the rescue root, base is rescueSegment-sanitized
	if err := writeFile(dest, raw, 0o600); err != nil {
		return false
	}
	//nolint:gosec // G304: dest is runner-owned under the rescue root.
	back, err := readFile(dest)
	if err != nil || !bytes.Equal(raw, back) {
		_ = os.Remove(dest)
		return false
	}
	return true
}
