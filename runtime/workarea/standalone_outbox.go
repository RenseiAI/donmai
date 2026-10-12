package workarea

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// This file owns the lease-independent terminal-status outbox: one durable
// donmai.terminal-status-outbox.v1 record per (session, attempt), persisted
// BEFORE the first send so a runner killed between persist and send has its
// exact bytes replayed once by the daemon replayer.
//
// It reuses the TerminalStatusOutbox record shape (the same schema the
// lease-bound SaveTerminalStatus path persists) and the same receiver
// registry, retry bounds, and sender type. What differs is the KEY: a lease
// binds (session, terminal result, workarea) after a successful hold, while
// this outbox binds (session, attempt) for every terminal status — completed,
// failed, or cancelled — whether or not any lease was acquired. The daemon
// replayer drains exactly the records that have no terminal workarea lease.
//
// The attempt is the session-scoped terminal sequence the runner assigns to
// its terminal body: 1 for the runner's own terminal status. Both writers —
// the runner's terminal record and the worker_exited_without_result
// fallback — derive the same key from (session, attempt), and the first
// writer wins atomically; the runner record and the fallback record never
// both exist. The key deliberately does NOT track the shim launch contract's
// monotonic process epoch: a resume that stops one incarnation and starts
// another must replay the same terminal record rather than orphan it under a
// new key.

// StandaloneOutboxKind names which writer produced a standalone outbox record.
type StandaloneOutboxKind string

const (
	// StandaloneOutboxTerminal records the runner's own terminal status body.
	StandaloneOutboxTerminal StandaloneOutboxKind = "terminal"
	// StandaloneOutboxWorkerExited records the fallback body written after the
	// worker's process group was proved gone without a terminal record.
	StandaloneOutboxWorkerExited StandaloneOutboxKind = "worker_exited_without_result"
)

// StandaloneOutboxSaveSpec carries the fields needed to persist one
// lease-independent terminal-status record.
type StandaloneOutboxSaveSpec struct {
	SessionID string
	// Attempt is the session-scoped terminal sequence the runner assigns to
	// its terminal body: 1 for the runner's own terminal status. It
	// disambiguates repeated terminal writes of the same session so a stale
	// record can never shadow a fresh run, and it stays fixed across shim
	// resumes: a resume must not change the key, or the replay would orphan
	// the record the previous incarnation persisted.
	Attempt uint64
	// TerminalResultID carries the body digest (the runner's stable terminal
	// result identity): it binds the record to the exact bytes it replays.
	TerminalResultID string
	ReceiverKey      string
	Body             []byte
	DeadlineAt       time.Time
	Kind             StandaloneOutboxKind
	// GroupReaped proves the worker's process group is gone. It is required
	// for the worker_exited_without_result fallback and must stay false for
	// the runner's own terminal record: only a dead worker's seat may carry
	// the fallback, and only a live runner may claim its own terminal body.
	GroupReaped bool
}

// StandaloneOutboxKey derives the durable key for one (session, attempt).
// The key is a pure function of its inputs so the runner, the daemon
// replayer, and the fallback writer all address the same record without a
// separate link: the tombstone-side link lands separately. The attempt is
// the session-scoped terminal sequence, not the shim launch contract's
// process epoch: a resume advances the epoch but must keep addressing the
// same record.
func StandaloneOutboxKey(sessionID string, attempt uint64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x1f%d", sessionID, attempt)))
	return "txo_" + hex.EncodeToString(sum[:16])
}

// validateOutboxSessionID accepts the session identities the outbox keys:
// canonical platform UUIDs and the shipped local runtime's session_<hex>
// references. Both are opaque key material here — the key derivation hashes
// them, so neither shape can alias the other — but anything else (empty, a
// path, an overlong string) is refused before it can key a record.
func validateOutboxSessionID(value string) error {
	if err := validateCanonicalUUID(value); err == nil {
		return nil
	}
	if len(value) == len("session_")+32 && strings.HasPrefix(value, "session_") && validLowerHex(value[len("session_"):], 32) {
		return nil
	}
	return errors.New("identity must be a canonical lowercase hyphenated UUID or session_<32 lowercase hex>")
}

