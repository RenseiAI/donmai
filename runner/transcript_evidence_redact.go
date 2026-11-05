package runner

import (
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/internal/diagnostic"
)

// Redaction for failed-session transcript evidence.
//
// Contract. An archived record is valid JSON, and no key, string, or scalar
// it carries lets a reader recover:
//   - a known session secret (see transcriptEvidenceSecretValues), compared
//     by bytes, by numeric value for numeric secrets, and leaf-by-leaf for a
//     JSON-shaped secret, nor any line of a multi-line secret;
//   - a value the shared output rail (diagnostic.Redact) would mask, such as
//     an unknown password, bearer, or token-shaped string;
//   - a value of any type under a credential-named key;
//
// under any combination of these decodings, applied repeatedly: JSON and
// Python-style string escapes, URL percent-encoding, HTML entities, base64
// tokens, and JSON embedded anywhere inside a string.
//
// Design. Earlier versions enumerated encodings inside the redactor, and each
// review found the next encoding it missed. The redactor is now best-effort
// only; the guarantee comes from proving each output string clean. Every
// key and string value is rewritten, then every decoding view of the
// rewritten text is checked. A string that cannot be proved clean is
// replaced wholesale with [REDACTED]. The record keeps its other fields, and
// only the unprovable string is dropped. A missed encoding therefore costs
// that one string, never a leak. Transforms outside the decoding set
// (hashing, splitting a secret across values, encrypting) cannot be found by
// any text redactor and are outside this contract. The archive remains
// local-only, with owner-only modes.

const transcriptRedacted = "[REDACTED]"

// transcriptEvidenceMaxViews bounds the decoding views of one string. A
// string whose views exceed the budget is not proved clean and is dropped.
const transcriptEvidenceMaxViews = 64

// transcriptEvidenceSecretValues collects every secret value the evidence
// archiver must redact: the session bearers, ALL session env values, and
// the process env values with secret-shaped names, plus any userinfo
// password embedded in a URL-style value. There is no length floor or
// ceiling: a short bearer must not survive, and a long value fits under the
// per-file cap.
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
// list. Any value under one of these keys is replaced, whatever its type.
var transcriptSensitiveKeyRE = regexp.MustCompile(`(?i)(authorization|bearer|api[_-]?keys?|access[_-]?tokens?|refresh[_-]?tokens?|auth[_-]?tokens?|client[_-]?secrets?|private[_-]?keys?|secrets?|tokens?|passwords?|passwds?|credentials?|cookies?|sessions?)`)

// transcriptUsageCounterKeys allowlists exact token-usage counter names. They
// are telemetry and stay numeric, but only when the value is a JSON number;
// any other value under a counter key is redacted.
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
}

func isUsageTranscriptCounterKey(k string) bool {
	norm := strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(k))
	return transcriptUsageCounterKeys[norm]
}

func isSensitiveTranscriptKey(k string) bool {
	return !isUsageTranscriptCounterKey(k) && transcriptSensitiveKeyRE.MatchString(k)
}

// transcriptRedactor holds the matching state derived from one session's
// secrets.
type transcriptRedactor struct {
	// replacer replaces every needle in one pass (longest first), so a
	// replacement marker is never re-scanned by a later needle.
	replacer *strings.Replacer
	// literals is every base secret, used for exact scalar comparison.
	literals map[string]bool
	// numbers is the canonical value of every numeric base secret.
	numbers map[string]bool
}

