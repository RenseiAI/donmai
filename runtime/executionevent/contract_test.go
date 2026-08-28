package executionevent

import (
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
