package runner

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Bounded redacted transcript evidence for failed sessions.
//
// Before a failed-session teardown, the runner places safe transcript tails
// in the same rescue attempt as unpublished work (including an
// evidence-only attempt when there is no unpublished work). Only regular
// declared files are archived: symlinks (including symlinked directories on
// a relative declaration) and identity changes between validation and read
// are refused. The newest complete lines are kept under explicit per-file
// and per-attempt caps, redacted record by record (see
// transcript_evidence_redact.go for the contract), written atomically with
// owner-only modes, and verified by readback. Any read, validation, write,
// or readback failure is reported to the caller with the failing file's
// index; the caller keeps the workarea when it owns the teardown and records
// a value-free, path-free warning.

// transcriptEvidencePerFileCapBytes bounds one archived transcript tail.
var transcriptEvidencePerFileCapBytes int64 = 64 * 1024

// transcriptEvidencePerAttemptCapBytes bounds all archived transcript tails
// in one rescue attempt.
var transcriptEvidencePerAttemptCapBytes int64 = 256 * 1024

// transcriptEvidenceIOTimeout bounds every blocking evidence file operation
// (validation, open, stat, tail read, atomic write, readback) so a stalled
// transcript on network/FUSE storage cannot hang failed-session
// finalization. Each operation fails closed on timeout and the caller keeps
// the workarea. A variable so tests prove the deadline without waiting the
// production budget.
var transcriptEvidenceIOTimeout = 800 * time.Millisecond

// transcriptEvidenceLstat validates the declared path without following a
// final symlink. A variable so tests can prove the fail-closed branches.
var transcriptEvidenceLstat = os.Lstat

// transcriptEvidenceOpen opens the validated path for reading without
// blocking on a FIFO swap. A variable so tests can inject an open failure
// without touching the filesystem.
var transcriptEvidenceOpen = openTranscriptEvidenceFile

// openTranscriptEvidenceFile opens path with O_NONBLOCK so a
// post-validation swap to a FIFO without a writer cannot hang
// failed-session finalization: the non-blocking open succeeds and the
// caller's Stat/SameFile checks refuse the non-regular file before any
// read. Regular-file reads are unaffected by O_NONBLOCK.
func openTranscriptEvidenceFile(path string) (*os.File, error) {
	//nolint:gosec // G304: path is the validated declared transcript path.
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

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

// transcriptRedactionTimeout bounds one file's redaction (see
// redactTranscriptEvidence): JSON nested in strings costs twice per level,
// so a pathological line must not stall failed-session finalization. On
// timeout the file falls back to a single {"redactedRecord":true} record.
// A variable so tests prove the deadline without waiting the production
// budget.
var transcriptRedactionTimeout = 2 * time.Second

// transcriptEvidenceUnleasedRetains counts workareas kept after an
// unleased tail failure ("workarea retained"). With no lease no reaper
// owns the workarea, so the keep would otherwise be invisible: the count
// plus the warning names every kept workarea for the operator sweep. It is
// a process-local metric; tests read it via
// transcriptEvidenceRetainedWorkareas.
var transcriptEvidenceUnleasedRetains atomic.Int64

// transcriptEvidenceRetainedWorkareas reports how many unleased workareas
// this process kept after a tail failure.
func transcriptEvidenceRetainedWorkareas() int64 {
	return transcriptEvidenceUnleasedRetains.Load()
}

// resolveTranscriptEvidencePath resolves one declared transcript path: an
// absolute path is used as-is (trusted handle output; archives stay
// local-only and are never uploaded), a relative path resolves against the
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
// file does not end in newline) dropped. When the window already starts at
// a line boundary (the preceding byte is a newline) the whole window is
// kept. It returns nil when no complete line fits.
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
		offset := size - perFileCap
		// A window that starts at a line boundary is already complete:
		// only drop a leading partial line. The byte before the window
		// decides; offset > 0 here because size > perFileCap.
		atBoundary := false
		if _, err := f.Seek(offset-1, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek transcript tail boundary: %w", err)
		}
		prev := make([]byte, 1)
		if _, err := io.ReadFull(f, prev); err != nil {
			return nil, fmt.Errorf("read transcript tail boundary: %w", err)
		}
		atBoundary = prev[0] == '\n'
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek transcript tail: %w", err)
		}
		buf := make([]byte, perFileCap)
		if _, err := io.ReadFull(f, buf); err != nil {
			return nil, fmt.Errorf("read transcript tail: %w", err)
		}
		raw = buf
		if !atBoundary {
			// Drop the leading partial line: keep from after the first newline.
			if idx := strings.IndexByte(string(raw), '\n'); idx >= 0 {
				raw = raw[idx+1:]
			} else {
				return nil, nil
			}
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

// transcriptEvidenceSafeErr renders err for structured logs without
// exposing the declared transcript path or any embedded secret. PathError
// paths are the resolved declared path (which may embed a session
// credential in its filename); only the operation and the underlying
// reason are kept, redacted through both rails.
func transcriptEvidenceSafeErr(err error, values []string) string {
	if err == nil {
		return ""
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		msg := pe.Op
		if pe.Err != nil {
			msg += ": " + pe.Err.Error()
		}
		return newTranscriptRedactor(values).redactString(msg)
	}
	return newTranscriptRedactor(values).redactString(err.Error())
}

// transcriptEvidenceCallWithTimeout runs fn with the evidence I/O deadline.
// A read that never returns (a regular transcript on stalled network/FUSE
// storage, where O_NONBLOCK gives regular-file reads no timeout) fails
// closed here so Runner.Run can still reach its terminal result delivery
// and retention decision instead of hanging finalization.
func transcriptEvidenceCallWithTimeout(fn func() error) error {
	if transcriptEvidenceIOTimeout <= 0 {
		return fn()
	}
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(transcriptEvidenceIOTimeout):
		return fmt.Errorf("transcript evidence I/O timed out")
	}
}