// newTranscriptRedactor expands the secret values into base secrets (the
// value, each scalar leaf of a JSON-shaped value, and each line of a
// multi-line value) and their common encodings. The encodings only make the
// best-effort rewrite precise. Detection never depends on them, because the
// clean check decodes instead.
func newTranscriptRedactor(values []string) *transcriptRedactor {
	r := &transcriptRedactor{literals: map[string]bool{}, numbers: map[string]bool{}}
	var base []string
	addBase := func(v string) {
		if v == "" || r.literals[v] {
			return
		}
		r.literals[v] = true
		base = append(base, v)
		if transcriptJSONNumberRE.MatchString(v) {
			r.numbers[canonicalTranscriptNumber(v)] = true
		}
	}
	for _, v := range values {
		addBase(v)
		if parsed, ok := parseWholeTranscriptJSON(v); ok {
			for _, leaf := range transcriptScalarLeaves(parsed) {
				addBase(leaf)
			}
		}
		if strings.ContainsAny(v, "\r\n") {
			for _, line := range strings.FieldsFunc(v, func(c rune) bool { return c == '\n' || c == '\r' }) {
				addBase(strings.TrimSpace(line))
			}
		}
	}
	seen := map[string]bool{}
	var needles []string
	addNeedle := func(n string) {
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		needles = append(needles, n)
	}
	for _, b := range base {
		addNeedle(b)
		for _, form := range transcriptEvidenceEscapedForms(b) {
			addNeedle(form)
		}
		addNeedle(url.QueryEscape(b))
		addNeedle(url.PathEscape(b))
		addNeedle(html.EscapeString(b))
		if len(b) >= 6 {
			addNeedle(base64.RawStdEncoding.EncodeToString([]byte(b)))
			addNeedle(base64.RawURLEncoding.EncodeToString([]byte(b)))
		}
	}
	sort.SliceStable(needles, func(i, j int) bool { return len(needles[i]) > len(needles[j]) })
	var pairs []string
	for _, n := range needles {
		pairs = append(pairs, n, transcriptRedacted)
	}
	if len(pairs) > 0 {
		r.replacer = strings.NewReplacer(pairs...)
	}
	return r
}

// containsSecret reports whether text carries a needle by bytes. Markers
// already written are blanked first so a secret that is a substring of the
// marker itself cannot make every redacted string look dirty.
func (r *transcriptRedactor) containsSecret(text string) bool {
	if r.replacer == nil {
		return false
	}
	blanked := strings.ReplaceAll(text, transcriptRedacted, "\x00")
	return r.replacer.Replace(blanked) != blanked
}

// containsNumericSecret reports whether any number literal in text has the
// value of a numeric secret (987654321 written as 9.87654321e8).
func (r *transcriptRedactor) containsNumericSecret(text string) bool {
	if len(r.numbers) == 0 {
		return false
	}
	for _, tok := range transcriptNumberTokenRE.FindAllString(text, -1) {
		if r.numbers[canonicalTranscriptNumber(tok)] {
			return true
		}
	}
	return false
}

// rewrite is the best-effort text pass: secret needles, then the shared rail.
func (r *transcriptRedactor) rewrite(s string) string {
	if r.replacer != nil {
		s = r.replacer.Replace(s)
	}
	return diagnostic.Redact(s)
}

// clean is the guarantee: no decoding view of s carries a secret, a numeric
// secret, or an unmasked shared-rail value. Exceeding the view budget is
// treated as unclean.
func (r *transcriptRedactor) clean(s string) bool {
	views, complete := transcriptDecodingViews(s)
	if !complete {
		return false
	}
	for _, v := range views {
		if r.containsSecret(v) || r.containsNumericSecret(v) || diagnostic.Unredacted(v) {
			return false
		}
	}
	return true
}

// redactString returns s rewritten when the rewrite is provably clean,
// otherwise a JSON-shaped s redacted structurally when that is provably
// clean, otherwise the marker. It never returns unproved text.
func (r *transcriptRedactor) redactString(s string) string {
	if out := r.rewrite(s); r.clean(out) {
		return out
	}
	if parsed, ok := parseWholeTranscriptJSON(s); ok {
		if b, err := json.Marshal(r.redactValue(parsed, false)); err == nil && r.clean(string(b)) {
			return string(b)
		}
	}
	return transcriptRedacted
}

