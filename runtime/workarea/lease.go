// Package workarea owns provider-neutral workarea lifecycle primitives.
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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// DefaultSettlementBudget is the complete terminal settlement window the
	// default lease must outlive, including verification, retries, and delivery.
	DefaultSettlementBudget = 10 * time.Minute
	// DefaultLeaseSafetyMargin separates expiry from the settlement budget.
	DefaultLeaseSafetyMargin = time.Minute
	// DefaultLeaseDuration is the initial bounded terminal workarea hold.
	DefaultLeaseDuration = 30 * time.Minute
	// DefaultMaxLeaseDuration is the absolute renewal ceiling fixed at acquire.
	DefaultMaxLeaseDuration = 2 * time.Hour
	// DefaultReaperInterval bounds how often expired leases are considered.
	DefaultReaperInterval = 30 * time.Second
	// DefaultReaperBatchSize bounds one reaper pass.
	DefaultReaperBatchSize = 32
	// DefaultReleaseAttemptTimeout bounds one provider release attempt.
	DefaultReleaseAttemptTimeout = 30 * time.Second
	// DefaultReleaseRetryBase is the initial provider release retry delay.
	DefaultReleaseRetryBase = 30 * time.Second
	// DefaultReleaseRetryMax caps provider release retry backoff.
	DefaultReleaseRetryMax = 5 * time.Minute
	// DefaultTerminalResultReplayInterval bounds restart replay cadence.
	DefaultTerminalResultReplayInterval = 5 * time.Second
	// DefaultTerminalResultReplayBatchSize bounds one outbox replay pass.
	DefaultTerminalResultReplayBatchSize = 32
	// DefaultTerminalResultReplayAttemptTimeout bounds one terminal status post.
	DefaultTerminalResultReplayAttemptTimeout = 30 * time.Second
	// DefaultTerminalResultReplayRetryBase is the first durable post retry delay.
	DefaultTerminalResultReplayRetryBase = time.Second
	// DefaultTerminalResultReplayRetryMax caps durable post retry backoff.
	DefaultTerminalResultReplayRetryMax = time.Minute

	lockRetryInterval = 10 * time.Millisecond

	actionableIndexSchemaV2      = "donmai.terminal-workarea-lease-actionable-index.v2"
	actionableAuthoritySchemaV1  = "donmai.terminal-workarea-lease-actionable-authority.v1"
	actionableStateSchemaV1      = "donmai.terminal-workarea-lease-actionable-state.v1"
	actionableMutationSchemaV1   = "donmai.terminal-workarea-lease-mutation.v1"
	actionableIndexDirName       = "actionable"
	actionableAuthorityDirName   = "actionable-authority"
	actionableIndexMarker        = "index.json"
	actionableStateFileName      = "actionable-state.json"
	actionableMutationFileName   = "actionable-mutation.json"
	actionableIndexLockKey       = "store:actionable-index"
	actionableIndexEntryExt      = ".json"
	actionableRebuildMaxAttempts = 3
)

// LeaseState is the durable terminal workarea lease state.
type LeaseState string

// Terminal lease lifecycle states.
const (
	LeaseActive         LeaseState = "active"
	LeaseAcknowledged   LeaseState = "acknowledged"
	LeaseExpired        LeaseState = "expired"
	LeaseReleasePending LeaseState = "release-pending"
	LeaseReleased       LeaseState = "released"
)

// TerminalResultPostState is the durable terminal status outbox state.
type TerminalResultPostState string

// Terminal result outbox lifecycle states.
const (
	TerminalResultPostPending  TerminalResultPostState = "pending"
	TerminalResultPostObserved TerminalResultPostState = "observed"
	TerminalResultPostExpired  TerminalResultPostState = "expired"
)

var (
	// ErrWorkareaLeased reports an exclusive lease owned by another terminal
	// result or session.
	ErrWorkareaLeased = errors.New("runtime/workarea: workarea already leased")
	// ErrLeaseConflict reports an idempotency-key replay with different identity.
	ErrLeaseConflict = errors.New("runtime/workarea: terminal lease identity conflict")
	// ErrLeaseNotFound reports an unknown lease id.
	ErrLeaseNotFound = errors.New("runtime/workarea: terminal lease not found")
	// ErrAcknowledgementRequired reports a non-semantic release attempt.
	ErrAcknowledgementRequired = errors.New("runtime/workarea: explicit terminal result acknowledgement required")
	// ErrLeaseExpired reports a renewal attempted after expiry.
	ErrLeaseExpired = errors.New("runtime/workarea: terminal lease expired")
	// ErrReleaseRequired reports an acknowledgement or reap without the normal
	// provider release policy needed to keep the workarea unavailable until the
	// disposition completes.
	ErrReleaseRequired = errors.New("runtime/workarea: workarea releaser required")
	// ErrLeaseExecutionRequired reports acknowledgement without a durable
	// invocation/claim owner for the exclusive verification execution.
	ErrLeaseExecutionRequired = errors.New("runtime/workarea: terminal lease execution claim required")
	// ErrLeaseExecutionClaimed reports a competing invocation/claim owner.
	ErrLeaseExecutionClaimed = errors.New("runtime/workarea: terminal lease execution already claimed")
	// ErrLeaseExecutionConflict reports acknowledgement by an identity other than
	// the durable exclusive execution owner.
	ErrLeaseExecutionConflict = errors.New("runtime/workarea: terminal lease execution identity conflict")
	// ErrTerminalResultPostNotFound reports a lease with no durable terminal
	// result outbox entry.
	ErrTerminalResultPostNotFound = errors.New("runtime/workarea: terminal result post not found")
)

// LeasePolicy declares the finite settlement and renewal bounds.
type LeasePolicy struct {
	SettlementBudget time.Duration
	SafetyMargin     time.Duration
	LeaseDuration    time.Duration
	MaxLeaseDuration time.Duration
}

// DefaultLeasePolicy returns the OSS defaults.
func DefaultLeasePolicy() LeasePolicy {
	return LeasePolicy{
		SettlementBudget: DefaultSettlementBudget,
		SafetyMargin:     DefaultLeaseSafetyMargin,
		LeaseDuration:    DefaultLeaseDuration,
		MaxLeaseDuration: DefaultMaxLeaseDuration,
	}
}

// AcquireSpec identifies one terminal result attempt and its exact workarea.
type AcquireSpec struct {
	SessionID        string
	TerminalResultID string
	WorkareaID       string
	WorkareaPath     string
	Policy           LeasePolicy
	ReleaseMetadata  map[string]string
	// ReleaseRequested atomically records the originating session's ordinary
	// teardown intent with lease acquisition, before any terminal status post.
	ReleaseRequested bool
	// TerminalResultPayload is the provider-neutral logical terminal result
	// persisted atomically with the lease. The poster callback interprets it;
	// the lease store treats it as opaque JSON.
	TerminalResultPayload json.RawMessage
}

// ExecutionClaimSpec identifies the invocation/claim pair that exclusively owns
// workarea-backed verification for one lease.
type ExecutionClaimSpec struct {
	LeaseID          string
	SessionID        string
	TerminalResultID string
	WorkareaID       string
	InvocationID     string
	ClaimID          string
}

// LeaseExecutionClaim is the durable exclusive verifier ownership record.
type LeaseExecutionClaim struct {
	InvocationID string    `json:"invocationId"`
	ClaimID      string    `json:"claimId"`
	ClaimedAt    time.Time `json:"claimedAt"`
}

// TerminalResultPost is the durable bounded terminal-status outbox entry.
type TerminalResultPost struct {
	State         TerminalResultPostState `json:"state"`
	Payload       json.RawMessage         `json:"payload"`
	CreatedAt     time.Time               `json:"createdAt"`
	ExpiresAt     time.Time               `json:"expiresAt"`
	Attempts      int                     `json:"attempts,omitempty"`
	LastAttemptAt *time.Time              `json:"lastAttemptAt,omitempty"`
	NextAttemptAt *time.Time              `json:"nextAttemptAt,omitempty"`
	LastError     string                  `json:"lastError,omitempty"`
	ObservedAt    *time.Time              `json:"observedAt,omitempty"`
	ExpiredAt     *time.Time              `json:"expiredAt,omitempty"`
}

