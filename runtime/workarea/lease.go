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
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// DefaultSettlementBudget is the fixed v1 terminal settlement budget.
	DefaultSettlementBudget = time.Duration(SettlementBudgetMS) * time.Millisecond
	// DefaultLeaseSafetyMargin is the fixed v1 margin beyond settlement time.
	DefaultLeaseSafetyMargin = time.Duration(LeaseSafetyMarginMS) * time.Millisecond
	// DefaultLeaseDuration is the fixed v1 initial lease duration.
	DefaultLeaseDuration = time.Duration(LeaseDurationMS) * time.Millisecond
	// DefaultMaxLeaseDuration is the fixed v1 acquisition-relative maximum.
	DefaultMaxLeaseDuration = time.Duration(MaxLeaseDurationMS) * time.Millisecond
	// DefaultReaperInterval is the standard fixed-delay release scan interval.
	DefaultReaperInterval = 30 * time.Second
	// DefaultReaperBatchSize is the standard release scan batch capacity.
	DefaultReaperBatchSize = 32
	// DefaultReaperConcurrency is the standard provider-attempt concurrency.
	DefaultReaperConcurrency = 4
	// DefaultReleaseAttemptTimeout bounds one provider release attempt.
	DefaultReleaseAttemptTimeout = 30 * time.Second
	// DefaultReleaseRetryBase is the first provider release retry delay.
	DefaultReleaseRetryBase = 30 * time.Second
	// DefaultReleaseRetryMax caps provider release retry delay.
	DefaultReleaseRetryMax = 5 * time.Minute
	// DefaultTerminalResultReplayInterval is the standard outbox scan interval.
	DefaultTerminalResultReplayInterval = 5 * time.Second
	// DefaultTerminalResultReplayBatchSize is the standard outbox batch capacity.
	DefaultTerminalResultReplayBatchSize = 32
	// DefaultTerminalResultReplayConcurrency is the standard send concurrency.
	DefaultTerminalResultReplayConcurrency = 4
	// DefaultTerminalResultReplayTimeout bounds one terminal-status send.
	DefaultTerminalResultReplayTimeout = 30 * time.Second
	// DefaultTerminalResultReplayRetryBase is the first outbox retry delay.
	DefaultTerminalResultReplayRetryBase = time.Second
	// DefaultTerminalResultReplayRetryMax caps outbox retry delay.
	DefaultTerminalResultReplayRetryMax = time.Minute

	lockRetryInterval = 10 * time.Millisecond
	clockLockKey      = "store:logical-clock"
)

// LeaseState implements the documented terminal-workarea contract.
type LeaseState string

const (
	// LeaseActive retains exclusive ownership while settlement remains possible.
	LeaseActive LeaseState = "active"
	// LeaseReleasePending retains ownership while provider release is retried.
	LeaseReleasePending LeaseState = "release-pending"
	// LeaseReleased records completed provider disposition and ends ownership.
	LeaseReleased LeaseState = "released"
)

var (
	// ErrWorkareaLeased reports that a non-released lease owns the workarea.
	ErrWorkareaLeased = errors.New("runtime/workarea: workarea already leased")
	// ErrWorkareaQuarantined reports independent quarantine exclusion.
	ErrWorkareaQuarantined = errors.New("runtime/workarea: workarea quarantined")
	// ErrProviderRootUnready reports a fail-closed durable authority.
	ErrProviderRootUnready = errors.New("runtime/workarea: provider root is unready")
	// ErrLeaseConflict reports identity or invariant disagreement.
	ErrLeaseConflict = errors.New("runtime/workarea: terminal lease identity conflict")
	// ErrLeaseNotFound reports an unknown terminal lease identity.
	ErrLeaseNotFound = errors.New("runtime/workarea: terminal lease not found")
	// ErrAcknowledgementRequired reports a non-semantic acknowledgement.
	ErrAcknowledgementRequired = errors.New("runtime/workarea: explicit terminal result acknowledgement required")
	// ErrLeaseExpired reports a lease that cannot accept the requested operation.
	ErrLeaseExpired = errors.New("runtime/workarea: terminal lease expired")
	// ErrInsufficientLeaseTime reports a strict remaining-time boundary failure.
	ErrInsufficientLeaseTime = errors.New("runtime/workarea: insufficient remaining lease time")
	// ErrReleaseRequired reports a missing provider release callback.
	ErrReleaseRequired = errors.New("runtime/workarea: workarea releaser required")
	// ErrLeaseExecutionRequired reports a missing durable local claim.
	ErrLeaseExecutionRequired = errors.New("runtime/workarea: terminal lease execution claim required")
	// ErrLeaseExecutionClaimed reports an existing claim owned by another invocation.
	ErrLeaseExecutionClaimed = errors.New("runtime/workarea: terminal lease execution already claimed")
	// ErrLeaseExecutionConflict reports claim identity disagreement.
	ErrLeaseExecutionConflict = errors.New("runtime/workarea: terminal lease execution identity conflict")
	// ErrTerminalStatusNotFound reports a lease without a retained outbox record.
	ErrTerminalStatusNotFound = errors.New("runtime/workarea: terminal status outbox not found")
	// ErrRenewalAfterBodySave reports immutable terminal-status body enforcement.
	ErrRenewalAfterBodySave = errors.New("runtime/workarea: terminal status body already persisted")
)

// LeasePolicy implements the documented terminal-workarea contract.
type LeasePolicy struct {
	SettlementBudget time.Duration
	SafetyMargin     time.Duration
	LeaseDuration    time.Duration
	MaxLeaseDuration time.Duration
}

// DefaultLeasePolicy implements the documented terminal-workarea contract.
func DefaultLeasePolicy() LeasePolicy {
	return LeasePolicy{
		SettlementBudget: DefaultSettlementBudget,
		SafetyMargin:     DefaultLeaseSafetyMargin,
		LeaseDuration:    DefaultLeaseDuration,
		MaxLeaseDuration: DefaultMaxLeaseDuration,
	}
}

// Validate implements the documented terminal-workarea contract.
func (p LeasePolicy) Validate() error {
	if p != DefaultLeasePolicy() {
		return errors.New("runtime/workarea: lease policy must use the fixed v1 profile")
	}
	return nil
}

// AcquireSpec implements the documented terminal-workarea contract.
type AcquireSpec struct {
	SessionID          string
	TerminalResultID   string
	WorkareaID         string
	WorkareaPath       string
	Policy             LeasePolicy
	ReleaseMetadata    map[string]string
	ReleaseRequested   bool
	ReleaseDisposition string
}

// ExecutionClaimSpec implements the documented terminal-workarea contract.
type ExecutionClaimSpec struct {
	LeaseID          string
	SessionID        string
	TerminalResultID string
	WorkareaID       string
	InvocationID     string
	ClaimID          string
}

// ExecutionClaimResult implements the documented terminal-workarea contract.
type ExecutionClaimResult struct {
	Lease      *TerminalLease
	Claim      LeaseExecutionClaim
	ClaimNowMS int64
}

// RenewSpec implements the documented terminal-workarea contract.
type RenewSpec struct {
	LeaseID          string
	SessionID        string
	TerminalResultID string
	WorkareaID       string
	Extension        time.Duration
	// Duration is the deprecated spelling retained for source compatibility.
	Duration time.Duration
}

// TerminalStatusSaveSpec implements the documented terminal-workarea contract.
type TerminalStatusSaveSpec struct {
	LeaseID           string
	SessionID         string
	TerminalResultID  string
	WorkareaID        string
	ReceiverKey       string
	Body              []byte
	ExpectedExpiresAt time.Time
	DeadlineAt        time.Time
}

