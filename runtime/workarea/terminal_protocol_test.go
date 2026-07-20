package workarea

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	testSessionID    = "11111111-1111-4111-8111-111111111111"
	testInvocationID = "22222222-2222-4222-8222-222222222222"
	testClaimID      = "33333333-3333-4333-8333-333333333333"
	testLeaseID      = "twl_11111111111111111111111111111111"
	testResultID     = "tr_22222222222222222222222222222222"
	testWorkareaID   = "wa_33333333333333333333333333333333"
	testReceiverKey  = "rcv_44444444444444444444444444444444"
	testQuarantineID = "twq_55555555555555555555555555555555"
)

var testNow = time.Date(2026, 7, 18, 12, 0, 0, 123_000_000, time.UTC)

func TestTerminalLeaseRequestUsesExactFixedProfile(t *testing.T) {
	t.Parallel()
	request := DefaultTerminalLeaseRequest()
	got, err := CanonicalBytes(request)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schemaVersion":"donmai.terminal-workarea-lease-request.v1","settlementBudgetMs":977000,"safetyMarginMs":60000,"leaseDurationMs":1800000,"maxLeaseDurationMs":7200000}`
	if string(got) != want {
		t.Fatalf("canonical request = %s", got)
	}
	request.SettlementBudgetMS--
	if _, err := request.Policy(); err == nil {
		t.Fatal("variable v1 profile accepted")
	}
}

func TestTerminalProtocolCanonicalRoundTripAndProjection(t *testing.T) {
	t.Parallel()
	descriptor := fixtureDescriptor()
	full, err := CanonicalBytes(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(full), "workareaPath") {
		t.Fatalf("descriptor leaked path: %s", full)
	}
	projection, err := CanonicalBytes(descriptor.Projection())
	if err != nil {
		t.Fatal(err)
	}
	wantProjection := `{"leaseId":"twl_11111111111111111111111111111111","workareaId":"wa_33333333333333333333333333333333","terminalResultId":"tr_22222222222222222222222222222222","expiresAt":"2026-07-18T12:30:00.123Z"}`
	if string(projection) != wantProjection {
		t.Fatalf("projection = %s", projection)
	}

	raw := []byte(` { "expiresAt" : "2026-07-18T12:30:00.123Z", "terminalResultId":"tr_22222222222222222222222222222222", "workareaId":"wa_33333333333333333333333333333333", "leaseId":"twl_11111111111111111111111111111111" } `)
	var decoded TerminalLeaseProjection
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	reencoded, err := CanonicalBytes(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(reencoded) != wantProjection {
		t.Fatalf("re-encoded projection = %s", reencoded)
	}
}

func TestTerminalProtocolStringEscaping(t *testing.T) {
	t.Parallel()
	path := filepath.Join(string(filepath.Separator), "tmp", "<&>", "雪 line")
	digest := sha256.Sum256([]byte(path))
	lastError := "quote=\" slash=\\ controls=\b\t\n\f\r <&> 雪    "
	q := TerminalWorkareaQuarantine{
		SchemaVersion: TerminalWorkareaQuarantineSchemaV1, QuarantineID: testQuarantineID,
		WorkareaID: testWorkareaID, SessionID: testSessionID, TerminalResultID: testResultID,
		WorkareaPath: path, PathSHA256: hex.EncodeToString(digest[:]), Reason: "lease-acquisition-failed",
		State: QuarantineQuarantined, CreatedAt: testNow, UpdatedAt: testNow, LastError: &lastError,
	}
	got, err := CanonicalBytes(q)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, literal := range []string{"<&>", "雪", "\\u2028", "\\u2029", "\\b", "\\t", "\\n", "\\f", "\\r"} {
		if !strings.Contains(text, literal) {
			t.Fatalf("canonical quarantine lacks %q: %s", literal, text)
		}
	}
	for _, forbidden := range []string{"\\u003c", "\\u003e", "\\u0026"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("canonical quarantine contains forbidden HTML escape %q: %s", forbidden, text)
		}
	}
}

