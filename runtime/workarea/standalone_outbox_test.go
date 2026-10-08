package workarea

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// testOutboxStore opens a LeaseStore with a fixed clock for standalone-outbox
// tests. The store doubles as the standalone outbox authority.
func standaloneOutboxStore(t *testing.T) (*LeaseStore, *standaloneManualClock) {
	t.Helper()
	clock := &standaloneManualClock{now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	store, err := NewLeaseStore(StoreOptions{Dir: t.TempDir(), Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	return store, clock
}

type standaloneManualClock struct {
	now time.Time
}

func (c *standaloneManualClock) Now() time.Time { return c.now }

func standaloneOutboxSpec(sessionID string, attempt uint64, digest, receiver string, body []byte, deadline time.Time) StandaloneOutboxSaveSpec {
	return StandaloneOutboxSaveSpec{
		SessionID: sessionID, Attempt: attempt, TerminalResultID: digest,
		ReceiverKey: receiver, Body: body, DeadlineAt: deadline,
		Kind: StandaloneOutboxTerminal,
	}
}

func standaloneReceiverKey(t *testing.T) string {
	t.Helper()
	key, err := NewGeneratedID("rcv_")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func standaloneTerminalDigest() string {
	sum := sha256.Sum256([]byte("terminal-body"))
	return "tr_" + hex.EncodeToString(sum[:16])
}

func TestStandaloneOutboxKeyIsSessionAndAttemptBound(t *testing.T) {
	t.Parallel()
	session := "11111111-1111-4111-8111-111111111111"
	first := StandaloneOutboxKey(session, 1)
	again := StandaloneOutboxKey(session, 1)
	otherAttempt := StandaloneOutboxKey(session, 2)
	otherSession := StandaloneOutboxKey("22222222-2222-4222-8222-222222222222", 1)
	if first != again {
		t.Fatal("same (session, attempt) produced different keys")
	}
	if first == otherAttempt || first == otherSession || otherAttempt == otherSession {
		t.Fatal("distinct (session, attempt) pairs share an outbox key")
	}
}

func TestSaveStandaloneOutboxPersistsExactBytes(t *testing.T) {
	t.Parallel()
	store, clock := standaloneOutboxStore(t)
	session := "11111111-1111-4111-8111-111111111111"
	receiver := standaloneReceiverKey(t)
	body := []byte(`{"workerId":"worker-1","status":"failed","failureMode":"silent-exit"}`)
	deadline := clock.now.Add(MaximumLeaseDuration)
	key, err := store.SaveStandaloneOutbox(context.Background(), standaloneOutboxSpec(session, 3, standaloneTerminalDigest(), receiver, body, deadline))
	if err != nil {
		t.Fatal(err)
	}
	if key != StandaloneOutboxKey(session, 3) {
		t.Fatalf("key=%q, want %q", key, StandaloneOutboxKey(session, 3))
	}
	loaded, err := store.LoadStandaloneOutbox(context.Background(), session, 3)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Kind != StandaloneOutboxTerminal || loaded.SessionID != session || loaded.Attempt != 3 {
		t.Fatalf("envelope=%+v", loaded)
	}
	retained, err := loaded.Record.Body()
	if err != nil || !bytes.Equal(retained, body) {
		t.Fatalf("retained=%q err=%v", retained, err)
	}
	if loaded.Record.ReceiverKey != receiver || loaded.Record.TerminalResultID != standaloneTerminalDigest() {
		t.Fatalf("record=%+v", loaded.Record)
	}
	if loaded.Record.DeliveryState != TerminalStatusPending {
		t.Fatalf("delivery=%q, want pending", loaded.Record.DeliveryState)
	}
}

func TestSaveStandaloneOutboxIsFirstWriterWins(t *testing.T) {
	t.Parallel()
	store, clock := standaloneOutboxStore(t)
	session := "11111111-1111-4111-8111-111111111111"
	receiver := standaloneReceiverKey(t)
	deadline := clock.now.Add(MaximumLeaseDuration)
	runnerBody := []byte(`{"workerId":"worker-1","status":"completed","summary":"done"}`)
	runnerDigest := standaloneTerminalDigest()
	if _, err := store.SaveStandaloneOutbox(context.Background(), standaloneOutboxSpec(session, 1, runnerDigest, receiver, runnerBody, deadline)); err != nil {
		t.Fatal(err)
	}
	// An identical runner retry is idempotent.
	if _, err := store.SaveStandaloneOutbox(context.Background(), standaloneOutboxSpec(session, 1, runnerDigest, receiver, runnerBody, deadline)); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	// The fallback record for the same key never coexists with the runner
	// record: the second writer loses.
	fallback := standaloneOutboxSpec(session, 1, runnerDigest, receiver, []byte(`{"workerId":"worker-1","status":"failed","failureMode":"worker_exited_without_result"}`), deadline)
	fallback.Kind = StandaloneOutboxWorkerExited
	fallback.GroupReaped = true
	if _, err := store.SaveStandaloneOutbox(context.Background(), fallback); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("fallback after runner record = %v, want conflict", err)
	}
	loaded, err := store.LoadStandaloneOutbox(context.Background(), session, 1)
	if err != nil {
		t.Fatal(err)
	}
	retained, _ := loaded.Record.Body()
	if !bytes.Equal(retained, runnerBody) || loaded.Kind != StandaloneOutboxTerminal {
		t.Fatalf("winner changed: kind=%q body=%q", loaded.Kind, retained)
	}
}

func TestFallbackWinsWhenRunnerNeverPersisted(t *testing.T) {
	t.Parallel()
	store, clock := standaloneOutboxStore(t)
	session := "11111111-1111-4111-8111-111111111111"
	receiver := standaloneReceiverKey(t)
	deadline := clock.now.Add(MaximumLeaseDuration)
	fallbackBody := []byte(`{"workerId":"worker-1","status":"failed","failureMode":"worker_exited_without_result"}`)
	fallbackDigest := func() string {
		sum := sha256.Sum256(fallbackBody)
		return "tr_" + hex.EncodeToString(sum[:16])
	}()
	fallback := standaloneOutboxSpec(session, 1, fallbackDigest, receiver, fallbackBody, deadline)
	fallback.Kind = StandaloneOutboxWorkerExited
	fallback.GroupReaped = true
	if _, err := store.SaveStandaloneOutbox(context.Background(), fallback); err != nil {
		t.Fatal(err)
	}
	// A late runner body for the same key loses: the records never both exist.
	late := standaloneOutboxSpec(session, 1, standaloneTerminalDigest(), receiver, []byte(`{"workerId":"worker-1","status":"completed"}`), deadline)
	if _, err := store.SaveStandaloneOutbox(context.Background(), late); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("late runner after fallback = %v, want conflict", err)
	}
}

func TestFallbackRequiresProofTheGroupIsGone(t *testing.T) {
	t.Parallel()
	store, clock := standaloneOutboxStore(t)
	session := "11111111-1111-4111-8111-111111111111"
	receiver := standaloneReceiverKey(t)
	deadline := clock.now.Add(MaximumLeaseDuration)
	unproved := standaloneOutboxSpec(session, 1, standaloneTerminalDigest(), receiver, []byte(`{"workerId":"worker-1","status":"failed"}`), deadline)
	unproved.Kind = StandaloneOutboxWorkerExited
	if _, err := store.SaveStandaloneOutbox(context.Background(), unproved); err == nil {
		t.Fatal("fallback without group-gone proof was accepted")
	}
	if _, err := store.LoadStandaloneOutbox(context.Background(), session, 1); !errors.Is(err, ErrTerminalStatusNotFound) {
		t.Fatalf("refused fallback left a record: %v", err)
	}
}

func TestStandaloneOutboxReplayDeliversExactBytesOnce(t *testing.T) {
	t.Parallel()
	store, clock := standaloneOutboxStore(t)
	session := "11111111-1111-4111-8111-111111111111"
	receiver := standaloneReceiverKey(t)
	body := []byte(`{"workerId":"worker-1","status":"failed","failureMode":"silent-exit"}`)
	deadline := clock.now.Add(MaximumLeaseDuration)
	if _, err := store.SaveStandaloneOutbox(context.Background(), standaloneOutboxSpec(session, 7, standaloneTerminalDigest(), receiver, body, deadline)); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	var sent [][]byte
	sender := func(_ context.Context, key string, payload []byte) error {
		calls.Add(1)
		if key != receiver {
			return fmt.Errorf("receiverKey=%q, want %q", key, receiver)
		}
		sent = append(sent, append([]byte(nil), payload...))
		return nil
	}
	considered, err := store.ReplayStandaloneOutbox(context.Background(), 32, time.Second, sender)
	if err != nil || considered != 1 {
		t.Fatalf("considered=%d err=%v", considered, err)
	}
	if calls.Load() != 1 || len(sent) != 1 || !bytes.Equal(sent[0], body) {
		t.Fatalf("calls=%d sent=%q", calls.Load(), sent)
	}
	// The record is now delivered: a second pass replays nothing.
	considered, err = store.ReplayStandaloneOutbox(context.Background(), 32, time.Second, sender)
	if err != nil || considered != 0 {
		t.Fatalf("second pass considered=%d err=%v", considered, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("second pass sent again: calls=%d", calls.Load())
	}
	loaded, err := store.LoadStandaloneOutbox(context.Background(), session, 7)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Record.DeliveryState != TerminalStatusDelivered {
		t.Fatalf("delivery=%q, want delivered", loaded.Record.DeliveryState)
	}
}

func TestStandaloneOutboxReplaySurvivesStoreReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	clock := &standaloneManualClock{now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	store, err := NewLeaseStore(StoreOptions{Dir: dir, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	session := "11111111-1111-4111-8111-111111111111"
	receiver := standaloneReceiverKey(t)
	body := []byte(`{"workerId":"worker-1","status":"failed","failureMode":"timeout"}`)
	if _, err := store.SaveStandaloneOutbox(context.Background(), standaloneOutboxSpec(session, 1, standaloneTerminalDigest(), receiver, body, clock.now.Add(MaximumLeaseDuration))); err != nil {
		t.Fatal(err)
	}
	// Reopen the authority the way a restarted daemon does, then drain.
	reopened, err := NewLeaseStore(StoreOptions{Dir: dir, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	var sent [][]byte
	considered, err := reopened.ReplayStandaloneOutbox(context.Background(), 32, time.Second, func(_ context.Context, _ string, payload []byte) error {
		sent = append(sent, append([]byte(nil), payload...))
		return nil
	})
	if err != nil || considered != 1 || len(sent) != 1 || !bytes.Equal(sent[0], body) {
		t.Fatalf("considered=%d sent=%q err=%v", considered, sent, err)
	}
}

func TestStandaloneOutboxSendsThroughRegisteredReceiver(t *testing.T) {
	t.Parallel()
	store, _ := standaloneOutboxStore(t)
	receiver := standaloneReceiverKey(t)
	endpoint := "http://127.0.0.1:1/status"
	if err := store.RegisterReceiver(receiver, endpoint); err != nil {
		t.Fatal(err)
	}
	resolved, err := store.receivers.Resolve(receiver)
	if err != nil || resolved != endpoint {
		t.Fatalf("resolved=%q err=%v", resolved, err)
	}
}
