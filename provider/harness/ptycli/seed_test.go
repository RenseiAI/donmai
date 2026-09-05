package ptycli

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

type seedTestSession struct {
	agent.InteractiveSession
	mu       sync.Mutex
	bytes    []byte
	maxWrite int
	err      error
	started  chan struct{}
	release  chan struct{}
}

func (s *seedTestSession) WriteInput(p []byte) (int, error) {
	if s.started != nil {
		select {
		case <-s.started:
		default:
			close(s.started)
		}
		<-s.release
	}
	limit := len(p)
	if s.maxWrite > 0 && limit > s.maxWrite {
		limit = s.maxWrite
	}
	s.mu.Lock()
	s.bytes = append(s.bytes, p[:limit]...)
	s.mu.Unlock()
	return limit, s.err
}

type seedTestHandle struct {
	events  chan agent.Event
	stops   atomic.Int64
	release chan struct{}
}

func (h *seedTestHandle) SessionID() string                    { return "" }
func (h *seedTestHandle) Events() <-chan agent.Event           { return h.events }
func (h *seedTestHandle) Inject(context.Context, string) error { return agent.ErrUnsupported }
func (h *seedTestHandle) Stop(context.Context) error {
	h.stops.Add(1)
	if h.release != nil {
		select {
		case <-h.release:
		default:
			close(h.release)
		}
	}
	return nil
}

func TestDeliverSeed_ExactBytesAndShortWrites(t *testing.T) {
	t.Parallel()
	session := &seedTestSession{maxWrite: 2}
	handle := &seedTestHandle{events: make(chan agent.Event)}
	if err := DeliverSeed(context.Background(), handle, session, "  雪\nnext  "); err != nil {
		t.Fatalf("DeliverSeed: %v", err)
	}
	session.mu.Lock()
	got := string(session.bytes)
	session.mu.Unlock()
	if want := "  雪\nnext  \n"; got != want {
		t.Fatalf("PTY seed bytes = %q, want %q", got, want)
	}
	if got := handle.stops.Load(); got != 0 {
		t.Fatalf("Stop calls = %d, want 0", got)
	}
}

func TestDeliverSeed_PartialErrorStopsAndFails(t *testing.T) {
	t.Parallel()
	session := &seedTestSession{maxWrite: 2, err: errors.New("closed")}
	handle := &seedTestHandle{events: make(chan agent.Event)}
	err := DeliverSeed(context.Background(), handle, session, "seed")
	if err == nil || !errors.Is(err, session.err) {
		t.Fatalf("DeliverSeed error = %v, want closed", err)
	}
	if got := handle.stops.Load(); got != 1 {
		t.Fatalf("Stop calls = %d, want 1", got)
	}
}

func TestDeliverSeed_CancellationStopsBlockedWrite(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	session := &seedTestSession{started: started, release: release}
	handle := &seedTestHandle{events: make(chan agent.Event), release: release}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- DeliverSeed(ctx, handle, session, "seed") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("seed write did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DeliverSeed error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not unblock seed delivery")
	}
	if got := handle.stops.Load(); got != 1 {
		t.Fatalf("Stop calls = %d, want 1", got)
	}
}