// RenewSpec identifies the only owner allowed to renew a lease.
type RenewSpec struct {
	LeaseID          string
	SessionID        string
	TerminalResultID string
	WorkareaID       string
	Duration         time.Duration
}

// TerminalResultAcknowledgement is the later semantic verification
// acknowledgement. A transport receipt is not this acknowledgement.
type TerminalResultAcknowledgement struct {
	SchemaVersion    string `json:"schemaVersion"`
	Acknowledged     bool   `json:"acknowledged"`
	InvocationID     string `json:"invocationId"`
	ClaimID          string `json:"claimId"`
	LeaseID          string `json:"leaseId"`
	SessionID        string `json:"sessionId"`
	TerminalResultID string `json:"terminalResultId"`
	WorkareaID       string `json:"workareaId"`
}

// TerminalLease is the crash-recoverable lease record.
type TerminalLease struct {
	LeaseID            string               `json:"leaseId"`
	SessionID          string               `json:"sessionId"`
	TerminalResultID   string               `json:"terminalResultId"`
	WorkareaID         string               `json:"workareaId"`
	WorkareaPath       string               `json:"workareaPath"`
	AcquiredAt         time.Time            `json:"acquiredAt"`
	ExpiresAt          time.Time            `json:"expiresAt"`
	MaxExpiresAt       time.Time            `json:"maxExpiresAt"`
	SettlementBudget   time.Duration        `json:"settlementBudget"`
	State              LeaseState           `json:"state"`
	ExecutionClaim     *LeaseExecutionClaim `json:"executionClaim,omitempty"`
	TerminalResultPost *TerminalResultPost  `json:"terminalResultPost,omitempty"`
	ReleaseRequested   bool                 `json:"releaseRequested,omitempty"`
	AcknowledgedAt     *time.Time           `json:"acknowledgedAt,omitempty"`
	ReleasedAt         *time.Time           `json:"releasedAt,omitempty"`
	ReleaseAttempts    int                  `json:"releaseAttempts,omitempty"`
	NextReleaseAttempt *time.Time           `json:"nextReleaseAttempt,omitempty"`
	LastReleaseError   string               `json:"lastReleaseError,omitempty"`
	ReleaseMetadata    map[string]string    `json:"releaseMetadata,omitempty"`
}

// RetainsWorkarea reports whether release/archive/reuse/delete must remain
// blocked for this record.
func (l TerminalLease) RetainsWorkarea() bool {
	return l.State != LeaseReleased
}

// StoreOptions configure a durable LeaseStore.
type StoreOptions struct {
	Dir string
	Now func() time.Time
}

type leaseStoreDependencies struct {
	readFile func(string) ([]byte, error)
}

type actionableState struct {
	SchemaVersion          string `json:"schemaVersion"`
	Generation             uint64 `json:"generation"`
	EntryCount             int    `json:"entryCount"`
	EntriesDigest          string `json:"entriesDigest"`
	RecordsModTimeUnixNano int64  `json:"recordsModTimeUnixNano"`
}

type actionableIndexMetadata struct {
	SchemaVersion string `json:"schemaVersion"`
	Generation    uint64 `json:"generation"`
	EntryCount    int    `json:"entryCount"`
	EntriesDigest string `json:"entriesDigest"`
}

type actionableMutation struct {
	SchemaVersion      string `json:"schemaVersion"`
	PreviousGeneration uint64 `json:"previousGeneration"`
	TargetGeneration   uint64 `json:"targetGeneration"`
	LeaseID            string `json:"leaseId"`
}

type actionableIndexEntry struct {
	SchemaVersion string `json:"schemaVersion"`
	LeaseID       string `json:"leaseId"`
}

// LeaseStore persists one JSON record per terminal result and uses short-lived
// per-identity filesystem locks, so independent workareas remain parallel even
// when separate worker processes share the same host state directory.
type LeaseStore struct {
	dir                string
	records            string
	locks              string
	actionableIndex     string
	actionableAuthority string
	actionableState     string
	actionableMutation  string
	now                func() time.Time
	readFile           func(string) ([]byte, error)
}

// NewLeaseStore opens or creates a crash-recoverable terminal lease store.
func NewLeaseStore(opts StoreOptions) (*LeaseStore, error) {
	return newLeaseStore(opts, leaseStoreDependencies{})
}

func newLeaseStore(opts StoreOptions, deps leaseStoreDependencies) (*LeaseStore, error) {
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, errors.New("runtime/workarea: lease store directory required")
	}
	abs, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("runtime/workarea: resolve lease store: %w", err)
	}
	s := &LeaseStore{
		dir:                abs,
		records:            filepath.Join(abs, "records"),
		locks:              filepath.Join(abs, "locks"),
		actionableIndex:     filepath.Join(abs, actionableIndexDirName),
		actionableAuthority: filepath.Join(abs, actionableAuthorityDirName),
		actionableState:     filepath.Join(abs, actionableStateFileName),
		actionableMutation:  filepath.Join(abs, actionableMutationFileName),
		now:                opts.Now,
		readFile:           deps.readFile,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.readFile == nil {
		s.readFile = os.ReadFile
	}
	if err := os.MkdirAll(s.records, 0o750); err != nil {
		return nil, fmt.Errorf("runtime/workarea: create lease records: %w", err)
	}
	if err := os.MkdirAll(s.locks, 0o750); err != nil {
		return nil, fmt.Errorf("runtime/workarea: create lease locks: %w", err)
	}
	if err := os.MkdirAll(s.actionableIndex, 0o750); err != nil {
		return nil, fmt.Errorf("runtime/workarea: create actionable lease index: %w", err)
	}
	if err := os.MkdirAll(s.actionableAuthority, 0o750); err != nil {
		return nil, fmt.Errorf("runtime/workarea: create actionable lease authority: %w", err)
	}
	if err := s.withLocks(context.Background(), []string{actionableIndexLockKey}, s.recoverActionableIndexLocked); err != nil {
		return nil, fmt.Errorf("runtime/workarea: recover actionable lease index: %w", err)
	}
	return s, nil
}

// Dir returns the durable store root.
func (s *LeaseStore) Dir() string { return s.dir }

