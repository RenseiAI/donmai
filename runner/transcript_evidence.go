package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

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
// diagnostic.Redact and every session secret value, written
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

// transcriptEvidenceSecretValues collects every secret value the evidence
// archiver must redact: the session bearers, ALL session env values, and
// the process env values with secret-shaped names, plus any userinfo
// password embedded in a URL-style value. Unlike the checkpoint content
// gate it has no minimum-length floor: a short session secret that reaches
// a transcript must still fail closed.
func transcriptEvidenceSecretValues(qw QueuedWork) []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if v == "" || len(v) > wipSecretMaxValueLen || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	add(qw.AuthToken)
	add(qw.McpAuthToken)
	for _, value := range qw.Env {
		add(value)
	}
	for _, kv := range os.Environ() {
		if name, value, ok := strings.Cut(kv, "="); ok && wipSecretEnvNameRE.MatchString(name) {
			add(value)
		}
	}
	for _, v := range append([]string{}, out...) {
		if u, err := url.Parse(v); err == nil && u.User != nil {
			if pw, ok := u.User.Password(); ok {
				add(pw)
			}
		}
	}
	return out
}

// transcriptSensitiveKeyRE mirrors the shared output rail's secret-name
// list for JSON-aware redaction. A string value under one of these keys is
// replaced wholesale so an unknown password never survives with its JSON
// quoting intact. Numeric, boolean, and null values are left alone so
// ordinary token-usage counters stay valid JSON numbers.
var transcriptSensitiveKeyRE = regexp.MustCompile(`(?i)(authorization|bearer|api[_-]?keys?|access[_-]?tokens?|refresh[_-]?tokens?|auth[_-]?tokens?|client[_-]?secrets?|private[_-]?keys?|secrets?|tokens?|passwords?|passwds?|credentials?|cookies?|sessions?)`)

func isSensitiveTranscriptKey(k string) bool {
	return transcriptSensitiveKeyRE.MatchString(k)
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

// redactTranscriptEvidence redacts one tail with both rails while keeping
// every archived record valid JSONL. Each complete line is decoded, its
// string values are redacted in decoded form (so raw, JSON-escaped,
// Unicode-escaped, and nested-JSON encodings of a secret all match the same
// decoded bytes), and the record is re-encoded. Decoding first is what
// makes short, Unicode-escaped, and doubly-encoded secrets unrecoverable:
// after decoding there is only one form left to replace. Sensitive-keyed
// string values are replaced wholesale so an unknown password keeps its
// JSON quoting. Non-JSON lines fall back to text redaction.
func redactTranscriptEvidence(tail string, values []string) string {
	if tail == "" {
		return ""
	}
	parts := strings.SplitAfter(tail, "\n")
	var sb strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		if !strings.HasSuffix(part, "\n") {
			continue
		}
		line := strings.TrimSuffix(part, "\n")
		if line == "" {
			sb.WriteString("\n")
			continue
		}
		var v interface{}
		if err := parseTranscriptJSONLine(line, &v); err == nil {
			redacted := redactJSONValue(v, values)
			b, err := json.Marshal(redacted)
			if err == nil {
				sb.Write(b)
				sb.WriteByte('\n')
				continue
			}
		}
		sb.WriteString(redactTextWithSecrets(line, values))
		sb.WriteByte('\n')
	}
	return sb.String()
}