// TestDenyPromptReceiptAfterSeedFailure covers the shared retraction directly,
// now that more than one harness depends on it.
func TestDenyPromptReceiptAfterSeedFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		spec     func(*[]agent.PromptDeliveryReceipt) agent.Spec
		wantEmit bool
		check    func(*testing.T, agent.PromptDeliveryReceipt)
	}{
		{
			name: "no hook is a no-op",
			spec: func(*[]agent.PromptDeliveryReceipt) agent.Spec {
				return agent.Spec{PromptReceipt: &agent.PromptDeliveryReceipt{Decision: "ready"}}
			},
		},
		{
			name: "no receipt is a no-op",
			spec: func(got *[]agent.PromptDeliveryReceipt) agent.Spec {
				return agent.Spec{OnPromptAdapted: func(r agent.PromptDeliveryReceipt) error {
					*got = append(*got, r)
					return nil
				}}
			},
		},
		{
			name: "delivered and downgraded entries are retracted",
			spec: func(got *[]agent.PromptDeliveryReceipt) agent.Spec {
				return agent.Spec{
					PromptReceipt: &agent.PromptDeliveryReceipt{
						Decision: "ready",
						Entries: []agent.PromptDeliveryEntry{
							{ID: "user", Outcome: agent.PromptOutcomeDelivered, Delivery: agent.PromptDeliveryShellPTYSeed},
							{ID: "protocol", Outcome: agent.PromptOutcomeDowngraded, DowngradeAuthID: "auth-1"},
						},
					},
					OnPromptAdapted: func(r agent.PromptDeliveryReceipt) error {
						*got = append(*got, r)
						return nil
					},
				}
			},
			wantEmit: true,
			check: func(t *testing.T, r agent.PromptDeliveryReceipt) {
				if r.Decision != "denied" {
					t.Errorf("Decision = %q, want denied", r.Decision)
				}
				for _, entry := range r.Entries {
					if entry.Outcome != agent.PromptOutcomeDenied ||
						entry.DenialCode != agent.PromptDenialApplicationFailed {
						t.Errorf("entry %q = %+v, want an application_failed denial", entry.ID, entry)
					}
					if entry.Delivery != "" || entry.DowngradeAuthID != "" {
						t.Errorf("entry %q still names a delivery/authorization: %+v", entry.ID, entry)
					}
				}
			},
		},
		{
			// An entry already denied at adaptation time was never in flight;
			// rewriting it would blame the seed for a decision made before it ran.
			name: "an already-denied entry keeps its original denial code",
			spec: func(got *[]agent.PromptDeliveryReceipt) agent.Spec {
				return agent.Spec{
					PromptReceipt: &agent.PromptDeliveryReceipt{
						Decision: "ready",
						Entries: []agent.PromptDeliveryEntry{
							{ID: "ctx", Outcome: agent.PromptOutcomeDenied, DenialCode: agent.PromptDenialDeliveryUnsupported},
						},
					},
					OnPromptAdapted: func(r agent.PromptDeliveryReceipt) error {
						*got = append(*got, r)
						return nil
					},
				}
			},
			wantEmit: true,
			check: func(t *testing.T, r agent.PromptDeliveryReceipt) {
				if got := r.Entries[0].DenialCode; got != agent.PromptDenialDeliveryUnsupported {
					t.Errorf("DenialCode = %q, want the original %q", got, agent.PromptDenialDeliveryUnsupported)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []agent.PromptDeliveryReceipt
			if err := DenyPromptReceiptAfterSeedFailure(tc.spec(&got)); err != nil {
				t.Fatalf("DenyPromptReceiptAfterSeedFailure: %v", err)
			}
			if !tc.wantEmit {
				if len(got) != 0 {
					t.Fatalf("emitted %d receipts, want none", len(got))
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("emitted %d receipts, want 1", len(got))
			}
			tc.check(t, got[0])
		})
	}
}

// TestDenyPromptReceiptAfterSeedFailureDoesNotMutateTheOriginal pins the copy:
// the retraction must not rewrite the receipt the adapted Spec still points at.
func TestDenyPromptReceiptAfterSeedFailureDoesNotMutateTheOriginal(t *testing.T) {
	t.Parallel()

	original := agent.PromptDeliveryReceipt{
		Decision: "ready",
		Entries:  []agent.PromptDeliveryEntry{{ID: "user", Outcome: agent.PromptOutcomeDelivered}},
	}
	spec := agent.Spec{
		PromptReceipt:   &original,
		OnPromptAdapted: func(agent.PromptDeliveryReceipt) error { return nil },
	}
	if err := DenyPromptReceiptAfterSeedFailure(spec); err != nil {
		t.Fatalf("DenyPromptReceiptAfterSeedFailure: %v", err)
	}
	if original.Decision != "ready" || original.Entries[0].Outcome != agent.PromptOutcomeDelivered {
		t.Errorf("the original receipt was mutated: %+v", original)
	}
}