func TestTerminalProtocolRejectsInvalidRawJSON(t *testing.T) {
	t.Parallel()
	valid := `{"schemaVersion":"donmai.terminal-workarea-lease-request.v1","settlementBudgetMs":977000,"safetyMarginMs":60000,"leaseDurationMs":1800000,"maxLeaseDurationMs":7200000}`
	cases := map[string][]byte{
		"bom":                append([]byte{0xef, 0xbb, 0xbf}, []byte(valid)...),
		"duplicate escaped":  []byte(`{"schemaVersion":"donmai.terminal-workarea-lease-request.v1","schemaVersion":"donmai.terminal-workarea-lease-request.v1","settlementBudgetMs":977000,"safetyMarginMs":60000,"leaseDurationMs":1800000,"maxLeaseDurationMs":7200000}`),
		"unknown":            []byte(strings.Replace(valid, `"maxLeaseDurationMs":7200000`, `"maxLeaseDurationMs":7200000,"extra":0`, 1)),
		"trailing":           []byte(valid + `{}`),
		"fraction":           []byte(strings.Replace(valid, "977000", "977000.0", 1)),
		"exponent":           []byte(strings.Replace(valid, "977000", "977e3", 1)),
		"leading zero":       []byte(strings.Replace(valid, "977000", "0977000", 1)),
		"isolated surrogate": []byte(strings.Replace(valid, "donmai.terminal-workarea-lease-request.v1", `\ud800`, 1)),
		"reversed surrogate": []byte(strings.Replace(valid, "donmai.terminal-workarea-lease-request.v1", `\udc00\ud800`, 1)),
		"malformed utf8":     append([]byte(valid[:10]), append([]byte{0xff}, []byte(valid[10:])...)...),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var request TerminalLeaseRequest
			if err := json.Unmarshal(raw, &request); err == nil {
				t.Fatalf("invalid raw JSON accepted: %q", raw)
			}
		})
	}
}

func TestTerminalProtocolRejectsNoncanonicalScalars(t *testing.T) {
	t.Parallel()
	projection := fixtureDescriptor().Projection()
	valid, err := CanonicalBytes(projection)
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{
		strings.Replace(string(valid), testLeaseID, "twl_ABCDEF11111111111111111111111111", 1),
		strings.Replace(string(valid), "2026-07-18T12:30:00.123Z", "2026-07-18T12:30:00Z", 1),
		strings.Replace(string(valid), "2026-07-18T12:30:00.123Z", "2026-07-18T08:30:00.123-04:00", 1),
	}
	for _, raw := range cases {
		var got TerminalLeaseProjection
		if err := json.Unmarshal([]byte(raw), &got); err == nil {
			t.Fatalf("noncanonical scalar accepted: %s", raw)
		}
	}

	outbox := fixtureOutbox()
	outbox.BodyBase64 = strings.TrimRight(outbox.BodyBase64, "=")
	if err := outbox.Validate(); err == nil {
		t.Fatal("unpadded base64 accepted")
	}
	outbox = fixtureOutbox()
	outbox.BodySHA256 = strings.ToUpper(outbox.BodySHA256)
	if err := outbox.Validate(); err == nil {
		t.Fatal("uppercase digest accepted")
	}
}

type fixtureArtifact struct {
	ArtifactVersion string            `json:"artifactVersion"`
	Vectors         []fixtureVector   `json:"vectors"`
	Accepted        []fixtureAccepted `json:"accepted"`
	Invalid         []fixtureInvalid  `json:"invalid"`
}

type fixtureVector struct {
	Name            string `json:"name"`
	Schema          string `json:"schema"`
	CanonicalBase64 string `json:"canonicalBase64"`
	SHA256          string `json:"sha256"`
}

type fixtureAccepted struct {
	Name            string `json:"name"`
	Contract        string `json:"contract"`
	RawBase64       string `json:"rawBase64"`
	CanonicalBase64 string `json:"canonicalBase64"`
	SHA256          string `json:"sha256"`
}

