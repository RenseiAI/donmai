package workarea

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync/atomic"
	"syscall"
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

func TestSaveStandaloneOutboxRejectsInvalidSaveSpec(t *testing.T) {
	t.Parallel()
	store, clock := standaloneOutboxStore(t)
	session := "11111111-1111-4111-8111-111111111111"
	receiver := standaloneReceiverKey(t)
	body := []byte(`{"workerId":"worker-1","status":"failed"}`)
	deadline := clock.now.Add(MaximumLeaseDuration)
	valid := standaloneOutboxSpec(session, 1, standaloneTerminalDigest(), receiver, body, deadline)

	cases := []struct {
		name   string
		mutate func(*StandaloneOutboxSaveSpec)
	}{
		{"empty session", func(s *StandaloneOutboxSaveSpec) { s.SessionID = "" }},
		{"non-UUID session", func(s *StandaloneOutboxSaveSpec) { s.SessionID = "owner-session" }},
		{"empty body", func(s *StandaloneOutboxSaveSpec) { s.Body = nil }},
		{"zero deadline", func(s *StandaloneOutboxSaveSpec) { s.DeadlineAt = time.Time{} }},
	}
	// The parallel subtests run inside a group so the leftover check below
	// runs after every refused save has finished. Paused t.Parallel subtests
	// only start once their parent function returns, so a check placed
	// directly after the loop would run before any save was attempted.
	t.Run("refused", func(t *testing.T) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				spec := valid
				tc.mutate(&spec)
				if _, err := store.SaveStandaloneOutbox(context.Background(), spec); err == nil {
					t.Fatal("invalid save spec was accepted")
				}
			})
		}
	})

	// None of the refused saves left a record behind. Count raw directory
	// entries rather than decoded envelopes: a leftover record from a
	// refused spec need not decode.
	entries, err := os.ReadDir(store.standaloneOutboxDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("refused saves left %d records behind", len(entries))
	}
}