// TerminalLease implements the documented terminal-workarea contract.
type TerminalLease struct {
	LeaseID                string                          `json:"leaseId"`
	SessionID              string                          `json:"sessionId"`
	TerminalResultID       string                          `json:"terminalResultId"`
	WorkareaID             string                          `json:"workareaId"`
	WorkareaPath           string                          `json:"workareaPath"`
	AcquiredAt             time.Time                       `json:"acquiredAt"`
	ExpiresAt              time.Time                       `json:"expiresAt"`
	MaxExpiresAt           time.Time                       `json:"maxExpiresAt"`
	SettlementBudget       time.Duration                   `json:"settlementBudget"`
	Policy                 LeasePolicy                     `json:"policy"`
	RequestBytes           []byte                          `json:"requestBytes"`
	ClockHighWatermarkMS   int64                           `json:"clockHighWatermarkMs"`
	State                  LeaseState                      `json:"state"`
	ExecutionClaim         *LeaseExecutionClaim            `json:"executionClaim,omitempty"`
	TerminalStatus         *TerminalStatusOutbox           `json:"terminalStatus,omitempty"`
	AcknowledgementBytes   []byte                          `json:"acknowledgementBytes,omitempty"`
	AcknowledgementOutcome *TerminalAcknowledgementOutcome `json:"acknowledgementOutcome,omitempty"`
	ReleaseRequested       bool                            `json:"releaseRequested"`
	ReleaseDisposition     string                          `json:"releaseDisposition"`
	ReleaseReason          string                          `json:"releaseReason,omitempty"`
	ReleaseEligibleAt      *time.Time                      `json:"releaseEligibleAt,omitempty"`
	ReleasedAt             *time.Time                      `json:"releasedAt,omitempty"`
	ReleaseAttempts        int64                           `json:"releaseAttempts"`
	NextReleaseAttempt     *time.Time                      `json:"nextReleaseAttempt,omitempty"`
	LastReleaseError       string                          `json:"lastReleaseError,omitempty"`
	ReleaseMetadata        map[string]string               `json:"releaseMetadata,omitempty"`
}

// RetainsWorkarea implements the documented terminal-workarea contract.
func (l TerminalLease) RetainsWorkarea() bool { return l.State != LeaseReleased }

// StoreOptions implements the documented terminal-workarea contract.
type StoreOptions struct {
	Dir           string
	QuarantineDir string
	ReceiverDir   string
	Now           func() time.Time
}

type leaseStoreDependencies struct {
	readFile func(string) ([]byte, error)
}

// LeaseStore implements the documented terminal-workarea contract.
type LeaseStore struct {
	dir        string
	records    string
	locks      string
	actionable string
	clockPath  string
	now        func() time.Time
	readFile   func(string) ([]byte, error)
	quarantine *QuarantineStore
	receivers  *ReceiverRegistry
	readyMu    sync.RWMutex
	readyErr   error
}

// NewLeaseStore implements the documented terminal-workarea contract.
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
	quarantineDir := opts.QuarantineDir
	if quarantineDir == "" {
		quarantineDir = filepath.Join(filepath.Dir(abs), ".terminal-quarantine")
	}
	quarantine, err := newQuarantineStore(quarantineDir)
	if err != nil {
		return nil, fmt.Errorf("runtime/workarea: open quarantine authority: %w", err)
	}
	receiverDir := opts.ReceiverDir
	if receiverDir == "" {
		receiverDir = filepath.Join(filepath.Dir(abs), ".terminal-receivers")
	}
	receivers, err := newReceiverRegistry(receiverDir)
	if err != nil {
		return nil, fmt.Errorf("runtime/workarea: open receiver registry: %w", err)
	}
	s := &LeaseStore{
		dir: abs, records: filepath.Join(abs, "records"), locks: filepath.Join(abs, "locks"),
		actionable: filepath.Join(abs, "actionable"), clockPath: filepath.Join(abs, "clock-high-watermark-ms"),
		now: opts.Now, readFile: deps.readFile, quarantine: quarantine, receivers: receivers,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.readFile == nil {
		s.readFile = os.ReadFile
	}
	for _, dir := range []string{s.dir, s.records, s.locks, s.actionable} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("runtime/workarea: create durable authority %s: %w", dir, err)
		}
		if err := syncDir(dir); err != nil {
			return nil, fmt.Errorf("runtime/workarea: sync durable authority %s: %w", dir, err)
		}
	}
	if err := s.initializeClock(); err != nil {
		return nil, fmt.Errorf("runtime/workarea: initialize logical clock: %w", err)
	}
	// Quarantine is already loaded above. Lease records follow, then interrupted
	// outbox attempts and actionable indexes are reconciled before the store is ready.
	if err := s.reconcile(); err != nil {
		return nil, fmt.Errorf("runtime/workarea: reconcile lease authority: %w", err)
	}
	return s, nil
}

// Dir implements the documented terminal-workarea contract.
func (s *LeaseStore) Dir() string { return s.dir }

// QuarantineDir implements the documented terminal-workarea contract.
func (s *LeaseStore) QuarantineDir() string { return s.quarantine.dir }

// RegisterReceiver implements the documented terminal-workarea contract.
func (s *LeaseStore) RegisterReceiver(receiverKey, endpoint string) error {
	if err := s.Ready(); err != nil {
		return err
	}
	if err := s.receivers.Register(receiverKey, endpoint); err != nil {
		return s.failClosed(err)
	}
	return nil
}

// TerminalStatusHTTPSender implements the documented terminal-workarea contract.
func (s *LeaseStore) TerminalStatusHTTPSender(client *http.Client, auth ReceiverAuthorizationResolver) TerminalStatusSender {
	return s.receivers.HTTPSender(client, auth)
}

// Ready implements the documented terminal-workarea contract.
func (s *LeaseStore) Ready() error {
	s.readyMu.RLock()
	defer s.readyMu.RUnlock()
	if s.readyErr != nil {
		return fmt.Errorf("%w: %v", ErrProviderRootUnready, s.readyErr)
	}
	return nil
}

func (s *LeaseStore) failClosed(err error) error {
	if err == nil {
		return nil
	}
	s.readyMu.Lock()
	if s.readyErr == nil {
		s.readyErr = err
	}
	s.readyMu.Unlock()
	return fmt.Errorf("%w: %v", ErrProviderRootUnready, err)
}

// Acquire implements the documented terminal-workarea contract.
func (s *LeaseStore) Acquire(ctx context.Context, spec AcquireSpec) (*TerminalLease, error) {
	if err := s.Ready(); err != nil {
		return nil, err
	}
	if spec.Policy == (LeasePolicy{}) {
		spec.Policy = DefaultLeasePolicy()
	}
	if spec.ReleaseDisposition == "" {
		spec.ReleaseDisposition = "destroy"
	}
	if err := validateAcquireSpec(spec); err != nil {
		return nil, err
	}
	leaseID := leaseIDFor(spec.TerminalResultID)
	keys := []string{"result:" + spec.TerminalResultID, "workarea:" + spec.WorkareaID, clockLockKey}
	var out *TerminalLease
	err := s.withLocks(ctx, keys, func() error {
		if existing, err := s.load(leaseID); err == nil {
			if err := validateAcquireReplay(*existing, spec); err != nil {
				return err
			}
			out = existing
			return nil
		} else if !errors.Is(err, ErrLeaseNotFound) {
			// The lease authority became unreadable after store activation. Establish
			// exclusion in the independent authority before failing the provider root.
			nowMS, clockErr := s.sampleClockLocked()
			if clockErr == nil {
				now := time.UnixMilli(nowMS).UTC()
				if guard, guardErr := s.quarantine.createGuard(spec, now); guardErr == nil {
					_ = s.quarantine.promote(guard.QuarantineID, QuarantineQuarantined, err.Error(), now)
				}
			}
			return s.failClosed(err)
		}
		if quarantined, err := s.quarantine.findByWorkarea(spec.WorkareaID, spec.WorkareaPath); err != nil {
			return s.failClosed(err)
		} else if quarantined != nil {
			return fmt.Errorf("%w: %s", ErrWorkareaQuarantined, spec.WorkareaID)
		}
		leases, err := s.listActionableUnlocked()
		if err != nil {
			return s.failClosed(err)
		}
		for i := range leases {
			if leases[i].WorkareaID == spec.WorkareaID || filepath.Clean(leases[i].WorkareaPath) == filepath.Clean(spec.WorkareaPath) {
				return fmt.Errorf("%w: %s owned by session %s", ErrWorkareaLeased, spec.WorkareaID, leases[i].SessionID)
			}
		}
		nowMS, err := s.sampleClockLocked()
		if err != nil {
			return s.failClosed(err)
		}
		now := time.UnixMilli(nowMS).UTC()
		guard, err := s.quarantine.createGuard(spec, now)
		if err != nil {
			return s.failClosed(err)
		}
		requestBytes, _ := CanonicalBytes(DefaultTerminalLeaseRequest())
		lease := TerminalLease{
			LeaseID: leaseID, SessionID: spec.SessionID, TerminalResultID: spec.TerminalResultID,
			WorkareaID: spec.WorkareaID, WorkareaPath: filepath.Clean(spec.WorkareaPath),
			AcquiredAt: now, ExpiresAt: time.UnixMilli(nowMS + LeaseDurationMS).UTC(),
			MaxExpiresAt:     time.UnixMilli(nowMS + MaxLeaseDurationMS).UTC(),
			SettlementBudget: DefaultSettlementBudget, Policy: DefaultLeasePolicy(), RequestBytes: requestBytes,
			ClockHighWatermarkMS: nowMS, State: LeaseActive, ReleaseRequested: spec.ReleaseRequested,
			ReleaseDisposition: spec.ReleaseDisposition, ReleaseMetadata: cloneMetadata(spec.ReleaseMetadata),
		}
		if err := s.saveUnlocked(lease); err != nil {
			_ = s.quarantine.promote(guard.QuarantineID, QuarantineQuarantined, err.Error(), now)
			return s.failClosed(fmt.Errorf("commit terminal lease: %w", err))
		}
		persisted, err := s.load(leaseID)
		if err != nil || validateAcquireReplay(*persisted, spec) != nil {
			verifyErr := errors.New("durable lease reread did not match acquisition")
			_ = s.quarantine.promote(guard.QuarantineID, QuarantineQuarantined, verifyErr.Error(), now)
			return s.failClosed(verifyErr)
		}
		if err := s.quarantine.remove(guard.QuarantineID); err != nil {
			return s.failClosed(fmt.Errorf("clear redundant acquisition guard: %w", err))
		}
		out = persisted
		return nil
	})
	return out, err
}