// redactValue returns a redacted copy of one decoded JSON value. It never
// mutates its input.
func (r *transcriptRedactor) redactValue(v interface{}, sensitive bool) interface{} {
	switch typed := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(typed))
		for k, val := range typed {
			childSensitive := sensitive || isSensitiveTranscriptKey(k)
			rk := r.redactString(k)
			if !childSensitive && isUsageTranscriptCounterKey(k) {
				if _, isNumber := val.(json.Number); !isNumber {
					out[rk] = transcriptRedacted
					continue
				}
			}
			out[rk] = r.redactValue(val, childSensitive)
		}
		return out
	case []interface{}:
		if sensitive {
			return transcriptRedacted
		}
		out := make([]interface{}, len(typed))
		for i, e := range typed {
			out[i] = r.redactValue(e, false)
		}
		return out
	case string:
		if sensitive {
			return transcriptRedacted
		}
		return r.redactString(typed)
	case json.Number:
		lit := string(typed)
		if sensitive || r.literals[lit] || r.numbers[canonicalTranscriptNumber(lit)] || r.containsSecret(lit) {
			return transcriptRedacted
		}
		return typed
	case bool:
		if sensitive || r.literals[strconv.FormatBool(typed)] {
			return transcriptRedacted
		}
		return typed
	case nil:
		if sensitive || r.literals["null"] {
			return transcriptRedacted
		}
		return nil
	default:
		return transcriptRedacted
	}
}

// redactLine redacts one complete transcript line into one JSON record. A
// non-JSON line becomes {"nonJsonLine": <redacted text>} so the archive
// stays valid JSONL and plain text gets the same proof as JSON strings.
func (r *transcriptRedactor) redactLine(line string) []byte {
	var out interface{}
	if parsed, ok := parseWholeTranscriptJSON(line); ok {
		out = r.redactValue(parsed, false)
	} else {
		out = map[string]interface{}{"nonJsonLine": r.redactString(line)}
	}
	b, err := json.Marshal(out)
	if err != nil || r.containsSecret(string(b)) {
		// Defense in depth for readers of the raw bytes: the encoded
		// record must not spell a secret either.
		return []byte(`{"redactedRecord":true}`)
	}
	return b
}