func TestStandaloneOutboxQuarantinesBadFileAndReplaysGood(t *testing.T) {
	t.Parallel()
	store, clock := standaloneOutboxStore(t)
	session := "11111111-1111-4111-8111-111111111111"
	receiver := standaloneReceiverKey(t)
	body := []byte(`{"workerId":"worker-1","status":"failed"}`)
	deadline := clock.now.Add(MaximumLeaseDuration)
	if _, err := store.SaveStandaloneOutbox(context.Background(), standaloneOutboxSpec(session, 1, standaloneTerminalDigest(), receiver, body, deadline)); err != nil {
		t.Fatal(err)
	}
	// A stray file, a truncated record, and an unknown-kind record land in
	// the outbox directory. Before quarantining, any one of them failed the
	// whole scan: good records sent 0 times and the store open failed.
	dir := store.standaloneOutboxDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"stray.json":     `{"hello":"world"}`,
		"truncated.json": `{"schemaVersion":`,
		"txo_bad.json":   `{"schemaVersion":"donmai.terminal-status-outbox.v1","outboxKey":"txo_bad","kind":"future-kind"}`,
	} {
		if err := os.WriteFile(dir+"/"+name, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var sent [][]byte
	considered, err := store.ReplayStandaloneOutbox(context.Background(), 32, time.Second, func(_ context.Context, _ string, payload []byte) error {
		sent = append(sent, append([]byte(nil), payload...))
		return nil
	})
	if err != nil {
		t.Fatalf("replay with bad files: %v", err)
	}
	if considered != 1 || len(sent) != 1 || !bytes.Equal(sent[0], body) {
		t.Fatalf("considered=%d sent=%d, want the good record delivered", considered, len(sent))
	}
	entries, err := os.ReadDir(store.standaloneOutboxQuarantineDir())
	if err != nil {
		t.Fatalf("quarantine dir: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("quarantined=%d, want 3", len(entries))
	}
	// A reopened store also survives the bad files: the open used to fail.
	reopened, err := NewLeaseStore(StoreOptions{Dir: store.Dir(), Now: clock.Now})
	if err != nil {
		t.Fatalf("reopen with bad files: %v", err)
	}
	if _, err := reopened.LoadStandaloneOutbox(context.Background(), session, 1); err != nil {
		t.Fatalf("good record missing after reopen: %v", err)
	}
}

func TestStandaloneOutboxPrunesDeliveredPastRetention(t *testing.T) {
	t.Parallel()
	store, clock := standaloneOutboxStore(t)
	session := "11111111-1111-4111-8111-111111111111"
	receiver := standaloneReceiverKey(t)
	body := []byte(`{"workerId":"worker-1","status":"completed"}`)
	deadline := clock.now.Add(MaximumLeaseDuration)
	if _, err := store.SaveStandaloneOutbox(context.Background(), standaloneOutboxSpec(session, 1, standaloneTerminalDigest(), receiver, body, deadline)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkStandaloneOutboxDelivered(context.Background(), session, 1); err != nil {
		t.Fatal(err)
	}
	// Inside the retention window the delivered record still decodes: a
	// second replay pass considers nothing but the file remains for
	// post-mortem reads.
	considered, err := store.ReplayStandaloneOutbox(context.Background(), 32, time.Second, func(_ context.Context, _ string, _ []byte) error {
		t.Fatal("delivered record must not send again")
		return nil
	})
	if err != nil || considered != 0 {
		t.Fatalf("considered=%d err=%v", considered, err)
	}
	if _, err := store.LoadStandaloneOutbox(context.Background(), session, 1); err != nil {
		t.Fatalf("delivered record pruned too early: %v", err)
	}
	// Past the retention window the next replay pass removes the file so
	// the 5s scan stays bounded.
	clock.now = clock.now.Add(StandaloneOutboxRetention + time.Hour)
	considered, err = store.ReplayStandaloneOutbox(context.Background(), 32, time.Second, func(_ context.Context, _ string, _ []byte) error {
		t.Fatal("pruned record must not send")
		return nil
	})
	if err != nil || considered != 0 {
		t.Fatalf("considered=%d err=%v", considered, err)
	}
	if _, err := store.LoadStandaloneOutbox(context.Background(), session, 1); !errors.Is(err, ErrTerminalStatusNotFound) {
		t.Fatalf("delivered record not pruned: %v", err)
	}
}

func TestStandaloneOutboxAcceptsLocalSessionID(t *testing.T) {
	t.Parallel()
	store, clock := standaloneOutboxStore(t)
	session := "session_11111111111111111111111111111111"
	receiver := standaloneReceiverKey(t)
	body := []byte(`{"workerId":"worker-1","status":"completed"}`)
	deadline := clock.now.Add(MaximumLeaseDuration)
	key, err := store.SaveStandaloneOutbox(context.Background(), standaloneOutboxSpec(session, 1, standaloneTerminalDigest(), receiver, body, deadline))
	if err != nil {
		t.Fatalf("local session save: %v", err)
	}
	if key != StandaloneOutboxKey(session, 1) {
		t.Fatalf("key=%q, want %q", key, StandaloneOutboxKey(session, 1))
	}
	loaded, err := store.LoadStandaloneOutbox(context.Background(), session, 1)
	if err != nil {
		t.Fatalf("local session load: %v", err)
	}
	retained, err := loaded.Record.Body()
	if err != nil || !bytes.Equal(retained, body) {
		t.Fatalf("retained=%q err=%v", retained, err)
	}
	var sent [][]byte
	considered, err := store.ReplayStandaloneOutbox(context.Background(), 32, time.Second, func(_ context.Context, _ string, payload []byte) error {
		sent = append(sent, append([]byte(nil), payload...))
		return nil
	})
	if err != nil || considered != 1 || len(sent) != 1 || !bytes.Equal(sent[0], body) {
		t.Fatalf("considered=%d sent=%d err=%v", considered, len(sent), err)
	}
}

func TestStandaloneOutboxReopenLeavesLiveAttemptAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	clock := &standaloneManualClock{now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	store, err := NewLeaseStore(StoreOptions{Dir: dir, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	session := "11111111-1111-4111-8111-111111111111"
	receiver := standaloneReceiverKey(t)
	body := []byte(`{"workerId":"worker-1","status":"failed"}`)
	if _, err := store.SaveStandaloneOutbox(context.Background(), standaloneOutboxSpec(session, 1, standaloneTerminalDigest(), receiver, body, clock.now.Add(MaximumLeaseDuration))); err != nil {
		t.Fatal(err)
	}
	// Drive the record into attempting the way a live replay does: flip
	// the state under the record lock, then hold the lock (on a separate
	// file description, the way a live sender in another process would)
	// while a second store handle opens the same directory. The open
	// must not rewind the live attempt to pending — that rewind is what
	// duplicated the send.
	key := StandaloneOutboxKey(session, 1)
	if err := store.withLocks(context.Background(), []string{"standalone-outbox:" + key}, func() error {
		envelope, err := store.loadStandaloneOutbox(key)
		if err != nil {
			return err
		}
		envelope.Record.DeliveryState = TerminalStatusAttempting
		return store.writeStandaloneOutbox(*envelope)
	}); err != nil {
		t.Fatalf("arm attempting: %v", err)
	}
	release := holdOutboxRecordLock(t, store, key)
	defer release()
	reopened, err := NewLeaseStore(StoreOptions{Dir: dir, Now: clock.Now})
	if err != nil {
		t.Fatalf("reopen with live attempt: %v", err)
	}
	// The assertion must not take the record lock: the live send still
	// holds it. Read the file directly and decode it instead.
	raw, err := os.ReadFile(dir + "/standalone-outbox/" + key + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope standaloneOutboxEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Record.DeliveryState != TerminalStatusAttempting {
		t.Fatalf("delivery=%q, want attempting (live send still owns it)", envelope.Record.DeliveryState)
	}
	_ = reopened
}

// holdOutboxRecordLock takes the record's lock file the way withLocks does
// and holds it until release is called, modelling a live sender mid-attempt.
// Flock locks are per-open-file-description: the same process must hold the
// lock on a SEPARATE file description for the contention to be real, so
// this opens the lock file directly rather than reusing the store handle.
func holdOutboxRecordLock(t *testing.T, store *LeaseStore, key string) func() {
	t.Helper()
	sum := sha256.Sum256([]byte("standalone-outbox:" + key))
	name := hex.EncodeToString(sum[:]) + ".lock"
	f, err := os.OpenFile(store.Dir()+"/locks/"+name, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := intFileDescriptor(f.Fd())
	if err != nil {
		_ = f.Close()
		t.Fatalf("record lock descriptor out of range: %v", err)
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		t.Fatalf("take record lock: %v", err)
	}
	return func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = f.Close()
	}
}

func TestStandaloneOutboxDeadlineIsNotABorrowedLease(t *testing.T) {
	t.Parallel()
	if StandaloneOutboxDeadline <= MaximumLeaseDuration {
		t.Fatalf("deadline=%v, want longer than the lease maximum %v", StandaloneOutboxDeadline, MaximumLeaseDuration)
	}
}

// TestLeaseOutboxReopenLeavesLiveAttemptAlone is the lease-side twin of
// TestStandaloneOutboxReopenLeavesLiveAttemptAlone: flip a lease-bound
// terminal record to attempting under its result/workarea locks, hold one
// of those locks on a separate file description the way a live sender in
// another process would, then reopen. The open must not rewind the live
// attempt to pending — emptying the reconcile's withLocksOrSkip key list
// would take no lock, always run the reset, and silently reinstate the
// duplicate send this guards.
func TestLeaseOutboxReopenLeavesLiveAttemptAlone(t *testing.T) {
	t.Parallel()
	clock := &standaloneManualClock{now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	store, err := NewLeaseStore(StoreOptions{Dir: dir, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	session := "11111111-1111-4111-8111-111111111111"
	resultID := "tr_11111111111111111111111111111111"
	workareaID := "wa_11111111111111111111111111111111"
	receiver := standaloneReceiverKey(t)
	lease, err := store.Acquire(context.Background(), AcquireSpec{
		SessionID: session, TerminalResultID: resultID, WorkareaID: workareaID,
		WorkareaPath: dir + "/" + workareaID, Policy: DefaultLeasePolicy(),
		ReleaseRequested: true, ReleaseDisposition: "destroy",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"workerId":"worker-1","status":"failed"}`)
	if _, err := store.SaveTerminalStatus(context.Background(), TerminalStatusSaveSpec{
		LeaseID: lease.LeaseID, SessionID: session, TerminalResultID: resultID,
		WorkareaID: workareaID, ReceiverKey: receiver, Body: body,
		ExpectedExpiresAt: lease.ExpiresAt,
	}); err != nil {
		t.Fatal(err)
	}
	// Arm attempting the way a live replay does: flip the state under the
	// record locks, so the flip itself cannot race a concurrent sender.
	if err := store.withLocks(context.Background(), []string{"result:" + resultID, "workarea:" + workareaID}, func() error {
		live, err := store.load(lease.LeaseID)
		if err != nil {
			return err
		}
		if live.TerminalStatus == nil {
			return errors.New("terminal status missing after save")
		}
		live.TerminalStatus.DeliveryState = TerminalStatusAttempting
		return store.writeRecord(*live)
	}); err != nil {
		t.Fatalf("arm attempting: %v", err)
	}
	release := holdLeaseRecordLock(t, store, "result:"+resultID)
	defer release()
	reopened, err := NewLeaseStore(StoreOptions{Dir: dir, Now: clock.Now})
	if err != nil {
		t.Fatalf("reopen with live attempt: %v", err)
	}
	// The assertion must not take the record locks: the live send still
	// holds one. The in-memory listing reflects the reconcile decision and
	// the record file below is decoded directly.
	persisted, err := reopened.load(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.TerminalStatus == nil || persisted.TerminalStatus.DeliveryState != TerminalStatusAttempting {
		t.Fatalf("delivery=%+v, want attempting (live send still owns it)", persisted.TerminalStatus)
	}
}

// holdLeaseRecordLock takes one of the lease record's lock files the way
// withLocks does and holds it until release is called, modelling a live
// sender mid-attempt. Flock locks are per-open-file-description: the same
// process must hold the lock on a SEPARATE file description for the
// contention to be real, so this opens the lock file directly rather than
// reusing the store handle.
func holdLeaseRecordLock(t *testing.T, store *LeaseStore, key string) func() {
	t.Helper()
	sum := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(sum[:]) + ".lock"
	f, err := os.OpenFile(store.Dir()+"/locks/"+name, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := intFileDescriptor(f.Fd())
	if err != nil {
		_ = f.Close()
		t.Fatalf("record lock descriptor out of range: %v", err)
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		t.Fatalf("take record lock: %v", err)
	}
	return func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = f.Close()
	}
}
