package runner

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// Hardening for the failed-session transcript-tail redaction: base64
// fragments, unknown credential formats, map keys under credential keys, a
// per-file redaction deadline, and the mutant gaps. Each test fails without
// its production fix and passes with it.

// TestTranscriptEvidenceHardening_Base64TruncatedMod1 pins the truncated
// view: a token whose length mod 4 is 1 still decodes after trimming.
func TestTranscriptEvidenceHardening_Base64TruncatedMod1(t *testing.T) {
	secret := "TRUNCATED-BASE64-SECRET-XYZ"
	raw := base64.RawStdEncoding.EncodeToString([]byte(secret))
	token := raw + "X"
	if len(token)%4 != 1 {
		t.Fatalf("setup token len mod 4 = %d; want 1", len(token)%4)
	}
	if got := decodeTranscriptBase64Tokens("prefix " + token + " suffix"); !strings.Contains(got, secret) {
		t.Fatalf("truncated view missing secret; view = %q", got)
	}
}

// TestTranscriptEvidenceHardening_Base64UnalignedFragment pins the offset
// view: a secret starting mid-token still decodes at offsets 0-3.
func TestTranscriptEvidenceHardening_Base64UnalignedFragment(t *testing.T) {
	secret := "UNALIGNED-FRAGMENT-SECRET-XYZ"
	raw := base64.RawStdEncoding.EncodeToString([]byte(secret))
	token := "ZZ" + raw
	if got := decodeTranscriptBase64Tokens("id " + token); !strings.Contains(got, secret) {
		t.Fatalf("unaligned view missing secret; view = %q", got)
	}
}

