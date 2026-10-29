package runner

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/RenseiAI/donmai/internal/diagnostic"
)

// Bounded redacted transcript evidence for failed sessions.
//
// Before a failed-session teardown, the runner places safe transcript tails
// in the same rescue attempt as unpublished work (including an
// evidence-only attempt when there is no unpublished work). Only regular
// declared files are archived: symlinks and identity changes between
// validation and read are refused. The newest complete JSONL tail is kept
// under explicit per-file and per-attempt caps, redacted with
// diagnostic.Redact and every wipCheckpointSecretValues value, written
// atomically with owner-only modes, and verified by readback. Any read,
// validation, write, or readback failure returns preservation failure with
// a value-free path-safe warning so the caller keeps the workarea.

// transcriptEvidencePerFileCapBytes bounds one archived transcript tail.
var transcriptEvidencePerFileCapBytes int64 = 64 * 1024

// transcriptEvidencePerAttemptCapBytes bounds all archived transcript tails
// in one rescue attempt.
var transcriptEvidencePerAttemptCapBytes int64 = 256 * 1024

// transcriptEvidenceLstat validates the declared path without following a
// final symlink. A variable so tests can prove the fail-closed branches.
var transcriptEvidenceLstat = os.Lstat

// transcriptEvidenceOpen opens the validated path for reading. A variable
// so tests can inject an open failure without touching the filesystem.
var transcriptEvidenceOpen = os.Open

// transcriptEvidenceAfterValidate runs between validation (Lstat) and read
// (Open). Nil in production; tests arm it to swap the file identity and
// prove the SameFile check refuses the swap before a byte is read.
var transcriptEvidenceAfterValidate func(path string)

// transcriptEvidenceWriteAtomic writes one evidence file atomically. A
// variable so tests can inject a write failure.
var transcriptEvidenceWriteAtomic = writeTranscriptEvidenceAtomic

// transcriptEvidenceReadback verifies one written evidence file. A variable
// so tests can inject a readback failure.
var transcriptEvidenceReadback = verifyTranscriptEvidenceReadback

// transcriptEvidenceReadTail reads the bounded tail of an already-opened
// transcript. A variable so tests can inject a read failure.
var transcriptEvidenceReadTail = readTranscriptTailBounded

// resolveTranscriptEvidencePath resolves one declared transcript path: an
// absolute path is used as-is, a relative path resolves against the
// session worktree (falling back to the workarea root). It never follows
// symlinks itself; validation does that.
func resolveTranscriptEvidencePath(declared string, res *Result) (string, error) {
	if declared == "" {
		return "", fmt.Errorf("empty transcript path")
	}
	if filepath.IsAbs(declared) {
		return filepath.Clean(declared), nil
	}
	base := ""
	if res != nil {
		if res.WorktreePath != "" {
			base = res.WorktreePath
		} else if res.WorkareaRoot != "" {
			base = res.WorkareaRoot
		}
	}
	if base == "" {
		return "", fmt.Errorf("no workarea to resolve transcript path")
	}
	return filepath.Join(base, filepath.Clean("/"+declared)), nil
}

// sameTranscriptFile reports whether two FileInfos describe the same file.
func sameTranscriptFile(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return false
	}
	return os.SameFile(a, b)
}

// readTranscriptTailBounded keeps the newest complete JSONL tail of an
// opened transcript under perFileCap bytes: the last perFileCap bytes with
// the leading partial line dropped and the trailing partial line (when the
// file does not end in newline) dropped. It returns nil when no complete
// line fits.
func readTranscriptTailBounded(f *os.File, finfo os.FileInfo, perFileCap int64) ([]byte, error) {
	if perFileCap <= 0 {
		return nil, fmt.Errorf("invalid per-file cap %d", perFileCap)
	}
	size := finfo.Size()
	if size <= 0 {
		return nil, nil
	}
	var raw []byte
	if size <= perFileCap {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek transcript: %w", err)
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(f, buf); err != nil {
			return nil, fmt.Errorf("read transcript: %w", err)
		}
		raw = buf
	} else {
		if _, err := f.Seek(size-perFileCap, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek transcript tail: %w", err)
		}
		buf := make([]byte, perFileCap)
		if _, err := io.ReadFull(f, buf); err != nil {
			return nil, fmt.Errorf("read transcript tail: %w", err)
		}
		raw = buf
		// Drop the leading partial line: keep from after the first newline.
		if idx := strings.IndexByte(string(raw), '\n'); idx >= 0 {
			raw = raw[idx+1:]
		} else {
			return nil, nil
		}
	}
	if len(raw) == 0 {
		return nil, nil
	}
	// Drop the trailing partial line when the file does not end in newline.
	// raw's last byte is the file's last byte (we always read to the end),
	// so a missing trailing newline means the last line is incomplete.
	if raw[len(raw)-1] != '\n' {
		last := strings.LastIndexByte(string(raw), '\n')
		if last < 0 {
			return nil, nil
		}
		raw = raw[:last+1]
	}
	if len(raw) == 0 {
		return nil, nil
	}
	return raw, nil
}