// parseTranscriptJSONLine decodes one JSONL record preserving numbers.
func parseTranscriptJSONLine(line string, out *interface{}) error {
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra interface{}
	if err := dec.Decode(&extra); err != io.EOF {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// redactJSONValue redacts one decoded JSON value in place.
func redactJSONValue(v interface{}, values []string) interface{} {
	switch typed := v.(type) {
	case string:
		return redactJSONString(typed, values)
	case map[string]interface{}:
		for k, val := range typed {
			if isSensitiveTranscriptKey(k) {
				switch sv := val.(type) {
				case string:
					_ = sv
					typed[k] = "[REDACTED]"
				case map[string]interface{}, []interface{}:
					typed[k] = redactJSONValue(val, values)
				default:
					// Numbers, booleans, and null under usage-style keys
					// (for example input_tokens) are counters, not secrets.
				}
			} else {
				typed[k] = redactJSONValue(val, values)
			}
		}
		return typed
	case []interface{}:
		for i, e := range typed {
			typed[i] = redactJSONValue(e, values)
		}
		return typed
	default:
		return v
	}
}

// redactJSONString redacts one decoded string value. When the whole string
// is itself JSON (a doubly-encoded record), the inner record is redacted
// recursively so nested secrets cannot survive behind a second encoding.
func redactJSONString(s string, values []string) string {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) >= 2 && ((trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}') || (trimmed[0] == '[' && trimmed[len(trimmed)-1] == ']')) {
		var inner interface{}
		if err := json.Unmarshal([]byte(trimmed), &inner); err == nil {
			redacted := redactJSONValue(inner, values)
			if b, err := json.Marshal(redacted); err == nil {
				return string(b)
			}
		}
	}
	for _, v := range values {
		if v != "" {
			s = strings.ReplaceAll(s, v, "[REDACTED]")
		}
	}
	return diagnostic.Redact(s)
}

// redactTextWithSecrets redacts one non-JSON line with both rails. Unicode
// escapes are expanded first so a \\uXXXX-encoded secret matches its raw
// form before replacement.
func redactTextWithSecrets(text string, values []string) string {
	text = expandTranscriptUnicodeEscapes(text)
	redacted := diagnostic.Redact(text)
	for _, v := range values {
		if v == "" {
			continue
		}
		redacted = strings.ReplaceAll(redacted, v, "[REDACTED]")
		for _, form := range transcriptEvidenceEscapedForms(v) {
			if form == "" || form == v {
				continue
			}
			redacted = strings.ReplaceAll(redacted, form, "[REDACTED]")
		}
	}
	return diagnostic.Redact(redacted)
}

// expandTranscriptUnicodeEscapes decodes \\uXXXX sequences so an encoded
// secret matches its raw form. Invalid sequences are left as-is.
func expandTranscriptUnicodeEscapes(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+5 < len(s) && (s[i+1] == 'u' || s[i+1] == 'U') {
			ok := true
			var r rune
			for j := i + 2; j < i+6; j++ {
				c := s[j]
				var d rune
				switch {
				case c >= '0' && c <= '9':
					d = rune(c - '0')
				case c >= 'a' && c <= 'f':
					d = rune(c-'a') + 10
				case c >= 'A' && c <= 'F':
					d = rune(c-'A') + 10
				default:
					ok = false
				}
				if !ok {
					break
				}
				r = r*16 + d
			}
			if ok {
				out.WriteRune(r)
				i += 6
				continue
			}
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

// transcriptEvidenceEscapedForms returns the JSON string-body encodings of
// v (without surrounding quotes) for the two common encoder flavours:
// with HTML escaping (encoding/json default) and without. Either flavour
// may appear in transcript JSONL depending on the writer.
func transcriptEvidenceEscapedForms(v string) []string {
	var out []string
	if b, err := json.Marshal(v); err == nil && len(b) >= 2 {
		if inner := string(b[1 : len(b)-1]); inner != v {
			out = append(out, inner)
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err == nil {
		s := strings.TrimSuffix(buf.String(), "\n")
		if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
			if inner := s[1 : len(s)-1]; inner != v {
				dup := false
				for _, have := range out {
					if have == inner {
						dup = true
						break
					}
				}
				if !dup {
					out = append(out, inner)
				}
			}
		}
	}
	return out
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
		return redactTranscriptEvidence(msg, values)
	}
	return redactTranscriptEvidence(err.Error(), values)
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
	values := transcriptEvidenceSecretValues(qw)
	remaining := transcriptEvidencePerAttemptCapBytes
	for i, desc := range res.terminalTranscripts {
		resolved, err := resolveTranscriptEvidencePath(desc.Path, res)
		if err != nil {
			r.logger.Warn("transcript evidence validation failed; keeping workarea", "sessionId", qw.SessionID, "err", transcriptEvidenceSafeErr(err, values))
			appendTranscriptEvidenceWarning(res, i)
			return false
		}
		linfo, err := transcriptEvidenceLstat(resolved)
		if err != nil {
			r.logger.Warn("transcript evidence validation failed; keeping workarea", "sessionId", qw.SessionID, "err", transcriptEvidenceSafeErr(err, values))
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
			r.logger.Warn("transcript evidence read failed; keeping workarea", "sessionId", qw.SessionID, "err", transcriptEvidenceSafeErr(err, values))
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
			r.logger.Warn("transcript evidence read failed; keeping workarea", "sessionId", qw.SessionID, "err", transcriptEvidenceSafeErr(readErr, values))
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
			r.logger.Warn("transcript evidence write failed; keeping workarea", "sessionId", qw.SessionID, "err", transcriptEvidenceSafeErr(err, values))
			appendTranscriptEvidenceWarning(res, i)
			return false
		}
		name := transcriptEvidenceName(i)
		if err := transcriptEvidenceWriteAtomic(dir, name, []byte(redacted)); err != nil {
			r.logger.Warn("transcript evidence write failed; keeping workarea", "sessionId", qw.SessionID, "err", transcriptEvidenceSafeErr(err, values))
			appendTranscriptEvidenceWarning(res, i)
			return false
		}
		if err := transcriptEvidenceReadback(filepath.Join(dir, name), []byte(redacted)); err != nil {
			r.logger.Warn("transcript evidence readback failed; keeping workarea", "sessionId", qw.SessionID, "err", transcriptEvidenceSafeErr(err, values))
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