// TestTranscriptEvidenceHardening_Base64Wrapped76 pins the whitespace-joined
// view through the real archive: GNU base64 wraps a 60-byte secret at 76
// columns, so no single line carries it.
func TestTranscriptEvidenceHardening_Base64Wrapped76(t *testing.T) {
	secret := strings.Repeat("s", 60)
	full := base64.StdEncoding.EncodeToString([]byte(secret))
	if len(full) <= 76 {
		t.Fatalf("setup base64 len = %d; want output wrapped past 76 columns", len(full))
	}
	wrapped := full[:76] + "\n" + full[76:]
	if got := decodeTranscriptBase64Tokens("prefix " + wrapped + " suffix"); !strings.Contains(got, secret) {
		t.Fatalf("joined view missing secret; view len = %d", len(got))
	}
	qw := QueuedWork{QueuedWork: queuedWorkBase("B64WRAP")}
	qw.AuthToken = secret
	got := archiveTranscriptForTest(t, toolResultRecord(t, "output "+wrapped), qw)
	if strings.Contains(got, full[:20]) {
		t.Fatalf("archive carries wrapped base64 of the secret:\n%s", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("archive missing [REDACTED]:\n%s", got)
	}
}

// TestTranscriptEvidenceHardening_Base64UTF16LE pins the UTF-16LE view
// through the real archive: a PowerShell-style UTF-16LE secret decodes to
// its UTF-8 form.
func TestTranscriptEvidenceHardening_Base64UTF16LE(t *testing.T) {
	secret := "hunter2-test-secret"
	u16 := make([]byte, 0, 2*len(secret))
	for i := 0; i < len(secret); i++ {
		u16 = append(u16, secret[i], 0)
	}
	encoded := base64.StdEncoding.EncodeToString(u16)
	if got := decodeTranscriptBase64Tokens("blob " + encoded); !strings.Contains(got, secret) {
		t.Fatalf("UTF-16LE view missing secret")
	}
	qw := QueuedWork{QueuedWork: queuedWorkBase("B64U16")}
	qw.AuthToken = secret
	got := archiveTranscriptForTest(t, toolResultRecord(t, "blob "+encoded), qw)
	if strings.Contains(got, encoded[:20]) {
		t.Fatalf("archive carries UTF-16LE base64 of the secret:\n%s", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("archive missing [REDACTED]:\n%s", got)
	}
}

// TestTranscriptEvidenceHardening_UnknownCredentialFormats pins the
// fail-closed unknown check: every listed format is refused even though the
// shared rail misses it.
func TestTranscriptEvidenceHardening_UnknownCredentialFormats(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	cases := []struct {
		name   string
		body   string
		secret string
	}{
		{name: "pem-key", body: "key:\n-----BEGIN PRIVATE KEY-----\nMIIEvwIBADANBgkqhkiG9w0BAQEFAASC\n-----END PRIVATE KEY-----\n", secret: "-----BEGIN PRIVATE KEY-----"},
		{name: "aws-access-key-id", body: "id AKIAIOSFODNN7EXAMPLE shipped", secret: "AKIAIOSFODNN7EXAMPLE"},
		{name: "aws-secret-access-key", body: "export AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY done", secret: "AWS_SECRET_ACCESS_KEY"},
		{name: "secret-key", body: "export SECRET_KEY=super-secret-value-12345 done", secret: "SECRET_KEY"},
		{name: "github-pat", body: "token github_pat_abcdefghijklmnopqrstuvwxyz0123456789ABCD here", secret: "github_pat_abcdefghijklmnopqrstuvwxyz0123456789ABCD"},
		{name: "bare-jwt", body: "output " + jwt, secret: jwt},
		{name: "docker-auth", body: `config {"auths":{"example.test":{"auth":"dXNlcjpwYXNzd29yZA=="}}} end`, secret: "dXNlcjpwYXNzd29yZA=="},
		{name: "netrc-password", body: "netrc machine example.com password mysecretpassword123 login myuser", secret: "mysecretpassword123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			qw := QueuedWork{QueuedWork: queuedWorkBase("UNKNOWN-" + tc.name)}
			got := archiveTranscriptForTest(t, toolResultRecord(t, tc.body), qw)
			assertNoRecoverableSecret(t, got, []string{tc.secret})
			if !strings.Contains(got, "[REDACTED]") {
				t.Fatalf("archive missing [REDACTED]:\n%s", got)
			}
		})
	}
}

// TestTranscriptEvidenceHardening_MapKeysUnderCredentialKey pins F3: under a
// credential key both the nested keys and the values are replaced.
func TestTranscriptEvidenceHardening_MapKeysUnderCredentialKey(t *testing.T) {
	qw := QueuedWork{QueuedWork: queuedWorkBase("MAPKEYS")}
	got := archiveTranscriptForTest(t, "{\"password\":{\"inner-key\":\"inner-value\"},\"msg\":\"clean\"}\n", qw)
	if strings.Contains(got, "inner-key") || strings.Contains(got, "inner-value") {
		t.Fatalf("archive carries map key or value under a credential key:\n%s", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("archive missing [REDACTED]:\n%s", got)
	}
	if !strings.Contains(got, "clean") {
		t.Fatalf("archive lost the clean sibling field:\n%s", got)
	}
}

// TestTranscriptEvidenceHardening_RedactionDeadline pins F4: a redaction
// that outlives the per-file deadline falls back to a single redacted
// record instead of stalling finalization.
func TestTranscriptEvidenceHardening_RedactionDeadline(t *testing.T) {
	old := transcriptRedactionTimeout
	transcriptRedactionTimeout = time.Nanosecond
	t.Cleanup(func() { transcriptRedactionTimeout = old })
	qw := QueuedWork{QueuedWork: queuedWorkBase("RDEADLINE")}
	got := archiveTranscriptForTest(t, toolResultRecord(t, "output "+strings.Repeat("x", 1024)), qw)
	if strings.Count(got, "redactedRecord") != 1 {
		t.Fatalf("deadline fallback = %q; want exactly one redactedRecord", got)
	}
}

// TestTranscriptEvidenceHardening_HTMLView pins the HTML decoding view: a
// secret hidden behind one entity layer is still detected.
func TestTranscriptEvidenceHardening_HTMLView(t *testing.T) {
	views, complete := transcriptDecodingViews("prefix pa&amp;ss-word-123 suffix")
	if !complete {
		t.Fatal("views incomplete for plain input")
	}
	found := false
	for _, v := range views {
		if strings.Contains(v, "pa&ss-word-123") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no view decodes the HTML entity; views = %q", views)
	}
}

// TestTranscriptEvidenceHardening_EmbeddedJSONView pins the embedded-JSON
// view: scalar leaves of JSON inside a string are visible without their
// braces.
func TestTranscriptEvidenceHardening_EmbeddedJSONView(t *testing.T) {
	views, complete := transcriptDecodingViews(`prefix {"k":"v-unique-leaf"} suffix`)
	if !complete {
		t.Fatal("views incomplete for plain input")
	}
	found := false
	for _, v := range views {
		if strings.Contains(v, "v-unique-leaf") && !strings.Contains(v, "{") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no braceless view carries the embedded leaf; views = %q", views)
	}
}

// TestTranscriptEvidenceHardening_CleanBudgetExceeded pins the fail-closed
// budget: a string whose views exceed the budget is not proved clean.
func TestTranscriptEvidenceHardening_CleanBudgetExceeded(t *testing.T) {
	old := transcriptEvidenceMaxViews
	transcriptEvidenceMaxViews = 1
	t.Cleanup(func() { transcriptEvidenceMaxViews = old })
	r := newTranscriptRedactor(nil)
	if r.clean("prefix aGVsbG8td29ybGQ= suffix", nil) {
		t.Fatal("clean = true with an exhausted view budget; want false")
	}
}

// TestTranscriptEvidenceHardening_WriteTimeoutKeepsWorkarea pins the write
// deadline: a write that never returns keeps the workarea instead of hanging
// finalization.
func TestTranscriptEvidenceHardening_WriteTimeoutKeepsWorkarea(t *testing.T) {
	release := make(chan struct{})
	exited := make(chan struct{})
	oldWrite := transcriptEvidenceWriteAtomic
	transcriptEvidenceWriteAtomic = func(string, string, []byte) error {
		<-release
		close(exited)
		return nil
	}
	oldTimeout := transcriptEvidenceIOTimeout
	transcriptEvidenceIOTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		close(release)
		<-exited
		transcriptEvidenceWriteAtomic = oldWrite
		transcriptEvidenceIOTimeout = oldTimeout
	})
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, ".agent/transcript.jsonl", "{\"msg\":\"hello\"}\n")
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
	if r.preserveUnpublishedWork(QueuedWork{QueuedWork: queuedWorkBase("WRITETO")}, res) {
		t.Fatal("preserve = true for timed-out write; want false so the workarea is kept")
	}
	if len(evidenceWarnings(res)) != 1 {
		t.Fatalf("warnings = %q; want exactly the retained warning", res.PostSessionWarnings)
	}
}