// Acquire atomically creates or idempotently returns a terminal lease.
func (s *LeaseStore) Acquire(ctx context.Context, spec AcquireSpec) (*TerminalLease, error) {
	policy := spec.Policy
	if policy == (LeasePolicy{}) {
		policy = DefaultLeasePolicy()
	}
	if err := validateAcquireSpec(spec, policy); err != nil {
		return nil, err
	}
	leaseID := leaseIDFor(spec.TerminalResultID)
	keys := []string{"result:" + spec.TerminalResultID, "workarea:" + spec.WorkareaID}

	var out *TerminalLease
	err := s.withLocks(ctx, keys, func() error {
		if existing, err := s.load(leaseID); err == nil {
			if err := validateIdentity(*existing, spec.SessionID, spec.TerminalResultID, spec.WorkareaID); err != nil {
				return err
			}
			needsSave := false
			if len(spec.TerminalResultPayload) > 0 {
				switch {
				case existing.TerminalResultPost == nil && existing.State == LeaseActive:
					existing.TerminalResultPost = newTerminalResultPost(spec.TerminalResultPayload, existing.AcquiredAt, existing.ExpiresAt)
					needsSave = true
				case existing.TerminalResultPost == nil:
					return fmt.Errorf("%w: cannot add terminal result payload to %s lease %s", ErrLeaseConflict, existing.State, existing.LeaseID)
				case !equalTerminalResultPayload(existing.TerminalResultPost.Payload, spec.TerminalResultPayload):
					return fmt.Errorf("%w: terminal result payload differs for lease %s", ErrLeaseConflict, existing.LeaseID)
				}
			}
			if spec.ReleaseRequested && !existing.ReleaseRequested {
				existing.ReleaseRequested = true
				needsSave = true
			}
			if needsSave {
				if err := s.save(ctx, *existing); err != nil {
					return err
				}
			}
			out = existing
			return nil
		} else if !errors.Is(err, ErrLeaseNotFound) {
			return err
		}

		leases, err := s.listActionable(ctx)
		if err != nil {
			return err
		}
		for i := range leases {
			other := leases[i]
			if other.WorkareaID == spec.WorkareaID && other.RetainsWorkarea() {
				return fmt.Errorf("%w: %s owned by session %s", ErrWorkareaLeased, spec.WorkareaID, other.SessionID)
			}
		}

		now := s.now().UTC()
		lease := &TerminalLease{
			LeaseID:          leaseID,
			SessionID:        spec.SessionID,
			TerminalResultID: spec.TerminalResultID,
			WorkareaID:       spec.WorkareaID,
			WorkareaPath:     spec.WorkareaPath,
			AcquiredAt:       now,
			ExpiresAt:        now.Add(policy.LeaseDuration),
			MaxExpiresAt:     now.Add(policy.MaxLeaseDuration),
			SettlementBudget: policy.SettlementBudget,
			State:            LeaseActive,
			ReleaseRequested: spec.ReleaseRequested,
			ReleaseMetadata:  cloneMetadata(spec.ReleaseMetadata),
		}
		if len(spec.TerminalResultPayload) > 0 {
			lease.TerminalResultPost = newTerminalResultPost(spec.TerminalResultPayload, now, lease.ExpiresAt)
		}
		if err := s.save(ctx, *lease); err != nil {
			return err
		}
		out = lease
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ClaimExecution durably and exclusively binds a lease to one verification
// invocation/claim pair. Replaying the same pair is idempotent; a competing pair
// is rejected and cannot access or release the workarea.
func (s *LeaseStore) ClaimExecution(ctx context.Context, spec ExecutionClaimSpec) (*TerminalLease, error) {
	if err := validateExecutionClaimSpec(spec); err != nil {
		return nil, err
	}
	var out *TerminalLease
	err := s.withLocks(ctx, []string{"result:" + spec.TerminalResultID, "workarea:" + spec.WorkareaID}, func() error {
		lease, err := s.load(spec.LeaseID)
		if err != nil {
			return err
		}
		if err := validateIdentity(*lease, spec.SessionID, spec.TerminalResultID, spec.WorkareaID); err != nil {
			return err
		}
		now := s.now().UTC()
		if lease.State != LeaseActive || !now.Before(lease.ExpiresAt) {
			return ErrLeaseExpired
		}
		if lease.ExecutionClaim != nil {
			if lease.ExecutionClaim.InvocationID != spec.InvocationID || lease.ExecutionClaim.ClaimID != spec.ClaimID {
				return fmt.Errorf("%w: lease %s owned by invocation %s", ErrLeaseExecutionClaimed, lease.LeaseID, lease.ExecutionClaim.InvocationID)
			}
			if lease.TerminalResultPost != nil && lease.TerminalResultPost.State == TerminalResultPostPending {
				observeTerminalResultPost(lease, now)
				if err := s.save(ctx, *lease); err != nil {
					return err
				}
			}
			out = lease
			return nil
		}
		lease.ExecutionClaim = &LeaseExecutionClaim{
			InvocationID: spec.InvocationID,
			ClaimID:      spec.ClaimID,
			ClaimedAt:    now,
		}
		// A correlated platform claim is itself durable evidence that the
		// terminal status was observed, even if the original worker crashed before
		// recording the successful HTTP response locally.
		observeTerminalResultPost(lease, now)
		if err := s.save(ctx, *lease); err != nil {
			return err
		}
		out = lease
		return nil
	})
	return out, err
}

// Renew extends an active lease for the same session and terminal result, but
// never past the absolute maximum fixed during acquisition.
func (s *LeaseStore) Renew(ctx context.Context, spec RenewSpec) (*TerminalLease, error) {
	if spec.Duration <= 0 {
		return nil, errors.New("runtime/workarea: renewal duration must be positive")
	}
	var out *TerminalLease
	err := s.withLocks(ctx, []string{"result:" + spec.TerminalResultID, "workarea:" + spec.WorkareaID}, func() error {
		lease, err := s.load(spec.LeaseID)
		if err != nil {
			return err
		}
		if err := validateIdentity(*lease, spec.SessionID, spec.TerminalResultID, spec.WorkareaID); err != nil {
			return err
		}
		now := s.now().UTC()
		if lease.State != LeaseActive || !now.Before(lease.ExpiresAt) {
			return ErrLeaseExpired
		}
		next := now.Add(spec.Duration)
		if next.After(lease.MaxExpiresAt) {
			return fmt.Errorf("runtime/workarea: renewal exceeds max expiry %s", lease.MaxExpiresAt.Format(time.RFC3339Nano))
		}
		if !next.After(lease.ExpiresAt) {
			out = lease
			return nil
		}
		lease.ExpiresAt = next
		if lease.TerminalResultPost != nil && lease.TerminalResultPost.State == TerminalResultPostPending {
			lease.TerminalResultPost.ExpiresAt = next
		}
		if err := s.save(ctx, *lease); err != nil {
			return err
		}
		out = lease
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Retained reports whether an active, acknowledged, expired, or release-pending
// lease still excludes teardown, archive, reuse, and deletion of the workarea.
// Read errors are returned so lifecycle callers can fail closed.
func (s *LeaseStore) Retained(workareaID string) (bool, error) {
	if strings.TrimSpace(workareaID) == "" {
		return false, errors.New("runtime/workarea: workarea id required")
	}
	leases, err := s.listActionable(context.Background())
	if err != nil {
		return false, err
	}
	for i := range leases {
		if leases[i].WorkareaID == workareaID && leases[i].RetainsWorkarea() {
			return true, nil
		}
	}
	return false, nil
}

// RequestRelease records that the normal workarea disposition was requested.
// It returns retained=true while any matching lease still blocks that action.
func (s *LeaseStore) RequestRelease(ctx context.Context, workareaID string) (retained bool, err error) {
	if strings.TrimSpace(workareaID) == "" {
		return false, errors.New("runtime/workarea: workarea id required")
	}
	err = s.withLocks(ctx, []string{"workarea:" + workareaID}, func() error {
		leases, listErr := s.listActionable(ctx)
		if listErr != nil {
			return listErr
		}
		for i := range leases {
			lease := leases[i]
			if lease.WorkareaID != workareaID || !lease.RetainsWorkarea() {
				continue
			}
			lease.ReleaseRequested = true
			if saveErr := s.save(ctx, lease); saveErr != nil {
				return saveErr
			}
			retained = true
			return nil
		}
		return nil
	})
	return retained, err
}

// MarkTerminalResultObserved records that the terminal status receiver durably
// observed the outbox payload. Replays of the same identity are idempotent.
func (s *LeaseStore) MarkTerminalResultObserved(ctx context.Context, leaseID, sessionID, terminalResultID, workareaID string) (*TerminalLease, error) {
	var out *TerminalLease
	err := s.withLocks(ctx, []string{"result:" + terminalResultID, "workarea:" + workareaID}, func() error {
		lease, err := s.load(leaseID)
		if err != nil {
			return err
		}
		if err := validateIdentity(*lease, sessionID, terminalResultID, workareaID); err != nil {
			return err
		}
		if lease.TerminalResultPost == nil {
			return ErrTerminalResultPostNotFound
		}
		if lease.TerminalResultPost.State == TerminalResultPostObserved {
			out = lease
			return nil
		}
		if lease.TerminalResultPost.State == TerminalResultPostExpired {
			return ErrLeaseExpired
		}
		now := s.now().UTC()
		lease.TerminalResultPost.State = TerminalResultPostObserved
		lease.TerminalResultPost.ObservedAt = &now
		lease.TerminalResultPost.NextAttemptAt = nil
		lease.TerminalResultPost.LastError = ""
		if err := s.save(ctx, *lease); err != nil {
			return err
		}
		out = lease
		return nil
	})
	return out, err
}

// TerminalResultPoster submits one opaque logical terminal result. A successful
// return means the receiver durably observed the status; ambiguous crashes may
// replay the same stable terminal-result identity.
type TerminalResultPoster func(context.Context, TerminalLease, json.RawMessage) error

// ReplayTerminalResults considers at most batchSize pending outbox records.
// Failed attempts remain pending with capped backoff. Records expire at the
// lease's finite expiry and are never posted after that bound.
func (s *LeaseStore) ReplayTerminalResults(ctx context.Context, batchSize int, attemptTimeout time.Duration, poster TerminalResultPoster) (int, error) {
	if poster == nil {
		return 0, errors.New("runtime/workarea: terminal result poster required")
	}
	if batchSize <= 0 {
		batchSize = DefaultTerminalResultReplayBatchSize
	}
	if attemptTimeout <= 0 {
		attemptTimeout = DefaultTerminalResultReplayAttemptTimeout
	}
	leases, err := s.listActionable(ctx)
	if err != nil {
		return 0, err
	}
	now := s.now().UTC()
	sort.Slice(leases, func(i, j int) bool {
		left, right := leases[i].TerminalResultPost, leases[j].TerminalResultPost
		if left == nil || right == nil {
			return left != nil
		}
		if left.Attempts != right.Attempts {
			return left.Attempts < right.Attempts
		}
		return left.CreatedAt.Before(right.CreatedAt)
	})
	eligible := make([]TerminalLease, 0, batchSize)
	for i := range leases {
		post := leases[i].TerminalResultPost
		if post == nil || post.State != TerminalResultPostPending {
			continue
		}
		if post.NextAttemptAt != nil && now.Before(*post.NextAttemptAt) && now.Before(post.ExpiresAt) {
			continue
		}
		eligible = append(eligible, leases[i])
		if len(eligible) == batchSize {
			break
		}
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(eligible))
	for i := range eligible {
		lease := eligible[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
			defer cancel()
			if replayErr := s.replayTerminalResult(attemptCtx, lease.LeaseID, lease.SessionID, lease.TerminalResultID, lease.WorkareaID, poster); replayErr != nil {
				errCh <- replayErr
			}
		}()
	}
	wg.Wait()
	close(errCh)
	errs := make([]error, 0, len(errCh))
	for replayErr := range errCh {
		errs = append(errs, replayErr)
	}
	return len(eligible), errors.Join(errs...)
}

// TerminalResultReplayOptions configure RunTerminalResultReplayer.
type TerminalResultReplayOptions struct {
	Interval       time.Duration
	BatchSize      int
	AttemptTimeout time.Duration
	OnError        func(error)
}

// RunTerminalResultReplayer replays pending outbox records until ctx is
// cancelled. It runs one recovery pass immediately on startup.
func (s *LeaseStore) RunTerminalResultReplayer(ctx context.Context, opts TerminalResultReplayOptions, poster TerminalResultPoster) {
	if opts.Interval <= 0 {
		opts.Interval = DefaultTerminalResultReplayInterval
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultTerminalResultReplayBatchSize
	}
	if opts.AttemptTimeout <= 0 {
		opts.AttemptTimeout = DefaultTerminalResultReplayAttemptTimeout
	}
	replay := func() {
		if _, err := s.ReplayTerminalResults(ctx, opts.BatchSize, opts.AttemptTimeout, poster); err != nil && opts.OnError != nil {
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

// Acknowledge applies the later semantic terminal-result acknowledgement. The
// matching lease moves through release-pending and releaser performs the normal
// provider disposition before the workarea stops being retained. Releaser must
// be idempotent because recovery can retry after a crash between provider
// release and the final durable state update.
func (s *LeaseStore) Acknowledge(ctx context.Context, ack TerminalResultAcknowledgement, releaser func(context.Context, TerminalLease) error) (*TerminalLease, error) {
	if ack.SchemaVersion != TerminalLeaseAcknowledgementSchemaV1 {
		return nil, fmt.Errorf("runtime/workarea: unsupported terminal lease acknowledgement schema %q", ack.SchemaVersion)
	}
	if !ack.Acknowledged {
		return nil, ErrAcknowledgementRequired
	}
	if strings.TrimSpace(ack.InvocationID) == "" || strings.TrimSpace(ack.ClaimID) == "" {
		return nil, ErrLeaseExecutionRequired
	}
	if releaser == nil {
		return nil, ErrReleaseRequired
	}
	return s.release(ctx, ack.LeaseID, ack.SessionID, ack.TerminalResultID, ack.WorkareaID, ack.InvocationID, ack.ClaimID, true, releaser)
}

// ReapExpired reclaims at most batchSize expired or release-pending records.
// Provider failures remain release-pending and therefore unavailable.
func (s *LeaseStore) ReapExpired(ctx context.Context, batchSize int, attemptTimeout time.Duration, releaser func(context.Context, TerminalLease) error) (int, error) {
	if releaser == nil {
		return 0, ErrReleaseRequired
	}
	if batchSize <= 0 {
		batchSize = DefaultReaperBatchSize
	}
	if attemptTimeout <= 0 {
		attemptTimeout = DefaultReleaseAttemptTimeout
	}
	leases, err := s.listActionable(ctx)
	if err != nil {
		return 0, err
	}
	now := s.now().UTC()
	sort.Slice(leases, func(i, j int) bool {
		if leases[i].ReleaseAttempts != leases[j].ReleaseAttempts {
			return leases[i].ReleaseAttempts < leases[j].ReleaseAttempts
		}
		if leases[i].ExpiresAt.Equal(leases[j].ExpiresAt) {
			return leases[i].LeaseID < leases[j].LeaseID
		}
		return leases[i].ExpiresAt.Before(leases[j].ExpiresAt)
	})

	eligible := make([]TerminalLease, 0, batchSize)
	for i := range leases {
		lease := leases[i]
		retryReady := lease.State == LeaseReleasePending &&
			(lease.NextReleaseAttempt == nil || !now.Before(*lease.NextReleaseAttempt))
		canRelease := (lease.State == LeaseActive && !now.Before(lease.ExpiresAt)) ||
			lease.State == LeaseAcknowledged || lease.State == LeaseExpired || retryReady
		if !canRelease {
			continue
		}
		eligible = append(eligible, lease)
		if len(eligible) >= batchSize {
			break
		}
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(eligible))
	for i := range eligible {
		lease := eligible[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
			defer cancel()
			_, releaseErr := s.release(attemptCtx, lease.LeaseID, lease.SessionID, lease.TerminalResultID, lease.WorkareaID, "", "", false, releaser)
			if releaseErr != nil {
				errCh <- releaseErr
			}
		}()
	}
	wg.Wait()
	close(errCh)

	errs := make([]error, 0, len(errCh))
	for releaseErr := range errCh {
		errs = append(errs, releaseErr)
	}
	return len(eligible), errors.Join(errs...)
}

// ReaperOptions configure RunReaper.
type ReaperOptions struct {
	Interval       time.Duration
	BatchSize      int
	AttemptTimeout time.Duration
	OnError        func(error)
}

// RunReaper periodically considers every expired lease within the configured
// capacity/batch bound until ctx is cancelled.
func (s *LeaseStore) RunReaper(ctx context.Context, opts ReaperOptions, releaser func(context.Context, TerminalLease) error) {
	if opts.Interval <= 0 {
		opts.Interval = DefaultReaperInterval
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultReaperBatchSize
	}
	if opts.AttemptTimeout <= 0 {
		opts.AttemptTimeout = DefaultReleaseAttemptTimeout
	}
	reap := func() {
		if _, err := s.ReapExpired(ctx, opts.BatchSize, opts.AttemptTimeout, releaser); err != nil && opts.OnError != nil {
			opts.OnError(err)
		}
	}
	reap()
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reap()
		}
	}
}

// Get returns one durable lease by id.
func (s *LeaseStore) Get(leaseID string) (*TerminalLease, error) { return s.load(leaseID) }

// List returns every durable lease sorted by lease id.
func (s *LeaseStore) List() ([]TerminalLease, error) {
	entries, err := os.ReadDir(s.records)
	if err != nil {
		return nil, fmt.Errorf("read lease records: %w", err)
	}
	leases := make([]TerminalLease, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		lease, err := s.load(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		leases = append(leases, *lease)
	}
	sort.Slice(leases, func(i, j int) bool { return leases[i].LeaseID < leases[j].LeaseID })
	return leases, nil
}

func (s *LeaseStore) release(ctx context.Context, leaseID, sessionID, terminalResultID, workareaID, invocationID, claimID string, acknowledged bool, releaser func(context.Context, TerminalLease) error) (*TerminalLease, error) {
	var out *TerminalLease
	err := s.withLocks(ctx, []string{"result:" + terminalResultID, "workarea:" + workareaID}, func() error {
		lease, err := s.load(leaseID)
		if err != nil {
			return err
		}
		if err := validateIdentity(*lease, sessionID, terminalResultID, workareaID); err != nil {
			return err
		}
		if acknowledged {
			if err := validateExecutionIdentity(*lease, invocationID, claimID); err != nil {
				return err
			}
		}
		if lease.State == LeaseReleased {
			out = lease
			return nil
		}

		now := s.now().UTC()
		if acknowledged {
			if lease.AcknowledgedAt == nil && !now.Before(lease.ExpiresAt) {
				return ErrLeaseExpired
			}
			if lease.AcknowledgedAt == nil {
				lease.AcknowledgedAt = &now
			}
			observeTerminalResultPost(lease, now)
		} else if lease.State == LeaseActive {
			expireTerminalResultPost(lease, now)
		}

		// Persist release-pending before invoking provider policy. The previous
		// error remains durable until this callback produces a new outcome.
		// Holding the per-result and per-workarea locks across the callback makes
		// concurrent acknowledgement/reap attempts idempotent while unrelated
		// workareas continue independently.
		lease.State = LeaseReleasePending
		lease.ReleaseAttempts++
		lease.NextReleaseAttempt = nil
		if err := s.save(ctx, *lease); err != nil {
			return err
		}

		releaseErr := releaser(ctx, *lease)
		now = s.now().UTC()
		if releaseErr != nil {
			nextAttempt := now.Add(releaseRetryDelay(lease.ReleaseAttempts))
			lease.NextReleaseAttempt = &nextAttempt
			lease.LastReleaseError = releaseErr.Error()
		} else {
			lease.State = LeaseReleased
			lease.ReleasedAt = &now
			lease.NextReleaseAttempt = nil
			lease.LastReleaseError = ""
		}
		if err := s.save(ctx, *lease); err != nil {
			return err
		}
		out = lease
		if releaseErr != nil {
			return fmt.Errorf("runtime/workarea: release %s: %w", leaseID, releaseErr)
		}
		return nil
	})
	return out, err
}

func (s *LeaseStore) replayTerminalResult(ctx context.Context, leaseID, sessionID, terminalResultID, workareaID string, poster TerminalResultPoster) error {
	return s.withLocks(ctx, []string{"result:" + terminalResultID, "workarea:" + workareaID}, func() error {
		lease, err := s.load(leaseID)
		if err != nil {
			return err
		}
		if err := validateIdentity(*lease, sessionID, terminalResultID, workareaID); err != nil {
			return err
		}
		post := lease.TerminalResultPost
		if post == nil || post.State != TerminalResultPostPending {
			return nil
		}
		now := s.now().UTC()
		if !now.Before(post.ExpiresAt) || !now.Before(lease.ExpiresAt) {
			expireTerminalResultPost(lease, now)
			return s.save(ctx, *lease)
		}
		if post.NextAttemptAt != nil && now.Before(*post.NextAttemptAt) {
			return nil
		}

		post.Attempts++
		post.LastAttemptAt = &now
		nextAttempt := now.Add(terminalResultReplayRetryDelay(post.Attempts))
		post.NextAttemptAt = &nextAttempt
		// Preserve the previous audit error until the poster produces a new
		// durable outcome. A crash at callback entry must not erase evidence.
		if err := s.save(ctx, *lease); err != nil {
			return err
		}

		postCtx, cancel := context.WithDeadline(ctx, post.ExpiresAt)
		postErr := poster(postCtx, *lease, append(json.RawMessage(nil), post.Payload...))
		cancel()
		now = s.now().UTC()
		switch {
		case postErr != nil && !now.Before(post.ExpiresAt):
			expireTerminalResultPost(lease, now)
		case postErr != nil:
			post.LastError = postErr.Error()
		default:
			observeTerminalResultPost(lease, now)
		}
		if err := s.save(ctx, *lease); err != nil {
			return err
		}
		if postErr != nil {
			return fmt.Errorf("runtime/workarea: replay terminal result %s: %w", terminalResultID, postErr)
		}
		return nil
	})
}

func equalTerminalResultPayload(left, right json.RawMessage) bool {
	var compactLeft, compactRight bytes.Buffer
	if err := json.Compact(&compactLeft, left); err != nil {
		return false
	}
	if err := json.Compact(&compactRight, right); err != nil {
		return false
	}
	return bytes.Equal(compactLeft.Bytes(), compactRight.Bytes())
}

func newTerminalResultPost(payload json.RawMessage, createdAt, expiresAt time.Time) *TerminalResultPost {
	return &TerminalResultPost{
		State:     TerminalResultPostPending,
		Payload:   append(json.RawMessage(nil), payload...),
		CreatedAt: createdAt.UTC(),
		ExpiresAt: expiresAt.UTC(),
	}
}

func observeTerminalResultPost(lease *TerminalLease, now time.Time) {
	if lease.TerminalResultPost == nil || lease.TerminalResultPost.State != TerminalResultPostPending {
		return
	}
	lease.TerminalResultPost.State = TerminalResultPostObserved
	lease.TerminalResultPost.ObservedAt = &now
	lease.TerminalResultPost.NextAttemptAt = nil
	lease.TerminalResultPost.LastError = ""
}

func expireTerminalResultPost(lease *TerminalLease, now time.Time) {
	if lease.TerminalResultPost == nil || lease.TerminalResultPost.State != TerminalResultPostPending {
		return
	}
	lease.TerminalResultPost.State = TerminalResultPostExpired
	lease.TerminalResultPost.ExpiredAt = &now
	lease.TerminalResultPost.NextAttemptAt = nil
}

func terminalResultReplayRetryDelay(attempts int) time.Duration {
	delay := DefaultTerminalResultReplayRetryBase
	for attempt := 1; attempt < attempts && delay < DefaultTerminalResultReplayRetryMax; attempt++ {
		delay *= 2
		if delay >= DefaultTerminalResultReplayRetryMax {
			return DefaultTerminalResultReplayRetryMax
		}
	}
	return delay
}

func releaseRetryDelay(attempts int) time.Duration {
	delay := DefaultReleaseRetryBase
	for attempt := 1; attempt < attempts && delay < DefaultReleaseRetryMax; attempt++ {
		delay *= 2
		if delay >= DefaultReleaseRetryMax {
			return DefaultReleaseRetryMax
		}
	}
	return delay
}

func validateAcquireSpec(spec AcquireSpec, policy LeasePolicy) error {
	switch {
	case strings.TrimSpace(spec.SessionID) == "":
		return errors.New("runtime/workarea: session id required")
	case strings.TrimSpace(spec.TerminalResultID) == "":
		return errors.New("runtime/workarea: terminal result id required")
	case strings.TrimSpace(spec.WorkareaID) == "":
		return errors.New("runtime/workarea: workarea id required")
	case strings.TrimSpace(spec.WorkareaPath) == "":
		return errors.New("runtime/workarea: workarea path required")
	case !filepath.IsAbs(spec.WorkareaPath):
		return errors.New("runtime/workarea: workarea path must be absolute")
	case policy.SettlementBudget <= 0:
		return errors.New("runtime/workarea: settlement budget must be positive")
	case policy.SafetyMargin < 0:
		return errors.New("runtime/workarea: safety margin must not be negative")
	case policy.LeaseDuration <= policy.SettlementBudget+policy.SafetyMargin:
		return errors.New("runtime/workarea: lease duration must exceed settlement budget plus safety margin")
	case policy.MaxLeaseDuration < policy.LeaseDuration:
		return errors.New("runtime/workarea: max lease duration must cover initial lease duration")
	case len(spec.TerminalResultPayload) > 0 && !json.Valid(spec.TerminalResultPayload):
		return errors.New("runtime/workarea: terminal result payload must be valid JSON")
	}
	return nil
}

func validateExecutionClaimSpec(spec ExecutionClaimSpec) error {
	switch {
	case strings.TrimSpace(spec.LeaseID) == "":
		return errors.New("runtime/workarea: lease id required")
	case strings.TrimSpace(spec.SessionID) == "":
		return errors.New("runtime/workarea: session id required")
	case strings.TrimSpace(spec.TerminalResultID) == "":
		return errors.New("runtime/workarea: terminal result id required")
	case strings.TrimSpace(spec.WorkareaID) == "":
		return errors.New("runtime/workarea: workarea id required")
	case strings.TrimSpace(spec.InvocationID) == "":
		return errors.New("runtime/workarea: invocation id required")
	case strings.TrimSpace(spec.ClaimID) == "":
		return errors.New("runtime/workarea: claim id required")
	}
	return nil
}

func validateIdentity(lease TerminalLease, sessionID, terminalResultID, workareaID string) error {
	if lease.SessionID != sessionID || lease.TerminalResultID != terminalResultID || lease.WorkareaID != workareaID {
		return fmt.Errorf("%w: lease %s", ErrLeaseConflict, lease.LeaseID)
	}
	return nil
}

func validateExecutionIdentity(lease TerminalLease, invocationID, claimID string) error {
	if lease.ExecutionClaim == nil {
		return ErrLeaseExecutionRequired
	}
	if lease.ExecutionClaim.InvocationID != invocationID || lease.ExecutionClaim.ClaimID != claimID {
		return fmt.Errorf("%w: lease %s", ErrLeaseExecutionConflict, lease.LeaseID)
	}
	return nil
}

func cloneMetadata(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func leaseIDFor(terminalResultID string) string {
	sum := sha256.Sum256([]byte(terminalResultID))
	return "twl_" + hex.EncodeToString(sum[:16])
}

func fileKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func actionableIndexDigest(leaseIDs []string) string {
	var digest [sha256.Size]byte
	for _, leaseID := range leaseIDs {
		leaseDigest := sha256.Sum256([]byte(leaseID))
		for i := range digest {
			digest[i] ^= leaseDigest[i]
		}
	}
	return hex.EncodeToString(digest[:])
}

func toggleActionableIndexDigest(digestHex, leaseID string) (string, error) {
	digest, err := hex.DecodeString(digestHex)
	if err != nil || len(digest) != sha256.Size {
		return "", errors.New("invalid actionable lease digest")
	}
	leaseDigest := sha256.Sum256([]byte(leaseID))
	for i := range digest {
		digest[i] ^= leaseDigest[i]
	}
	return hex.EncodeToString(digest), nil
}

func validActionableIndexDigest(digestHex string) bool {
	digest, err := hex.DecodeString(digestHex)
	return err == nil && len(digest) == sha256.Size
}

func (s *LeaseStore) recoverActionableIndexLocked() error {
	pending, err := s.actionableMutationPendingLocked()
	if err != nil {
		return err
	}
	if !pending {
		leaseIDs, readErr := s.readActionableIndexLocked()
		if readErr == nil {
			_, readErr = s.loadActionableLeasesLocked(context.Background(), leaseIDs)
		}
		if readErr == nil {
			return nil
		}
	}
	return s.rebuildActionableIndexLocked(context.Background())
}

func (s *LeaseStore) ensureActionableMetadataLocked(ctx context.Context) error {
	pending, err := s.actionableMutationPendingLocked()
	if err != nil {
		return err
	}
	if !pending {
		if _, _, readErr := s.readActionableMetadataLocked(); readErr == nil {
			return nil
		}
	}
	return s.rebuildActionableIndexLocked(ctx)
}

func (s *LeaseStore) actionableMutationPendingLocked() (bool, error) {
	_, err := os.Stat(s.actionableMutation)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("stat actionable lease mutation journal: %w", err)
	}
}

func (s *LeaseStore) listActionable(ctx context.Context) ([]TerminalLease, error) {
	var leases []TerminalLease
	err := s.withLocks(ctx, []string{actionableIndexLockKey}, func() error {
		leaseIDs, err := s.readActionableIndexLocked()
		if err == nil {
			leases, err = s.loadActionableLeasesLocked(ctx, leaseIDs)
		}
		if err == nil {
			return nil
		}
		if rebuildErr := s.rebuildActionableIndexLocked(ctx); rebuildErr != nil {
			return fmt.Errorf("rebuild actionable lease index after %v: %w", err, rebuildErr)
		}
		leaseIDs, err = s.readActionableIndexLocked()
		if err != nil {
			return err
		}
		leases, err = s.loadActionableLeasesLocked(ctx, leaseIDs)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("runtime/workarea: list actionable leases: %w", err)
	}
	return leases, nil
}

func (s *LeaseStore) loadActionableLeasesLocked(ctx context.Context, leaseIDs []string) ([]TerminalLease, error) {
	leases := make([]TerminalLease, 0, len(leaseIDs))
	for _, leaseID := range leaseIDs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lease, err := s.load(leaseID)
		if err != nil {
			return nil, err
		}
		if !lease.RetainsWorkarea() {
			return nil, fmt.Errorf("actionable lease index contains released lease %s", leaseID)
		}
		leases = append(leases, *lease)
	}
	return leases, nil
}

func (s *LeaseStore) readActionableIndexLocked() ([]string, error) {
	state, _, err := s.readActionableMetadataLocked()
	if err != nil {
		return nil, err
	}

	leaseIDs, err := s.readLeaseIDEntriesLocked(s.actionableIndex, actionableIndexSchemaV2, true)
	if err != nil {
		return nil, fmt.Errorf("read actionable lease index: %w", err)
	}
	authorityIDs, err := s.readLeaseIDEntriesLocked(s.actionableAuthority, actionableAuthoritySchemaV1, false)
	if err != nil {
		return nil, fmt.Errorf("read actionable lease authority: %w", err)
	}
	if len(leaseIDs) != state.EntryCount {
		return nil, fmt.Errorf("actionable lease index count mismatch: authority=%d entries=%d", state.EntryCount, len(leaseIDs))
	}
	if actionableIndexDigest(leaseIDs) != state.EntriesDigest {
		return nil, errors.New("actionable lease index digest mismatch")
	}
	if len(authorityIDs) != state.EntryCount || actionableIndexDigest(authorityIDs) != state.EntriesDigest {
		return nil, errors.New("actionable lease authority differs from authoritative state")
	}
	for i := range leaseIDs {
		if leaseIDs[i] != authorityIDs[i] {
			return nil, errors.New("actionable lease index differs from actionable authority")
		}
	}
	return leaseIDs, nil
}

func (s *LeaseStore) readLeaseIDEntriesLocked(dir, schemaVersion string, skipMarker bool) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	leaseIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || (skipMarker && name == actionableIndexMarker) {
			continue
		}
		if entry.IsDir() || filepath.Ext(name) != actionableIndexEntryExt {
			return nil, fmt.Errorf("unexpected lease identity entry %q", name)
		}
		entryData, readErr := s.readFile(filepath.Join(dir, name)) //nolint:gosec // name came from the configured directory.
		if readErr != nil {
			return nil, fmt.Errorf("read lease identity entry %q: %w", name, readErr)
		}
		var identity actionableIndexEntry
		if err := json.Unmarshal(entryData, &identity); err != nil {
			return nil, fmt.Errorf("decode lease identity entry %q: %w", name, err)
		}
		if identity.SchemaVersion != schemaVersion || !validLeaseID(identity.LeaseID) {
			return nil, fmt.Errorf("invalid lease identity entry %q", name)
		}
		if name != identity.LeaseID+actionableIndexEntryExt {
			return nil, fmt.Errorf("lease identity entry %q identity mismatch", name)
		}
		leaseIDs = append(leaseIDs, identity.LeaseID)
	}
	sort.Strings(leaseIDs)
	return leaseIDs, nil
}

func (s *LeaseStore) readActionableMetadataLocked() (actionableState, actionableIndexMetadata, error) {
	var state actionableState
	pending, err := s.actionableMutationPendingLocked()
	if err != nil {
		return state, actionableIndexMetadata{}, err
	}
	if pending {
		return state, actionableIndexMetadata{}, errors.New("actionable lease mutation journal is pending")
	}
	state, err = s.readActionableStateLocked()
	if err != nil {
		return state, actionableIndexMetadata{}, err
	}

	data, err := s.readFile(filepath.Join(s.actionableIndex, actionableIndexMarker)) //nolint:gosec // path is fixed under the configured store.
	if err != nil {
		return state, actionableIndexMetadata{}, fmt.Errorf("read actionable lease index marker: %w", err)
	}
	var metadata actionableIndexMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return state, actionableIndexMetadata{}, fmt.Errorf("decode actionable lease index marker: %w", err)
	}
	if metadata.SchemaVersion != actionableIndexSchemaV2 {
		return state, actionableIndexMetadata{}, fmt.Errorf("unsupported actionable lease index schema %q", metadata.SchemaVersion)
	}
	if metadata.Generation == 0 || metadata.EntryCount < 0 || !validActionableIndexDigest(metadata.EntriesDigest) {
		return state, actionableIndexMetadata{}, errors.New("invalid actionable lease index metadata")
	}
	if metadata.Generation != state.Generation || metadata.EntryCount != state.EntryCount || metadata.EntriesDigest != state.EntriesDigest {
		return state, actionableIndexMetadata{}, errors.New("actionable lease index differs from authoritative state")
	}
	return state, metadata, nil
}

func (s *LeaseStore) readActionableStateLocked() (actionableState, error) {
	var state actionableState
	data, err := s.readFile(s.actionableState) //nolint:gosec // path is fixed under the configured store.
	if err != nil {
		return state, fmt.Errorf("read authoritative actionable lease state: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("decode authoritative actionable lease state: %w", err)
	}
	if state.SchemaVersion != actionableStateSchemaV1 {
		return state, fmt.Errorf("unsupported actionable lease state schema %q", state.SchemaVersion)
	}
	if state.Generation == 0 || state.EntryCount < 0 || !validActionableIndexDigest(state.EntriesDigest) {
		return state, errors.New("invalid authoritative actionable lease state")
	}
	recordsInfo, err := os.Stat(s.records)
	if err != nil {
		return state, fmt.Errorf("stat lease records for actionable state: %w", err)
	}
	if recordsInfo.ModTime().UnixNano() != state.RecordsModTimeUnixNano {
		return state, errors.New("authoritative actionable lease record generation mismatch")
	}
	return state, nil
}

func (s *LeaseStore) rebuildActionableIndexLocked(ctx context.Context) error {
	baseGeneration := uint64(0)
	if state, err := s.readActionableStateFileLocked(); err == nil {
		baseGeneration = state.Generation
	}
	if baseGeneration == ^uint64(0) {
		return errors.New("runtime/workarea: actionable lease generation exhausted")
	}
	if err := s.markActionableIndexDirtyLocked(); err != nil {
		return err
	}

	for attempt := 1; attempt <= actionableRebuildMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		startInfo, err := os.Stat(s.records)
		if err != nil {
			return fmt.Errorf("stat lease records before actionable index rebuild: %w", err)
		}
		if err := s.clearActionableIndexEntriesLocked(); err != nil {
			return err
		}

		recordEntries, err := os.ReadDir(s.records)
		if err != nil {
			return fmt.Errorf("read lease records for actionable index rebuild: %w", err)
		}
		leaseIDs := make([]string, 0)
		for _, entry := range recordEntries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			leaseID := strings.TrimSuffix(entry.Name(), ".json")
			lease, loadErr := s.load(leaseID)
			if loadErr != nil {
				return loadErr
			}
			if !lease.RetainsWorkarea() {
				continue
			}
			if err := s.writeActionableIndexEntryForRebuildLocked(leaseID); err != nil {
				return err
			}
			if err := s.writeActionableAuthorityEntryForRebuildLocked(leaseID); err != nil {
				return err
			}
			leaseIDs = append(leaseIDs, leaseID)
		}
		if err := syncDir(s.actionableIndex); err != nil {
			return err
		}
		if err := syncDir(s.actionableAuthority); err != nil {
			return err
		}
		endInfo, err := os.Stat(s.records)
		if err != nil {
			return fmt.Errorf("stat lease records after actionable index rebuild: %w", err)
		}
		if startInfo.ModTime() != endInfo.ModTime() {
			continue
		}

		sort.Strings(leaseIDs)
		state := actionableState{
			SchemaVersion:          actionableStateSchemaV1,
			Generation:             baseGeneration + 1,
			EntryCount:             len(leaseIDs),
			EntriesDigest:          actionableIndexDigest(leaseIDs),
			RecordsModTimeUnixNano: endInfo.ModTime().UnixNano(),
		}
		if err := s.writeActionableStateLocked(state); err != nil {
			return err
		}
		if err := s.writeActionableIndexMetadataLocked(state); err != nil {
			return err
		}
		if err := s.clearActionableMutationLocked(); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("runtime/workarea: actionable lease index rebuild did not observe a stable record generation after %d attempts", actionableRebuildMaxAttempts)
}

func (s *LeaseStore) readActionableStateFileLocked() (actionableState, error) {
	var state actionableState
	data, err := s.readFile(s.actionableState) //nolint:gosec // path is fixed under the configured store.
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	if state.SchemaVersion != actionableStateSchemaV1 || state.Generation == 0 || state.EntryCount < 0 || !validActionableIndexDigest(state.EntriesDigest) {
		return state, errors.New("invalid authoritative actionable lease state")
	}
	return state, nil
}

func (s *LeaseStore) clearActionableIndexEntriesLocked() error {
	if err := clearLeaseIdentityEntries(s.actionableIndex, true); err != nil {
		return fmt.Errorf("clear actionable lease index: %w", err)
	}
	if err := clearLeaseIdentityEntries(s.actionableAuthority, false); err != nil {
		return fmt.Errorf("clear actionable lease authority: %w", err)
	}
	return nil
}

func clearLeaseIdentityEntries(dir string, preserveMarker bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if preserveMarker && entry.Name() == actionableIndexMarker {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("remove lease identity entry %q: %w", entry.Name(), err)
		}
	}
	return syncDir(dir)
}

func (s *LeaseStore) updateActionableIndexLocked(lease TerminalLease, previousRetained bool, state actionableState) (actionableState, error) {
	if state.Generation == ^uint64(0) {
		return state, errors.New("runtime/workarea: actionable lease generation exhausted")
	}
	state.Generation++
	nextRetained := lease.RetainsWorkarea()
	if previousRetained == nextRetained {
		return state, nil
	}
	digest, err := toggleActionableIndexDigest(state.EntriesDigest, lease.LeaseID)
	if err != nil {
		return state, err
	}
	state.EntriesDigest = digest

	entryPath := s.actionableIndexEntryPath(lease.LeaseID)
	if nextRetained {
		state.EntryCount++
		if err := s.writeActionableIndexEntryLocked(lease.LeaseID); err != nil {
			return state, err
		}
		if err := s.writeActionableAuthorityEntryLocked(lease.LeaseID); err != nil {
			return state, err
		}
	} else {
		state.EntryCount--
		if state.EntryCount < 0 {
			return state, errors.New("runtime/workarea: actionable lease count underflow")
		}
		if err := os.Remove(entryPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return state, fmt.Errorf("remove actionable lease index entry %s: %w", lease.LeaseID, err)
		}
		if err := os.Remove(s.actionableAuthorityEntryPath(lease.LeaseID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return state, fmt.Errorf("remove actionable lease authority entry %s: %w", lease.LeaseID, err)
		}
	}
	return state, nil
}

func (s *LeaseStore) writeActionableIndexEntryLocked(leaseID string) error {
	return writeLeaseIdentityEntryAtomic(s.actionableIndex, s.actionableIndexEntryPath(leaseID), actionableIndexSchemaV2, leaseID)
}

func (s *LeaseStore) writeActionableAuthorityEntryLocked(leaseID string) error {
	return writeLeaseIdentityEntryAtomic(s.actionableAuthority, s.actionableAuthorityEntryPath(leaseID), actionableAuthoritySchemaV1, leaseID)
}

func (s *LeaseStore) writeActionableIndexEntryForRebuildLocked(leaseID string) error {
	return writeLeaseIdentityEntryForRebuild(s.actionableIndexEntryPath(leaseID), actionableIndexSchemaV2, leaseID)
}

func (s *LeaseStore) writeActionableAuthorityEntryForRebuildLocked(leaseID string) error {
	return writeLeaseIdentityEntryForRebuild(s.actionableAuthorityEntryPath(leaseID), actionableAuthoritySchemaV1, leaseID)
}

func writeLeaseIdentityEntryAtomic(dir, path, schemaVersion, leaseID string) error {
	data, err := marshalLeaseIdentityEntry(schemaVersion, leaseID)
	if err != nil {
		return err
	}
	return writeFileAtomic(dir, path, ".actionable-entry-*.tmp", data)
}

func writeLeaseIdentityEntryForRebuild(path, schemaVersion, leaseID string) error {
	data, err := marshalLeaseIdentityEntry(schemaVersion, leaseID)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func marshalLeaseIdentityEntry(schemaVersion, leaseID string) ([]byte, error) {
	entry := actionableIndexEntry{SchemaVersion: schemaVersion, LeaseID: leaseID}
	data, err := json.Marshal(entry)
	if err != nil {
		return nil, fmt.Errorf("marshal actionable lease identity entry %s: %w", leaseID, err)
	}
	return data, nil
}

func (s *LeaseStore) writeActionableStateLocked(state actionableState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal authoritative actionable lease state: %w", err)
	}
	return writeFileAtomic(s.dir, s.actionableState, ".actionable-state-*.tmp", data)
}

func (s *LeaseStore) writeActionableIndexMetadataLocked(state actionableState) error {
	metadata := actionableIndexMetadata{
		SchemaVersion: actionableIndexSchemaV2,
		Generation:    state.Generation,
		EntryCount:    state.EntryCount,
		EntriesDigest: state.EntriesDigest,
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("marshal actionable lease index marker: %w", err)
	}
	return writeFileAtomic(s.actionableIndex, filepath.Join(s.actionableIndex, actionableIndexMarker), ".actionable-index-*.tmp", data)
}

func (s *LeaseStore) writeActionableMutationLocked(mutation actionableMutation) error {
	data, err := json.Marshal(mutation)
	if err != nil {
		return fmt.Errorf("marshal actionable lease mutation journal: %w", err)
	}
	return writeFileAtomic(s.dir, s.actionableMutation, ".actionable-mutation-*.tmp", data)
}

func (s *LeaseStore) clearActionableMutationLocked() error {
	if err := os.Remove(s.actionableMutation); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("clear actionable lease mutation journal: %w", err)
	}
	return syncDir(s.dir)
}

func (s *LeaseStore) markActionableIndexDirtyLocked() error {
	if err := os.Remove(filepath.Join(s.actionableIndex, actionableIndexMarker)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("mark actionable lease index dirty: %w", err)
	}
	return syncDir(s.actionableIndex)
}

func (s *LeaseStore) actionableIndexEntryPath(leaseID string) string {
	return filepath.Join(s.actionableIndex, leaseID+actionableIndexEntryExt)
}

func (s *LeaseStore) actionableAuthorityEntryPath(leaseID string) string {
	return filepath.Join(s.actionableAuthority, leaseID+actionableIndexEntryExt)
}

func validLeaseID(leaseID string) bool {
	return strings.TrimSpace(leaseID) != "" && !strings.ContainsAny(leaseID, `/\\`)
}

func (s *LeaseStore) recordPath(leaseID string) string {
	return filepath.Join(s.records, leaseID+".json")
}

func (s *LeaseStore) load(leaseID string) (*TerminalLease, error) {
	if strings.TrimSpace(leaseID) == "" || strings.ContainsAny(leaseID, `/\\`) {
		return nil, ErrLeaseNotFound
	}
	data, err := s.readFile(s.recordPath(leaseID)) //nolint:gosec // path is derived from a validated lease id under the configured store.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrLeaseNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read lease %s: %w", leaseID, err)
	}
	var lease TerminalLease
	if err := json.Unmarshal(data, &lease); err != nil {
		return nil, fmt.Errorf("decode lease %s: %w", leaseID, err)
	}
	if lease.LeaseID != leaseID {
		return nil, fmt.Errorf("decode lease %s: embedded id %q differs", leaseID, lease.LeaseID)
	}
	return &lease, nil
}

func (s *LeaseStore) save(ctx context.Context, lease TerminalLease) error {
	if !validLeaseID(lease.LeaseID) {
		return fmt.Errorf("runtime/workarea: invalid lease id %q", lease.LeaseID)
	}
	return s.withLocks(ctx, []string{actionableIndexLockKey}, func() error {
		if err := s.ensureActionableMetadataLocked(ctx); err != nil {
			return err
		}
		state, _, err := s.readActionableMetadataLocked()
		if err != nil {
			return err
		}
		previousRetained := false
		previous, loadErr := s.load(lease.LeaseID)
		switch {
		case loadErr == nil:
			previousRetained = previous.RetainsWorkarea()
		case errors.Is(loadErr, ErrLeaseNotFound):
		default:
			return loadErr
		}
		if state.Generation == ^uint64(0) {
			return errors.New("runtime/workarea: actionable lease generation exhausted")
		}
		mutation := actionableMutation{
			SchemaVersion:      actionableMutationSchemaV1,
			PreviousGeneration: state.Generation,
			TargetGeneration:   state.Generation + 1,
			LeaseID:            lease.LeaseID,
		}
		if err := s.writeActionableMutationLocked(mutation); err != nil {
			return err
		}
		data, err := json.MarshalIndent(lease, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal lease %s: %w", lease.LeaseID, err)
		}
		if err := writeFileAtomic(s.records, s.recordPath(lease.LeaseID), ".lease-*.tmp", data); err != nil {
			return fmt.Errorf("commit lease %s: %w", lease.LeaseID, err)
		}
		state, err = s.updateActionableIndexLocked(lease, previousRetained, state)
		if err != nil {
			return fmt.Errorf("update actionable lease index for %s: %w", lease.LeaseID, err)
		}
		recordsInfo, err := os.Stat(s.records)
		if err != nil {
			return fmt.Errorf("stat lease records after save: %w", err)
		}
		state.RecordsModTimeUnixNano = recordsInfo.ModTime().UnixNano()
		if err := s.writeActionableStateLocked(state); err != nil {
			return err
		}
		if err := s.writeActionableIndexMetadataLocked(state); err != nil {
			return err
		}
		if err := syncDir(s.actionableAuthority); err != nil {
			return err
		}
		return s.clearActionableMutationLocked()
	})
}

func writeFileAtomic(dirPath, finalPath, tempPattern string, data []byte) error {
	tmp, err := os.CreateTemp(dirPath, tempPattern)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	return syncDir(dirPath)
}

func syncDir(path string) error {
	dir, err := os.Open(path) //nolint:gosec // path is a configured durable store directory.
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("sync directory: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close synced directory: %w", err)
	}
	return nil
}

func (s *LeaseStore) withLocks(ctx context.Context, keys []string, fn func() error) error {
	unique := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if key != "" {
			unique[fileKey(key)] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(unique))
	for key := range unique {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)

	held := make([]*os.File, 0, len(ordered))
	for _, key := range ordered {
		path := filepath.Join(s.locks, key+".lock")
		lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path is a hash under the configured lock directory.
		if err != nil {
			s.releaseLocks(held)
			return fmt.Errorf("open lease lock: %w", err)
		}
		for {
			err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // Flock requires an int file descriptor.
			if err == nil {
				held = append(held, lock)
				break
			}
			if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
				_ = lock.Close()
				s.releaseLocks(held)
				return fmt.Errorf("acquire lease lock: %w", err)
			}
			timer := time.NewTimer(lockRetryInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				_ = lock.Close()
				s.releaseLocks(held)
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	defer s.releaseLocks(held)
	return fn()
}

func (s *LeaseStore) releaseLocks(locks []*os.File) {
	for i := len(locks) - 1; i >= 0; i-- {
		_ = syscall.Flock(int(locks[i].Fd()), syscall.LOCK_UN) //nolint:errcheck,gosec // best-effort unlock; Flock requires an int file descriptor.
		_ = locks[i].Close()
	}
}