func validateStandaloneOutboxSaveSpec(spec StandaloneOutboxSaveSpec) error {
	if err := validateOutboxSessionID(spec.SessionID); err != nil {
		return fmt.Errorf("runtime/workarea: sessionId: %w", err)
	}
	if err := validateGeneratedID(spec.TerminalResultID, "tr_"); err != nil {
		return fmt.Errorf("runtime/workarea: terminalResultId: %w", err)
	}
	if err := validateGeneratedID(spec.ReceiverKey, "rcv_"); err != nil {
		return fmt.Errorf("runtime/workarea: receiverKey: %w", err)
	}
	if len(spec.Body) == 0 {
		return errors.New("runtime/workarea: terminal status body required")
	}
	if spec.DeadlineAt.IsZero() {
		return errors.New("runtime/workarea: outbox deadline required")
	}
	switch spec.Kind {
	case StandaloneOutboxTerminal:
		if spec.GroupReaped {
			return errors.New("runtime/workarea: terminal record must not claim a reaped group")
		}
	case StandaloneOutboxWorkerExited:
		if !spec.GroupReaped {
			return errors.New("runtime/workarea: fallback record requires proof the group is gone")
		}
	default:
		return errors.New("runtime/workarea: unknown standalone outbox kind")
	}
	return nil
}

// StandaloneOutboxRecord is the exported projection of one lease-independent
// outbox envelope: the writer kind plus the reused TerminalStatusOutbox
// record carrying the exact replay bytes and its digest.
type StandaloneOutboxRecord struct {
	SessionID        string
	Attempt          uint64
	Kind             StandaloneOutboxKind
	TerminalResultID string
	Record           TerminalStatusOutbox
}

// standaloneOutboxEnvelope is the on-disk shape: the session/attempt key, the
// writer kind, and the reused TerminalStatusOutbox record carrying the exact
// replay bytes plus its digest.
type standaloneOutboxEnvelope struct {
	SchemaVersion    string               `json:"schemaVersion"`
	OutboxKey        string               `json:"outboxKey"`
	SessionID        string               `json:"sessionId"`
	Attempt          uint64               `json:"attempt"`
	Kind             StandaloneOutboxKind `json:"kind"`
	TerminalResultID string               `json:"terminalResultId"`
	Record           TerminalStatusOutbox `json:"record"`
}

// StandaloneOutboxRetention bounds how long a terminal record stays on
// disk after it stops needing replay. Delivered and dead-letter records
// carry no further work: the receiver already accepted the bytes (or the
// delivery policy gave up on them), so they are pruned rather than decoded
// on every replay pass and every store open. The bound is deliberately
// generous — a delivered record remains available for post-mortem reads for
// a full day before the next replay pass removes its file.
const StandaloneOutboxRetention = 24 * time.Hour

// standaloneOutboxQuarantineDir names the sibling directory that holds
// outbox files which failed to decode. A stray, truncated, or unknown-kind
// file quarantines there instead of failing the whole scan: without it one
// bad file blocks every good record (no sends at all) and fails the store
// open, which fails daemon start and worker preflight.
func (s *LeaseStore) standaloneOutboxQuarantineDir() string {
	return filepath.Join(s.dir, "standalone-outbox-quarantine")
}

func (s *LeaseStore) standaloneOutboxQuarantinePath(name string) string {
	return filepath.Join(s.standaloneOutboxQuarantineDir(), name)
}

func (s *LeaseStore) ensureStandaloneOutboxQuarantineDir() error {
	dir := s.standaloneOutboxQuarantineDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("runtime/workarea: create standalone outbox quarantine: %w", err)
	}
	return syncDir(dir)
}