type fixtureInvalid struct {
	Name      string `json:"name"`
	Contract  string `json:"contract"`
	RawBase64 string `json:"rawBase64"`
}

func TestTerminalProtocolFixtureArtifact(t *testing.T) {
	values := []struct {
		name   string
		schema string
		value  any
	}{
		{"lease-request", TerminalLeaseRequestSchemaV1, DefaultTerminalLeaseRequest()},
		{"lease-descriptor", TerminalLeaseSchemaV1, fixtureDescriptor()},
		{"lease-projection", "embedded-projection", fixtureDescriptor().Projection()},
		{"lease-claim", TerminalLeaseClaimSchemaV1, fixtureClaim()},
		{"lease-ack", TerminalLeaseAcknowledgementSchemaV1, fixtureAcknowledgement()},
		{"lease-ack-outcome", TerminalLeaseAckOutcomeSchemaV1, fixtureOutcome()},
		{"terminal-status-outbox", TerminalStatusOutboxSchemaV1, fixtureOutbox()},
		{"workarea-quarantine", TerminalWorkareaQuarantineSchemaV1, fixtureQuarantine()},
		{"workarea-quarantine-escaping", TerminalWorkareaQuarantineSchemaV1, fixtureEscapingQuarantine()},
	}
	artifact := fixtureArtifact{ArtifactVersion: "donmai.terminal-workarea-fixtures.v1"}
	for _, item := range values {
		canonical, err := CanonicalBytes(item.value)
		if err != nil {
			t.Fatalf("%s: %v", item.name, err)
		}
		digest := sha256.Sum256(canonical)
		artifact.Vectors = append(artifact.Vectors, fixtureVector{
			Name: item.name, Schema: item.schema,
			CanonicalBase64: base64.StdEncoding.EncodeToString(canonical),
			SHA256:          hex.EncodeToString(digest[:]),
		})
	}
	projectionCanonical, err := CanonicalBytes(fixtureDescriptor().Projection())
	if err != nil {
		t.Fatal(err)
	}
	escapingCanonical, err := CanonicalBytes(fixtureEscapingQuarantine())
	if err != nil {
		t.Fatal(err)
	}
	surrogatePairJSONEscape := []byte{'\\', 'u', 'd', '8', '3', 'd', '\\', 'u', 'd', 'e', '0', '0'}
	accepted := []struct {
		name      string
		contract  string
		raw       []byte
		target    any
		canonical []byte
	}{
		{
			name:     "projection-reordered-whitespace",
			contract: "embedded-projection",
			raw:      []byte(` { "expiresAt" : "2026-07-18T12:30:00.123Z", "terminalResultId":"tr_22222222222222222222222222222222", "workareaId":"wa_33333333333333333333333333333333", "leaseId":"twl_11111111111111111111111111111111" } `),
			target:   &TerminalLeaseProjection{}, canonical: projectionCanonical,
		},
		{
			name:     "quarantine-surrogate-pair-reencoding",
			contract: TerminalWorkareaQuarantineSchemaV1,
			raw:      bytes.Replace(escapingCanonical, []byte("😀"), surrogatePairJSONEscape, 1),
			target:   &TerminalWorkareaQuarantine{}, canonical: escapingCanonical,
		},
	}
	for _, item := range accepted {
		if err := json.Unmarshal(item.raw, item.target); err != nil {
			t.Fatalf("%s accepted raw: %v", item.name, err)
		}
		canonical, err := CanonicalBytes(item.target)
		if err != nil {
			t.Fatalf("%s canonical: %v", item.name, err)
		}
		if !bytes.Equal(canonical, item.canonical) {
			t.Fatalf("%s canonical mismatch: %s", item.name, canonical)
		}
		digest := sha256.Sum256(canonical)
		artifact.Accepted = append(artifact.Accepted, fixtureAccepted{
			Name: item.name, Contract: item.contract,
			RawBase64:       base64.StdEncoding.EncodeToString(item.raw),
			CanonicalBase64: base64.StdEncoding.EncodeToString(canonical),
			SHA256:          hex.EncodeToString(digest[:]),
		})
	}

	requestCanonical, err := CanonicalBytes(DefaultTerminalLeaseRequest())
	if err != nil {
		t.Fatal(err)
	}
	outbox := fixtureOutbox()
	outboxCanonical, err := CanonicalBytes(outbox)
	if err != nil {
		t.Fatal(err)
	}
	escapedLeaseIDKey := []byte{'"', '\\', 'u', '0', '0', '6', 'c', 'e', 'a', 's', 'e', 'I', 'd', '"'}
	invalid := map[string][]byte{
		"duplicate-escape-equivalent-key": bytes.Replace(
			[]byte(`{"leaseId":"twl_11111111111111111111111111111111","leaseId":"twl_11111111111111111111111111111111"}`),
			[]byte(`"leaseId"`), escapedLeaseIDKey, 1,
		),
		"isolated-surrogate":      []byte(`{"value":"\ud800"}`),
		"malformed-utf8":          {0x7b, 0x22, 0xff, 0x22, 0x3a, 0x30, 0x7d},
		"noncanonical-base64":     bytes.Replace(outboxCanonical, []byte(outbox.BodyBase64), []byte(strings.TrimRight(outbox.BodyBase64, "=")), 1),
		"noncanonical-digest":     bytes.Replace(outboxCanonical, []byte(outbox.BodySHA256), []byte(strings.ToUpper(outbox.BodySHA256)), 1),
		"noncanonical-identifier": bytes.Replace(projectionCanonical, []byte(testLeaseID), []byte("twl_ABCDEF11111111111111111111111111"), 1),
		"noncanonical-timestamp":  bytes.Replace(projectionCanonical, []byte("2026-07-18T12:30:00.123Z"), []byte("2026-07-18T12:30:00Z"), 1),
		"reversed-surrogate":      []byte(`{"value":"\udc00\ud800"}`),
		"trailing-value":          []byte(`{} {}`),
		"unknown-field":           bytes.Replace(requestCanonical, []byte("}"), []byte(`,"extra":0}`), 1),
	}
	names := make([]string, 0, len(invalid))
	for name := range invalid {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		contract := invalidFixtureContract(name)
		if err := decodeInvalidFixture(contract, invalid[name]); err == nil {
			t.Fatalf("invalid fixture %s was accepted by %s", name, contract)
		}
		artifact.Invalid = append(artifact.Invalid, fixtureInvalid{
			Name: name, Contract: contract, RawBase64: base64.StdEncoding.EncodeToString(invalid[name]),
		})
	}
	encoded, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	path := filepath.Join("testdata", "terminal_workarea_fixtures.json")
	if os.Getenv("UPDATE_TERMINAL_FIXTURES") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture artifact (regenerate with UPDATE_TERMINAL_FIXTURES=1): %v", err)
	}
	if string(golden) != string(encoded) {
		t.Fatal("fixture artifact drift; run UPDATE_TERMINAL_FIXTURES=1 go test ./runtime/workarea -run TestTerminalProtocolFixtureArtifact")
	}
}