// redactTranscriptEvidence redacts one tail with both rails: the shared
// output rail and every session secret value.
func redactTranscriptEvidence(tail string, values []string) string {
	redacted := diagnostic.Redact(tail)
	for _, v := range values {
		if v == "" {
			continue
		}
		redacted = strings.ReplaceAll(redacted, v, "[REDACTED]")
	}
	return redacted
}

// capTranscriptBytes keeps the newest complete lines of s under capBytes.
// s must end in newline; the result does too, or is empty when no complete
// line fits.
func capTranscriptBytes(s string, capBytes int64) string {
	if capBytes < 0 {
		return ""
	}
	if int64(len(s)) <= capBytes {
		return s
	}
	tail := s[len(s)-int(capBytes):]
	if idx := strings.IndexByte(tail, '\n'); idx >= 0 {
		return tail[idx+1:]
	}
	return ""
}

// transcriptEvidenceName names the i-th evidence file in an attempt. Index
// naming keeps the name value-free and path-safe: no declared path or
// secret ever reaches the filesystem name or a warning.
func transcriptEvidenceName(i int) string {
	return fmt.Sprintf("transcript-%d.jsonl", i)
}

// appendTranscriptEvidenceWarning records a value-free path-safe warning on
// both the runner envelope and the terminal wire copy.
func appendTranscriptEvidenceWarning(res *Result, index int) {
	warning := fmt.Sprintf("transcript evidence unavailable for file %d; workarea retained", index)
	if res == nil {
		return
	}
	res.PostSessionWarnings = append(res.PostSessionWarnings, warning)
	res.Result.PostSessionWarnings = append(res.Result.PostSessionWarnings, warning)
}

// writeTranscriptEvidenceAtomic writes data to dir/name atomically with
// owner-only modes: a 0600 temp file in the same directory, then rename.
func writeTranscriptEvidenceAtomic(dir, name string, data []byte) error {
	if strings.Contains(name, "/") || strings.Contains(name, string(filepath.Separator)) || !strings.HasSuffix(name, ".jsonl") {
		return fmt.Errorf("unsafe evidence name")
	}
	//nolint:gosec // G304: dir is the runner-owned rescue attempt, name is the fixed transcript-<n>.jsonl form.
	tmp, err := os.CreateTemp(dir, ".tmp-evidence-*")
	if err != nil {
		return fmt.Errorf("create evidence temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Best effort cleanup on failure; a successful rename removes tmpName.
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod evidence temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write evidence temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close evidence temp file: %w", err)
	}
	dest := filepath.Join(dir, name)
	//nolint:gosec // G304: dest is dir plus the fixed evidence name above.
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod evidence temp file: %w", err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("commit evidence file: %w", err)
	}
	// Owner-only mode must survive the rename across filesystems/umasks.
	//nolint:gosec // G304: dest is the runner-owned evidence file just renamed.
	if err := os.Chmod(dest, 0o600); err != nil {
		return fmt.Errorf("chmod evidence file: %w", err)
	}
	return nil
}

// verifyTranscriptEvidenceReadback re-reads a written evidence file and
// proves it matches what was intended to be written.
func verifyTranscriptEvidenceReadback(path string, expected []byte) error {
	//nolint:gosec // G304: path is the runner-owned evidence file just written.
	got, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read back evidence file: %w", err)
	}
	if string(got) != string(expected) {
		return fmt.Errorf("evidence readback mismatch")
	}
	//nolint:gosec // G304: path validated above as the runner-owned evidence file.
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat evidence file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("evidence file is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("evidence file has unsafe mode %o", info.Mode().Perm())
	}
	return nil
}

