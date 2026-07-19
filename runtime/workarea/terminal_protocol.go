package workarea

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const (
	// TerminalLeaseRequestSchemaV1 identifies the additive per-session request
	// that asks the runner to retain a successful terminal workarea.
	TerminalLeaseRequestSchemaV1 = "donmai.terminal-workarea-lease-request.v1"
	// TerminalLeaseSchemaV1 identifies the path-free descriptor persisted with
	// the terminal status observation.
	TerminalLeaseSchemaV1 = "donmai.terminal-workarea-lease.v1"
	// TerminalLeaseAcknowledgementSchemaV1 identifies the semantic settlement
	// acknowledgement that permits normal workarea disposition.
	TerminalLeaseAcknowledgementSchemaV1 = "donmai.terminal-workarea-lease-ack.v1"

	// MaximumLeaseDuration is the finite OSS ceiling accepted from a remote
	// lease request. Renewals remain bounded by the request's MaxLeaseDuration.
	MaximumLeaseDuration = DefaultMaxLeaseDuration
)

// TerminalLeaseRequest is the versioned, provider-neutral request threaded
// from a work claim to the runner. Durations are milliseconds on the wire.
type TerminalLeaseRequest struct {
	SchemaVersion      string `json:"schemaVersion"`
	SettlementBudgetMS int64  `json:"settlementBudgetMs"`
	SafetyMarginMS     int64  `json:"safetyMarginMs"`
	LeaseDurationMS    int64  `json:"leaseDurationMs"`
	MaxLeaseDurationMS int64  `json:"maxLeaseDurationMs"`
}

// Policy validates the wire request and converts it to the store's duration
// representation. The remote request may narrow, but never exceed, the finite
// OSS maximum.
func (r TerminalLeaseRequest) Policy() (LeasePolicy, error) {
	if r.SchemaVersion != TerminalLeaseRequestSchemaV1 {
		return LeasePolicy{}, fmt.Errorf("runtime/workarea: unsupported terminal lease request schema %q", r.SchemaVersion)
	}
	maxMS := MaximumLeaseDuration.Milliseconds()
	switch {
	case r.SettlementBudgetMS <= 0:
		return LeasePolicy{}, errors.New("runtime/workarea: settlement budget must be positive")
	case r.SafetyMarginMS < 0:
		return LeasePolicy{}, errors.New("runtime/workarea: safety margin must not be negative")
	case r.LeaseDurationMS <= 0:
		return LeasePolicy{}, errors.New("runtime/workarea: lease duration must be positive")
	case r.MaxLeaseDurationMS <= 0:
		return LeasePolicy{}, errors.New("runtime/workarea: max lease duration must be positive")
	case r.MaxLeaseDurationMS > maxMS:
		return LeasePolicy{}, fmt.Errorf("runtime/workarea: max lease duration exceeds finite maximum %dms", maxMS)
	case r.LeaseDurationMS > r.MaxLeaseDurationMS:
		return LeasePolicy{}, errors.New("runtime/workarea: max lease duration must cover initial lease duration")
	case r.SafetyMarginMS >= r.LeaseDurationMS || r.SettlementBudgetMS >= r.LeaseDurationMS-r.SafetyMarginMS:
		return LeasePolicy{}, errors.New("runtime/workarea: lease duration must exceed settlement budget plus safety margin")
	}
	return LeasePolicy{
		SettlementBudget: time.Duration(r.SettlementBudgetMS) * time.Millisecond,
		SafetyMargin:     time.Duration(r.SafetyMarginMS) * time.Millisecond,
		LeaseDuration:    time.Duration(r.LeaseDurationMS) * time.Millisecond,
		MaxLeaseDuration: time.Duration(r.MaxLeaseDurationMS) * time.Millisecond,
	}, nil
}

// IDForPath derives a stable opaque local identity without exposing the
// absolute path on any external wire.
func IDForPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("runtime/workarea: workarea path required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("runtime/workarea: resolve workarea path: %w", err)
	}
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	return "wa_" + hex.EncodeToString(sum[:16]), nil
}

// TerminalLeaseDescriptor is the immutable path-free lease identity carried on
// a terminal status and later repeated by a privileged consumer.
type TerminalLeaseDescriptor struct {
	SchemaVersion      string    `json:"schemaVersion"`
	LeaseID            string    `json:"leaseId"`
	SessionID          string    `json:"sessionId"`
	TerminalResultID   string    `json:"terminalResultId"`
	WorkareaID         string    `json:"workareaId"`
	AcquiredAt         time.Time `json:"acquiredAt"`
	ExpiresAt          time.Time `json:"expiresAt"`
	SettlementBudgetMS int64     `json:"settlementBudgetMs"`
}

// Descriptor returns the immutable path-free view of a durable lease.
func (l TerminalLease) Descriptor() TerminalLeaseDescriptor {
	return TerminalLeaseDescriptor{
		SchemaVersion:      TerminalLeaseSchemaV1,
		LeaseID:            l.LeaseID,
		SessionID:          l.SessionID,
		TerminalResultID:   l.TerminalResultID,
		WorkareaID:         l.WorkareaID,
		AcquiredAt:         l.AcquiredAt,
		ExpiresAt:          l.ExpiresAt,
		SettlementBudgetMS: l.SettlementBudget.Milliseconds(),
	}
}

// ValidateDescriptor checks a remote descriptor against local durable state
// before any filesystem access. The caller supplies the remaining-lifetime
// requirement for its claim plus settlement work.
func (l TerminalLease) ValidateDescriptor(desc TerminalLeaseDescriptor, now time.Time, minRemaining time.Duration) error {
	if desc.SchemaVersion != TerminalLeaseSchemaV1 {
		return fmt.Errorf("runtime/workarea: unsupported terminal lease schema %q", desc.SchemaVersion)
	}
	if strings.TrimSpace(desc.LeaseID) == "" || strings.TrimSpace(desc.SessionID) == "" ||
		strings.TrimSpace(desc.TerminalResultID) == "" || strings.TrimSpace(desc.WorkareaID) == "" {
		return errors.New("runtime/workarea: terminal lease descriptor identity required")
	}
	if desc.LeaseID != l.LeaseID || desc.SessionID != l.SessionID ||
		desc.TerminalResultID != l.TerminalResultID || desc.WorkareaID != l.WorkareaID ||
		!desc.AcquiredAt.Equal(l.AcquiredAt) || !desc.ExpiresAt.Equal(l.ExpiresAt) ||
		desc.SettlementBudgetMS != l.SettlementBudget.Milliseconds() {
		return fmt.Errorf("%w: lease %s", ErrLeaseConflict, l.LeaseID)
	}
	if l.State != LeaseActive {
		return fmt.Errorf("runtime/workarea: terminal lease %s is %s", l.LeaseID, l.State)
	}
	now = now.UTC()
	if !now.Before(l.ExpiresAt) {
		return ErrLeaseExpired
	}
	if minRemaining < 0 {
		return errors.New("runtime/workarea: minimum remaining lifetime must not be negative")
	}
	if l.ExpiresAt.Sub(now) < minRemaining {
		return fmt.Errorf("runtime/workarea: terminal lease has %s remaining; need %s", l.ExpiresAt.Sub(now), minRemaining)
	}
	return nil
}