// quarantineStandaloneOutboxFile moves one undecodable outbox file aside so
// the remaining records keep replaying. The quarantined name keeps the
// original filename plus a millisecond timestamp so two bad files with the
// same name never collide, and the move is a rename inside the same parent
// so it is atomic. Best-effort: a quarantine failure is returned so the
// caller can fail closed rather than silently dropping the record.
func (s *LeaseStore) quarantineStandaloneOutboxFile(name, reason string) error {
	if err := s.ensureStandaloneOutboxQuarantineDir(); err != nil {
		return err
	}
	src := filepath.Join(s.standaloneOutboxDir(), name)
	dst := s.standaloneOutboxQuarantinePath(fmt.Sprintf("%s.%d.quarantined", name, s.now().UnixMilli()))
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("runtime/workarea: quarantine standalone outbox %s (%s): %w", name, reason, err)
	}
	if err := syncDir(s.standaloneOutboxDir()); err != nil {
		return err
	}
	return syncDir(s.standaloneOutboxQuarantineDir())
}

func (s *LeaseStore) standaloneOutboxDir() string {
	return filepath.Join(s.dir, "standalone-outbox")
}

func (s *LeaseStore) standaloneOutboxPath(key string) string {
	return filepath.Join(s.standaloneOutboxDir(), key+".json")
}

func (s *LeaseStore) ensureStandaloneOutboxDir() error {
	dir := s.standaloneOutboxDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("runtime/workarea: create standalone outbox: %w", err)
	}
	return syncDir(dir)
}

// SaveStandaloneOutbox persists one lease-independent terminal-status record
// before the first send. The first writer wins: a second save for the same
// (session, attempt) with an identical body digest and kind is idempotent,
// while a save that disagrees returns an ErrLeaseConflict-wrapped conflict so
// callers observe one conflict vocabulary. The runner record and the fallback
// record therefore never both exist.
func (s *LeaseStore) SaveStandaloneOutbox(ctx context.Context, spec StandaloneOutboxSaveSpec) (string, error) {
	if err := s.Ready(); err != nil {
		return "", err
	}
	if err := validateStandaloneOutboxSaveSpec(spec); err != nil {
		return "", err
	}
	key := StandaloneOutboxKey(spec.SessionID, spec.Attempt)
	var out string
	err := s.withLocks(ctx, []string{"standalone-outbox:" + key, clockLockKey}, func() error {
		if existing, err := s.loadStandaloneOutbox(key); err == nil {
			if existing.SessionID != spec.SessionID || existing.Attempt != spec.Attempt ||
				existing.TerminalResultID != spec.TerminalResultID || existing.Kind != spec.Kind {
				return fmt.Errorf("%w: outbox %s", ErrLeaseConflict, key)
			}
			candidate := NewTerminalStatusOutbox(spec.TerminalResultID, spec.ReceiverKey, spec.Body, spec.DeadlineAt, existing.Record.NextAttemptAt)
			if !terminalStatusIdentityEqual(existing.Record, candidate) {
				return fmt.Errorf("%w: outbox %s body differs", ErrLeaseConflict, key)
			}
			out = key
			return nil
		} else if !errors.Is(err, ErrTerminalStatusNotFound) {
			return s.failClosed(err)
		}
		nowMS, err := s.sampleClockLocked()
		if err != nil {
			return s.failClosed(err)
		}
		candidate := NewTerminalStatusOutbox(spec.TerminalResultID, spec.ReceiverKey, spec.Body, spec.DeadlineAt, time.UnixMilli(nowMS).UTC())
		if err := candidate.Validate(); err != nil {
			return err
		}
		envelope := standaloneOutboxEnvelope{
			SchemaVersion: TerminalStatusOutboxSchemaV1, OutboxKey: key,
			SessionID: spec.SessionID, Attempt: spec.Attempt, Kind: spec.Kind,
			TerminalResultID: spec.TerminalResultID, Record: candidate,
		}
		if err := s.writeStandaloneOutbox(envelope); err != nil {
			return s.failClosed(err)
		}
		out = key
		return nil
	})
	return out, err
}