// ClaimExecution implements the documented terminal-workarea contract.
func (s *LeaseStore) ClaimExecution(ctx context.Context, spec ExecutionClaimSpec) (*ExecutionClaimResult, error) {
	if err := s.Ready(); err != nil {
		return nil, err
	}
	if err := validateExecutionClaimSpec(spec); err != nil {
		return nil, err
	}
	var result *ExecutionClaimResult
	err := s.withLocks(ctx, []string{"result:" + spec.TerminalResultID, "workarea:" + spec.WorkareaID, clockLockKey}, func() error {
		lease, err := s.load(spec.LeaseID)
		if err != nil {
			return err
		}
		if err := validateIdentity(*lease, spec.SessionID, spec.TerminalResultID, spec.WorkareaID); err != nil {
			return err
		}
		if lease.ExecutionClaim != nil {
			if lease.ExecutionClaim.InvocationID != spec.InvocationID || lease.ExecutionClaim.ClaimID != spec.ClaimID {
				return fmt.Errorf("%w: lease %s owned by invocation %s", ErrLeaseExecutionClaimed, lease.LeaseID, lease.ExecutionClaim.InvocationID)
			}
			claim := *lease.ExecutionClaim
			result = &ExecutionClaimResult{Lease: lease, Claim: claim, ClaimNowMS: claim.ClaimedAt.UnixMilli()}
			return nil
		}
		nowMS, err := s.sampleClockLocked()
		if err != nil {
			return s.failClosed(err)
		}
		if lease.State != LeaseActive {
			return ErrLeaseExpired
		}
		if remaining := lease.ExpiresAt.UnixMilli() - nowMS; remaining <= ClaimMinimumMS {
			return fmt.Errorf("%w: remainingMs=%d must be > %d", ErrInsufficientLeaseTime, remaining, ClaimMinimumMS)
		}
		claim := LeaseExecutionClaim{
			SchemaVersion: TerminalLeaseClaimSchemaV1, InvocationID: spec.InvocationID, ClaimID: spec.ClaimID,
			LeaseID: lease.LeaseID, SessionID: lease.SessionID, TerminalResultID: lease.TerminalResultID,
			WorkareaID: lease.WorkareaID, ClaimedAt: time.UnixMilli(nowMS).UTC(),
		}
		if err := claim.Validate(); err != nil {
			return err
		}
		lease.ExecutionClaim = &claim
		lease.ClockHighWatermarkMS = nowMS
		if err := s.saveUnlocked(*lease); err != nil {
			return s.failClosed(err)
		}
		result = &ExecutionClaimResult{Lease: lease, Claim: claim, ClaimNowMS: nowMS}
		return nil
	})
	return result, err
}

// CheckQueueAdmission applies the optional pre-claim queue boundary using one
// persisted logical-clock sample. Equality at 1097000ms is rejected.
func (s *LeaseStore) CheckQueueAdmission(ctx context.Context, leaseID, sessionID, terminalResultID, workareaID string) (int64, error) {
	var nowMS int64
	err := s.withLocks(ctx, []string{"result:" + terminalResultID, "workarea:" + workareaID, clockLockKey}, func() error {
		lease, err := s.load(leaseID)
		if err != nil {
			return err
		}
		if err := validateIdentity(*lease, sessionID, terminalResultID, workareaID); err != nil {
			return err
		}
		nowMS, err = s.sampleClockLocked()
		if err != nil {
			return s.failClosed(err)
		}
		if lease.State != LeaseActive {
			return ErrLeaseExpired
		}
		if remaining := lease.ExpiresAt.UnixMilli() - nowMS; remaining <= QueueMinimumMS {
			return fmt.Errorf("%w: remainingMs=%d must be > %d", ErrInsufficientLeaseTime, remaining, QueueMinimumMS)
		}
		return nil
	})
	return nowMS, err
}

// Renew implements the documented terminal-workarea contract.
func (s *LeaseStore) Renew(ctx context.Context, spec RenewSpec) (*TerminalLease, error) {
	if err := s.Ready(); err != nil {
		return nil, err
	}
	extension := spec.Extension
	if extension == 0 {
		extension = spec.Duration
	}
	if extension <= 0 || extension%time.Millisecond != 0 {
		return nil, errors.New("runtime/workarea: renewal extension must be a positive whole number of milliseconds")
	}
	var out *TerminalLease
	err := s.withLocks(ctx, []string{"result:" + spec.TerminalResultID, "workarea:" + spec.WorkareaID, clockLockKey}, func() error {
		lease, err := s.load(spec.LeaseID)
		if err != nil {
			return err
		}
		if err := validateIdentity(*lease, spec.SessionID, spec.TerminalResultID, spec.WorkareaID); err != nil {
			return err
		}
		if lease.TerminalStatus != nil {
			return ErrRenewalAfterBodySave
		}
		nowMS, err := s.sampleClockLocked()
		if err != nil {
			return s.failClosed(err)
		}
		if lease.State != LeaseActive || nowMS >= lease.ExpiresAt.UnixMilli() {
			return ErrLeaseExpired
		}
		currentMS := lease.ExpiresAt.UnixMilli()
		extensionMS := extension.Milliseconds()
		if extensionMS > int64(^uint64(0)>>1)-currentMS {
			return errors.New("runtime/workarea: renewal overflow")
		}
		nextMS := currentMS + extensionMS
		if nextMS > lease.MaxExpiresAt.UnixMilli() {
			nextMS = lease.MaxExpiresAt.UnixMilli()
		}
		if nextMS <= currentMS {
			return errors.New("runtime/workarea: renewal is a clipped no-op")
		}
		lease.ExpiresAt = time.UnixMilli(nextMS).UTC()
		lease.ClockHighWatermarkMS = nowMS
		if err := s.saveUnlocked(*lease); err != nil {
			return s.failClosed(err)
		}
		out = lease
		return nil
	})
	return out, err
}

// SaveTerminalStatus implements the documented terminal-workarea contract.
func (s *LeaseStore) SaveTerminalStatus(ctx context.Context, spec TerminalStatusSaveSpec) (*TerminalLease, error) {
	if err := s.Ready(); err != nil {
		return nil, err
	}
	if err := validateTerminalStatusSaveSpec(spec); err != nil {
		return nil, err
	}
	var out *TerminalLease
	err := s.withLocks(ctx, []string{"result:" + spec.TerminalResultID, "workarea:" + spec.WorkareaID, clockLockKey}, func() error {
		lease, err := s.load(spec.LeaseID)
		if err != nil {
			return err
		}
		if err := validateIdentity(*lease, spec.SessionID, spec.TerminalResultID, spec.WorkareaID); err != nil {
			return err
		}
		if lease.State != LeaseActive {
			return fmt.Errorf("%w: body save requires active lease", ErrLeaseConflict)
		}
		if !lease.ExpiresAt.Equal(spec.ExpectedExpiresAt) {
			return fmt.Errorf("%w: body save lost renewal CAS", ErrLeaseConflict)
		}
		nowMS, err := s.sampleClockLocked()
		if err != nil {
			return s.failClosed(err)
		}
		deadline := spec.DeadlineAt.UTC()
		if deadline.IsZero() {
			deadline = lease.ExpiresAt
		}
		candidate := NewTerminalStatusOutbox(spec.TerminalResultID, spec.ReceiverKey, spec.Body, deadline, time.UnixMilli(nowMS).UTC())
		if err := candidate.Validate(); err != nil {
			return err
		}
		if lease.TerminalStatus != nil {
			if !terminalStatusIdentityEqual(*lease.TerminalStatus, candidate) {
				return fmt.Errorf("%w: terminal status body differs", ErrLeaseConflict)
			}
			out = lease
			return nil
		}
		lease.TerminalStatus = &candidate
		lease.ClockHighWatermarkMS = nowMS
		if err := s.saveUnlocked(*lease); err != nil {
			return s.failClosed(err)
		}
		out = lease
		return nil
	})
	return out, err
}

