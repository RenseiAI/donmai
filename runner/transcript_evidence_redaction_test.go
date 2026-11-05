package runner

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/RenseiAI/donmai/agent"
)

// This file pins the transcript-evidence redaction contract as a class, not
// as a list of patched encodings: every case below is a way a reviewer
// recovered a known session secret (or an unknown credential under a
// sensitive key) from a successful archive in an earlier round, plus the
// neighbouring encodings of the same class. Each case runs through the real
// archive path (preserveUnpublishedWork: read, redact, atomic write,
// readback) and the archive is then attacked by an oracle that is
// implemented independently of the production redactor.

// evidenceOracleViews decodes s every way the oracle knows: JSON string
// escapes (any depth), Python-style \x escapes, URL percent-encoding, HTML
// entities, standalone base64 tokens, and JSON values embedded anywhere in
// the text. It returns every distinct view reachable within a fixed budget.
func evidenceOracleViews(s string) []string {
	seen := map[string]bool{s: true}
	queue := []string{s}
	out := []string{s}
	push := func(v string) {
		if v == "" || seen[v] || len(out) >= 400 {
			return
		}
		seen[v] = true
		queue = append(queue, v)
		out = append(out, v)
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		push(oracleUnescapeJSONish(cur))
		if v, err := url.QueryUnescape(cur); err == nil {
			push(v)
		}
		if v, err := url.PathUnescape(cur); err == nil {
			push(v)
		}
		push(html.UnescapeString(cur))
		for _, tok := range oracleBase64RE.FindAllString(cur, -1) {
			for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
				if b, err := enc.DecodeString(tok); err == nil {
					push(string(b))
				}
			}
		}
		for i := 0; i < len(cur); i++ {
			if cur[i] != '{' && cur[i] != '[' && cur[i] != '"' {
				continue
			}
			dec := json.NewDecoder(strings.NewReader(cur[i:]))
			dec.UseNumber()
			var v interface{}
			if err := dec.Decode(&v); err == nil {
				for _, leaf := range oracleLeaves(v) {
					push(leaf)
				}
			}
		}
	}
	return out
}

var (
	oracleBase64RE  = regexp.MustCompile(`[A-Za-z0-9+/_-]{8,}={0,2}`)
	oracleUnicodeRE = regexp.MustCompile(`\\u([dD][89abAB][0-9a-fA-F]{2})\\u([dD][c-fC-F][0-9a-fA-F]{2})|\\[uU]([0-9a-fA-F]{4})|\\x([0-9a-fA-F]{2})|\\(["\\/bfnrt])`)
	oracleNumberRE  = regexp.MustCompile(`-?[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?`)
)

