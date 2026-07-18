// Package workarea owns provider-neutral workarea lifecycle primitives.
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

	lockRetryInterval = 10 * time.Millisecond
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
	LeaseID          string
	SessionID        string
	TerminalResultID string
	WorkareaID       string
	Acknowledged     bool
}

// TerminalLease is the crash-recoverable lease record.
type TerminalLease struct {
	LeaseID            string            `json:"leaseId"`
	SessionID          string            `json:"sessionId"`
	TerminalResultID   string            `json:"terminalResultId"`
	WorkareaID         string            `json:"workareaId"`
	WorkareaPath       string            `json:"workareaPath"`
	AcquiredAt         time.Time         `json:"acquiredAt"`
	ExpiresAt          time.Time         `json:"expiresAt"`
	MaxExpiresAt       time.Time         `json:"maxExpiresAt"`
	SettlementBudget   time.Duration     `json:"settlementBudget"`
	State              LeaseState        `json:"state"`
	ReleaseRequested   bool              `json:"releaseRequested,omitempty"`
	AcknowledgedAt     *time.Time        `json:"acknowledgedAt,omitempty"`
	ReleasedAt         *time.Time        `json:"releasedAt,omitempty"`
	ReleaseAttempts    int               `json:"releaseAttempts,omitempty"`
	NextReleaseAttempt *time.Time        `json:"nextReleaseAttempt,omitempty"`
	LastReleaseError   string            `json:"lastReleaseError,omitempty"`
	ReleaseMetadata    map[string]string `json:"releaseMetadata,omitempty"`
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

// LeaseStore persists one JSON record per terminal result and uses short-lived
// per-identity filesystem locks, so independent workareas remain parallel even
// when separate worker processes share the same host state directory.
type LeaseStore struct {
	dir     string
	records string
	locks   string
	now     func() time.Time
}

// NewLeaseStore opens or creates a crash-recoverable terminal lease store.
func NewLeaseStore(opts StoreOptions) (*LeaseStore, error) {
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, errors.New("runtime/workarea: lease store directory required")
	}
	abs, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("runtime/workarea: resolve lease store: %w", err)
	}
	s := &LeaseStore{
		dir:     abs,
		records: filepath.Join(abs, "records"),
		locks:   filepath.Join(abs, "locks"),
		now:     opts.Now,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if err := os.MkdirAll(s.records, 0o750); err != nil {
		return nil, fmt.Errorf("runtime/workarea: create lease records: %w", err)
	}
	if err := os.MkdirAll(s.locks, 0o750); err != nil {
		return nil, fmt.Errorf("runtime/workarea: create lease locks: %w", err)
	}
	if _, err := s.List(); err != nil {
		return nil, fmt.Errorf("runtime/workarea: recover leases: %w", err)
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
			out = existing
			return nil
		} else if !errors.Is(err, ErrLeaseNotFound) {
			return err
		}

		leases, err := s.List()
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
			ReleaseMetadata:  cloneMetadata(spec.ReleaseMetadata),
		}
		if err := s.save(*lease); err != nil {
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
		if err := s.save(*lease); err != nil {
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
	leases, err := s.List()
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
		leases, listErr := s.List()
		if listErr != nil {
			return listErr
		}
		for i := range leases {
			lease := leases[i]
			if lease.WorkareaID != workareaID || !lease.RetainsWorkarea() {
				continue
			}
			lease.ReleaseRequested = true
			if saveErr := s.save(lease); saveErr != nil {
				return saveErr
			}
			retained = true
			return nil
		}
		return nil
	})
	return retained, err
}

// Acknowledge applies the later semantic terminal-result acknowledgement. The
// matching lease moves through release-pending and releaser performs the normal
// provider disposition before the workarea stops being retained. Releaser must
// be idempotent because recovery can retry after a crash between provider
// release and the final durable state update.
func (s *LeaseStore) Acknowledge(ctx context.Context, ack TerminalResultAcknowledgement, releaser func(context.Context, TerminalLease) error) (*TerminalLease, error) {
	if !ack.Acknowledged {
		return nil, ErrAcknowledgementRequired
	}
	if releaser == nil {
		return nil, ErrReleaseRequired
	}
	return s.release(ctx, ack.LeaseID, ack.SessionID, ack.TerminalResultID, ack.WorkareaID, true, releaser)
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
	leases, err := s.List()
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
			_, releaseErr := s.release(attemptCtx, lease.LeaseID, lease.SessionID, lease.TerminalResultID, lease.WorkareaID, false, releaser)
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

func (s *LeaseStore) release(ctx context.Context, leaseID, sessionID, terminalResultID, workareaID string, acknowledged bool, releaser func(context.Context, TerminalLease) error) (*TerminalLease, error) {
	var out *TerminalLease
	err := s.withLocks(ctx, []string{"result:" + terminalResultID, "workarea:" + workareaID}, func() error {
		lease, err := s.load(leaseID)
		if err != nil {
			return err
		}
		if err := validateIdentity(*lease, sessionID, terminalResultID, workareaID); err != nil {
			return err
		}
		if lease.State == LeaseReleased {
			out = lease
			return nil
		}

		now := s.now().UTC()
		if acknowledged {
			lease.State = LeaseAcknowledged
			if lease.AcknowledgedAt == nil {
				lease.AcknowledgedAt = &now
			}
		} else if lease.State == LeaseActive {
			lease.State = LeaseExpired
		}
		lease.LastReleaseError = ""
		if err := s.save(*lease); err != nil {
			return err
		}

		// Persist release-pending before invoking provider policy. Holding the
		// per-result and per-workarea locks across the callback makes concurrent
		// acknowledgement/reap attempts idempotent while unrelated workareas
		// continue independently.
		lease.State = LeaseReleasePending
		lease.ReleaseAttempts++
		lease.NextReleaseAttempt = nil
		if err := s.save(*lease); err != nil {
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
		if err := s.save(*lease); err != nil {
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
	}
	return nil
}

func validateIdentity(lease TerminalLease, sessionID, terminalResultID, workareaID string) error {
	if lease.SessionID != sessionID || lease.TerminalResultID != terminalResultID || lease.WorkareaID != workareaID {
		return fmt.Errorf("%w: lease %s", ErrLeaseConflict, lease.LeaseID)
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

func (s *LeaseStore) recordPath(leaseID string) string {
	return filepath.Join(s.records, leaseID+".json")
}

func (s *LeaseStore) load(leaseID string) (*TerminalLease, error) {
	if strings.TrimSpace(leaseID) == "" || strings.ContainsAny(leaseID, `/\\`) {
		return nil, ErrLeaseNotFound
	}
	data, err := os.ReadFile(s.recordPath(leaseID)) //nolint:gosec // path is derived from a validated lease id under the configured store.
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

func (s *LeaseStore) save(lease TerminalLease) error {
	data, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal lease %s: %w", lease.LeaseID, err)
	}
	tmp, err := os.CreateTemp(s.records, ".lease-*.tmp")
	if err != nil {
		return fmt.Errorf("create lease temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod lease temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write lease temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync lease temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close lease temp: %w", err)
	}
	if err := os.Rename(tmpPath, s.recordPath(lease.LeaseID)); err != nil {
		return fmt.Errorf("commit lease %s: %w", lease.LeaseID, err)
	}
	dir, err := os.Open(s.records) //nolint:gosec // records is the configured durable store directory.
	if err != nil {
		return fmt.Errorf("open lease records for sync: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("sync lease records: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close lease records: %w", err)
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