// LoadStandaloneOutbox returns the record for one (session, attempt), or
// ErrTerminalStatusNotFound when no writer has persisted it yet.
func (s *LeaseStore) LoadStandaloneOutbox(ctx context.Context, sessionID string, attempt uint64) (*StandaloneOutboxRecord, error) {
	if err := s.Ready(); err != nil {
		return nil, err
	}
	if err := validateOutboxSessionID(sessionID); err != nil {
		return nil, fmt.Errorf("runtime/workarea: sessionId: %w", err)
	}
	key := StandaloneOutboxKey(sessionID, attempt)
	var out *StandaloneOutboxRecord
	err := s.withLocks(ctx, []string{"standalone-outbox:" + key}, func() error {
		envelope, err := s.loadStandaloneOutbox(key)
		if err != nil {
			return err
		}
		out = &StandaloneOutboxRecord{
			SessionID: envelope.SessionID, Attempt: envelope.Attempt, Kind: envelope.Kind,
			TerminalResultID: envelope.TerminalResultID, Record: envelope.Record,
		}
		return nil
	})
	return out, err
}

// MarkStandaloneOutboxDelivered records receiver transport acceptance for one
// (session, attempt) without granting acknowledgement-path release authority.
// Replays after this point observe the delivered state and stand down, so a
// runner killed after persist but before send has its exact bytes replayed
// exactly once.
func (s *LeaseStore) MarkStandaloneOutboxDelivered(ctx context.Context, sessionID string, attempt uint64) (*StandaloneOutboxRecord, error) {
	if err := s.Ready(); err != nil {
		return nil, err
	}
	if err := validateOutboxSessionID(sessionID); err != nil {
		return nil, fmt.Errorf("runtime/workarea: sessionId: %w", err)
	}
	key := StandaloneOutboxKey(sessionID, attempt)
	var out *StandaloneOutboxRecord
	err := s.withLocks(ctx, []string{"standalone-outbox:" + key, clockLockKey}, func() error {
		envelope, err := s.loadStandaloneOutbox(key)
		if err != nil {
			return err
		}
		if envelope.Record.DeliveryState == TerminalStatusDelivered {
			out = &StandaloneOutboxRecord{
				SessionID: envelope.SessionID, Attempt: envelope.Attempt, Kind: envelope.Kind,
				TerminalResultID: envelope.TerminalResultID, Record: envelope.Record,
			}
			return nil
		}
		nowMS, err := s.sampleClockLocked()
		if err != nil {
			return s.failClosed(err)
		}
		envelope.Record.DeliveryState = TerminalStatusDelivered
		envelope.Record.NextAttemptAt = time.UnixMilli(nowMS).UTC()
		envelope.Record.LastError = nil
		if err := s.writeStandaloneOutbox(*envelope); err != nil {
			return s.failClosed(err)
		}
		out = &StandaloneOutboxRecord{
			SessionID: envelope.SessionID, Attempt: envelope.Attempt, Kind: envelope.Kind,
			TerminalResultID: envelope.TerminalResultID, Record: envelope.Record,
		}
		return nil
	})
	return out, err
}

// ReplayStandaloneOutbox performs one bounded recovery pass over records that
// have no terminal workarea lease, sending each record's exact retained bytes
// through sender. It returns the number of records considered for send.
func (s *LeaseStore) ReplayStandaloneOutbox(ctx context.Context, batchSize int, attemptTimeout time.Duration, sender TerminalStatusSender) (int, error) {
	return s.replayStandaloneOutbox(ctx, batchSize, DefaultTerminalResultReplayConcurrency, attemptTimeout, sender)
}