// oracleUnescapeJSONish decodes one layer of JSON/Python-style escapes with
// a regular expression (deliberately not sharing code with production).
func oracleUnescapeJSONish(s string) string {
	return oracleUnicodeRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := oracleUnicodeRE.FindStringSubmatch(m)
		switch {
		case sub[1] != "":
			return string(utf16.DecodeRune(oracleHexRune(sub[1]), oracleHexRune(sub[2])))
		case sub[3] != "":
			return string(oracleHexRune(sub[3]))
		case sub[4] != "":
			b, _ := hex.DecodeString(sub[4])
			return string(b)
		default:
			return map[string]string{`"`: `"`, `\`: `\`, `/`: `/`, "b": "\b", "f": "\f", "n": "\n", "r": "\r", "t": "\t"}[sub[5]]
		}
	})
}

// oracleHexRune reads a validated hex string as a rune.
func oracleHexRune(h string) rune {
	b, _ := hex.DecodeString(h)
	var r rune
	for _, c := range b {
		r = r<<8 | rune(c)
	}
	return r
}

// oracleLeaves flattens one decoded JSON value into every key, string, and
// scalar literal it carries.
func oracleLeaves(v interface{}) []string {
	switch t := v.(type) {
	case map[string]interface{}:
		var out []string
		for k, val := range t {
			out = append(out, k)
			out = append(out, oracleLeaves(val)...)
		}
		return out
	case []interface{}:
		var out []string
		for _, e := range t {
			out = append(out, oracleLeaves(e)...)
		}
		return out
	case string:
		return []string{t}
	case json.Number:
		return []string{string(t)}
	case bool:
		return []string{strconv.FormatBool(t)}
	case nil:
		return []string{"null"}
	}
	return nil
}

// oracleSecretNeedles turns a secret into what must never be recoverable:
// the value itself, each scalar value of a JSON-shaped secret, or each line
// of a multi-line secret.
func oracleSecretNeedles(secret string) []string {
	trimmed := strings.TrimSpace(secret)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var v interface{}
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		if dec.Decode(&v) == nil {
			// Field names of a structured credential are schema, not
			// secret; its scalar values are what must not survive.
			var out []string
			for _, leaf := range oracleLeaves(v) {
				if !strings.Contains(trimmed, `"`+leaf+`":`) && !strings.Contains(trimmed, `"`+leaf+`" :`) {
					out = append(out, leaf)
				}
			}
			return out
		}
	}
	if strings.ContainsAny(secret, "\r\n") {
		var out []string
		for _, line := range strings.FieldsFunc(secret, func(r rune) bool { return r == '\n' || r == '\r' }) {
			if line = strings.TrimSpace(line); line != "" {
				out = append(out, line)
			}
		}
		return out
	}
	return []string{secret}
}

// assertNoRecoverableSecret fails when any archived JSONL record is invalid
// JSON or when any oracle view of any key or value contains one of the
// secrets, by bytes or (for numeric secrets) by numeric value.
func assertNoRecoverableSecret(t *testing.T, archive string, secrets []string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSuffix(archive, "\n"), "\n") {
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		var rec interface{}
		if err := dec.Decode(&rec); err != nil {
			t.Fatalf("archived record is not valid JSON %q: %v", line, err)
		}
		for _, leaf := range oracleLeaves(rec) {
			for _, view := range evidenceOracleViews(leaf) {
				for _, secret := range secrets {
					for _, needle := range oracleSecretNeedles(secret) {
						if strings.Contains(view, needle) {
							t.Fatalf("secret %q recoverable from archived record %q (view %q)", needle, line, view)
						}
						nv, err := strconv.ParseFloat(needle, 64)
						if err != nil {
							continue
						}
						for _, tok := range oracleNumberRE.FindAllString(view, -1) {
							if tv, err := strconv.ParseFloat(tok, 64); err == nil && tv == nv {
								t.Fatalf("numeric secret %q recoverable as %q from archived record %q", needle, tok, line)
							}
						}
					}
				}
			}
		}
	}
}

// archiveTranscriptForTest runs one transcript body through the real
// failed-session archive path and returns the archived bytes.
func archiveTranscriptForTest(t *testing.T, body string, qw QueuedWork) string {
	t.Helper()
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, ".agent/transcript.jsonl", body)
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
	if !r.preserveUnpublishedWork(qw, res) {
		t.Fatalf("preserve = false (warnings %v)", res.PostSessionWarnings)
	}
	files := findEvidenceFiles(t, r.rescueDir)
	if len(files) != 1 {
		t.Fatalf("evidence files = %v; want one", files)
	}
	return readEvidence(t, files[0])
}

// jsonlRecord encodes one transcript record whose msg field carries text.
func jsonlRecord(t *testing.T, fields map[string]interface{}) string {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	return string(b) + "\n"
}

// toolResultRecord mirrors the recorded CLI JSONL tool_result shape: the
// tool's stdout is an untrusted string nested inside the record.
func toolResultRecord(t *testing.T, output string) string {
	t.Helper()
	return jsonlRecord(t, map[string]interface{}{
		"type": "user",
		"message": map[string]interface{}{
			"role": "user",
			"content": []interface{}{map[string]interface{}{
				"type": "tool_result", "tool_use_id": "toolu_abc", "is_error": false, "content": output,
			}},
		},
	})
}

// TestTranscriptEvidence_SecretEncodingClass drives every known recovery
// route through the archive. Earlier designs enumerated encodings in the
// redactor and each round found the next one; the archive now proves each
// archived string clean against every decoding view (or drops that string),
// so the whole class closes rather than this list.
func TestTranscriptEvidence_SecretEncodingClass(t *testing.T) {
	long := strings.Repeat("qz", 4100)
	cases := []struct {
		name    string
		env     map[string]string
		bearer  string
		body    func(t *testing.T) string
		secrets []string // what must not be recoverable; defaults to env values + bearer
	}{
		{name: "embedded-unicode-array", env: map[string]string{"K": "café-passphrase"}, body: func(t *testing.T) string {
			return toolResultRecord(t, `tool output: ["caf\u00e9-passphrase"]`)
		}},
		{name: "embedded-numeric-array", env: map[string]string{"K": "987654321"}, body: func(t *testing.T) string {
			return toolResultRecord(t, `tool output: [9.87654321e8]`)
		}},
		{name: "prose-json-string-literal", env: map[string]string{"K": "café-passphrase"}, body: func(t *testing.T) string {
			return toolResultRecord(t, `tool output: "caf\u00e9-passphrase"`)
		}},
		{name: "double-escaped-unicode", env: map[string]string{"K": "café-passphrase"}, body: func(t *testing.T) string {
			return toolResultRecord(t, `tool output: "caf\\u00e9-passphrase"`)
		}},
		{name: "boolean-secret", env: map[string]string{"K": "true"}, body: func(*testing.T) string {
			return "{\"value\":true}\n"
		}},
		{name: "null-secret", env: map[string]string{"K": "null"}, body: func(*testing.T) string {
			return "{\"value\":null}\n"
		}},
		{name: "plain-text-structured", env: map[string]string{"K": `{"mykey": "probe-structured-value"}`}, body: func(*testing.T) string {
			return "tool output: {\"mykey\":\"probe-structured-value\"}\n"
		}},
		{name: "plain-text-unknown-password", secrets: []string{"zz-plain-unknown"}, body: func(*testing.T) string {
			return "export PASSWORD=zz-plain-unknown\n"
		}},
		{name: "structured-object-record", env: map[string]string{"K": `{"mykey": "probe-structured-value"}`}, body: func(t *testing.T) string {
			return jsonlRecord(t, map[string]interface{}{"msg": map[string]interface{}{"mykey": "probe-structured-value"}})
		}},
		{name: "ninth-embedded-object", env: map[string]string{"K": "shadow-canary"}, body: func(t *testing.T) string {
			return toolResultRecord(t, strings.Repeat(` {"data":"shadow\u002dcanary"}`, 9))
		}},
		{name: "numeric-equivalent-number", env: map[string]string{"K": "987654321"}, body: func(*testing.T) string {
			return "{\"code\":9.87654321e8}\n"
		}},
		{name: "counter-key-object", secrets: []string{"unknown-probe-value"}, body: func(*testing.T) string {
			return "{\"input_access_token\":{\"value\":\"unknown-probe-value\"}}\n"
		}},
		{name: "counter-key-array", secrets: []string{"unknown-array-value"}, body: func(*testing.T) string {
			return "{\"input_tokens\":[\"unknown-array-value\"],\"msg\":\"clean\"}\n"
		}},
		{name: "ninth-embedded-structured", env: map[string]string{"K": `{"mykey": "probe-structured-ninth-xyz"}`}, body: func(t *testing.T) string {
			return jsonlRecord(t, map[string]interface{}{"msg": "prefix" + strings.Repeat(` {"mykey":"probe-structured-ninth-xyz"}`, 9) + " suffix"})
		}},
		{name: "sensitive-numeric-unknown", secrets: []string{"84627519"}, body: func(*testing.T) string {
			return "{\"password\":84627519}\n"
		}},
		{name: "escaped-nested-unknown-password", secrets: []string{"zz-unknown-pw"}, body: func(t *testing.T) string {
			return toolResultRecord(t, `result: {\"password\":\"zz-unknown-pw\"}`)
		}},
		{name: "multiline-plain-lines", env: map[string]string{"K": "line-one-secret\nline-two-secret"}, body: func(*testing.T) string {
			return "line-one-secret\nline-two-secret\n"
		}},
		{name: "multiline-fragment-only", env: map[string]string{"K": "line-one-secret\nline-two-secret"}, body: func(t *testing.T) string {
			return toolResultRecord(t, "tail shows line-two-secret only")
		}},
		{name: "url-encoded", env: map[string]string{"K": "p@ss/w0rd!"}, body: func(t *testing.T) string {
			return toolResultRecord(t, "GET /cb?k=p%40ss%2Fw0rd%21 200")
		}},
		{name: "python-bytes-repr", env: map[string]string{"K": "café-passphrase"}, body: func(t *testing.T) string {
			return toolResultRecord(t, `b'caf\xc3\xa9-passphrase'`)
		}},
		{name: "html-entity", env: map[string]string{"K": "pa&ss-word-123"}, body: func(t *testing.T) string {
			return toolResultRecord(t, "<td>pa&amp;ss-word-123</td>")
		}},
		{name: "standalone-base64", env: map[string]string{"K": "hunter2-secret"}, body: func(t *testing.T) string {
			return toolResultRecord(t, "echo "+base64.StdEncoding.EncodeToString([]byte("hunter2-secret")))
		}},
		{name: "letter-case-variant", env: map[string]string{"K": "AbCdEf0123-Token"}, secrets: []string{"abcdef0123-token"}, body: func(t *testing.T) string {
			return toolResultRecord(t, "printed lower: abcdef0123-token")
		}},
		{name: "secret-as-key", env: map[string]string{"K": "key-shaped-secret"}, body: func(*testing.T) string {
			return "{\"key-shaped-secret\":1}\n"
		}},
		{name: "quoted-bearer-in-prefixed-json", bearer: `ab"cd-bearer-xyz`, body: func(t *testing.T) string {
			return toolResultRecord(t, `tool output: {"t":"ab\"cd-bearer-xyz"}`)
		}},
		{name: "short-bearer", bearer: "sh0rt", body: func(t *testing.T) string {
			return toolResultRecord(t, "holds sh0rt here")
		}},
		{name: "long-env-value", env: map[string]string{"K": long}, body: func(t *testing.T) string {
			return jsonlRecord(t, map[string]interface{}{"msg": "v=" + long})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			qw := QueuedWork{QueuedWork: queuedWorkBase("CLASS")}
			qw.Env = tc.env
			qw.AuthToken = tc.bearer
			secrets := tc.secrets
			if secrets == nil {
				for _, v := range tc.env {
					secrets = append(secrets, v)
				}
				if tc.bearer != "" {
					secrets = append(secrets, tc.bearer)
				}
			}
			got := archiveTranscriptForTest(t, tc.body(t), qw)
			assertNoRecoverableSecret(t, got, secrets)
		})
	}
}