func invalidFixtureContract(name string) string {
	switch name {
	case "noncanonical-base64", "noncanonical-digest":
		return TerminalStatusOutboxSchemaV1
	case "noncanonical-identifier", "noncanonical-timestamp":
		return "embedded-projection"
	case "unknown-field":
		return TerminalLeaseRequestSchemaV1
	default:
		return "canonical-json-object"
	}
}

func decodeInvalidFixture(contract string, raw []byte) error {
	switch contract {
	case TerminalLeaseRequestSchemaV1:
		var value TerminalLeaseRequest
		return json.Unmarshal(raw, &value)
	case TerminalStatusOutboxSchemaV1:
		var value TerminalStatusOutbox
		return json.Unmarshal(raw, &value)
	case "embedded-projection":
		var value TerminalLeaseProjection
		return json.Unmarshal(raw, &value)
	case "canonical-json-object":
		_, err := parseCanonicalJSONObject(raw)
		return err
	default:
		return nil
	}
}

func fixtureDescriptor() TerminalLeaseDescriptor {
	return TerminalLeaseDescriptor{
		SchemaVersion: TerminalLeaseSchemaV1, LeaseID: testLeaseID, SessionID: testSessionID,
		TerminalResultID: testResultID, WorkareaID: testWorkareaID, AcquiredAt: testNow,
		ExpiresAt: testNow.Add(DefaultLeaseDuration), SettlementBudgetMS: SettlementBudgetMS,
	}
}

