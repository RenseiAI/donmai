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
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"time"

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

// transcriptEvidenceSecretValues collects every secret value the evidence
// archiver must redact: the session bearers, ALL session env values, and
// the process env values with secret-shaped names, plus any userinfo
// password embedded in a URL-style value. Unlike the checkpoint content
// gate it has no minimum-length floor and no maximum-length exclusion: a
// short session secret that reaches a transcript must still fail closed,
// and a long session value up to the per-file cap must not survive intact
// in a successful archive. Tails are bounded (per-file and per-attempt
// caps) so the scan stays cheap without dropping long secrets.
func transcriptEvidenceSecretValues(qw QueuedWork) []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if v == "" || seen[v] {
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
// quoting intact. Sensitive numbers, booleans, and nulls are replaced
// wholesale as well so an unknown numeric PIN cannot survive; ordinary
// token-usage counters are allowlisted explicitly so they stay valid JSON
// numbers.
var transcriptSensitiveKeyRE = regexp.MustCompile(`(?i)(authorization|bearer|api[_-]?keys?|access[_-]?tokens?|refresh[_-]?tokens?|auth[_-]?tokens?|client[_-]?secrets?|private[_-]?keys?|secrets?|tokens?|passwords?|passwds?|credentials?|cookies?|sessions?)`)

// transcriptUsageCounterKeys allowlists ordinary token-usage counters that
// happen to contain the substring "token": they are telemetry, not
// credentials, and must stay numeric so evidence keeps its shape.
var transcriptUsageCounterKeys = map[string]bool{
	"inputtokens":              true,
	"outputtokens":             true,
	"totaltokens":              true,
	"totalinputtokens":         true,
	"totaloutputtokens":        true,
	"cachedtokens":             true,
	"cachetokens":              true,
	"cachereadinputtokens":     true,
	"cachecreationinputtokens": true,
	"prompttokens":             true,
	"completiontokens":         true,
	"prompt_tokencount":        true,
}

func isUsageTranscriptCounterKey(k string) bool {
	lower := strings.ToLower(k)
	norm := strings.ReplaceAll(strings.ReplaceAll(lower, "_", ""), "-", "")
	if transcriptUsageCounterKeys[norm] {
		return true
	}
	// Camel/snake variants collapse to the same normalized form above;
	// anything else containing a usage prefix plus "token" is still
	// telemetry (for example future cache_*_tokens counters).
	if strings.Contains(norm, "token") && (strings.HasPrefix(norm, "input") || strings.HasPrefix(norm, "output") || strings.HasPrefix(norm, "total") || strings.HasPrefix(norm, "cache") || strings.HasPrefix(norm, "prompt") || strings.HasPrefix(norm, "completion")) {
		return true
	}
	return false
}

func isSensitiveTranscriptKey(k string) bool {
	if isUsageTranscriptCounterKey(k) {
		return false
	}
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
	// Multiline known values must not survive split across JSONL records.
	// Replace every value containing a line break while the tail is still
	// whole: per-line redaction below would otherwise see only fragments
	// that never match the complete secret. Both the raw form (plain-text
	// lines) and the JSON string-body encodings (newlines inside JSON
	// strings) are replaced so neither representation survives intact.
	tail = redactMultilineSecretsInTail(tail, values)
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

// redactJSONValue redacts one decoded JSON value. Object keys carrying a
// known secret are rewritten and passed through the shared output rail so
// token-shaped keys cannot survive, json.Number values equal to a known
// secret are replaced, structured values equal to a known JSON credential
// are replaced wholesale, and sensitive context propagates into nested
// objects and arrays so unknown credentials under a sensitive key cannot
// survive.
func redactJSONValue(v interface{}, values []string) interface{} {
	return redactJSONValueSensitive(v, values, false)
}

// redactJSONValueSensitive redacts one decoded JSON value, treating the
// value as sensitive when it sits under a sensitive key. Sensitive string
// values are replaced wholesale (unknown passwords fail closed);
// sensitive arrays are replaced wholesale so array elements cannot lose
// that context. Sensitive numbers, booleans, and nulls are replaced
// wholesale so an unknown numeric PIN or flag under a sensitive key cannot
// survive with its JSON shape intact; other numbers, booleans, and null
// are left alone so ordinary token-usage counters stay valid JSON.
func redactJSONValueSensitive(v interface{}, values []string, sensitive bool) interface{} {
	if isStructuredTranscriptSecret(v, values) {
		return "[REDACTED]"
	}
	switch typed := v.(type) {
	case string:
		if sensitive {
			return "[REDACTED]"
		}
		return redactJSONString(typed, values)
	case json.Number:
		if sensitive {
			return "[REDACTED]"
		}
		s := string(typed)
		for _, sec := range values {
			if sec != "" && s == sec {
				return "[REDACTED]"
			}
		}
		return v
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		if sensitive {
			return "[REDACTED]"
		}
		s := fmt.Sprint(typed)
		for _, sec := range values {
			if sec != "" && s == sec {
				return "[REDACTED]"
			}
		}
		return v
	case bool:
		if sensitive {
			return "[REDACTED]"
		}
		return v
	case nil:
		if sensitive {
			return "[REDACTED]"
		}
		return v
	case map[string]interface{}:
		out := make(map[string]interface{}, len(typed))
		for k, val := range typed {
			rk := k
			for _, sec := range values {
				if sec != "" && strings.Contains(rk, sec) {
					rk = strings.ReplaceAll(rk, sec, "[REDACTED]")
				}
			}
			rk = diagnostic.Redact(rk)
			childSensitive := sensitive || isSensitiveTranscriptKey(k)
			if !childSensitive && isUsageTranscriptCounterKey(k) {
				// Usage counters stay numeric so evidence keeps its shape,
				// but a string under a counter key has no numeric shape to
				// keep: the shared output rail masks
				// {"input_tokens":"secret"} wholesale, so the JSON-aware
				// rail must too. Otherwise an unknown password under a
				// counter key would survive while the same bytes through
				// diagnostic.Redact are masked.
				if s, ok := val.(string); ok && s != "" {
					childSensitive = true
				}
			}
			out[rk] = redactJSONValueSensitive(val, values, childSensitive)
		}
		return out
	case []interface{}:
		if sensitive {
			return "[REDACTED]"
		}
		for i, e := range typed {
			typed[i] = redactJSONValueSensitive(e, values, false)
		}
		return typed
	default:
		return v
	}
}

// isStructuredTranscriptSecret reports whether the decoded JSON value equals
// a known session secret that is itself JSON (an object or array credential
// value). Traversal otherwise splits the credential into ordinary fields
// without matching the complete secret, leaving the value recoverable.
// Comparison is structural with numeric normalization so whitespace and
// number-encoding differences cannot bypass the match.
func isStructuredTranscriptSecret(v interface{}, values []string) bool {
	switch v.(type) {
	case map[string]interface{}, []interface{}:
	default:
		return false
	}
	for _, sec := range values {
		vt := strings.TrimSpace(sec)
		if len(vt) < 2 {
			continue
		}
		isSecretJSON := (vt[0] == '{' && vt[len(vt)-1] == '}') || (vt[0] == '[' && vt[len(vt)-1] == ']')
		if !isSecretJSON {
			continue
		}
		var secretRaw interface{}
		dec := json.NewDecoder(strings.NewReader(vt))
		dec.UseNumber()
		if err := dec.Decode(&secretRaw); err != nil {
			continue
		}
		var extra interface{}
		if err := dec.Decode(&extra); err != io.EOF {
			continue
		}
		if transcriptJSONValuesEqual(v, secretRaw) {
			return true
		}
	}
	return false
}

// transcriptJSONValuesEqual compares two decoded JSON values structurally,
// normalizing numbers across json.Number and float/int encodings so the
// same credential written with different spacing or number forms still
// matches.
func transcriptJSONValuesEqual(a, b interface{}) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case json.Number:
		return transcriptJSONNumberEqual(string(av), b)
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return transcriptJSONNumberEqual(fmt.Sprint(av), b)
	case map[string]interface{}:
		bv, ok := b.(map[string]interface{})
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, va := range av {
			vb, ok := bv[k]
			if !ok || !transcriptJSONValuesEqual(va, vb) {
				return false
			}
		}
		return true
	case []interface{}:
		bv, ok := b.([]interface{})
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !transcriptJSONValuesEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}

// transcriptJSONNumberEqual compares one number literal against any decoded
// JSON number form by string literal first, then by numeric value so 123
// and 123.0 match fail-closed.
func transcriptJSONNumberEqual(a string, b interface{}) bool {
	var bs string
	switch bv := b.(type) {
	case json.Number:
		bs = string(bv)
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		bs = fmt.Sprint(bv)
	case string:
		bs = bv
	default:
		return false
	}
	if a == bs {
		return true
	}
	af, aerr := parseTranscriptJSONFloat(a)
	bf, berr := parseTranscriptJSONFloat(bs)
	return aerr == nil && berr == nil && af == bf
}

// parseTranscriptJSONFloat parses one JSON number literal for equality.
func parseTranscriptJSONFloat(s string) (float64, error) {
	var f float64
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &f); err != nil {
		return 0, err
	}
	return f, nil
}