func (s *LeaseStore) replayStandaloneOutbox(ctx context.Context, batchSize, concurrency int, attemptTimeout time.Duration, sender TerminalStatusSender) (int, error) {
	if sender == nil {
		return 0, errors.New("runtime/workarea: terminal status sender required")
	}
	if batchSize <= 0 {
		batchSize = DefaultTerminalResultReplayBatchSize
	}
	if concurrency <= 0 || concurrency > batchSize {
		concurrency = min(DefaultTerminalResultReplayConcurrency, batchSize)
	}
	if attemptTimeout <= 0 {
		attemptTimeout = DefaultTerminalResultReplayTimeout
	}
	envelopes, err := s.listStandaloneOutbox()
	if err != nil {
		return 0, err
	}
	nowMS, err := s.sampleClock(ctx)
	if err != nil {
		return 0, err
	}
	sort.Slice(envelopes, func(i, j int) bool { return envelopes[i].OutboxKey < envelopes[j].OutboxKey })
	eligible := make([]standaloneOutboxEnvelope, 0, batchSize)
	for i := range envelopes {
		post := envelopes[i].Record
		if post.DeliveryState == TerminalStatusDelivered || post.DeliveryState == TerminalStatusDeadLetter {
			continue
		}
		if nowMS >= post.DeadlineAt.UnixMilli() {
			_ = s.markStandaloneOutboxDeadLetter(context.Background(), envelopes[i].OutboxKey, "delivery deadline reached")
			continue
		}
		if nowMS < post.NextAttemptAt.UnixMilli() {
			continue
		}
		eligible = append(eligible, envelopes[i])
		if len(eligible) == batchSize {
			break
		}
	}
	return len(eligible), runConcurrent(ctx, eligible, concurrency, func(envelope standaloneOutboxEnvelope) error {
		return s.replayOneStandaloneOutbox(ctx, envelope, attemptTimeout, sender)
	})
}

// RunStandaloneOutboxReplayer runs lease-independent terminal status recovery
// until ctx is cancelled. The sender must resolve the stored receiver key on
// every attempt. Pair it with RunTerminalResultReplayer: that loop drains
// lease-bound records, this one drains the records that have no lease.
func (s *LeaseStore) RunStandaloneOutboxReplayer(ctx context.Context, opts TerminalResultReplayOptions, sender TerminalStatusSender) {
	if opts.Interval <= 0 {
		opts.Interval = DefaultTerminalResultReplayInterval
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultTerminalResultReplayBatchSize
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultTerminalResultReplayConcurrency
	}
	if opts.AttemptTimeout <= 0 {
		opts.AttemptTimeout = DefaultTerminalResultReplayTimeout
	}
	replay := func() {
		if _, err := s.replayStandaloneOutbox(ctx, opts.BatchSize, opts.Concurrency, opts.AttemptTimeout, sender); err != nil && !isOnlyOperationContextCancellation(ctx, err) && opts.OnError != nil {
			opts.OnError(err)
		}
	}
	replay()
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			replay()
		}
	}
}

