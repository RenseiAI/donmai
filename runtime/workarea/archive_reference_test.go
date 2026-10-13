package workarea

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var archiveReferenceNow = time.Date(2026, 7, 18, 12, 0, 0, 123_000_000, time.UTC)

func fixtureArchiveReference() TerminalArchiveReference {
	return TerminalArchiveReference{
		SchemaVersion:    TerminalArchiveReferenceSchemaV1,
		ArchiveID:        testWorkareaID,
		ArchiveDigest:    "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		SessionID:        testSessionID,
		WorkareaID:       testWorkareaID,
		TerminalResultID: testResultID,
		CommittedAt:      archiveReferenceNow,
	}
}

func TestTerminalArchiveReferenceUsesExactCanonicalBytes(t *testing.T) {
	t.Parallel()
	got, err := CanonicalBytes(fixtureArchiveReference())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schemaVersion":"donmai.terminal-archive-reference.v1","archiveId":"wa_33333333333333333333333333333333","archiveDigest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","sessionId":"11111111-1111-4111-8111-111111111111","workareaId":"wa_33333333333333333333333333333333","terminalResultId":"tr_22222222222222222222222222222222","committedAt":"2026-07-18T12:00:00.123Z"}`
	if string(got) != want {
		t.Fatalf("canonical archive reference = %s", got)
	}
}

func TestTerminalArchiveReferenceClosedDecoder(t *testing.T) {
	t.Parallel()
	raw := []byte(` { "committedAt" : "2026-07-18T12:00:00.123Z", "terminalResultId":"tr_22222222222222222222222222222222", "workareaId":"wa_33333333333333333333333333333333", "sessionId":"11111111-1111-4111-8111-111111111111", "archiveDigest":"sha256:1111111111111111111111111111111111111111111111111111111111111111", "archiveId":"wa_33333333333333333333333333333333", "schemaVersion":"donmai.terminal-archive-reference.v1" } `)
	var decoded TerminalArchiveReference
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	reencoded, err := CanonicalBytes(decoded)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schemaVersion":"donmai.terminal-archive-reference.v1","archiveId":"wa_33333333333333333333333333333333","archiveDigest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","sessionId":"11111111-1111-4111-8111-111111111111","workareaId":"wa_33333333333333333333333333333333","terminalResultId":"tr_22222222222222222222222222222222","committedAt":"2026-07-18T12:00:00.123Z"}`
	if string(reencoded) != want {
		t.Fatalf("re-encoded archive reference = %s", reencoded)
	}
}

func TestTerminalArchiveReferenceRejectsBadShapes(t *testing.T) {
	t.Parallel()
	valid, err := CanonicalBytes(fixtureArchiveReference())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"unknown field":     []byte(strings.Replace(string(valid), "}", `,"extra":0}`, 1)),
		"missing digest":    []byte(strings.Replace(string(valid), `"archiveDigest":"sha256:1111111111111111111111111111111111111111111111111111111111111111",`, "", 1)),
		"bad digest":        []byte(strings.Replace(string(valid), "sha256:1111", "sha256:XXXX", 1)),
		"relative archive":  []byte(strings.Replace(string(valid), `"archiveId":"wa_`, `"archiveId":"../wa_`, 1)),
		"bad session":       []byte(strings.Replace(string(valid), testSessionID, "not-a-uuid", 1)),
		"bad timestamp":     []byte(strings.Replace(string(valid), "2026-07-18T12:00:00.123Z", "2026-07-18T12:00:00Z", 1)),
		"wrong schema":      []byte(strings.Replace(string(valid), TerminalArchiveReferenceSchemaV1, "donmai.terminal-archive-reference.v0", 1)),
		"duplicate member":  []byte(strings.Replace(string(valid), `"archiveId"`, `"archiveId","archiveId"`, 1)),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var decoded TerminalArchiveReference
			if err := json.Unmarshal(raw, &decoded); err == nil {
				t.Fatalf("invalid archive reference accepted: %s", raw)
			}
		})
	}
}

func TestTerminalArchiveReferenceIdentityEquality(t *testing.T) {
	t.Parallel()
	first := fixtureArchiveReference()
	second := fixtureArchiveReference()
	second.CommittedAt = second.CommittedAt.Add(time.Second)
	if !first.IdentityEqual(second) {
		t.Fatal("commit instant must not change archive identity")
	}
	second.ArchiveDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	if first.IdentityEqual(second) {
		t.Fatal("conflicting digest compared equal")
	}
}

func TestTerminalLeaseProjectionStaysFourFields(t *testing.T) {
	t.Parallel()
	projection := fixtureDescriptor().Projection()
	raw, err := CanonicalBytes(projection)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 4 {
		t.Fatalf("terminal lease projection changed: %s", raw)
	}
	for _, field := range []string{"leaseId", "workareaId", "terminalResultId", "expiresAt"} {
		if decoded[field] == nil {
			t.Fatalf("terminal lease projection lost %s: %s", field, raw)
		}
	}
}
