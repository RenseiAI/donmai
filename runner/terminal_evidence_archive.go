package runner

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// terminalEvidenceMaxBytes bounds one archived evidence file. Beyond it the
// archive is abandoned and the workarea is kept instead: a bounded local
// copy, never an unbounded read.
const terminalEvidenceMaxBytes = 64 << 20

// terminalEvidenceWarnMessage is the only warning terminal evidence
// preservation emits. It is path-safe and value-free: no filesystem path,
// no file content, no error detail. The workarea is retained alongside it,
// so the evidence is never deleted without a copy.
const terminalEvidenceWarnMessage = "terminal evidence preservation failed; workarea retained"

// readTerminalEvidenceChunk reads one opened evidence file with a bounded
// probe: up to limit+1 bytes. The extra byte distinguishes a file that
// grew past the bound after Stat (the read observes limit+1 bytes, which
// the caller refuses) from one that fits (the read observes at most limit
// bytes, accepted). Without the probe byte a file that grew mid-read
// would archive as a truncated success and authorize teardown of the
// workarea holding the only complete copy.
func readTerminalEvidenceChunk(f *os.File, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(f, limit+1))
}

// needsTerminalEvidenceArchive reports whether a terminal result owes a
// terminal evidence archive. Only failed and stopped sessions archive;
// completed sessions (and empty or descriptor-free results) are untouched.
func needsTerminalEvidenceArchive(res *Result) bool {
	if res == nil {
		return false
	}
	if res.Status != "failed" && res.Status != "stopped" {
		return false
	}
	return len(res.terminalTranscripts) > 0
}

// preserveTerminalEvidence archives every declared terminal evidence file
// before teardown and verifies each copy by readback. It returns false when
// any declared file cannot be safely preserved; the caller must then keep
// the workarea instead of tearing it down.
//
// The archive lives under the rescue root
// (<rescue>/<session>/<stamp>-<unique>/evidence-<index>-<base>) so the copy
// survives the workarea deletion that follows. Completed sessions and
// descriptor-free results are a no-op returning true. No network call, no
// timeout: plain filesystem copies verified byte for byte.
func (r *Runner) preserveTerminalEvidence(qw QueuedWork, res *Result) bool {
	if !needsTerminalEvidenceArchive(res) {
		return true
	}
	sessionRoot := filepath.Join(r.rescueRoot(), rescueSegment(qw.SessionID))
	if err := os.MkdirAll(sessionRoot, 0o700); err != nil {
		r.warnTerminalEvidencePreservation(qw, res)
		return false
	}
	stamp := r.now().UTC().Format("20060102T150405Z")
	archiveDir, err := os.MkdirTemp(sessionRoot, stamp+"-")
	if err != nil {
		r.warnTerminalEvidencePreservation(qw, res)
		return false
	}
	for i, desc := range res.terminalTranscripts {
		if desc.Path == "" {
			r.warnTerminalEvidencePreservation(qw, res)
			return false
		}
		fi, err := os.Lstat(desc.Path)
		if err != nil || !fi.Mode().IsRegular() {
			r.warnTerminalEvidencePreservation(qw, res)
			return false
		}
		// Reject oversized files before reading so the size check bounds
		// allocation: an unbounded ReadFile would load the whole transcript
		// into memory before the limit could apply.
		if fi.Size() > terminalEvidenceMaxBytes {
			r.warnTerminalEvidencePreservation(qw, res)
			return false
		}
		// Open without following a trailing symlink and without blocking
		// on a FIFO: a path swapped to a symlink between Lstat and Open
		// fails here with ELOOP instead of archiving its target, and a
		// path swapped to a FIFO opens without blocking so the
		// regular-file check below can reject it. The post-open Stat is
		// authoritative; the pre-open Lstat is only a fast-path hint.
		openFile := r.terminalEvidenceOpenFile
		if openFile == nil {
			openFile = openTerminalEvidenceFile
		}
		f, err := openFile(desc.Path)
		if err != nil {
			r.warnTerminalEvidencePreservation(qw, res)
			return false
		}
		// Re-verify after open: the descriptor or one of its parent
		// directories could have been swapped between the Lstat hint and
		// Open. O_NOFOLLOW stops only a swapped trailing symlink; a
		// swapped parent directory redirects the open through a symlink
		// to an outside file with the same basename, which opens as a
		// regular file. The SameFile identity comparison closes that
		// seam: any file that is not the Lstat-observed file refuses, so
		// readback can only verify the original evidence and teardown can
		// only delete a workarea whose evidence was actually copied. The
		// LimitReader in the source read additionally caps a file that
		// grew in that window, so the read never allocates beyond the
		// bound plus one probe byte.
		if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || st.Size() > terminalEvidenceMaxBytes || !os.SameFile(fi, st) {
			_ = f.Close()
			r.warnTerminalEvidencePreservation(qw, res)
			return false
		}
		readSource := r.terminalEvidenceReadSource
		if readSource == nil {
			readSource = func(f *os.File) ([]byte, error) {
				return readTerminalEvidenceChunk(f, terminalEvidenceMaxBytes)
			}
		}
		raw, err := readSource(f)
		_ = f.Close()
		if err != nil {
			r.warnTerminalEvidencePreservation(qw, res)
			return false
		}
		if int64(len(raw)) > terminalEvidenceMaxBytes {
			r.warnTerminalEvidencePreservation(qw, res)
			return false
		}
		base := rescueSegment(filepath.Base(desc.Path))
		dest := filepath.Join(archiveDir, fmt.Sprintf("evidence-%02d-%s", i, base))
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
			r.warnTerminalEvidencePreservation(qw, res)
			return false
		}
		//nolint:gosec // G304: dest is runner-owned under the rescue root.
		back, err := readFile(dest)
		if err != nil || !bytes.Equal(raw, back) {
			r.warnTerminalEvidencePreservation(qw, res)
			return false
		}
	}
	r.logger.Info("terminal evidence preserved before teardown",
		"sessionId", qw.SessionID, "files", len(res.terminalTranscripts))
	return true
}

// warnTerminalEvidencePreservation emits the single value-free warning for a
// failed preservation. It carries no path and no file content on either the
// envelope or the wire copy, so redacted transcript bytes can never leak
// through the warning.
func (r *Runner) warnTerminalEvidencePreservation(qw QueuedWork, res *Result) {
	if res != nil {
		res.PostSessionWarnings = append(res.PostSessionWarnings, terminalEvidenceWarnMessage)
		res.Result.PostSessionWarnings = append(res.Result.PostSessionWarnings, terminalEvidenceWarnMessage)
	}
	r.logger.Warn(terminalEvidenceWarnMessage, "sessionId", qw.SessionID)
}
