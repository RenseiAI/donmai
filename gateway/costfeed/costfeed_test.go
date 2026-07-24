package costfeed

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJSONLLedger_AppendsRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway", "cost-events.jsonl")
	l, err := NewJSONLLedger(path)
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	events := []Event{
		{DispatchID: "d1", ProviderID: "openai", Harness: "opencode", Model: "gpt-4o", TokensIn: 10, TokensOut: 5},
		{DispatchID: "d2", ProviderID: "openai", Harness: "opencode", Model: "gpt-4o", TokensIn: 3, TokensOut: 1},
	}
	for _, ev := range events {
		if err := l.Record(ev); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	defer func() { _ = f.Close() }()
	var lines int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var got Event
		if err := json.Unmarshal(sc.Bytes(), &got); err != nil {
			t.Fatalf("bad json line: %v", err)
		}
		if got.Host != Host {
			t.Errorf("ledger row host = %q, want gateway (defaulted)", got.Host)
		}
		if got.EmittedAt.IsZero() {
			t.Error("ledger row emittedAt not stamped")
		}
		lines++
	}
	if lines != 2 {
		t.Fatalf("ledger had %d lines, want 2", lines)
	}
}

func TestJSONLLedger_FileMode0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cost-events.jsonl")
	l, _ := NewJSONLLedger(path)
	if err := l.Record(Event{DispatchID: "d"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("ledger mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestDefaultLedgerPath(t *testing.T) {
	got := DefaultLedgerPath("/home/x/.donmai")
	want := filepath.Join("/home/x/.donmai", "gateway", "cost-events.jsonl")
	if got != want {
		t.Errorf("DefaultLedgerPath = %q, want %q", got, want)
	}
}

func TestMemorySink_RetainsOrder(t *testing.T) {
	m := &MemorySink{}
	for i, id := range []string{"a", "b", "c"} {
		if err := m.Record(Event{DispatchID: id, EmittedAt: time.Unix(int64(i), 0)}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	evs := m.Events()
	got := make([]string, len(evs))
	for i, e := range evs {
		got[i] = e.DispatchID
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("order = %v, want a,b,c", got)
	}
	// Host defaulted on the memory sink too.
	if evs[0].Host != Host {
		t.Errorf("memory sink host = %q, want gateway", evs[0].Host)
	}
}