func (s *LeaseStore) replayOneStandaloneOutbox(ctx context.Context, snapshot standaloneOutboxEnvelope, attemptTimeout time.Duration, sender TerminalStatusSender) error {
	var body []byte
	var receiverKey string
	var attempt int64
	err := s.withLocks(ctx, []string{"standalone-outbox:" + snapshot.OutboxKey, clockLockKey}, func() error {
		envelope, err := s.loadStandaloneOutbox(snapshot.OutboxKey)
		if err != nil {
			if errors.Is(err, ErrTerminalStatusNotFound) {
				return nil
			}
			return err
		}
		post := envelope.Record
		if post.DeliveryState != TerminalStatusPending && post.DeliveryState != TerminalStatusAttempting {
			return nil
		}
		nowMS, err := s.sampleClockLocked()
		if err != nil {
			return s.failClosed(err)
		}
		if nowMS >= post.DeadlineAt.UnixMilli() {
			envelope.Record.DeliveryState = TerminalStatusDeadLetter
			message := "delivery deadline reached"
			envelope.Record.LastError = &message
			return s.writeStandaloneOutbox(*envelope)
		}
		decoded, err := post.Body()
		if err != nil {
			return s.failClosed(err)
		}
		envelope.Record.DeliveryState = TerminalStatusAttempting
		envelope.Record.AttemptCount++
		attempt = envelope.Record.AttemptCount
		now := time.UnixMilli(nowMS).UTC()
		envelope.Record.LastAttemptAt = &now
		envelope.Record.NextAttemptAt = time.UnixMilli(nowMS + terminalResultReplayRetryDelay(attempt).Milliseconds()).UTC()
		if err := s.writeStandaloneOutbox(*envelope); err != nil {
			return s.failClosed(err)
		}
		body = decoded
		receiverKey = post.ReceiverKey
		return nil
	})
	if err != nil || body == nil {
		return err
	}
	attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	sendErr := sender(attemptCtx, receiverKey, body)
	cancel()
	return s.withLocks(context.Background(), []string{"standalone-outbox:" + snapshot.OutboxKey, clockLockKey}, func() error {
		envelope, err := s.loadStandaloneOutbox(snapshot.OutboxKey)
		if err != nil {
			if errors.Is(err, ErrTerminalStatusNotFound) {
				return nil
			}
			return err
		}
		post := envelope.Record
		if post.AttemptCount != attempt || post.DeliveryState != TerminalStatusAttempting {
			return nil
		}
		nowMS, err := s.sampleClockLocked()
		if err != nil {
			return s.failClosed(err)
		}
		switch {
		case sendErr == nil:
			envelope.Record.DeliveryState = TerminalStatusDelivered
			envelope.Record.LastError = nil
		case nowMS >= post.DeadlineAt.UnixMilli():
			envelope.Record.DeliveryState = TerminalStatusDeadLetter
			message := sendErr.Error()
			envelope.Record.LastError = &message
		default:
			envelope.Record.DeliveryState = TerminalStatusPending
			message := sendErr.Error()
			envelope.Record.LastError = &message
		}
		if err := s.writeStandaloneOutbox(*envelope); err != nil {
			return s.failClosed(err)
		}
		if sendErr != nil {
			return fmt.Errorf("runtime/workarea: replay standalone outbox %s: %w", snapshot.OutboxKey, sendErr)
		}
		return nil
	})
}

func (s *LeaseStore) markStandaloneOutboxDeadLetter(ctx context.Context, key, message string) error {
	return s.withLocks(ctx, []string{"standalone-outbox:" + key}, func() error {
		envelope, err := s.loadStandaloneOutbox(key)
		if err != nil {
			if errors.Is(err, ErrTerminalStatusNotFound) {
				return nil
			}
			return err
		}
		if envelope.Record.DeliveryState == TerminalStatusDelivered || envelope.Record.DeliveryState == TerminalStatusDeadLetter {
			return nil
		}
		envelope.Record.DeliveryState = TerminalStatusDeadLetter
		envelope.Record.LastError = &message
		return s.writeStandaloneOutbox(*envelope)
	})
}

func (s *LeaseStore) loadStandaloneOutbox(key string) (*standaloneOutboxEnvelope, error) {
	if !strings.HasPrefix(key, "txo_") || len(key) != len("txo_")+32 {
		return nil, ErrTerminalStatusNotFound
	}
	data, err := s.readFile(s.standaloneOutboxPath(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrTerminalStatusNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("runtime/workarea: read standalone outbox %s: %w", key, err)
	}
	var envelope standaloneOutboxEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("runtime/workarea: decode standalone outbox %s: %w", key, err)
	}
	if envelope.OutboxKey != key || envelope.SchemaVersion != TerminalStatusOutboxSchemaV1 {
		return nil, fmt.Errorf("runtime/workarea: invalid standalone outbox %s", key)
	}
	if err := validateOutboxSessionID(envelope.SessionID); err != nil {
		return nil, fmt.Errorf("runtime/workarea: invalid standalone outbox %s: %w", key, err)
	}
	if envelope.Kind != StandaloneOutboxTerminal && envelope.Kind != StandaloneOutboxWorkerExited {
		return nil, fmt.Errorf("runtime/workarea: invalid standalone outbox %s kind", key)
	}
	if err := envelope.Record.Validate(); err != nil {
		return nil, fmt.Errorf("runtime/workarea: invalid standalone outbox %s: %w", key, err)
	}
	if envelope.TerminalResultID != envelope.Record.TerminalResultID {
		return nil, fmt.Errorf("runtime/workarea: standalone outbox %s digest disagrees with record", key)
	}
	return &envelope, nil
}