// MarkTerminalStatusDelivered implements the documented terminal-workarea contract.
func (s *LeaseStore) MarkTerminalStatusDelivered(ctx context.Context, leaseID, sessionID, terminalResultID, workareaID string) (*TerminalLease, error) {
	var out *TerminalLease
	err := s.withLocks(ctx, []string{"result:" + terminalResultID, "workarea:" + workareaID, clockLockKey}, func() error {
		lease, err := s.load(leaseID)
		if err != nil {
			return err
		}
		if err := validateIdentity(*lease, sessionID, terminalResultID, workareaID); err != nil {
			return err
		}
		if lease.TerminalStatus == nil {
			return ErrTerminalStatusNotFound
		}
		if lease.TerminalStatus.DeliveryState == TerminalStatusDelivered {
			out = lease
			return nil
		}
		nowMS, err := s.sampleClockLocked()
		if err != nil {
			return s.failClosed(err)
		}
		lease.TerminalStatus.DeliveryState = TerminalStatusDelivered
		lease.TerminalStatus.NextAttemptAt = time.UnixMilli(nowMS).UTC()
		lease.TerminalStatus.LastError = nil
		lease.ClockHighWatermarkMS = nowMS
		if err := s.saveUnlocked(*lease); err != nil {
			return s.failClosed(err)
		}
		out = lease
		return nil
	})
	return out, err
}

// TerminalStatusSender implements the documented terminal-workarea contract.
type TerminalStatusSender func(context.Context, string, []byte) error

// TerminalResultReplayOptions implements the documented terminal-workarea contract.
type TerminalResultReplayOptions struct {
	Interval       time.Duration
	BatchSize      int
	Concurrency    int
	AttemptTimeout time.Duration
	OnError        func(error)
}

// ReplayTerminalResults implements the documented terminal-workarea contract.
func (s *LeaseStore) ReplayTerminalResults(ctx context.Context, batchSize int, attemptTimeout time.Duration, sender TerminalStatusSender) (int, error) {
	return s.replayTerminalResults(ctx, batchSize, DefaultTerminalResultReplayConcurrency, attemptTimeout, sender)
}

func (s *LeaseStore) replayTerminalResults(ctx context.Context, batchSize, concurrency int, attemptTimeout time.Duration, sender TerminalStatusSender) (int, error) {
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
	leases, err := s.listActionable()
	if err != nil {
		return 0, err
	}
	nowMS, err := s.sampleClock(ctx)
	if err != nil {
		return 0, err
	}
	sort.Slice(leases, func(i, j int) bool { return leases[i].TerminalResultID < leases[j].TerminalResultID })
	eligible := make([]TerminalLease, 0, batchSize)
	for i := range leases {
		post := leases[i].TerminalStatus
		if post == nil || post.DeliveryState == TerminalStatusDelivered || post.DeliveryState == TerminalStatusDeadLetter {
			continue
		}
		if nowMS >= post.DeadlineAt.UnixMilli() {
			_ = s.markOutboxDeadLetter(context.Background(), leases[i], "delivery deadline reached")
			continue
		}
		if nowMS < post.NextAttemptAt.UnixMilli() {
			continue
		}
		eligible = append(eligible, leases[i])
		if len(eligible) == batchSize {
			break
		}
	}
	return len(eligible), runConcurrent(ctx, eligible, concurrency, func(lease TerminalLease) error {
		return s.replayOneTerminalStatus(ctx, lease, attemptTimeout, sender)
	})
}

