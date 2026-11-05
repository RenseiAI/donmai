package runner

import (
	"encoding/base64"
	"encoding/hex"
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
//     by bytes, by numeric value for numeric secrets (including zero-padded
//     ones with at least four significant digits), by each string and
//     number leaf of a JSON-shaped secret (its booleans and nulls carry no
//     secret material), and by each line of a multi-line secret;
//   - a value the shared output rail (diagnostic.Redact) would mask, such as
//     an unknown password, bearer, or token-shaped string, or a value the
//     WIP checkpoint content gate (wipSecretContentRE) would refuse, such as
//     a PEM block, an access-key ID, a PAT, a JWT, a docker auth entry, a
//     netrc password, or a SECRET_KEY assignment;
//   - a value of any type under a credential-named key (the key is judged in
//     all of its decoding views), including inside JSON embedded in a string;
//     under such a key both the nested keys and the values are replaced;
//   - an unknown credential found that way (four bytes or longer) repeated
//     anywhere else in the same string;
//
// under any combination of these decodings, applied repeatedly: JSON and
// Python-style string escapes, URL percent-encoding, HTML entities, base64
// tokens (at offsets 0-3 with trimmed lengths, whitespace-joined runs, and
// UTF-16LE forms), and JSON embedded anywhere inside a string. Known
// secrets also match in any letter case.
//
// Design. Earlier versions enumerated encodings inside the redactor, and each
// review found the next encoding it missed. The redactor is now best-effort
// only; the guarantee comes from proving each output string clean. Every
// key and string value is rewritten, then every decoding view of the
// rewritten text is checked. A string that cannot be proved clean is
// replaced wholesale with [REDACTED]. The record keeps its other fields, and
// only the unprovable string is dropped. A missed encoding therefore costs
// that one string, never a leak. Known secrets are also rewritten in their
// hex spelling. Transforms outside the decoding set (hashing, splitting a
// secret across values, encrypting, Unicode normalization) cannot be found
// by any text redactor and are outside this contract. The archive remains
// local-only, with owner-only modes.

const transcriptRedacted = "[REDACTED]"

// transcriptEvidenceMaxViews bounds the decoding views of one string. A
// string whose views exceed the budget is not proved clean and is dropped.
// A variable so tests can prove the fail-closed budget branch without
// building a 64-view closure.
var transcriptEvidenceMaxViews = 64

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

// isSensitiveTranscriptKey reports whether a key names a credential in any
// of its decoding views (pass%77ord, base64 of "password", ...). A key whose
// views cannot all be computed is treated as sensitive.
func isSensitiveTranscriptKey(k string) bool {
	if isUsageTranscriptCounterKey(k) {
		return false
	}
	if transcriptSensitiveKeyRE.MatchString(k) {
		return true
	}
	views, complete := transcriptDecodingViews(k)
	if !complete {
		return true
	}
	for _, v := range views {
		if transcriptSensitiveKeyRE.MatchString(v) {
			return true
		}
	}
	return false
}

// transcriptRedactor holds the matching state derived from one session's
// secrets.
type transcriptRedactor struct {
	// replacer replaces every needle in one pass (longest first), so a
	// replacement marker is never re-scanned by a later needle.
	replacer *strings.Replacer
	// folded matches the case-folded needles against case-folded text, so a
	// secret printed in another letter case is still detected.
	folded *strings.Replacer
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
		} else if transcriptPaddedNumberRE.MatchString(v) {
			// A zero-padded PIN printed as a number loses its padding
			// (012345 -> 12345). Padded values with fewer than four
			// significant digits are left to byte matching: as numbers
			// they would match ordinary small counts everywhere.
			if c := canonicalTranscriptNumber(v); len(strings.TrimLeft(strings.SplitN(c, "e", 2)[0], "-")) >= 4 {
				r.numbers[c] = true
			}
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
		addNeedle(strings.ToValidUTF8(b, "\uFFFD"))
		for _, form := range transcriptEvidenceEscapedForms(b) {
			addNeedle(form)
		}
		addNeedle(url.QueryEscape(b))
		addNeedle(url.PathEscape(b))
		addNeedle(html.EscapeString(b))
		if len(b) >= 3 {
			addNeedle(base64.RawStdEncoding.EncodeToString([]byte(b)))
			addNeedle(base64.RawURLEncoding.EncodeToString([]byte(b)))
		}
		if len(b) >= 4 {
			addNeedle(hex.EncodeToString([]byte(b)))
		}
	}
	sort.SliceStable(needles, func(i, j int) bool { return len(needles[i]) > len(needles[j]) })
	var pairs, foldedPairs []string
	for _, n := range needles {
		pairs = append(pairs, n, transcriptRedacted)
		foldedPairs = append(foldedPairs, strings.ToLower(n), transcriptRedacted)
	}
	if len(pairs) > 0 {
		r.replacer = strings.NewReplacer(pairs...)
		r.folded = strings.NewReplacer(foldedPairs...)
	}
	return r
}