// transcriptEvidenceValueWithTimeout runs a value-returning evidence file
// operation with the same deadline. The buffered channel avoids a data race
// when the operation outlives the timeout: the late sender never blocks and
// the caller discards its result fail-closed.
func transcriptEvidenceValueWithTimeout[T any](fn func() (T, error)) (T, error) {
	if transcriptEvidenceIOTimeout <= 0 {
		return fn()
	}
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := fn()
		done <- result{v: v, err: err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-time.After(transcriptEvidenceIOTimeout):
		var zero T
		return zero, fmt.Errorf("transcript evidence I/O timed out")
	}
}

// transcriptRedactionValueWithTimeout runs a value-returning redaction
// with the per-file redaction deadline. The buffered channel avoids a data
// race when the redaction outlives the timeout: the late sender never blocks
// and the caller discards its result fail-closed to {"redactedRecord":true}.
func transcriptRedactionValueWithTimeout(fn func() (string, error)) (string, error) {
	if transcriptRedactionTimeout <= 0 {
		return fn()
	}
	type result struct {
		v   string
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := fn()
		done <- result{v: v, err: err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-time.After(transcriptRedactionTimeout):
		return "", fmt.Errorf("transcript redaction timed out")
	}
}

// capTranscriptBytes keeps the newest complete lines of s under capBytes.
// s must end in newline; the result does too, or is empty when no complete
// line fits. When the window already starts at a line boundary the whole
// window is kept; otherwise the leading partial line is dropped.
func capTranscriptBytes(s string, capBytes int64) string {
	if capBytes < 0 {
		return ""
	}
	if capBytes == 0 {
		return ""
	}
	if int64(len(s)) <= capBytes {
		return s
	}
	// len(s) > capBytes, so the index before the window exists.
	if s[len(s)-int(capBytes)-1] == '\n' {
		return s[len(s)-int(capBytes):]
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
// both the runner envelope and the terminal wire copy. disposition states
// what happened to the workarea; it is a fixed caller literal. An unleased
// "workarea retained" keep has no reaper, so it also bumps the retained
// metric the operator sweep reads.
func appendTranscriptEvidenceWarning(res *Result, index int, disposition string) {
	if disposition == "workarea retained" {
		transcriptEvidenceUnleasedRetains.Add(1)
	}
	warning := fmt.Sprintf("transcript evidence unavailable for file %d; %s", index, disposition)
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
// proves it matches what was intended to be written. The type and mode are
// checked before any read and the file is opened non-blocking so a
// post-write swap to a FIFO without a writer cannot hang finalization.
func verifyTranscriptEvidenceReadback(path string, expected []byte) error {
	//nolint:gosec // G304: path is the runner-owned evidence file just written.
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
	//nolint:gosec // G304: path validated above as the runner-owned evidence file.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("read back evidence file: %w", err)
	}
	finfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat evidence file: %w", err)
	}
	if !finfo.Mode().IsRegular() || finfo.Mode()&os.ModeSymlink != 0 {
		_ = f.Close()
		return fmt.Errorf("evidence file is not a regular file")
	}
	if !os.SameFile(info, finfo) {
		_ = f.Close()
		return fmt.Errorf("evidence file identity changed")
	}
	if finfo.Mode().Perm()&0o077 != 0 {
		_ = f.Close()
		return fmt.Errorf("evidence file has unsafe mode %o", finfo.Mode().Perm())
	}
	got, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("read back evidence file: %w", err)
	}
	if string(got) != string(expected) {
		return fmt.Errorf("evidence readback mismatch")
	}
	return nil
}

// archiveFailedSessionEvidence places redacted transcript tails for a failed
// session into the current rescue attempt from dirFor. It returns the index
// of the first declared file that could not be safely preserved and false;
// the caller decides what that means for the workarea and records the
// warning. Non-failed sessions and sessions without descriptors are a no-op
// success.
func (r *Runner) archiveFailedSessionEvidence(qw QueuedWork, res *Result, dirFor func() (string, error)) (int, bool) {
	if res == nil || res.Status != "failed" {
		return 0, true
	}
	values := transcriptEvidenceSecretValues(qw)
	remaining := transcriptEvidencePerAttemptCapBytes
	for i, desc := range res.terminalTranscripts {
		tail, err := readTranscriptEvidenceTail(desc.Path, res)
		if err != nil {
			r.logger.Warn("transcript evidence read refused", "sessionId", qw.SessionID, "file", i, "err", transcriptEvidenceSafeErr(err, values))
			return i, false
		}
		if len(tail) == 0 {
			continue
		}
		// A pathological line (JSON nested in strings) must not stall
		// finalization: on deadline the file falls back to a single
		// redacted record rather than leaking or hanging.
		redacted, err := transcriptRedactionValueWithTimeout(func() (string, error) {
			return redactTranscriptEvidence(string(tail), values), nil
		})
		if err != nil {
			r.logger.Warn("transcript evidence redaction timed out; storing redacted record", "sessionId", qw.SessionID, "file", i)
			redacted = "{\"redactedRecord\":true}\n"
		}
		redacted = capTranscriptBytes(redacted, transcriptEvidencePerFileCapBytes)
		if int64(len(redacted)) > remaining {
			redacted = capTranscriptBytes(redacted, remaining)
		}
		if len(redacted) == 0 {
			continue
		}
		dir, err := dirFor()
		if err != nil {
			r.logger.Warn("transcript evidence write failed", "sessionId", qw.SessionID, "file", i, "err", transcriptEvidenceSafeErr(err, values))
			return i, false
		}
		name := transcriptEvidenceName(i)
		if err := transcriptEvidenceCallWithTimeout(func() error {
			return transcriptEvidenceWriteAtomic(dir, name, []byte(redacted))
		}); err != nil {
			r.logger.Warn("transcript evidence write failed", "sessionId", qw.SessionID, "file", i, "err", transcriptEvidenceSafeErr(err, values))
			return i, false
		}
		if err := transcriptEvidenceCallWithTimeout(func() error {
			return transcriptEvidenceReadback(filepath.Join(dir, name), []byte(redacted))
		}); err != nil {
			r.logger.Warn("transcript evidence readback failed", "sessionId", qw.SessionID, "file", i, "err", transcriptEvidenceSafeErr(err, values))
			return i, false
		}
		remaining -= int64(len(redacted))
		r.logger.Warn("failed-session transcript evidence preserved",
			"sessionId", qw.SessionID,
			"evidence", filepath.Join(dir, name),
			"bytes", len(redacted),
		)
	}
	return 0, true
}

// readTranscriptEvidenceTail validates one declared transcript and reads its
// bounded tail. A relative declaration must not traverse a symlinked
// directory inside the workarea (the agent controls that tree, so a link
// could point the archiver at a file outside it); the file itself must be
// regular and keep its identity from validation through the read. Every
// blocking step runs under the evidence I/O deadline.
func readTranscriptEvidenceTail(declared string, res *Result) ([]byte, error) {
	resolved, err := resolveTranscriptEvidencePath(declared, res)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(declared) {
		if err := transcriptEvidenceCallWithTimeout(func() error {
			return refuseTranscriptAncestorLinks(declared, res)
		}); err != nil {
			return nil, err
		}
	}
	linfo, err := transcriptEvidenceValueWithTimeout(func() (os.FileInfo, error) {
		return transcriptEvidenceLstat(resolved)
	})
	if err != nil {
		return nil, err
	}
	if linfo.Mode()&os.ModeSymlink != 0 || !linfo.Mode().IsRegular() {
		return nil, fmt.Errorf("transcript is not a regular file")
	}
	if transcriptEvidenceAfterValidate != nil {
		transcriptEvidenceAfterValidate(resolved)
	}
	f, err := transcriptEvidenceValueWithTimeout(func() (*os.File, error) {
		return transcriptEvidenceOpen(resolved)
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return transcriptEvidenceValueWithTimeout(func() ([]byte, error) {
		finfo, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if finfo.Mode()&os.ModeSymlink != 0 || !finfo.Mode().IsRegular() {
			return nil, fmt.Errorf("transcript is not a regular file")
		}
		if !sameTranscriptFile(linfo, finfo) {
			return nil, fmt.Errorf("transcript identity changed between validation and read")
		}
		return transcriptEvidenceReadTail(f, finfo, transcriptEvidencePerFileCapBytes)
	})
}

// refuseTranscriptAncestorLinks rejects a relative declaration whose
// directories below the workarea base include a symlink or a non-directory.
func refuseTranscriptAncestorLinks(declared string, res *Result) error {
	base := res.WorktreePath
	if base == "" {
		base = res.WorkareaRoot
	}
	parts := strings.Split(strings.TrimPrefix(filepath.Clean("/"+declared), "/"), "/")
	cur := base
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		info, err := transcriptEvidenceLstat(cur)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("transcript directory is not a plain directory")
		}
	}
	return nil
}