// redactJSONString redacts one decoded string value. A JSON-shaped string
// that contains a known session secret is itself a credential value and is
// replaced wholesale so the secret cannot survive behind a second encoding.
// Otherwise, when the whole string is itself JSON (a doubly-encoded record),
// the inner record is redacted recursively and the re-encoded form is
// redacted again with the literal rail so a complete JSON credential value
// emitted as an escaped JSON string cannot bypass the exact-match loop.
// Non-JSON prose (including a JSON object embedded after a prefix) is
// redacted with both rails over the raw and JSON-escaped byte forms so a
// secret containing quotes or backslashes cannot survive escaped, and any
// embedded JSON object is redacted recursively before the literal rail.
func redactJSONString(s string, values []string) string {
	trimmed := strings.TrimSpace(s)
	isJSONShape := len(trimmed) >= 2 && ((trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}') || (trimmed[0] == '[' && trimmed[len(trimmed)-1] == ']'))
	if isJSONShape {
		for _, v := range values {
			if v == "" {
				continue
			}
			if strings.Contains(s, v) {
				return "[REDACTED]"
			}
			for _, form := range transcriptEvidenceEscapedForms(v) {
				if form != "" && form != v && strings.Contains(s, form) {
					return "[REDACTED]"
				}
			}
		}
		var innerRaw interface{}
		if err := json.Unmarshal([]byte(trimmed), &innerRaw); err == nil {
			for _, v := range values {
				vt := strings.TrimSpace(v)
				if len(vt) < 2 {
					continue
				}
				isSecretJSON := (vt[0] == '{' && vt[len(vt)-1] == '}') || (vt[0] == '[' && vt[len(vt)-1] == ']')
				if !isSecretJSON {
					continue
				}
				var secretRaw interface{}
				if err := json.Unmarshal([]byte(vt), &secretRaw); err != nil {
					continue
				}
				if reflect.DeepEqual(innerRaw, secretRaw) {
					return "[REDACTED]"
				}
			}
		}
		var inner interface{}
		if err := json.Unmarshal([]byte(trimmed), &inner); err == nil {
			redacted := redactJSONValue(inner, values)
			if b, err := json.Marshal(redacted); err == nil {
				s2 := string(b)
				for _, v := range values {
					if v != "" {
						s2 = strings.ReplaceAll(s2, v, "[REDACTED]")
					}
				}
				return diagnostic.Redact(s2)
			}
		}
	}
	s = diagnostic.Redact(s)
	for _, v := range values {
		if v == "" {
			continue
		}
		s = strings.ReplaceAll(s, v, "[REDACTED]")
		for _, form := range transcriptEvidenceEscapedForms(v) {
			if form == "" || form == v {
				continue
			}
			s = strings.ReplaceAll(s, form, "[REDACTED]")
		}
	}
	s = redactEmbeddedTranscriptJSON(s, values)
	return diagnostic.Redact(s)
}

// redactEmbeddedTranscriptJSON redacts JSON objects embedded in prose (a
// prefix followed by a JSON credential object). The whole-string JSON
// parser cannot see them, and the literal rail alone cannot match secrets
// whose quoting changed the byte form, so each balanced JSON candidate is
// decoded and redacted recursively. Candidates that fail to parse are left
// for the literal rail.
func redactEmbeddedTranscriptJSON(s string, values []string) string {
	type span struct {
		start int
		end   int
		text  string
	}
	var spans []span
	for i := 0; i < len(s); i++ {
		if s[i] != '{' && s[i] != '[' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		dec.UseNumber()
		var cand interface{}
		if err := dec.Decode(&cand); err != nil {
			continue
		}
		end := i + int(dec.InputOffset())
		if end <= i || end > len(s) {
			continue
		}
		// Only treat it as embedded JSON when the decoded form round-trips
		// to the same bytes modulo whitespace: avoids redacting prose
		// braces that merely happen to parse as JSON fragments.
		raw := strings.TrimSpace(s[i:end])
		if raw == "" {
			continue
		}
		redacted := redactJSONValue(cand, values)
		if b, err := json.Marshal(redacted); err == nil {
			if string(b) != raw {
				// Only record when redaction changed something or the
				// candidate is a plausible credential container; plain
				// prose braces that parse but need no redaction are left
				// untouched to keep the archive faithful.
				if redactedNeedsReplacement(cand, redacted) {
					spans = append(spans, span{start: i, end: end, text: string(b)})
					i = end - 1
				}
			}
		}
		if len(spans) >= 8 {
			break
		}
	}
	if len(spans) == 0 {
		return s
	}
	var sb strings.Builder
	prev := 0
	for _, sp := range spans {
		sb.WriteString(s[prev:sp.start])
		sb.WriteString(sp.text)
		prev = sp.end
	}
	sb.WriteString(s[prev:])
	return sb.String()
}

// redactedNeedsReplacement reports whether redacting a JSON candidate
// changed it: only then does the prose embedding need rewriting.
func redactedNeedsReplacement(before, after interface{}) bool {
	bb, err1 := json.Marshal(before)
	ab, err2 := json.Marshal(after)
	if err1 != nil || err2 != nil {
		return true
	}
	return string(bb) != string(ab)
}

// redactMultilineSecretsInTail replaces multiline known values while the
// tail is still whole. Per-line redaction would otherwise see only
// fragments split across records that never match the complete secret.
func redactMultilineSecretsInTail(tail string, values []string) string {
	for _, v := range values {
		if v == "" {
			continue
		}
		if !strings.Contains(v, "\n") && !strings.Contains(v, "\r") {
			continue
		}
		tail = strings.ReplaceAll(tail, v, "[REDACTED]")
		for _, form := range transcriptEvidenceEscapedForms(v) {
			if form == "" || form == v {
				continue
			}
			tail = strings.ReplaceAll(tail, form, "[REDACTED]")
		}
	}
	return tail
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
		linfo, err := transcriptEvidenceValueWithTimeout(func() (os.FileInfo, error) {
			return transcriptEvidenceLstat(resolved)
		})
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
		f, err := transcriptEvidenceValueWithTimeout(func() (*os.File, error) {
			//nolint:gosec // G304: resolved is the validated declared transcript path.
			return transcriptEvidenceOpen(resolved)
		})
		if err != nil {
			r.logger.Warn("transcript evidence read failed; keeping workarea", "sessionId", qw.SessionID, "err", transcriptEvidenceSafeErr(err, values))
			appendTranscriptEvidenceWarning(res, i)
			return false
		}
		finfo, statErr := transcriptEvidenceValueWithTimeout(func() (os.FileInfo, error) {
			return f.Stat()
		})
		tail, readErr := transcriptEvidenceValueWithTimeout(func() ([]byte, error) {
			if statErr != nil {
				return nil, statErr
			}
			if finfo == nil {
				return nil, fmt.Errorf("transcript stat missing")
			}
			if finfo.Mode()&os.ModeSymlink != 0 || !finfo.Mode().IsRegular() {
				return nil, fmt.Errorf("transcript is not a regular file")
			}
			if !sameTranscriptFile(linfo, finfo) {
				return nil, fmt.Errorf("transcript identity changed between validation and read")
			}
			return transcriptEvidenceReadTail(f, finfo, transcriptEvidencePerFileCapBytes)
		})
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
		if err := transcriptEvidenceCallWithTimeout(func() error {
			return transcriptEvidenceWriteAtomic(dir, name, []byte(redacted))
		}); err != nil {
			r.logger.Warn("transcript evidence write failed; keeping workarea", "sessionId", qw.SessionID, "err", transcriptEvidenceSafeErr(err, values))
			appendTranscriptEvidenceWarning(res, i)
			return false
		}
		if err := transcriptEvidenceCallWithTimeout(func() error {
			return transcriptEvidenceReadback(filepath.Join(dir, name), []byte(redacted))
		}); err != nil {
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