func fixtureClaim() LeaseExecutionClaim {
	return LeaseExecutionClaim{
		SchemaVersion: TerminalLeaseClaimSchemaV1, InvocationID: testInvocationID, ClaimID: testClaimID,
		LeaseID: testLeaseID, SessionID: testSessionID, TerminalResultID: testResultID,
		WorkareaID: testWorkareaID, ClaimedAt: testNow,
	}
}

func fixtureAcknowledgement() TerminalResultAcknowledgement {
	return TerminalResultAcknowledgement{
		SchemaVersion: TerminalLeaseAcknowledgementSchemaV1, Acknowledged: true,
		InvocationID: testInvocationID, ClaimID: testClaimID, LeaseID: testLeaseID,
		SessionID: testSessionID, TerminalResultID: testResultID, WorkareaID: testWorkareaID,
	}
}

func fixtureOutcome() TerminalAcknowledgementOutcome {
	return TerminalAcknowledgementOutcome{
		SchemaVersion: TerminalLeaseAckOutcomeSchemaV1, Outcome: AcknowledgementApplied,
		LeaseID: testLeaseID, TerminalResultID: testResultID, LeaseState: LeaseReleasePending,
	}
}

func fixtureOutbox() TerminalStatusOutbox {
	return NewTerminalStatusOutbox(testResultID, testReceiverKey, []byte(`{"status":"completed","terminalWorkareaLease":{"leaseId":"twl_11111111111111111111111111111111"}}`), testNow.Add(DefaultLeaseDuration), testNow)
}

func fixtureQuarantine() TerminalWorkareaQuarantine {
	path := filepath.Join(string(filepath.Separator), "tmp", "workarea")
	digest := sha256.Sum256([]byte(path))
	return TerminalWorkareaQuarantine{
		SchemaVersion: TerminalWorkareaQuarantineSchemaV1, QuarantineID: testQuarantineID,
		WorkareaID: testWorkareaID, SessionID: testSessionID, TerminalResultID: testResultID,
		WorkareaPath: path, PathSHA256: hex.EncodeToString(digest[:]), Reason: "lease-acquisition-failed",
		State: QuarantineGuarded, CreatedAt: testNow, UpdatedAt: testNow,
	}
}

func fixtureEscapingQuarantine() TerminalWorkareaQuarantine {
	path := filepath.Join(string(filepath.Separator), "tmp", "<&>", "雪")
	digest := sha256.Sum256([]byte(path))
	lastError := "quote=\" reverse=\\ slash=/ controls=\b\t\n\f\r <>& 雪 😀    "
	return TerminalWorkareaQuarantine{
		SchemaVersion:    TerminalWorkareaQuarantineSchemaV1,
		QuarantineID:     "twq_66666666666666666666666666666666",
		WorkareaID:       testWorkareaID,
		SessionID:        testSessionID,
		TerminalResultID: testResultID,
		WorkareaPath:     path, PathSHA256: hex.EncodeToString(digest[:]), Reason: "lease-acquisition-failed",
		State: QuarantineQuarantined, CreatedAt: testNow, UpdatedAt: testNow, LastError: &lastError,
	}
}