// TestTranscriptEvidence_CleanRecordsKeepContent pins evidence fidelity:
// records that carry no secret survive with their content intact (key order
// aside), so the fail-closed rail does not degrade into archiving nothing.
func TestTranscriptEvidence_CleanRecordsKeepContent(t *testing.T) {
	qw := QueuedWork{QueuedWork: queuedWorkBase("FIDELITY")}
	qw.Env = map[string]string{"API_KEY": "an-unrelated-secret-value"}
	body := toolResultRecord(t, `go test ./... FAIL: TestX (0.01s) {"ok":false,"n":3}`) +
		"{\"type\":\"assistant\",\"usage\":{\"input_tokens\":1234,\"output_tokens\":56},\"text\":\"I will retry the build.\"}\n" +
		"plain stderr line: exit status 2\n"
	got := archiveTranscriptForTest(t, body, qw)
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("archived %d records; want 3:\n%s", len(lines), got)
	}
	for i, want := range []string{
		`go test ./... FAIL: TestX (0.01s) {"ok":false,"n":3}`,
		"I will retry the build.",
		"plain stderr line: exit status 2",
	} {
		if !strings.Contains(oracleDecodedText(t, lines[i]), want) {
			t.Fatalf("record %d lost clean content %q:\n%s", i, want, lines[i])
		}
	}
	if !strings.Contains(lines[1], `"input_tokens":1234`) {
		t.Fatalf("usage counters lost numeric shape: %s", lines[1])
	}
}

// oracleDecodedText joins every decoded leaf of one record.
func oracleDecodedText(t *testing.T, line string) string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(line)))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("invalid record %q: %v", line, err)
	}
	return strings.Join(oracleLeaves(v), "\n")
}