func (s *LeaseStore) writeStandaloneOutbox(envelope standaloneOutboxEnvelope) error {
	if err := s.ensureStandaloneOutboxDir(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime/workarea: marshal standalone outbox %s: %w", envelope.OutboxKey, err)
	}
	return writeFileAtomic(s.standaloneOutboxDir(), s.standaloneOutboxPath(envelope.OutboxKey), ".outbox-*.tmp", data)
}

func (s *LeaseStore) listStandaloneOutbox() ([]standaloneOutboxEnvelope, error) {
	if err := s.Ready(); err != nil {
		return nil, err
	}
	return s.scanStandaloneOutbox(true)
}

// scanStandaloneOutbox reads every outbox file, quarantining the ones that
// fail to decode instead of failing the whole scan. Quarantining is what
// keeps one bad file from blocking every good record: the scan decodes all
// of them today, and without this a stray, truncated, or unknown-kind file
// means good records send 0 times. When prune is true, delivered and
// dead-letter records older than StandaloneOutboxRetention are removed
// instead of decoded on every 5s replay pass and every store open — those
// terminal records carry no further work, and decoding all of them costs
// ~390ms per pass at 10k records (1.2s at 30k). Records still inside the
// retention window decode normally so post-mortem reads keep working.
func (s *LeaseStore) scanStandaloneOutbox(prune bool) ([]standaloneOutboxEnvelope, error) {
	dir := s.standaloneOutboxDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	nowMS := s.now().UnixMilli()
	envelopes := make([]standaloneOutboxEnvelope, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		key := strings.TrimSuffix(entry.Name(), ".json")
		envelope, err := s.loadStandaloneOutbox(key)
		if err != nil {
			if errors.Is(err, ErrTerminalStatusNotFound) {
				// A filename that is not an outbox key (a stray file
				// dropped in the directory) quarantines like a
				// corrupt record: it is not addressable, so no
				// sender will ever drain it.
				if qErr := s.quarantineStandaloneOutboxFile(entry.Name(), err.Error()); qErr != nil {
					return nil, qErr
				}
				continue
			}
			if qErr := s.quarantineStandaloneOutboxFile(entry.Name(), err.Error()); qErr != nil {
				return nil, qErr
			}
			continue
		}
		if prune && (envelope.Record.DeliveryState == TerminalStatusDelivered ||
			envelope.Record.DeliveryState == TerminalStatusDeadLetter) {
			settledMS := envelope.Record.NextAttemptAt.UnixMilli()
			if nowMS-settledMS >= StandaloneOutboxRetention.Milliseconds() {
				if err := os.Remove(s.standaloneOutboxPath(envelope.OutboxKey)); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return nil, fmt.Errorf("runtime/workarea: prune standalone outbox %s: %w", envelope.OutboxKey, err)
				}
				continue
			}
		}
		envelopes = append(envelopes, *envelope)
	}
	if prune {
		if err := syncDir(dir); err != nil {
			return nil, err
		}
	}
	return envelopes, nil
}

// reconcileStandaloneOutbox returns interrupted in-flight sends to pending so
// a daemon restart replays rather than strands them. It runs at store open,
// before the store reports ready. It shares the quarantine-and-prune scan
// with the replay path so a bad file cannot fail the open (which would fail
// daemon start and worker preflight), and terminal records past retention
// are pruned before the store reports ready.
func (s *LeaseStore) reconcileStandaloneOutbox() error {
	envelopes, err := s.scanStandaloneOutbox(true)
	if err != nil {
		return err
	}
	for i := range envelopes {
		if envelopes[i].Record.DeliveryState == TerminalStatusAttempting {
			envelopes[i].Record.DeliveryState = TerminalStatusPending
			if err := s.writeStandaloneOutbox(envelopes[i]); err != nil {
				return err
			}
		}
	}
	return nil
}