// archiveFailedSessionEvidence places redacted transcript tails for a failed
// session into the current rescue attempt from dirFor. It returns false when
// any declared file cannot be safely preserved; the caller must then keep
// the workarea. Non-failed sessions and sessions without descriptors are a
// no-op success.
func (r *Runner) archiveFailedSessionEvidence(qw QueuedWork, res *Result, dirFor func() (string, error)) bool {
	if res == nil || res.Status != "failed" {
		return true
	}
	if len(res.terminalTranscripts) == 0 {
		return true
	}
	values := wipCheckpointSecretValues(qw)
	remaining := transcriptEvidencePerAttemptCapBytes
	for i, desc := range res.terminalTranscripts {
		resolved, err := resolveTranscriptEvidencePath(desc.Path, res)
		if err != nil {
			r.logger.Warn("transcript evidence validation failed; keeping workarea", "sessionId", qw.SessionID, "err", err)
			appendTranscriptEvidenceWarning(res, i)
			return false
		}
		linfo, err := transcriptEvidenceLstat(resolved)
		if err != nil {
			r.logger.Warn("transcript evidence validation failed; keeping workarea", "sessionId", qw.SessionID, "err", err)
			appendTranscriptEvidenceWarning(res, i)
			return false
		}
		if linfo.Mode()&os.ModeSymlink != 0 || !linfo.Mode().IsRegular() {
			r.logger.Warn("transcript evidence is not a regular file; keeping workarea", "sessionId", qw.SessionID)
			appendTranscriptEvidenceWarning(res, i)
			return false
		}
		if transcriptEvidenceAfterValidate != nil {
			transcriptEvidenceAfterValidate(resolved)
		}
		//nolint:gosec // G304: resolved is the validated declared transcript path.
		f, err := transcriptEvidenceOpen(resolved)
		if err != nil {
			r.logger.Warn("transcript evidence read failed; keeping workarea", "sessionId", qw.SessionID, "err", err)
			appendTranscriptEvidenceWarning(res, i)
			return false
		}
		finfo, statErr := f.Stat()
		tail, readErr := func() ([]byte, error) {
			if statErr != nil {
				return nil, statErr
			}
			if finfo.Mode()&os.ModeSymlink != 0 || !finfo.Mode().IsRegular() {
				return nil, fmt.Errorf("transcript is not a regular file")
			}
			if !sameTranscriptFile(linfo, finfo) {
				return nil, fmt.Errorf("transcript identity changed between validation and read")
			}
			return transcriptEvidenceReadTail(f, finfo, transcriptEvidencePerFileCapBytes)
		}()
		_ = f.Close()
		if readErr != nil {
			r.logger.Warn("transcript evidence read failed; keeping workarea", "sessionId", qw.SessionID, "err", readErr)
			appendTranscriptEvidenceWarning(res, i)
			return false
		}
		if len(tail) == 0 {
			continue
		}
		redacted := redactTranscriptEvidence(string(tail), values)
		redacted = capTranscriptBytes(redacted, transcriptEvidencePerFileCapBytes)
		if int64(len(redacted)) > remaining {
			redacted = capTranscriptBytes(redacted, remaining)
		}
		if len(redacted) == 0 {
			continue
		}
		dir, err := dirFor()
		if err != nil {
			r.logger.Warn("transcript evidence write failed; keeping workarea", "sessionId", qw.SessionID, "err", err)
			appendTranscriptEvidenceWarning(res, i)
			return false
		}
		name := transcriptEvidenceName(i)
		if err := transcriptEvidenceWriteAtomic(dir, name, []byte(redacted)); err != nil {
			r.logger.Warn("transcript evidence write failed; keeping workarea", "sessionId", qw.SessionID, "err", err)
			appendTranscriptEvidenceWarning(res, i)
			return false
		}
		if err := transcriptEvidenceReadback(filepath.Join(dir, name), []byte(redacted)); err != nil {
			r.logger.Warn("transcript evidence readback failed; keeping workarea", "sessionId", qw.SessionID, "err", err)
			appendTranscriptEvidenceWarning(res, i)
			return false
		}
		remaining -= int64(len(redacted))
		r.logger.Warn("failed-session transcript evidence preserved",
			"sessionId", qw.SessionID,
			"evidence", filepath.Join(dir, name),
			"bytes", len(redacted),
		)
	}
	return true
}
