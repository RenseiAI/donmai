// Package costfeed meters every gateway exchange into a cost record shaped for
// the platform cost_events ledger (08 §7), carrying the `harness` sibling
// column ADR-2026-06-06 D4 planned — the gateway is its first structurally-
// reliable producer, since it sees both the wire traffic and the session token
// that names the harness.
//
// OSS ships a WORKING sink: a local JSONL ledger under the state home. Platform
// rides the same Event shape through a Poster (interface here, implementation
// downstream) that ships rows to the hosted ingest — never in this package.
package costfeed

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Host is the constant serving-host stamped on every gateway cost row — the
// routing fact D4 wants visible while attribution stays company-primary.
const Host = "gateway"

// Event is one metered exchange, shaped for the cost_events schema (primary
// attribution key = ProviderID (company) + Host; Harness is the sibling
// column). Values never carry a secret.
type Event struct {
	DispatchID      string    `json:"dispatchId"`
	SessionID       string    `json:"sessionId,omitempty"`
	ProviderID      string    `json:"providerId"` // model-endpoint company (primary attribution)
	Host            string    `json:"host"`       // always "gateway"
	Harness         string    `json:"harness"`    // sibling attribution column
	AuthMode        string    `json:"authMode"`   // byok | metered | host-session
	Model           string    `json:"model"`
	TokensIn        int       `json:"tokensIn"`
	TokensOut       int       `json:"tokensOut"`
	ReasoningTokens int       `json:"reasoningTokens,omitempty"`
	RawCostUSD      float64   `json:"rawCostUsd"`
	EmittedAt       time.Time `json:"emittedAt"`
}

// Sink accepts metered events. Record must be safe for concurrent callers and
// must not block the request hot path meaningfully.
type Sink interface {
	Record(ev Event) error
}

// Poster is the platform seam: a downstream implementation ships rows to the
// hosted cost ingest. Defined here (OSS) so the gateway depends only on the
// interface; no poster implementation ever lands in this repo (same pattern as
// the span poster, ADR-2026-06-28).
type Poster interface {
	Post(ev Event) error
}

// JSONLLedger is the OSS sink: append-only JSON Lines under the state home
// (default ~/.donmai/gateway/cost-events.jsonl). Concurrency-safe via a mutex;
// each Record is one line flushed to disk.
type JSONLLedger struct {
	path string
	mu   sync.Mutex
}

// NewJSONLLedger opens (creating parent dirs) an append-only ledger at path.
func NewJSONLLedger(path string) (*JSONLLedger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("gateway/costfeed: ledger dir: %w", err)
	}
	return &JSONLLedger{path: path}, nil
}

// DefaultLedgerPath returns the canonical ledger path under stateHome
// (~/.donmai/gateway/cost-events.jsonl). stateHome is the daemon's state home
// (e.g. ~/.donmai); callers pass it explicitly so this package never resolves
// the home itself.
func DefaultLedgerPath(stateHome string) string {
	return filepath.Join(stateHome, "gateway", "cost-events.jsonl")
}

// Record implements Sink. It appends one JSON line, stamping EmittedAt when the
// caller left it zero.
func (l *JSONLLedger) Record(ev Event) error {
	if ev.EmittedAt.IsZero() {
		ev.EmittedAt = time.Now().UTC()
	}
	if ev.Host == "" {
		ev.Host = Host
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("gateway/costfeed: marshal event: %w", err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("gateway/costfeed: open ledger: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("gateway/costfeed: write event: %w", err)
	}
	return nil
}

// Path returns the ledger file path (for status surfaces / tests).
func (l *JSONLLedger) Path() string { return l.path }

// MemorySink is an in-memory Sink for tests and for a daemon started without a
// writable state home. It retains events in order.
type MemorySink struct {
	mu     sync.Mutex
	events []Event
}

// Record implements Sink.
func (m *MemorySink) Record(ev Event) error {
	if ev.EmittedAt.IsZero() {
		ev.EmittedAt = time.Now().UTC()
	}
	if ev.Host == "" {
		ev.Host = Host
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, ev)
	return nil
}

// Events returns a copy of the recorded events.
func (m *MemorySink) Events() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Event, len(m.events))
	copy(out, m.events)
	return out
}