// RunTerminalResultReplayer implements the documented terminal-workarea contract.
func (s *LeaseStore) RunTerminalResultReplayer(ctx context.Context, opts TerminalResultReplayOptions, sender TerminalStatusSender) {
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
		if _, err := s.replayTerminalResults(ctx, opts.BatchSize, opts.Concurrency, opts.AttemptTimeout, sender); err != nil && opts.OnError != nil {
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

func (s *LeaseStore) replayOneTerminalStatus(ctx context.Context, snapshot TerminalLease, attemptTimeout time.Duration, sender TerminalStatusSender) error {
	var body []byte
	var receiverKey string
	var attempt int64
	err := s.withLocks(ctx, []string{"result:" + snapshot.TerminalResultID, "workarea:" + snapshot.WorkareaID, clockLockKey}, func() error {
		lease, err := s.load(snapshot.LeaseID)
		if err != nil {
			return err
		}
		post := lease.TerminalStatus
		if post == nil || (post.DeliveryState != TerminalStatusPending && post.DeliveryState != TerminalStatusAttempting) {
			return nil
		}
		nowMS, err := s.sampleClockLocked()
		if err != nil {
			return s.failClosed(err)
		}
		if nowMS >= post.DeadlineAt.UnixMilli() {
			post.DeliveryState = TerminalStatusDeadLetter
			message := "delivery deadline reached"
			post.LastError = &message
			return s.saveUnlocked(*lease)
		}
		decoded, err := post.Body()
		if err != nil {
			return s.failClosed(err)
		}
		post.DeliveryState = TerminalStatusAttempting
		post.AttemptCount++
		attempt = post.AttemptCount
		now := time.UnixMilli(nowMS).UTC()
		post.LastAttemptAt = &now
		post.NextAttemptAt = time.UnixMilli(nowMS + terminalResultReplayRetryDelay(attempt).Milliseconds()).UTC()
		lease.ClockHighWatermarkMS = nowMS
		if err := s.saveUnlocked(*lease); err != nil {
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
	return s.withLocks(context.Background(), []string{"result:" + snapshot.TerminalResultID, "workarea:" + snapshot.WorkareaID, clockLockKey}, func() error {
		lease, err := s.load(snapshot.LeaseID)
		if err != nil {
			return err
		}
		post := lease.TerminalStatus
		if post == nil || post.AttemptCount != attempt || post.DeliveryState != TerminalStatusAttempting {
			return nil
		}
		nowMS, err := s.sampleClockLocked()
		if err != nil {
			return s.failClosed(err)
		}
		switch {
		case sendErr == nil:
			post.DeliveryState = TerminalStatusDelivered
			post.LastError = nil
		case nowMS >= post.DeadlineAt.UnixMilli():
			post.DeliveryState = TerminalStatusDeadLetter
			message := sendErr.Error()
			post.LastError = &message
		default:
			post.DeliveryState = TerminalStatusPending
			message := sendErr.Error()
			post.LastError = &message
		}
		lease.ClockHighWatermarkMS = nowMS
		if err := s.saveUnlocked(*lease); err != nil {
			return s.failClosed(err)
		}
		if sendErr != nil {
			return fmt.Errorf("runtime/workarea: replay terminal status %s: %w", snapshot.TerminalResultID, sendErr)
		}
		return nil
	})
}

// Acknowledge implements the documented terminal-workarea contract.
func (s *LeaseStore) Acknowledge(ctx context.Context, ack TerminalResultAcknowledgement) (*TerminalAcknowledgementOutcome, error) {
	if err := ack.Validate(); err != nil {
		return nil, err
	}
	canonicalAck, err := CanonicalBytes(ack)
	if err != nil {
		return nil, err
	}
	var out *TerminalAcknowledgementOutcome
	err = s.withLocks(ctx, []string{"result:" + ack.TerminalResultID, "workarea:" + ack.WorkareaID, clockLockKey}, func() error {
		lease, err := s.load(ack.LeaseID)
		if err != nil {
			return err
		}
		nowMS, err := s.sampleClockLocked()
		if err != nil {
			return s.failClosed(err)
		}
		now := time.UnixMilli(nowMS).UTC()
		makeOutcome := func(value AcknowledgementOutcomeValue, reason *AcknowledgementReason) TerminalAcknowledgementOutcome {
			return TerminalAcknowledgementOutcome{
				SchemaVersion: TerminalLeaseAckOutcomeSchemaV1, Outcome: value, Reason: reason,
				LeaseID: lease.LeaseID, TerminalResultID: lease.TerminalResultID,
				LeaseState: lease.State, ProviderReleaseComplete: lease.State == LeaseReleased,
			}
		}
		if lease.ExecutionClaim == nil {
			reason := AcknowledgementClaimMissing
			outcome := makeOutcome(AcknowledgementRejected, &reason)
			lease.AcknowledgementOutcome = &outcome
			if lease.TerminalStatus != nil {
				lease.TerminalStatus.ApplicationState = TerminalApplicationNotAuthoritative
			}
			lease.ClockHighWatermarkMS = nowMS
			if err := s.saveUnlocked(*lease); err != nil {
				return s.failClosed(err)
			}
			out = &outcome
			return nil
		}
		identityMatches := ack.SessionID == lease.SessionID && ack.TerminalResultID == lease.TerminalResultID && ack.WorkareaID == lease.WorkareaID &&
			ack.InvocationID == lease.ExecutionClaim.InvocationID && ack.ClaimID == lease.ExecutionClaim.ClaimID
		if !identityMatches {
			reason := AcknowledgementIdentityMismatch
			outcome := makeOutcome(AcknowledgementRejected, &reason)
			lease.AcknowledgementOutcome = &outcome
			if lease.TerminalStatus != nil {
				lease.TerminalStatus.ApplicationState = TerminalApplicationRejected
			}
			lease.ClockHighWatermarkMS = nowMS
			if err := s.saveUnlocked(*lease); err != nil {
				return s.failClosed(err)
			}
			out = &outcome
			return nil
		}
		if len(lease.AcknowledgementBytes) > 0 {
			if bytes.Equal(lease.AcknowledgementBytes, canonicalAck) {
				outcome := makeOutcome(AcknowledgementAlreadyApplied, nil)
				lease.AcknowledgementOutcome = &outcome
				lease.ClockHighWatermarkMS = nowMS
				if err := s.saveUnlocked(*lease); err != nil {
					return s.failClosed(err)
				}
				out = &outcome
				return nil
			}
			reason := AcknowledgementStateConflict
			outcome := makeOutcome(AcknowledgementRejected, &reason)
			lease.AcknowledgementOutcome = &outcome
			if lease.TerminalStatus != nil {
				lease.TerminalStatus.ApplicationState = TerminalApplicationRejected
			}
			if err := s.saveUnlocked(*lease); err != nil {
				return s.failClosed(err)
			}
			out = &outcome
			return nil
		}
		if lease.State != LeaseActive {
			reason := AcknowledgementStateConflict
			outcome := makeOutcome(AcknowledgementRejected, &reason)
			lease.AcknowledgementOutcome = &outcome
			if err := s.saveUnlocked(*lease); err != nil {
				return s.failClosed(err)
			}
			out = &outcome
			return nil
		}
		lease.State = LeaseReleasePending
		lease.ReleaseReason = "acknowledgement"
		lease.ReleaseEligibleAt = &now
		lease.AcknowledgementBytes = append([]byte(nil), canonicalAck...)
		if lease.TerminalStatus != nil {
			lease.TerminalStatus.ApplicationState = TerminalApplicationApplied
		}
		outcome := makeOutcome(AcknowledgementApplied, nil)
		outcome.LeaseState = LeaseReleasePending
		lease.AcknowledgementOutcome = &outcome
		lease.ClockHighWatermarkMS = nowMS
		if err := s.saveUnlocked(*lease); err != nil {
			return s.failClosed(err)
		}
		out = &outcome
		return nil
	})
	return out, err
}

// SchedulerOptions implements the documented terminal-workarea contract.
type SchedulerOptions struct {
	BatchSize      int
	Concurrency    int
	AdmissionDelay time.Duration
	AttemptTimeout time.Duration
}

// ReaperOptions implements the documented terminal-workarea contract.
type ReaperOptions struct {
	Interval       time.Duration
	BatchSize      int
	Concurrency    int
	AttemptTimeout time.Duration
	OnError        func(error)
}

// ReapExpired implements the documented terminal-workarea contract.
func (s *LeaseStore) ReapExpired(ctx context.Context, batchSize int, attemptTimeout time.Duration, releaser func(context.Context, TerminalLease) error) (int, error) {
	return s.reapSnapshot(ctx, SchedulerOptions{BatchSize: batchSize, Concurrency: min(DefaultReaperConcurrency, max(batchSize, 1)), AttemptTimeout: attemptTimeout}, releaser, true)
}

// ReapSnapshot implements the documented terminal-workarea contract.
func (s *LeaseStore) ReapSnapshot(ctx context.Context, opts SchedulerOptions, releaser func(context.Context, TerminalLease) error) (int, error) {
	return s.reapSnapshot(ctx, opts, releaser, false)
}

func (s *LeaseStore) reapSnapshot(ctx context.Context, opts SchedulerOptions, releaser func(context.Context, TerminalLease) error, oneBatch bool) (int, error) {
	if releaser == nil {
		return 0, ErrReleaseRequired
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultReaperBatchSize
	}
	if opts.Concurrency <= 0 || opts.Concurrency > opts.BatchSize {
		opts.Concurrency = min(DefaultReaperConcurrency, opts.BatchSize)
	}
	if opts.AttemptTimeout <= 0 {
		opts.AttemptTimeout = DefaultReleaseAttemptTimeout
	}
	leases, err := s.listActionable()
	if err != nil {
		return 0, err
	}
	nowMS, err := s.sampleClock(ctx)
	if err != nil {
		return 0, err
	}
	actionable := make([]TerminalLease, 0, len(leases))
	for i := range leases {
		lease := leases[i]
		if lease.State == LeaseActive && nowMS >= lease.ExpiresAt.UnixMilli() {
			if err := s.markExpiryEligible(ctx, &lease, nowMS); err != nil {
				return len(actionable), err
			}
		}
		if lease.State == LeaseReleasePending && (lease.NextReleaseAttempt == nil || nowMS >= lease.NextReleaseAttempt.UnixMilli()) {
			actionable = append(actionable, lease)
		}
	}
	sort.Slice(actionable, func(i, j int) bool {
		if actionable[i].ReleaseAttempts != actionable[j].ReleaseAttempts {
			return actionable[i].ReleaseAttempts < actionable[j].ReleaseAttempts
		}
		return actionable[i].LeaseID < actionable[j].LeaseID
	})
	if oneBatch && len(actionable) > opts.BatchSize {
		actionable = actionable[:opts.BatchSize]
	}
	var errs []error
	for start := 0; start < len(actionable); start += opts.BatchSize {
		end := min(start+opts.BatchSize, len(actionable))
		if opts.AdmissionDelay > 0 {
			timer := time.NewTimer(opts.AdmissionDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return len(actionable), errors.Join(append(errs, ctx.Err())...)
			case <-timer.C:
			}
		}
		if err := runConcurrent(ctx, actionable[start:end], opts.Concurrency, func(lease TerminalLease) error {
			return s.releaseOne(ctx, lease, nowMS, opts.AttemptTimeout, releaser)
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return len(actionable), errors.Join(errs...)
}

// RunReaper implements the documented terminal-workarea contract.
func (s *LeaseStore) RunReaper(ctx context.Context, opts ReaperOptions, releaser func(context.Context, TerminalLease) error) {
	if opts.Interval <= 0 {
		opts.Interval = DefaultReaperInterval
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultReaperBatchSize
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultReaperConcurrency
	}
	if opts.AttemptTimeout <= 0 {
		opts.AttemptTimeout = DefaultReleaseAttemptTimeout
	}
	reap := func() {
		_, err := s.ReapSnapshot(ctx, SchedulerOptions{BatchSize: opts.BatchSize, Concurrency: opts.Concurrency, AdmissionDelay: opts.Interval, AttemptTimeout: opts.AttemptTimeout}, releaser)
		if err != nil && opts.OnError != nil {
			opts.OnError(err)
		}
	}
	reap()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(opts.Interval):
			reap()
		}
	}
}

func (s *LeaseStore) releaseOne(ctx context.Context, snapshot TerminalLease, reapNowMS int64, attemptTimeout time.Duration, releaser func(context.Context, TerminalLease) error) error {
	var attempt int64
	var leaseForProvider TerminalLease
	err := s.withLocks(ctx, []string{"result:" + snapshot.TerminalResultID, "workarea:" + snapshot.WorkareaID}, func() error {
		lease, err := s.load(snapshot.LeaseID)
		if err != nil {
			return err
		}
		if lease.State != LeaseReleasePending {
			return nil
		}
		if lease.NextReleaseAttempt != nil && reapNowMS < lease.NextReleaseAttempt.UnixMilli() {
			return nil
		}
		lease.ReleaseAttempts++
		attempt = lease.ReleaseAttempts
		lease.NextReleaseAttempt = nil
		lease.ClockHighWatermarkMS = max(lease.ClockHighWatermarkMS, reapNowMS)
		if err := s.saveUnlocked(*lease); err != nil {
			return s.failClosed(err)
		}
		leaseForProvider = *lease
		return nil
	})
	if err != nil || attempt == 0 {
		return err
	}
	attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	releaseErr := releaser(attemptCtx, leaseForProvider)
	cancel()
	return s.withLocks(context.Background(), []string{"result:" + snapshot.TerminalResultID, "workarea:" + snapshot.WorkareaID, clockLockKey}, func() error {
		lease, err := s.load(snapshot.LeaseID)
		if err != nil {
			return err
		}
		if lease.State != LeaseReleasePending || lease.ReleaseAttempts != attempt {
			return nil
		}
		nowMS, err := s.sampleClockLocked()
		if err != nil {
			return s.failClosed(err)
		}
		now := time.UnixMilli(nowMS).UTC()
		if releaseErr == nil {
			lease.State = LeaseReleased
			lease.ReleasedAt = &now
			lease.NextReleaseAttempt = nil
			lease.LastReleaseError = ""
			if lease.AcknowledgementOutcome != nil {
				lease.AcknowledgementOutcome.LeaseState = LeaseReleased
				lease.AcknowledgementOutcome.ProviderReleaseComplete = true
			}
		} else {
			next := time.UnixMilli(nowMS + releaseRetryDelay(attempt).Milliseconds()).UTC()
			lease.NextReleaseAttempt = &next
			lease.LastReleaseError = releaseErr.Error()
		}
		lease.ClockHighWatermarkMS = nowMS
		if err := s.saveUnlocked(*lease); err != nil {
			return s.failClosed(err)
		}
		if releaseErr != nil {
			return fmt.Errorf("runtime/workarea: release %s: %w", lease.LeaseID, releaseErr)
		}
		return nil
	})
}

// Retained implements the documented terminal-workarea contract.
func (s *LeaseStore) Retained(workareaID string) (bool, error) {
	if err := s.Ready(); err != nil {
		return false, err
	}
	leases, err := s.listActionable()
	if err != nil {
		return false, s.failClosed(err)
	}
	for i := range leases {
		if leases[i].WorkareaID == workareaID {
			return true, nil
		}
	}
	return false, nil
}

// RetainedPath implements the documented terminal-workarea contract.
func (s *LeaseStore) RetainedPath(path string) (bool, error) {
	if err := s.Ready(); err != nil {
		return false, err
	}
	if quarantined, err := s.quarantine.findByWorkarea("", path); err != nil {
		return false, s.failClosed(err)
	} else if quarantined != nil {
		return true, nil
	}
	leases, err := s.listActionable()
	if err != nil {
		return false, s.failClosed(err)
	}
	for i := range leases {
		if filepath.Clean(leases[i].WorkareaPath) == filepath.Clean(path) {
			return true, nil
		}
	}
	return false, nil
}

// RequestRelease implements the documented terminal-workarea contract.
func (s *LeaseStore) RequestRelease(ctx context.Context, workareaID string) (bool, error) {
	leases, err := s.listActionable()
	if err != nil {
		return false, err
	}
	for i := range leases {
		if leases[i].WorkareaID != workareaID {
			continue
		}
		err := s.withLocks(ctx, []string{"result:" + leases[i].TerminalResultID, "workarea:" + workareaID}, func() error {
			lease, err := s.load(leases[i].LeaseID)
			if err != nil {
				return err
			}
			lease.ReleaseRequested = true
			return s.saveUnlocked(*lease)
		})
		return true, err
	}
	return false, nil
}

// RequestReleasePath implements the documented terminal-workarea contract.
func (s *LeaseStore) RequestReleasePath(ctx context.Context, path string) (bool, error) {
	leases, err := s.listActionable()
	if err != nil {
		return false, err
	}
	for i := range leases {
		if filepath.Clean(leases[i].WorkareaPath) != filepath.Clean(path) {
			continue
		}
		return s.RequestRelease(ctx, leases[i].WorkareaID)
	}
	if quarantined, err := s.quarantine.findByWorkarea("", path); err != nil {
		return false, s.failClosed(err)
	} else if quarantined != nil {
		return true, nil
	}
	return false, nil
}

// Get implements the documented terminal-workarea contract.
func (s *LeaseStore) Get(leaseID string) (*TerminalLease, error) { return s.load(leaseID) }

// List implements the documented terminal-workarea contract.
func (s *LeaseStore) List() ([]TerminalLease, error) {
	entries, err := os.ReadDir(s.records)
	if err != nil {
		return nil, fmt.Errorf("runtime/workarea: read lease records: %w", err)
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

// Quarantines implements the documented terminal-workarea contract.
func (s *LeaseStore) Quarantines() ([]TerminalWorkareaQuarantine, error) { return s.quarantine.list() }

// CleanupQuarantines implements the documented terminal-workarea contract.
func (s *LeaseStore) CleanupQuarantines(ctx context.Context, opts SchedulerOptions, destroy func(context.Context, TerminalWorkareaQuarantine) error) (int, error) {
	if destroy == nil {
		return 0, ErrReleaseRequired
	}
	items, err := s.quarantine.list()
	if err != nil {
		return 0, s.failClosed(err)
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultReaperBatchSize
	}
	if opts.Concurrency <= 0 || opts.Concurrency > opts.BatchSize {
		opts.Concurrency = min(DefaultReaperConcurrency, opts.BatchSize)
	}
	if opts.AttemptTimeout <= 0 {
		opts.AttemptTimeout = DefaultReleaseAttemptTimeout
	}
	sort.Slice(items, func(i, j int) bool { return items[i].QuarantineID < items[j].QuarantineID })
	var errs []error
	for start := 0; start < len(items); start += opts.BatchSize {
		end := min(start+opts.BatchSize, len(items))
		if opts.AdmissionDelay > 0 {
			select {
			case <-ctx.Done():
				return len(items), ctx.Err()
			case <-time.After(opts.AdmissionDelay):
			}
		}
		batch := items[start:end]
		if err := runConcurrent(ctx, batch, opts.Concurrency, func(item TerminalWorkareaQuarantine) error {
			now := time.UnixMilli(s.now().UnixMilli()).UTC()
			if err := s.quarantine.promote(item.QuarantineID, QuarantineCleanupPending, "", now); err != nil {
				return s.failClosed(err)
			}
			attemptCtx, cancel := context.WithTimeout(ctx, opts.AttemptTimeout)
			destroyErr := destroy(attemptCtx, item)
			cancel()
			if destroyErr != nil {
				_ = s.quarantine.promote(item.QuarantineID, QuarantineCleanupPending, destroyErr.Error(), now)
				return destroyErr
			}
			return s.quarantine.remove(item.QuarantineID)
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return len(items), errors.Join(errs...)
}

func (s *LeaseStore) reconcile() error {
	leases, err := s.List()
	if err != nil {
		return err
	}
	if err := clearDirectoryFiles(s.actionable); err != nil {
		return err
	}
	for i := range leases {
		lease := leases[i]
		if lease.TerminalStatus != nil && lease.TerminalStatus.DeliveryState == TerminalStatusAttempting {
			lease.TerminalStatus.DeliveryState = TerminalStatusPending
			if err := s.writeRecord(lease); err != nil {
				return err
			}
		}
		if lease.RetainsWorkarea() {
			if err := s.writeActionableMarker(lease.LeaseID); err != nil {
				return err
			}
		}
	}
	guards, err := s.quarantine.list()
	if err != nil {
		return err
	}
	for i := range guards {
		guard := guards[i]
		for j := range leases {
			if quarantineGuardMatchesLease(guard, leases[j]) {
				if err := s.quarantine.remove(guard.QuarantineID); err != nil {
					return err
				}
				break
			}
		}
	}
	return syncDir(s.actionable)
}

func (s *LeaseStore) markExpiryEligible(ctx context.Context, snapshot *TerminalLease, nowMS int64) error {
	return s.withLocks(ctx, []string{"result:" + snapshot.TerminalResultID, "workarea:" + snapshot.WorkareaID, clockLockKey}, func() error {
		lease, err := s.load(snapshot.LeaseID)
		if err != nil {
			return err
		}
		if lease.State != LeaseActive || nowMS < lease.ExpiresAt.UnixMilli() {
			*snapshot = *lease
			return nil
		}
		now := time.UnixMilli(nowMS).UTC()
		lease.State = LeaseReleasePending
		lease.ReleaseReason = "expiry"
		lease.ReleaseEligibleAt = &now
		lease.ClockHighWatermarkMS = nowMS
		if err := s.saveUnlocked(*lease); err != nil {
			return s.failClosed(err)
		}
		*snapshot = *lease
		return nil
	})
}

func (s *LeaseStore) markOutboxDeadLetter(ctx context.Context, snapshot TerminalLease, message string) error {
	return s.withLocks(ctx, []string{"result:" + snapshot.TerminalResultID, "workarea:" + snapshot.WorkareaID}, func() error {
		lease, err := s.load(snapshot.LeaseID)
		if err != nil {
			return err
		}
		if lease.TerminalStatus == nil {
			return nil
		}
		lease.TerminalStatus.DeliveryState = TerminalStatusDeadLetter
		lease.TerminalStatus.LastError = &message
		return s.saveUnlocked(*lease)
	})
}

func (s *LeaseStore) listActionable() ([]TerminalLease, error) {
	if err := s.Ready(); err != nil {
		return nil, err
	}
	return s.listActionableUnlocked()
}

func (s *LeaseStore) listActionableUnlocked() ([]TerminalLease, error) {
	entries, err := os.ReadDir(s.actionable)
	if err != nil {
		return nil, err
	}
	leases := make([]TerminalLease, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		leaseID := strings.TrimSuffix(entry.Name(), ".json")
		lease, err := s.load(leaseID)
		if err != nil {
			return nil, err
		}
		if !lease.RetainsWorkarea() {
			return nil, fmt.Errorf("runtime/workarea: actionable index contains released lease %s", leaseID)
		}
		leases = append(leases, *lease)
	}
	return leases, nil
}

func (s *LeaseStore) load(leaseID string) (*TerminalLease, error) {
	if err := validateGeneratedID(leaseID, "twl_"); err != nil {
		return nil, ErrLeaseNotFound
	}
	data, err := s.readFile(s.recordPath(leaseID))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrLeaseNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("runtime/workarea: read lease %s: %w", leaseID, err)
	}
	var lease TerminalLease
	if err := json.Unmarshal(data, &lease); err != nil {
		return nil, fmt.Errorf("runtime/workarea: decode lease %s: %w", leaseID, err)
	}
	if lease.LeaseID != leaseID || lease.State == "" {
		return nil, fmt.Errorf("runtime/workarea: invalid durable lease %s", leaseID)
	}
	return &lease, nil
}

func (s *LeaseStore) saveUnlocked(lease TerminalLease) error {
	if err := s.writeRecord(lease); err != nil {
		return err
	}
	marker := s.actionablePath(lease.LeaseID)
	if lease.RetainsWorkarea() {
		if _, err := os.Stat(marker); errors.Is(err, fs.ErrNotExist) {
			if err := s.writeActionableMarker(lease.LeaseID); err != nil {
				return err
			}
		} else if err != nil {
			return fmt.Errorf("runtime/workarea: stat actionable marker: %w", err)
		}
	} else if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("runtime/workarea: remove actionable marker: %w", err)
	} else if err == nil {
		if err := syncDir(s.actionable); err != nil {
			return err
		}
	}
	return nil
}

func (s *LeaseStore) writeRecord(lease TerminalLease) error {
	data, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime/workarea: marshal lease %s: %w", lease.LeaseID, err)
	}
	return writeFileAtomic(s.records, s.recordPath(lease.LeaseID), ".lease-*.tmp", data)
}

func (s *LeaseStore) writeActionableMarker(leaseID string) error {
	data := []byte(`{"leaseId":"` + leaseID + `"}`)
	return writeFileAtomic(s.actionable, s.actionablePath(leaseID), ".actionable-*.tmp", data)
}

func (s *LeaseStore) recordPath(leaseID string) string {
	return filepath.Join(s.records, leaseID+".json")
}

func (s *LeaseStore) actionablePath(leaseID string) string {
	return filepath.Join(s.actionable, leaseID+".json")
}

func (s *LeaseStore) initializeClock() error {
	data, err := os.ReadFile(s.clockPath)
	if errors.Is(err, fs.ErrNotExist) {
		return writeFileAtomic(s.dir, s.clockPath, ".clock-*.tmp", []byte("0"))
	}
	if err != nil {
		return err
	}
	_, err = parseClock(data)
	return err
}

func (s *LeaseStore) sampleClock(ctx context.Context) (int64, error) {
	var nowMS int64
	err := s.withLocks(ctx, []string{clockLockKey}, func() error {
		var err error
		nowMS, err = s.sampleClockLocked()
		return err
	})
	if err != nil {
		return 0, s.failClosed(err)
	}
	return nowMS, nil
}

func (s *LeaseStore) sampleClockLocked() (int64, error) {
	data, err := os.ReadFile(s.clockPath)
	if err != nil {
		return 0, err
	}
	persisted, err := parseClock(data)
	if err != nil {
		return 0, err
	}
	raw := s.now().UnixNano() / int64(time.Millisecond)
	nowMS := max(raw, persisted)
	if err := writeFileAtomic(s.dir, s.clockPath, ".clock-*.tmp", []byte(strconv.FormatInt(nowMS, 10))); err != nil {
		return 0, err
	}
	return nowMS, nil
}

func parseClock(data []byte) (int64, error) {
	text := string(data)
	if text == "" || strings.TrimSpace(text) != text {
		return 0, errors.New("runtime/workarea: invalid clock high-water mark")
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value < 0 {
		return 0, errors.New("runtime/workarea: invalid clock high-water mark")
	}
	return value, nil
}

func validateAcquireSpec(spec AcquireSpec) error {
	if err := validateCanonicalUUID(spec.SessionID); err != nil {
		return fmt.Errorf("runtime/workarea: sessionId: %w", err)
	}
	if err := validateGeneratedID(spec.TerminalResultID, "tr_"); err != nil {
		return fmt.Errorf("runtime/workarea: terminalResultId: %w", err)
	}
	if err := validateGeneratedID(spec.WorkareaID, "wa_"); err != nil {
		return fmt.Errorf("runtime/workarea: workareaId: %w", err)
	}
	if !filepath.IsAbs(spec.WorkareaPath) {
		return errors.New("runtime/workarea: workarea path must be absolute")
	}
	if err := spec.Policy.Validate(); err != nil {
		return err
	}
	if spec.ReleaseDisposition != "destroy" && spec.ReleaseDisposition != "return-to-pool" && spec.ReleaseDisposition != "pause" && spec.ReleaseDisposition != "archive" {
		return errors.New("runtime/workarea: invalid release disposition")
	}
	return nil
}

func validateAcquireReplay(lease TerminalLease, spec AcquireSpec) error {
	if err := validateIdentity(lease, spec.SessionID, spec.TerminalResultID, spec.WorkareaID); err != nil {
		return err
	}
	requestBytes, _ := CanonicalBytes(DefaultTerminalLeaseRequest())
	if filepath.Clean(lease.WorkareaPath) != filepath.Clean(spec.WorkareaPath) || lease.Policy != spec.Policy ||
		lease.ReleaseRequested != spec.ReleaseRequested || lease.ReleaseDisposition != spec.ReleaseDisposition ||
		!reflect.DeepEqual(lease.ReleaseMetadata, spec.ReleaseMetadata) || !bytes.Equal(lease.RequestBytes, requestBytes) {
		return fmt.Errorf("%w: terminal lease invariants differ for %s", ErrLeaseConflict, lease.LeaseID)
	}
	return nil
}

func validateExecutionClaimSpec(spec ExecutionClaimSpec) error {
	if err := validateGeneratedID(spec.LeaseID, "twl_"); err != nil {
		return err
	}
	if err := validateCanonicalUUID(spec.SessionID); err != nil {
		return err
	}
	if err := validateGeneratedID(spec.TerminalResultID, "tr_"); err != nil {
		return err
	}
	if err := validateGeneratedID(spec.WorkareaID, "wa_"); err != nil {
		return err
	}
	if err := validateCanonicalUUID(spec.InvocationID); err != nil {
		return err
	}
	return validateCanonicalUUID(spec.ClaimID)
}

func validateTerminalStatusSaveSpec(spec TerminalStatusSaveSpec) error {
	if err := validateGeneratedID(spec.LeaseID, "twl_"); err != nil {
		return err
	}
	if err := validateCanonicalUUID(spec.SessionID); err != nil {
		return err
	}
	if err := validateGeneratedID(spec.TerminalResultID, "tr_"); err != nil {
		return err
	}
	if err := validateGeneratedID(spec.WorkareaID, "wa_"); err != nil {
		return err
	}
	if err := validateGeneratedID(spec.ReceiverKey, "rcv_"); err != nil {
		return err
	}
	if len(spec.Body) == 0 {
		return errors.New("runtime/workarea: terminal status body required")
	}
	if spec.ExpectedExpiresAt.IsZero() {
		return errors.New("runtime/workarea: expected lease expiry required")
	}
	return nil
}

func validateIdentity(lease TerminalLease, sessionID, terminalResultID, workareaID string) error {
	if lease.SessionID != sessionID || lease.TerminalResultID != terminalResultID || lease.WorkareaID != workareaID {
		return fmt.Errorf("%w: lease %s", ErrLeaseConflict, lease.LeaseID)
	}
	return nil
}

func quarantineGuardMatchesLease(guard TerminalWorkareaQuarantine, lease TerminalLease) bool {
	return lease.RetainsWorkarea() &&
		guard.WorkareaID == lease.WorkareaID &&
		guard.SessionID == lease.SessionID &&
		guard.TerminalResultID == lease.TerminalResultID &&
		filepath.Clean(guard.WorkareaPath) == filepath.Clean(lease.WorkareaPath)
}

func terminalStatusIdentityEqual(left, right TerminalStatusOutbox) bool {
	return left.TerminalResultID == right.TerminalResultID && left.ReceiverKey == right.ReceiverKey &&
		left.BodyBase64 == right.BodyBase64 && left.BodySHA256 == right.BodySHA256 && left.DeadlineAt.Equal(right.DeadlineAt)
}

func cloneMetadata(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func leaseIDFor(terminalResultID string) string {
	sum := sha256.Sum256([]byte(terminalResultID))
	return "twl_" + hex.EncodeToString(sum[:16])
}

func terminalResultReplayRetryDelay(attempts int64) time.Duration {
	return cappedRetryDelay(DefaultTerminalResultReplayRetryBase, DefaultTerminalResultReplayRetryMax, attempts)
}

func releaseRetryDelay(attempts int64) time.Duration {
	return cappedRetryDelay(DefaultReleaseRetryBase, DefaultReleaseRetryMax, attempts)
}

func cappedRetryDelay(base, maximum time.Duration, attempts int64) time.Duration {
	delay := base
	for attempt := int64(1); attempt < attempts && delay < maximum; attempt++ {
		delay *= 2
		if delay >= maximum {
			return maximum
		}
	}
	return delay
}

func runConcurrent[T any](ctx context.Context, items []T, concurrency int, fn func(T) error) error {
	if len(items) == 0 {
		return nil
	}
	concurrency = min(max(concurrency, 1), len(items))
	jobs := make(chan T)
	errs := make(chan error, len(items))
	var wg sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				if err := fn(item); err != nil {
					errs <- err
				}
			}
		}()
	}
	for _, item := range items {
		select {
		case jobs <- item:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			close(errs)
			all := []error{ctx.Err()}
			for err := range errs {
				all = append(all, err)
			}
			return errors.Join(all...)
		}
	}
	close(jobs)
	wg.Wait()
	close(errs)
	var all []error
	for err := range errs {
		all = append(all, err)
	}
	return errors.Join(all...)
}

func clearDirectoryFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return syncDir(dir)
}

// QuarantineStore implements the documented terminal-workarea contract.
type QuarantineStore struct {
	dir     string
	records string
}

func newQuarantineStore(dir string) (*QuarantineStore, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	store := &QuarantineStore{dir: abs, records: filepath.Join(abs, "records")}
	for _, path := range []string{store.dir, store.records} {
		if err := os.MkdirAll(path, 0o750); err != nil {
			return nil, err
		}
		if err := syncDir(path); err != nil {
			return nil, err
		}
	}
	if _, err := store.list(); err != nil {
		return nil, err
	}
	return store, nil
}

func (q *QuarantineStore) createGuard(spec AcquireSpec, now time.Time) (*TerminalWorkareaQuarantine, error) {
	quarantineID, err := NewGeneratedID("twq_")
	if err != nil {
		return nil, err
	}
	path := filepath.Clean(spec.WorkareaPath)
	digest := sha256.Sum256([]byte(path))
	record := TerminalWorkareaQuarantine{
		SchemaVersion: TerminalWorkareaQuarantineSchemaV1, QuarantineID: quarantineID,
		WorkareaID: spec.WorkareaID, SessionID: spec.SessionID, TerminalResultID: spec.TerminalResultID,
		WorkareaPath: path, PathSHA256: hex.EncodeToString(digest[:]), Reason: "lease-acquisition-failed",
		State: QuarantineGuarded, CreatedAt: now, UpdatedAt: now,
	}
	if err := q.write(record); err != nil {
		return nil, err
	}
	return &record, nil
}

func (q *QuarantineStore) promote(id string, state QuarantineState, message string, now time.Time) error {
	record, err := q.get(id)
	if err != nil {
		return err
	}
	record.State = state
	record.UpdatedAt = now
	if message == "" {
		record.LastError = nil
	} else {
		record.LastError = &message
	}
	return q.write(*record)
}

func (q *QuarantineStore) remove(id string) error {
	if err := os.Remove(filepath.Join(q.records, id+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDir(q.records)
}

func (q *QuarantineStore) findByWorkarea(workareaID, path string) (*TerminalWorkareaQuarantine, error) {
	items, err := q.list()
	if err != nil {
		return nil, err
	}
	for i := range items {
		if (workareaID != "" && items[i].WorkareaID == workareaID) || (path != "" && filepath.Clean(items[i].WorkareaPath) == filepath.Clean(path)) {
			return &items[i], nil
		}
	}
	return nil, nil
}

func (q *QuarantineStore) list() ([]TerminalWorkareaQuarantine, error) {
	entries, err := os.ReadDir(q.records)
	if err != nil {
		return nil, err
	}
	items := make([]TerminalWorkareaQuarantine, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(q.records, entry.Name()))
		if err != nil {
			return nil, err
		}
		var item TerminalWorkareaQuarantine
		if err := json.Unmarshal(data, &item); err != nil {
			return nil, err
		}
		if entry.Name() != item.QuarantineID+".json" {
			return nil, errors.New("runtime/workarea: quarantine filename identity mismatch")
		}
		items = append(items, item)
	}
	return items, nil
}

func (q *QuarantineStore) get(id string) (*TerminalWorkareaQuarantine, error) {
	if err := validateGeneratedID(id, "twq_"); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(q.records)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(id + ".json")
	if err != nil {
		return nil, err
	}
	var item TerminalWorkareaQuarantine
	if err := json.Unmarshal(data, &item); err != nil {
		return nil, err
	}
	return &item, nil
}

func (q *QuarantineStore) write(record TerminalWorkareaQuarantine) error {
	data, err := CanonicalBytes(record)
	if err != nil {
		return err
	}
	return writeFileAtomic(q.records, filepath.Join(q.records, record.QuarantineID+".json"), ".quarantine-*.tmp", data)
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
	root, err := os.OpenRoot(path)
	if err != nil {
		return fmt.Errorf("open directory root for sync: %w", err)
	}
	defer func() { _ = root.Close() }()
	dir, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("sync directory: %w", err)
	}
	return dir.Close()
}

func fileKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
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
	lockRoot, err := os.OpenRoot(s.locks)
	if err != nil {
		return err
	}
	defer func() { _ = lockRoot.Close() }()
	held := make([]*os.File, 0, len(ordered))
	for _, key := range ordered {
		lock, err := lockRoot.OpenFile(key+".lock", os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			releaseLocks(held)
			return err
		}
		for {
			err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				held = append(held, lock)
				break
			}
			if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
				_ = lock.Close()
				releaseLocks(held)
				return err
			}
			select {
			case <-ctx.Done():
				_ = lock.Close()
				releaseLocks(held)
				return ctx.Err()
			case <-time.After(lockRetryInterval):
			}
		}
	}
	defer releaseLocks(held)
	return fn()
}

func releaseLocks(locks []*os.File) {
	for i := len(locks) - 1; i >= 0; i-- {
		_ = syscall.Flock(int(locks[i].Fd()), syscall.LOCK_UN)
		_ = locks[i].Close()
	}
}