// containsSecret reports whether text carries a needle, by bytes or in any
// letter case. Markers already written are blanked first so a secret that is
// a substring of the marker itself cannot make every redacted string look
// dirty.
func (r *transcriptRedactor) containsSecret(text string) bool {
	if r.replacer == nil {
		return false
	}
	blanked := strings.ReplaceAll(text, transcriptRedacted, "\x00")
	if r.replacer.Replace(blanked) != blanked {
		return true
	}
	lower := strings.ToLower(blanked)
	return r.folded.Replace(lower) != lower
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

// rewrite is the best-effort text pass. Embedded JSON objects and arrays
// are redacted structurally first, while their key context is intact (a
// text rule applied to a credential object would mask only its opening
// brace). Then secret needles, then the shared rail. It reports false when
// the embedded-JSON parse budget runs out.
func (r *transcriptRedactor) rewrite(s string) (string, bool) {
	s, ok := r.spliceEmbeddedJSON(s)
	if !ok {
		return "", false
	}
	if r.replacer != nil {
		s = r.replacer.Replace(s)
	}
	return diagnostic.Redact(s), true
}

// spliceEmbeddedJSON replaces each JSON object or array embedded in s with
// its structurally redacted encoding, leaving spans that need no change
// byte-for-byte as written.
func (r *transcriptRedactor) spliceEmbeddedJSON(s string) (string, bool) {
	if !strings.ContainsAny(s, "{[") {
		return s, true
	}
	budget := transcriptEmbeddedJSONBudget(s)
	var sb strings.Builder
	prev := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '{' && s[i] != '[' {
			continue
		}
		v, n, ok := decodeEmbeddedTranscriptJSON(s[i:], &budget)
		if budget < 0 {
			return "", false
		}
		if !ok {
			continue
		}
		before, err1 := json.Marshal(v)
		after, err2 := json.Marshal(r.redactValue(v, false))
		if err1 != nil || err2 != nil {
			return "", false
		}
		if string(before) != string(after) {
			sb.WriteString(s[prev:i])
			sb.Write(after)
			prev = i + n
		}
		i += n - 1
	}
	if prev == 0 {
		return s, true
	}
	sb.WriteString(s[prev:])
	return sb.String(), true
}

// unknownSecrets returns the values the shared rail would mask, and every
// scalar under a credential-named key of embedded JSON, found in any
// decoding view of the input. They are unknown credentials local to this
// string: the output must not carry them anywhere, whatever the rewrite did
// to their surroundings. Values shorter than four bytes are left to the
// in-place rewrite, because as needles they would match ordinary text.
func unknownSecrets(s string) ([]string, bool) {
	views, complete := transcriptDecodingViews(s)
	if !complete {
		return nil, false
	}
	var out []string
	for _, v := range views {
		out = append(out, diagnostic.UnredactedValues(v)...)
		sensitive, ok := embeddedSensitiveTranscriptValues(v)
		if !ok {
			return nil, false
		}
		out = append(out, sensitive...)
	}
	kept := out[:0]
	for _, v := range out {
		if v = strings.TrimSpace(v); len(v) >= 4 && v != transcriptRedacted {
			kept = append(kept, v)
		}
	}
	return kept, true
}

// clean is the guarantee: no decoding view of s carries a known secret, a
// numeric secret, one of the unknown secrets found in the input, an
// unmasked shared-rail value, or a common token shape the WIP checkpoint
// gate would refuse (see wipSecretContentRE). Exceeding a budget is treated
// as unclean.
func (r *transcriptRedactor) clean(s string, unknown *strings.Replacer) bool {
	views, complete := transcriptDecodingViews(s)
	if !complete {
		return false
	}
	for _, v := range views {
		if r.containsSecret(v) || r.containsNumericSecret(v) || diagnostic.Unredacted(v) || wipSecretContentRE.MatchString(v) {
			return false
		}
		if unknown != nil {
			blanked := strings.ToLower(strings.ReplaceAll(v, transcriptRedacted, "\x00"))
			if unknown.Replace(blanked) != blanked {
				return false
			}
		}
	}
	return true
}