// TestTranscriptEvidenceHardening_ReadbackTimeoutKeepsWorkarea pins the
// readback deadline the same way.
func TestTranscriptEvidenceHardening_ReadbackTimeoutKeepsWorkarea(t *testing.T) {
	release := make(chan struct{})
	exited := make(chan struct{})
	oldReadback := transcriptEvidenceReadback
	transcriptEvidenceReadback = func(string, []byte) error {
		<-release
		close(exited)
		return nil
	}
	oldTimeout := transcriptEvidenceIOTimeout
	transcriptEvidenceIOTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		close(release)
		<-exited
		transcriptEvidenceReadback = oldReadback
		transcriptEvidenceIOTimeout = oldTimeout
	})
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, ".agent/transcript.jsonl", "{\"msg\":\"hello\"}\n")
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
	if r.preserveUnpublishedWork(QueuedWork{QueuedWork: queuedWorkBase("READTO")}, res) {
		t.Fatal("preserve = true for timed-out readback; want false so the workarea is kept")
	}
	if len(evidenceWarnings(res)) != 1 {
		t.Fatalf("warnings = %q; want exactly the retained warning", res.PostSessionWarnings)
	}
}

// TestTranscriptEvidenceHardening_LineLevelContainsSecret pins the raw-bytes
// defense in depth: when the session secret is the wrapper key itself, the
// line falls back to a single redacted record.
func TestTranscriptEvidenceHardening_LineLevelContainsSecret(t *testing.T) {
	r := newTranscriptRedactor([]string{"nonJsonLine"})
	if got := string(r.redactLine("hello")); got != "{\"redactedRecord\":true}" {
		t.Fatalf("redactLine = %q; want the redacted record", got)
	}
}

// TestTranscriptEvidenceHardening_BoolNullUnderCredentialKey pins the type
// coverage: booleans and nulls under a credential key are replaced, not
// kept as JSON literals.
func TestTranscriptEvidenceHardening_BoolNullUnderCredentialKey(t *testing.T) {
	r := newTranscriptRedactor(nil)
	for _, tc := range []struct{ name, line, banned string }{
		{name: "bool", line: `{"password":true,"msg":"clean"}`, banned: ":true"},
		{name: "null", line: `{"password":null,"msg":"clean"}`, banned: ":null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := string(r.redactLine(tc.line))
			if strings.Contains(got, tc.banned) {
				t.Fatalf("redactLine keeps %q under a credential key: %q", tc.banned, got)
			}
			if !strings.Contains(got, "[REDACTED]") {
				t.Fatalf("redactLine missing [REDACTED]: %q", got)
			}
		})
	}
}

// TestTranscriptEvidenceHardening_UnleasedRetainMetric pins F6: an unleased
// workarea kept after a tail failure bumps the retained metric the operator
// sweep reads.
func TestTranscriptEvidenceHardening_UnleasedRetainMetric(t *testing.T) {
	before := transcriptEvidenceRetainedWorkareas()
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, ".agent/transcript.jsonl", "{\"msg\":\"hello\"}\n")
	oldRead := transcriptEvidenceReadTail
	transcriptEvidenceReadTail = func(*os.File, os.FileInfo, int64) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}
	t.Cleanup(func() { transcriptEvidenceReadTail = oldRead })
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{}
	res.Status = "failed"
	res.WorktreePath = clone
	res.rescueTargets = []rescueTarget{{path: clone, base: base}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ".agent/transcript.jsonl"}}
	if r.preserveUnpublishedWork(QueuedWork{QueuedWork: queuedWorkBase("METRIC")}, res) {
		t.Fatal("preserve = true for refused tail; want false so the workarea is kept")
	}
	if got := transcriptEvidenceRetainedWorkareas(); got != before+1 {
		t.Fatalf("retained metric = %d; want %d", got, before+1)
	}
}