// redactTranscriptEvidence redacts one tail of complete lines. Every
// non-empty line becomes one valid JSON record; empty lines are kept.
func redactTranscriptEvidence(tail string, values []string) string {
	if tail == "" {
		return ""
	}
	r := newTranscriptRedactor(values)
	var sb strings.Builder
	for _, part := range strings.SplitAfter(tail, "\n") {
		if !strings.HasSuffix(part, "\n") {
			continue
		}
		line := strings.TrimSuffix(part, "\n")
		if line != "" {
			sb.Write(r.redactLine(line))
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// parseWholeTranscriptJSON decodes s as exactly one JSON value (numbers
// preserved). Only object, array, and string values count: a bare number
// or literal is not a container another encoding could hide in.
func parseWholeTranscriptJSON(s string) (interface{}, bool) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || !strings.ContainsRune(`{["`, rune(trimmed[0])) {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return v, true
}

// transcriptScalarLeaves flattens a decoded JSON value into its string and
// number leaves.
func transcriptScalarLeaves(v interface{}) []string {
	switch typed := v.(type) {
	case map[string]interface{}:
		var out []string
		for _, val := range typed {
			out = append(out, transcriptScalarLeaves(val)...)
		}
		return out
	case []interface{}:
		var out []string
		for _, e := range typed {
			out = append(out, transcriptScalarLeaves(e)...)
		}
		return out
	case string:
		return []string{typed}
	case json.Number:
		return []string{string(typed)}
	}
	return nil
}

var (
	transcriptJSONNumberRE  = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)
	transcriptNumberTokenRE = regexp.MustCompile(`-?[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?`)
	transcriptBase64TokenRE = regexp.MustCompile(`[A-Za-z0-9+/_-]{8,}={0,2}`)
)

// canonicalTranscriptNumber renders a decimal literal as sign, significant
// digits, and exponent, so 987654321, 9.87654321e8, and 987654321.0 compare
// equal without float rounding. A literal it cannot read maps to itself.
func canonicalTranscriptNumber(lit string) string {
	s := lit
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	mant, expPart, hasExp := strings.Cut(strings.ToLower(s), "e")
	intPart, frac, _ := strings.Cut(mant, ".")
	exp := 0
	if hasExp {
		if len(strings.TrimLeft(expPart, "+-")) > 9 {
			return "literal:" + lit
		}
		e, err := strconv.Atoi(expPart)
		if err != nil {
			return "literal:" + lit
		}
		exp = e
	}
	digits := strings.TrimLeft(intPart+frac, "0")
	exp -= len(frac)
	for strings.HasSuffix(digits, "0") {
		digits = digits[:len(digits)-1]
		exp++
	}
	if digits == "" {
		return "0"
	}
	sign := ""
	if neg {
		sign = "-"
	}
	return sign + digits + "e" + strconv.Itoa(exp)
}

// transcriptDecodingViews returns s and every distinct text reachable from
// it by repeatedly applying the decoders below. It reports false when the
// view budget runs out before the closure is complete.
func transcriptDecodingViews(s string) ([]string, bool) {
	seen := map[string]bool{s: true}
	views := []string{s}
	for i := 0; i < len(views); i++ {
		cur := views[i]
		embedded, ok := embeddedTranscriptJSONLeaves(cur)
		if !ok {
			return views, false
		}
		for _, next := range []string{
			embedded,
			unescapeTranscriptText(cur),
			percentDecodeTranscriptText(cur, false),
			percentDecodeTranscriptText(cur, true),
			html.UnescapeString(cur),
			decodeTranscriptBase64Tokens(cur),
		} {
			if next == "" || seen[next] {
				continue
			}
			if len(views) >= transcriptEvidenceMaxViews {
				return views, false
			}
			seen[next] = true
			views = append(views, next)
		}
	}
	return views, true
}

// unescapeTranscriptText decodes one layer of backslash escapes: JSON
// (\" \\ \/ \b \f \n \r \t \uXXXX with surrogate pairs) and Python-style
// \xHH and \UXXXXXXXX. Unknown escapes are kept as written.
func unescapeTranscriptText(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			out.WriteByte(s[i])
			continue
		}
		switch c := s[i+1]; c {
		case '"', '\\', '/':
			out.WriteByte(c)
			i++
		case 'b':
			out.WriteByte('\b')
			i++
		case 'f':
			out.WriteByte('\f')
			i++
		case 'n':
			out.WriteByte('\n')
			i++
		case 'r':
			out.WriteByte('\r')
			i++
		case 't':
			out.WriteByte('\t')
			i++
		case 'x':
			if v, ok := parseTranscriptHexByte(s, i+2); ok {
				out.WriteByte(v)
				i += 3
				continue
			}
			out.WriteByte(s[i])
		case 'u':
			v, ok := parseTranscriptHex(s, i+2, 4)
			if !ok {
				out.WriteByte(s[i])
				continue
			}
			r := v
			if utf16.IsSurrogate(r) && i+11 < len(s) && s[i+6] == '\\' && s[i+7] == 'u' {
				if lo, ok := parseTranscriptHex(s, i+8, 4); ok {
					if pair := utf16.DecodeRune(r, lo); pair != utf8.RuneError {
						out.WriteRune(pair)
						i += 11
						continue
					}
				}
			}
			out.WriteRune(r)
			i += 5
		case 'U':
			if v, ok := parseTranscriptHex(s, i+2, 8); ok && v >= 0 && v <= utf8.MaxRune {
				out.WriteRune(v)
				i += 9
				continue
			}
			out.WriteByte(s[i])
		default:
			out.WriteByte(s[i])
		}
	}
	return out.String()
}

func parseTranscriptHex(s string, start, n int) (rune, bool) {
	if start+n > len(s) {
		return 0, false
	}
	var v rune
	for i := start; i < start+n; i++ {
		d, ok := transcriptHexDigit(s[i])
		if !ok {
			return 0, false
		}
		v = v<<4 | rune(d)
	}
	return v, true
}

// parseTranscriptHexByte reads exactly two hex digits at start.
func parseTranscriptHexByte(s string, start int) (byte, bool) {
	if start+2 > len(s) {
		return 0, false
	}
	hi, ok1 := transcriptHexDigit(s[start])
	lo, ok2 := transcriptHexDigit(s[start+1])
	return hi<<4 | lo, ok1 && ok2
}

func transcriptHexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// percentDecodeTranscriptText decodes every valid %HH (and '+' as a space
// when plus is set), leaving malformed escapes as written.
func percentDecodeTranscriptText(s string, plus bool) string {
	if !strings.Contains(s, "%") && (!plus || !strings.Contains(s, "+")) {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '%':
			if v, ok := parseTranscriptHexByte(s, i+1); ok {
				out.WriteByte(v)
				i += 2
				continue
			}
			out.WriteByte('%')
		case plus && s[i] == '+':
			out.WriteByte(' ')
		default:
			out.WriteByte(s[i])
		}
	}
	return out.String()
}

// decodeTranscriptBase64Tokens joins the printable decodings of every
// base64-shaped token in s into one newline-separated view, so a long text
// adds one view, not one per word. Ordinary words that happen to decode
// yield binary noise, which cannot spell a printable secret, and is skipped.
func decodeTranscriptBase64Tokens(s string) string {
	var parts []string
	for _, tok := range transcriptBase64TokenRE.FindAllString(s, -1) {
		trimmed := strings.TrimRight(tok, "=")
		for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
			if b, err := enc.DecodeString(trimmed); err == nil {
				if printableTranscriptText(b) {
					parts = append(parts, string(b))
				}
				break
			}
		}
	}
	return strings.Join(parts, "\n")
}

// embeddedTranscriptJSONLeaves decodes every JSON value embedded in s
// (starting at a brace, bracket, or quote) and joins its keys and scalar
// leaves into one newline-separated view, so escapes inside embedded
// strings are decoded by JSON's own rules. Parsing is budgeted by bytes
// read: adversarial nesting cannot turn the scan quadratic. It reports false
// when the budget runs out, which the caller treats as unclean.
func embeddedTranscriptJSONLeaves(s string) (string, bool) {
	if !strings.ContainsAny(s, `{["`) {
		return "", true
	}
	budget := int64(16*len(s)) + 64*1024
	var parts []string
	for i := 0; i < len(s); i++ {
		if s[i] != '{' && s[i] != '[' && s[i] != '"' {
			continue
		}
		src := &countingTranscriptReader{r: strings.NewReader(s[i:])}
		dec := json.NewDecoder(src)
		dec.UseNumber()
		var v interface{}
		err := dec.Decode(&v)
		budget -= src.n
		if budget < 0 {
			return "", false
		}
		if err != nil {
			continue
		}
		parts = appendTranscriptJSONText(parts, v)
		i += int(dec.InputOffset()) - 1
	}
	return strings.Join(parts, "\n"), true
}

// countingTranscriptReader counts bytes handed to a JSON decoder.
type countingTranscriptReader struct {
	r io.Reader
	n int64
}

func (c *countingTranscriptReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// printableTranscriptText reports whether b is valid UTF-8 text without
// control characters other than tab and line breaks.
func printableTranscriptText(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return false
		}
	}
	return true
}

func appendTranscriptJSONText(parts []string, v interface{}) []string {
	switch typed := v.(type) {
	case map[string]interface{}:
		for k, val := range typed {
			parts = append(parts, k)
			parts = appendTranscriptJSONText(parts, val)
		}
	case []interface{}:
		for _, e := range typed {
			parts = appendTranscriptJSONText(parts, e)
		}
	case string:
		parts = append(parts, typed)
	case json.Number:
		parts = append(parts, string(typed))
	}
	return parts
}

// transcriptEvidenceEscapedForms returns the JSON string-body encodings of
// v (without quotes) with and without HTML escaping.
func transcriptEvidenceEscapedForms(v string) []string {
	var out []string
	if b, err := json.Marshal(v); err == nil && len(b) >= 2 {
		if inner := string(b[1 : len(b)-1]); inner != v {
			out = append(out, inner)
		}
	}
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err == nil {
		s := strings.TrimSuffix(sb.String(), "\n")
		if len(s) >= 2 {
			if inner := s[1 : len(s)-1]; inner != v && (len(out) == 0 || out[0] != inner) {
				out = append(out, inner)
			}
		}
	}
	return out
}