// redactString returns s rewritten when the rewrite is provably clean,
// otherwise a JSON-shaped s redacted structurally when that is provably
// clean, otherwise the marker. It never returns unproved text.
func (r *transcriptRedactor) redactString(s string) string {
	found, ok := unknownSecrets(s)
	if !ok {
		return transcriptRedacted
	}
	var unknown *strings.Replacer
	if len(found) > 0 {
		pairs := make([]string, 0, len(found))
		for _, v := range found {
			pairs = append(pairs, strings.ToLower(v), transcriptRedacted)
		}
		unknown = strings.NewReplacer(pairs...)
	}
	if out, ok := r.rewrite(s); ok && r.clean(out, unknown) {
		return out
	}
	if parsed, ok := parseWholeTranscriptJSON(s); ok {
		if b, err := json.Marshal(r.redactValue(parsed, false)); err == nil && r.clean(string(b), unknown) {
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
			// A map under a credential key holds credential material in its
			// keys as well as its values: redact both. The credential key
			// itself is schema and stays readable (it is redacted above only
			// when it carries a secret value).
			rk := transcriptRedacted
			if !sensitive {
				rk = r.redactString(k)
			}
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
	transcriptPaddedNumberRE = regexp.MustCompile(`^-?0[0-9]+(?:\.[0-9]+)?$`)
	transcriptJSONNumberRE   = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)
	transcriptNumberTokenRE  = regexp.MustCompile(`-?[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?`)
	transcriptBase64TokenRE  = regexp.MustCompile(`[A-Za-z0-9+/_-]{2,}={0,2}`)
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
// \' \xHH \UXXXXXXXX, and C-style octal \NNN. Unknown escapes are kept as written.
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
		case '"', '\'', '\\', '/':
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
		case '0', '1', '2', '3':
			// C-style octal byte (\303\251), as git and ls quote names.
			if i+3 < len(s) && isOctalDigit(s[i+2]) && isOctalDigit(s[i+3]) {
				out.WriteByte((c-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
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

func isOctalDigit(c byte) bool { return c >= '0' && c <= '7' }

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

// decodeTranscriptBase64Tokens joins the decodings of every base64-shaped
// token in s into one newline-separated view, so a long text adds one view,
// not one per word. Each token is decoded at offsets 0-3 with its length
// trimmed, so a truncated (length mod 4 == 1) or unaligned tail fragment
// still decodes to the secret it carries. Runs split by whitespace (GNU
// base64 wraps at 76 columns) are joined and decoded as one. Bytes that
// are not printable text (invalid UTF-8, control characters) become line
// breaks: a secret wrapped in binary noise still reads as a line of its
// own. Base64 of UTF-16LE text decodes to its UTF-8 form as well.
func decodeTranscriptBase64Tokens(s string) string {
	var parts []string
	seen := map[string]bool{}
	add := func(texts []string) {
		for _, text := range texts {
			if text != "" && !seen[text] {
				seen[text] = true
				parts = append(parts, text)
			}
		}
	}
	for _, tok := range transcriptBase64TokenRE.FindAllString(s, -1) {
		add(decodeBase64TranscriptVariants(tok))
	}
	for _, run := range transcriptBase64RunRE.FindAllString(s, -1) {
		stripped := strings.Join(strings.Fields(run), "")
		if len(strings.TrimRight(stripped, "=")) < 8 {
			continue
		}
		add(decodeBase64TranscriptVariants(stripped))
	}
	return strings.Join(parts, "\n")
}

// transcriptBase64RunRE finds whitespace-split base64 runs: GNU base64
// wraps at 76 columns, so a secret of 58 bytes or more spans a newline.
// Each piece is at least 4 base64 characters so ordinary word pairs do not
// join; short runs are skipped by the caller.
var transcriptBase64RunRE = regexp.MustCompile(`[A-Za-z0-9+/=_-]{4,}(?:\s+[A-Za-z0-9+/=_-]{4,})+`)

// decodeBase64TranscriptVariants decodes tok at offsets 0-3 with 0-2
// trailing characters trimmed, in both the standard and URL-safe
// alphabets. Lengths mod 4 == 1 never decode and are skipped. Each
// successful decode contributes its printable runs and, when the bytes
// read as UTF-16LE text, the UTF-8 form.
func decodeBase64TranscriptVariants(tok string) []string {
	stripped := strings.TrimRight(tok, "=")
	if len(stripped) < 2 {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	addBytes := func(b []byte) {
		if text := printableTranscriptRuns(b); text != "" && !seen[text] {
			seen[text] = true
			out = append(out, text)
		}
		if text := utf16LETranscriptText(b); text != "" && !seen[text] {
			seen[text] = true
			out = append(out, text)
		}
	}
	for offset := 0; offset < 4 && offset < len(stripped); offset++ {
		for trim := 0; trim <= 2; trim++ {
			end := len(stripped) - trim
			if end <= offset || end-offset < 2 {
				continue
			}
			sub := stripped[offset:end]
			if len(sub)%4 == 1 {
				continue
			}
			for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
				if b, err := enc.DecodeString(sub); err == nil {
					addBytes(b)
					break
				}
			}
		}
	}
	return out
}

// utf16LETranscriptText decodes b as UTF-16LE and returns its printable
// runs, or "" when b does not read as UTF-16LE text. The NUL heuristic
// keeps ordinary ASCII (no NUL at an odd byte) on the raw view alone, so
// every base64 token does not gain a redundant second view.
func utf16LETranscriptText(b []byte) string {
	if len(b) < 4 || len(b)%2 != 0 {
		return ""
	}
	hasNUL := false
	for i := 1; i < len(b); i += 2 {
		if b[i] == 0 {
			hasNUL = true
			break
		}
	}
	if !hasNUL {
		return ""
	}
	u16 := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u16 = append(u16, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return printableTranscriptRuns([]byte(string(utf16.Decode(u16))))
}

// printableTranscriptRuns maps every invalid UTF-8 byte and control
// character (other than tab) in b to a line break and drops empty lines.
func printableTranscriptRuns(b []byte) string {
	var sb strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if (r == utf8.RuneError && size <= 1) || (r < 0x20 && r != '\t') || r == 0x7f {
			sb.WriteByte('\n')
		} else {
			sb.Write(b[:size])
		}
		b = b[size:]
	}
	var lines []string
	for _, line := range strings.Split(sb.String(), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// transcriptEmbeddedJSONBudget bounds the bytes all embedded-JSON parse
// attempts over s may read, so adversarial nesting cannot make a scan
// quadratic.
func transcriptEmbeddedJSONBudget(s string) int64 {
	return int64(16*len(s)) + 64*1024
}

// decodeEmbeddedTranscriptJSON decodes one JSON value at the start of s,
// charging the bytes read to budget. It returns the value and its length.
func decodeEmbeddedTranscriptJSON(s string, budget *int64) (interface{}, int, bool) {
	src := &countingTranscriptReader{r: strings.NewReader(s)}
	dec := json.NewDecoder(src)
	dec.UseNumber()
	var v interface{}
	err := dec.Decode(&v)
	*budget -= src.n
	if err != nil {
		return nil, 0, false
	}
	n := int(dec.InputOffset())
	if n <= 0 || n > len(s) {
		return nil, 0, false
	}
	return v, n, true
}

// embeddedTranscriptJSONLeaves decodes every JSON value embedded in s
// (starting at a brace, bracket, or quote) and joins its keys and scalar
// leaves into one newline-separated view, so escapes inside embedded
// strings are decoded by JSON's own rules. It reports false when the parse
// budget runs out, which the caller treats as unclean.
func embeddedTranscriptJSONLeaves(s string) (string, bool) {
	var parts []string
	ok := forEachEmbeddedTranscriptJSON(s, `{["`, func(v interface{}) {
		parts = appendTranscriptJSONText(parts, v)
	})
	return strings.Join(parts, "\n"), ok
}

// embeddedSensitiveTranscriptValues returns every scalar found under a
// credential-named key in the JSON objects and arrays embedded in s.
func embeddedSensitiveTranscriptValues(s string) ([]string, bool) {
	var out []string
	ok := forEachEmbeddedTranscriptJSON(s, "{[", func(v interface{}) {
		out = appendSensitiveTranscriptLeaves(out, v, false)
	})
	return out, ok
}

// forEachEmbeddedTranscriptJSON calls fn for each JSON value that starts at
// one of the starts bytes in s, skipping past each decoded value.
func forEachEmbeddedTranscriptJSON(s, starts string, fn func(interface{})) bool {
	if !strings.ContainsAny(s, starts) {
		return true
	}
	budget := transcriptEmbeddedJSONBudget(s)
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(starts, s[i]) < 0 {
			continue
		}
		v, n, ok := decodeEmbeddedTranscriptJSON(s[i:], &budget)
		if budget < 0 {
			return false
		}
		if !ok {
			continue
		}
		fn(v)
		i += n - 1
	}
	return true
}

// appendSensitiveTranscriptLeaves appends the scalar leaves of v that sit
// under a credential-named key (sensitive marks that context).
func appendSensitiveTranscriptLeaves(out []string, v interface{}, sensitive bool) []string {
	switch typed := v.(type) {
	case map[string]interface{}:
		for k, val := range typed {
			out = appendSensitiveTranscriptLeaves(out, val, sensitive || isSensitiveTranscriptKey(k))
		}
	case []interface{}:
		for _, e := range typed {
			out = appendSensitiveTranscriptLeaves(out, e, sensitive)
		}
	case string:
		if sensitive {
			out = append(out, typed)
		}
	case json.Number:
		if sensitive {
			out = append(out, string(typed))
		}
	}
	return out
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
