package executionevent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSourceBatchContractIsStrictAndContiguous(t *testing.T) {
	first, err := NewRecord("session_1", 1, time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC), "tool.called", map[string]any{
		"toolName": "Read",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewRecord("session_1", 2, firstObserved(first), "error.raised", map[string]any{
		"message": "provider stopped",
	})
	if err != nil {
		t.Fatal(err)
	}
	batch := Batch{Version: BatchVersion, SessionID: "session_1", Records: []Record{first, second}}
	if err := ValidateBatch(batch); err != nil {
		t.Fatalf("ValidateBatch: %v", err)
	}
	if _, err := MarshalCompact(batch); err != nil {
		t.Fatal(err)
	}
	if got := string(mustCompact(batch)); got[0] != '{' || got[len(got)-1] != '}' {
		t.Fatalf("unexpected compact JSON: %s", got)
	}

	second.StructuredSeq = 4
	if err := ValidateBatch(Batch{Version: BatchVersion, SessionID: "session_1", Records: []Record{first, second}}); err == nil {
		t.Fatal("expected non-contiguous sequence to fail")
	}
	if err := ValidateBatch(Batch{Version: BatchVersion, SessionID: "session_1", Records: []Record{{Version: RecordVersion, StructuredSeq: 1, EventID: "evt_bad"}}}); err == nil {
		t.Fatal("expected malformed record to fail")
	}
}

func firstObserved(r Record) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, r.ObservedAt)
	if err != nil {
		panic(err)
	}
	return parsed
}

func mustCompact(v any) []byte {
	b, err := MarshalCompact(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestCompactJSONAndExactRecordBatchCaps(t *testing.T) {
	compact, err := MarshalCompact(map[string]string{"text": "<safe>"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(compact, []byte(`\u003c`)) {
		t.Fatalf("SetEscapeHTML(false) not honored: %s", compact)
	}
	low, high := 0, MaxRecordBytes
	for low < high {
		mid := (low + high + 1) / 2
		candidate := testCapRecord(t, strings.Repeat("x", mid))
		if len(mustCompact(candidate)) <= MaxRecordBytes {
			low = mid
		} else {
			high = mid - 1
		}
	}
	fit := testCapRecord(t, strings.Repeat("x", low))
	if err := ValidateRecord("session_cap", fit, 0); err != nil {
		t.Fatalf("record at cap boundary rejected: %v", err)
	}
	over := testCapRecord(t, strings.Repeat("x", low+1))
	if err := ValidateRecord("session_cap", over, 0); err == nil {
		t.Fatal("record over exact byte cap accepted")
	}
	first := testCapRecord(t, 1)
	second := testCapRecordSeq(t, 2, 2)
	if err := ValidateBatch(Batch{Version: BatchVersion, SessionID: "session_cap", Records: []Record{first, second}}); err != nil {
		t.Fatalf("batch under cap rejected: %v", err)
	}
	if len(mustCompact(Batch{Version: BatchVersion, SessionID: "session_cap", Records: []Record{first, second}})) > MaxBatchBytes {
		t.Fatal("small batch unexpectedly exceeds cap")
	}
}

func testCapRecord(t *testing.T, value any) Record {
	return testCapRecordSeq(t, 1, value)
}

func testCapRecordSeq(t *testing.T, seq uint64, value any) Record {
	t.Helper()
	return Record{Version: RecordVersion, EventID: RuntimeSourceEventID("session_cap", seq), StructuredSeq: seq, ObservedAt: "2026-08-27T12:00:00Z", EventType: "tool.called", PersistencePolicy: "durable", Evidence: Evidence{Kind: "native"}, Payload: map[string]any{"toolName": value}}
}

func TestJournalReopenFailsClosedForMalformedOrOutOfOrderRows(t *testing.T) {
	for name, content := range map[string]string{
		"malformed":    "{not-json}\n",
		"out-of-order": "{\"version\":\"" + RecordVersion + "\",\"eventId\":\"evt_bad\",\"structuredSeq\":2,\"observedAt\":\"2026-08-27T12:00:00Z\",\"eventType\":\"tool.called\",\"persistencePolicy\":\"durable\",\"evidence\":{\"kind\":\"native\"},\"payload\":{\"toolName\":\"Read\"}}\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, journalFileName), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenJournal(dir, "session_cap"); err == nil {
				t.Fatal("malformed/out-of-order journal reopened successfully")
			}
		})
	}
}
