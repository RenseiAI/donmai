package workarea

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const (
	// TerminalLeaseRequestSchemaV1 and the related constants define the public terminal-workarea contract.
	TerminalLeaseRequestSchemaV1         = "donmai.terminal-workarea-lease-request.v1"
	TerminalLeaseSchemaV1                = "donmai.terminal-workarea-lease.v1"
	TerminalLeaseClaimSchemaV1           = "donmai.terminal-workarea-lease-claim.v1"
	TerminalLeaseAcknowledgementSchemaV1 = "donmai.terminal-workarea-lease-ack.v1"
	TerminalLeaseAckOutcomeSchemaV1      = "donmai.terminal-workarea-lease-ack-outcome.v1"
	TerminalStatusOutboxSchemaV1         = "donmai.terminal-status-outbox.v1"
	TerminalWorkareaQuarantineSchemaV1   = "donmai.terminal-workarea-quarantine.v1"

	SettlementBudgetMS  int64 = 977_000
	LeaseSafetyMarginMS int64 = 60_000
	LeaseDurationMS     int64 = 1_800_000
	MaxLeaseDurationMS  int64 = 7_200_000
	ClaimMinimumMS      int64 = SettlementBudgetMS + LeaseSafetyMarginMS
	QueueMinimumMS      int64 = ClaimMinimumMS + 60_000

	MaximumLeaseDuration = time.Duration(MaxLeaseDurationMS) * time.Millisecond
)

const canonicalMillisLayout = "2006-01-02T15:04:05.000Z"

// TerminalLeaseRequest implements the documented terminal-workarea contract.
type TerminalLeaseRequest struct {
	SchemaVersion      string `json:"schemaVersion"`
	SettlementBudgetMS int64  `json:"settlementBudgetMs"`
	SafetyMarginMS     int64  `json:"safetyMarginMs"`
	LeaseDurationMS    int64  `json:"leaseDurationMs"`
	MaxLeaseDurationMS int64  `json:"maxLeaseDurationMs"`
}

// DefaultTerminalLeaseRequest implements the documented terminal-workarea contract.
func DefaultTerminalLeaseRequest() TerminalLeaseRequest {
	return TerminalLeaseRequest{
		SchemaVersion:      TerminalLeaseRequestSchemaV1,
		SettlementBudgetMS: SettlementBudgetMS,
		SafetyMarginMS:     LeaseSafetyMarginMS,
		LeaseDurationMS:    LeaseDurationMS,
		MaxLeaseDurationMS: MaxLeaseDurationMS,
	}
}

// Validate implements the documented terminal-workarea contract.
func (r TerminalLeaseRequest) Validate() error {
	if r != DefaultTerminalLeaseRequest() {
		return errors.New("runtime/workarea: terminal lease request must use the fixed v1 profile")
	}
	return nil
}

// Policy implements the documented terminal-workarea contract.
func (r TerminalLeaseRequest) Policy() (LeasePolicy, error) {
	if err := r.Validate(); err != nil {
		return LeasePolicy{}, err
	}
	return DefaultLeasePolicy(), nil
}

// MarshalJSON implements the documented terminal-workarea contract.
func (r TerminalLeaseRequest) MarshalJSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	out := []byte{'{'}
	var err error
	out, err = appendCanonicalStringField(out, "schemaVersion", r.SchemaVersion, true)
	if err != nil {
		return nil, err
	}
	out, _ = appendCanonicalIntegerField(out, "settlementBudgetMs", r.SettlementBudgetMS, false)
	out, _ = appendCanonicalIntegerField(out, "safetyMarginMs", r.SafetyMarginMS, false)
	out, _ = appendCanonicalIntegerField(out, "leaseDurationMs", r.LeaseDurationMS, false)
	out, _ = appendCanonicalIntegerField(out, "maxLeaseDurationMs", r.MaxLeaseDurationMS, false)
	return append(out, '}'), nil
}

// UnmarshalJSON implements the documented terminal-workarea contract.
func (r *TerminalLeaseRequest) UnmarshalJSON(data []byte) error {
	values, err := parseCanonicalJSONObject(data)
	if err != nil {
		return err
	}
	if err := requireClosedFields(values, "schemaVersion", "settlementBudgetMs", "safetyMarginMs", "leaseDurationMs", "maxLeaseDurationMs"); err != nil {
		return err
	}
	if r.SchemaVersion, err = requireString(values, "schemaVersion"); err != nil {
		return err
	}
	if r.SettlementBudgetMS, err = requireInteger(values, "settlementBudgetMs"); err != nil {
		return err
	}
	if r.SafetyMarginMS, err = requireInteger(values, "safetyMarginMs"); err != nil {
		return err
	}
	if r.LeaseDurationMS, err = requireInteger(values, "leaseDurationMs"); err != nil {
		return err
	}
	if r.MaxLeaseDurationMS, err = requireInteger(values, "maxLeaseDurationMs"); err != nil {
		return err
	}
	return r.Validate()
}

// TerminalLeaseDescriptor implements the documented terminal-workarea contract.
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

// TerminalLeaseProjection implements the documented terminal-workarea contract.
type TerminalLeaseProjection struct {
	LeaseID          string    `json:"leaseId"`
	WorkareaID       string    `json:"workareaId"`
	TerminalResultID string    `json:"terminalResultId"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

// Projection implements the documented terminal-workarea contract.
func (d TerminalLeaseDescriptor) Projection() TerminalLeaseProjection {
	return TerminalLeaseProjection{
		LeaseID: d.LeaseID, WorkareaID: d.WorkareaID,
		TerminalResultID: d.TerminalResultID, ExpiresAt: d.ExpiresAt,
	}
}

// Validate implements the documented terminal-workarea contract.
func (d TerminalLeaseDescriptor) Validate() error {
	if d.SchemaVersion != TerminalLeaseSchemaV1 {
		return fmt.Errorf("runtime/workarea: unsupported terminal lease schema %q", d.SchemaVersion)
	}
	if err := validateGeneratedID(d.LeaseID, "twl_"); err != nil {
		return fmt.Errorf("runtime/workarea: leaseId: %w", err)
	}
	if err := validateCanonicalUUID(d.SessionID); err != nil {
		return fmt.Errorf("runtime/workarea: sessionId: %w", err)
	}
	if err := validateGeneratedID(d.TerminalResultID, "tr_"); err != nil {
		return fmt.Errorf("runtime/workarea: terminalResultId: %w", err)
	}
	if err := validateGeneratedID(d.WorkareaID, "wa_"); err != nil {
		return fmt.Errorf("runtime/workarea: workareaId: %w", err)
	}
	if _, err := formatCanonicalMillis(d.AcquiredAt); err != nil {
		return fmt.Errorf("runtime/workarea: acquiredAt: %w", err)
	}
	if _, err := formatCanonicalMillis(d.ExpiresAt); err != nil {
		return fmt.Errorf("runtime/workarea: expiresAt: %w", err)
	}
	if !d.ExpiresAt.After(d.AcquiredAt) {
		return errors.New("runtime/workarea: expiresAt must be after acquiredAt")
	}
	if d.SettlementBudgetMS != SettlementBudgetMS {
		return fmt.Errorf("runtime/workarea: settlementBudgetMs must be %d", SettlementBudgetMS)
	}
	return nil
}

// MarshalJSON implements the documented terminal-workarea contract.
func (d TerminalLeaseDescriptor) MarshalJSON() ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	acquiredAt, _ := formatCanonicalMillis(d.AcquiredAt)
	expiresAt, _ := formatCanonicalMillis(d.ExpiresAt)
	out := []byte{'{'}
	var err error
	for i, field := range [][2]string{
		{"schemaVersion", d.SchemaVersion}, {"leaseId", d.LeaseID}, {"sessionId", d.SessionID},
		{"terminalResultId", d.TerminalResultID}, {"workareaId", d.WorkareaID},
		{"acquiredAt", acquiredAt}, {"expiresAt", expiresAt},
	} {
		out, err = appendCanonicalStringField(out, field[0], field[1], i == 0)
		if err != nil {
			return nil, err
		}
	}
	out, _ = appendCanonicalIntegerField(out, "settlementBudgetMs", d.SettlementBudgetMS, false)
	return append(out, '}'), nil
}

// UnmarshalJSON implements the documented terminal-workarea contract.
func (d *TerminalLeaseDescriptor) UnmarshalJSON(data []byte) error {
	values, err := parseCanonicalJSONObject(data)
	if err != nil {
		return err
	}
	if err := requireClosedFields(values, "schemaVersion", "leaseId", "sessionId", "terminalResultId", "workareaId", "acquiredAt", "expiresAt", "settlementBudgetMs"); err != nil {
		return err
	}
	if d.SchemaVersion, err = requireString(values, "schemaVersion"); err != nil {
		return err
	}
	if d.LeaseID, err = requireString(values, "leaseId"); err != nil {
		return err
	}
	if d.SessionID, err = requireString(values, "sessionId"); err != nil {
		return err
	}
	if d.TerminalResultID, err = requireString(values, "terminalResultId"); err != nil {
		return err
	}
	if d.WorkareaID, err = requireString(values, "workareaId"); err != nil {
		return err
	}
	acquiredAt, err := requireString(values, "acquiredAt")
	if err != nil {
		return err
	}
	if d.AcquiredAt, err = parseCanonicalMillis(acquiredAt); err != nil {
		return err
	}
	expiresAt, err := requireString(values, "expiresAt")
	if err != nil {
		return err
	}
	if d.ExpiresAt, err = parseCanonicalMillis(expiresAt); err != nil {
		return err
	}
	if d.SettlementBudgetMS, err = requireInteger(values, "settlementBudgetMs"); err != nil {
		return err
	}
	return d.Validate()
}

// Validate implements the documented terminal-workarea contract.
func (p TerminalLeaseProjection) Validate() error {
	if err := validateGeneratedID(p.LeaseID, "twl_"); err != nil {
		return fmt.Errorf("runtime/workarea: leaseId: %w", err)
	}
	if err := validateGeneratedID(p.WorkareaID, "wa_"); err != nil {
		return fmt.Errorf("runtime/workarea: workareaId: %w", err)
	}
	if err := validateGeneratedID(p.TerminalResultID, "tr_"); err != nil {
		return fmt.Errorf("runtime/workarea: terminalResultId: %w", err)
	}
	if _, err := formatCanonicalMillis(p.ExpiresAt); err != nil {
		return fmt.Errorf("runtime/workarea: expiresAt: %w", err)
	}
	return nil
}

// MarshalJSON implements the documented terminal-workarea contract.
func (p TerminalLeaseProjection) MarshalJSON() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	expiresAt, _ := formatCanonicalMillis(p.ExpiresAt)
	out := []byte{'{'}
	var err error
	for i, field := range [][2]string{{"leaseId", p.LeaseID}, {"workareaId", p.WorkareaID}, {"terminalResultId", p.TerminalResultID}, {"expiresAt", expiresAt}} {
		out, err = appendCanonicalStringField(out, field[0], field[1], i == 0)
		if err != nil {
			return nil, err
		}
	}
	return append(out, '}'), nil
}

// UnmarshalJSON implements the documented terminal-workarea contract.
func (p *TerminalLeaseProjection) UnmarshalJSON(data []byte) error {
	values, err := parseCanonicalJSONObject(data)
	if err != nil {
		return err
	}
	if err := requireClosedFields(values, "leaseId", "workareaId", "terminalResultId", "expiresAt"); err != nil {
		return err
	}
	if p.LeaseID, err = requireString(values, "leaseId"); err != nil {
		return err
	}
	if p.WorkareaID, err = requireString(values, "workareaId"); err != nil {
		return err
	}
	if p.TerminalResultID, err = requireString(values, "terminalResultId"); err != nil {
		return err
	}
	expiresAt, err := requireString(values, "expiresAt")
	if err != nil {
		return err
	}
	if p.ExpiresAt, err = parseCanonicalMillis(expiresAt); err != nil {
		return err
	}
	return p.Validate()
}

// LeaseExecutionClaim implements the documented terminal-workarea contract.
type LeaseExecutionClaim struct {
	SchemaVersion    string    `json:"schemaVersion"`
	InvocationID     string    `json:"invocationId"`
	ClaimID          string    `json:"claimId"`
	LeaseID          string    `json:"leaseId"`
	SessionID        string    `json:"sessionId"`
	TerminalResultID string    `json:"terminalResultId"`
	WorkareaID       string    `json:"workareaId"`
	ClaimedAt        time.Time `json:"claimedAt"`
}

// Validate implements the documented terminal-workarea contract.
func (c LeaseExecutionClaim) Validate() error {
	if c.SchemaVersion != TerminalLeaseClaimSchemaV1 {
		return fmt.Errorf("runtime/workarea: unsupported terminal lease claim schema %q", c.SchemaVersion)
	}
	for field, value := range map[string]string{"invocationId": c.InvocationID, "claimId": c.ClaimID, "sessionId": c.SessionID} {
		if err := validateCanonicalUUID(value); err != nil {
			return fmt.Errorf("runtime/workarea: %s: %w", field, err)
		}
	}
	for _, item := range [][2]string{{c.LeaseID, "twl_"}, {c.TerminalResultID, "tr_"}, {c.WorkareaID, "wa_"}} {
		if err := validateGeneratedID(item[0], item[1]); err != nil {
			return err
		}
	}
	_, err := formatCanonicalMillis(c.ClaimedAt)
	return err
}

// MarshalJSON implements the documented terminal-workarea contract.
func (c LeaseExecutionClaim) MarshalJSON() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	claimedAt, _ := formatCanonicalMillis(c.ClaimedAt)
	fields := [][2]string{{"schemaVersion", c.SchemaVersion}, {"invocationId", c.InvocationID}, {"claimId", c.ClaimID}, {"leaseId", c.LeaseID}, {"sessionId", c.SessionID}, {"terminalResultId", c.TerminalResultID}, {"workareaId", c.WorkareaID}, {"claimedAt", claimedAt}}
	return marshalCanonicalStringObject(fields)
}

// UnmarshalJSON implements the documented terminal-workarea contract.
func (c *LeaseExecutionClaim) UnmarshalJSON(data []byte) error {
	values, err := parseCanonicalJSONObject(data)
	if err != nil {
		return err
	}
	fields := []string{"schemaVersion", "invocationId", "claimId", "leaseId", "sessionId", "terminalResultId", "workareaId", "claimedAt"}
	if err := requireClosedFields(values, fields...); err != nil {
		return err
	}
	outputs := []*string{&c.SchemaVersion, &c.InvocationID, &c.ClaimID, &c.LeaseID, &c.SessionID, &c.TerminalResultID, &c.WorkareaID}
	for i := range outputs {
		if *outputs[i], err = requireString(values, fields[i]); err != nil {
			return err
		}
	}
	claimedAt, err := requireString(values, "claimedAt")
	if err != nil {
		return err
	}
	if c.ClaimedAt, err = parseCanonicalMillis(claimedAt); err != nil {
		return err
	}
	return c.Validate()
}

// TerminalResultAcknowledgement implements the documented terminal-workarea contract.
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

// Validate implements the documented terminal-workarea contract.
func (a TerminalResultAcknowledgement) Validate() error {
	if a.SchemaVersion != TerminalLeaseAcknowledgementSchemaV1 || !a.Acknowledged {
		return ErrAcknowledgementRequired
	}
	for field, value := range map[string]string{"invocationId": a.InvocationID, "claimId": a.ClaimID, "sessionId": a.SessionID} {
		if err := validateCanonicalUUID(value); err != nil {
			return fmt.Errorf("runtime/workarea: %s: %w", field, err)
		}
	}
	for _, item := range [][2]string{{a.LeaseID, "twl_"}, {a.TerminalResultID, "tr_"}, {a.WorkareaID, "wa_"}} {
		if err := validateGeneratedID(item[0], item[1]); err != nil {
			return err
		}
	}
	return nil
}

// MarshalJSON implements the documented terminal-workarea contract.
func (a TerminalResultAcknowledgement) MarshalJSON() ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	out := []byte{'{'}
	var err error
	out, err = appendCanonicalStringField(out, "schemaVersion", a.SchemaVersion, true)
	if err != nil {
		return nil, err
	}
	out = appendCanonicalBoolField(out, "acknowledged", a.Acknowledged, false)
	for _, field := range [][2]string{{"invocationId", a.InvocationID}, {"claimId", a.ClaimID}, {"leaseId", a.LeaseID}, {"sessionId", a.SessionID}, {"terminalResultId", a.TerminalResultID}, {"workareaId", a.WorkareaID}} {
		out, err = appendCanonicalStringField(out, field[0], field[1], false)
		if err != nil {
			return nil, err
		}
	}
	return append(out, '}'), nil
}

// UnmarshalJSON implements the documented terminal-workarea contract.
func (a *TerminalResultAcknowledgement) UnmarshalJSON(data []byte) error {
	values, err := parseCanonicalJSONObject(data)
	if err != nil {
		return err
	}
	fields := []string{"schemaVersion", "acknowledged", "invocationId", "claimId", "leaseId", "sessionId", "terminalResultId", "workareaId"}
	if err := requireClosedFields(values, fields...); err != nil {
		return err
	}
	if a.SchemaVersion, err = requireString(values, "schemaVersion"); err != nil {
		return err
	}
	if a.Acknowledged, err = requireBool(values, "acknowledged"); err != nil {
		return err
	}
	outputs := []*string{&a.InvocationID, &a.ClaimID, &a.LeaseID, &a.SessionID, &a.TerminalResultID, &a.WorkareaID}
	for i, field := range fields[2:] {
		if *outputs[i], err = requireString(values, field); err != nil {
			return err
		}
	}
	return a.Validate()
}

// AcknowledgementOutcomeValue implements the documented terminal-workarea contract.
type AcknowledgementOutcomeValue string

// AcknowledgementReason implements the documented terminal-workarea contract.
type AcknowledgementReason string

const (
	// AcknowledgementApplied and the related constants define the public terminal-workarea contract.
	AcknowledgementApplied        AcknowledgementOutcomeValue = "applied"
	AcknowledgementAlreadyApplied AcknowledgementOutcomeValue = "already-applied"
	AcknowledgementRejected       AcknowledgementOutcomeValue = "rejected"

	AcknowledgementClaimMissing     AcknowledgementReason = "claim-missing"
	AcknowledgementIdentityMismatch AcknowledgementReason = "identity-mismatch"
	AcknowledgementStateConflict    AcknowledgementReason = "state-conflict"
)

// TerminalAcknowledgementOutcome implements the documented terminal-workarea contract.
type TerminalAcknowledgementOutcome struct {
	SchemaVersion           string                      `json:"schemaVersion"`
	Outcome                 AcknowledgementOutcomeValue `json:"outcome"`
	Reason                  *AcknowledgementReason      `json:"reason"`
	LeaseID                 string                      `json:"leaseId"`
	TerminalResultID        string                      `json:"terminalResultId"`
	LeaseState              LeaseState                  `json:"leaseState"`
	ProviderReleaseComplete bool                        `json:"providerReleaseComplete"`
}

// Validate implements the documented terminal-workarea contract.
func (o TerminalAcknowledgementOutcome) Validate() error {
	if o.SchemaVersion != TerminalLeaseAckOutcomeSchemaV1 {
		return errors.New("runtime/workarea: invalid acknowledgement outcome schema")
	}
	if err := validateGeneratedID(o.LeaseID, "twl_"); err != nil {
		return err
	}
	if err := validateGeneratedID(o.TerminalResultID, "tr_"); err != nil {
		return err
	}
	if o.LeaseState != LeaseActive && o.LeaseState != LeaseReleasePending && o.LeaseState != LeaseReleased {
		return errors.New("runtime/workarea: invalid acknowledgement outcome lease state")
	}
	if o.ProviderReleaseComplete != (o.LeaseState == LeaseReleased) {
		return errors.New("runtime/workarea: providerReleaseComplete disagrees with leaseState")
	}
	switch o.Outcome {
	case AcknowledgementApplied, AcknowledgementAlreadyApplied:
		if o.Reason != nil {
			return errors.New("runtime/workarea: successful acknowledgement outcome reason must be null")
		}
	case AcknowledgementRejected:
		if o.Reason == nil || (*o.Reason != AcknowledgementClaimMissing && *o.Reason != AcknowledgementIdentityMismatch && *o.Reason != AcknowledgementStateConflict) {
			return errors.New("runtime/workarea: rejected acknowledgement outcome reason is invalid")
		}
	default:
		return errors.New("runtime/workarea: invalid acknowledgement outcome")
	}
	return nil
}

// MarshalJSON implements the documented terminal-workarea contract.
func (o TerminalAcknowledgementOutcome) MarshalJSON() ([]byte, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	out := []byte{'{'}
	var err error
	out, err = appendCanonicalStringField(out, "schemaVersion", o.SchemaVersion, true)
	if err != nil {
		return nil, err
	}
	out, _ = appendCanonicalStringField(out, "outcome", string(o.Outcome), false)
	var reason *string
	if o.Reason != nil {
		text := string(*o.Reason)
		reason = &text
	}
	out, err = appendCanonicalNullableStringField(out, "reason", reason, false)
	if err != nil {
		return nil, err
	}
	out, _ = appendCanonicalStringField(out, "leaseId", o.LeaseID, false)
	out, _ = appendCanonicalStringField(out, "terminalResultId", o.TerminalResultID, false)
	out, _ = appendCanonicalStringField(out, "leaseState", string(o.LeaseState), false)
	out = appendCanonicalBoolField(out, "providerReleaseComplete", o.ProviderReleaseComplete, false)
	return append(out, '}'), nil
}

// UnmarshalJSON implements the documented terminal-workarea contract.
func (o *TerminalAcknowledgementOutcome) UnmarshalJSON(data []byte) error {
	values, err := parseCanonicalJSONObject(data)
	if err != nil {
		return err
	}
	if err := requireClosedFields(values, "schemaVersion", "outcome", "reason", "leaseId", "terminalResultId", "leaseState", "providerReleaseComplete"); err != nil {
		return err
	}
	if o.SchemaVersion, err = requireString(values, "schemaVersion"); err != nil {
		return err
	}
	outcome, err := requireString(values, "outcome")
	if err != nil {
		return err
	}
	o.Outcome = AcknowledgementOutcomeValue(outcome)
	reason, err := requireNullableString(values, "reason")
	if err != nil {
		return err
	}
	if reason != nil {
		value := AcknowledgementReason(*reason)
		o.Reason = &value
	} else {
		o.Reason = nil
	}
	if o.LeaseID, err = requireString(values, "leaseId"); err != nil {
		return err
	}
	if o.TerminalResultID, err = requireString(values, "terminalResultId"); err != nil {
		return err
	}
	state, err := requireString(values, "leaseState")
	if err != nil {
		return err
	}
	o.LeaseState = LeaseState(state)
	if o.ProviderReleaseComplete, err = requireBool(values, "providerReleaseComplete"); err != nil {
		return err
	}
	return o.Validate()
}

// TerminalStatusDeliveryState implements the documented terminal-workarea contract.
type TerminalStatusDeliveryState string

// TerminalStatusApplicationState implements the documented terminal-workarea contract.
type TerminalStatusApplicationState string

const (
	// TerminalStatusPending and the related constants define the public terminal-workarea contract.
	TerminalStatusPending    TerminalStatusDeliveryState = "pending"
	TerminalStatusAttempting TerminalStatusDeliveryState = "attempting"
	TerminalStatusDelivered  TerminalStatusDeliveryState = "delivered"
	TerminalStatusDeadLetter TerminalStatusDeliveryState = "dead-letter"

	TerminalApplicationPending          TerminalStatusApplicationState = "pending"
	TerminalApplicationApplied          TerminalStatusApplicationState = "applied"
	TerminalApplicationNotAuthoritative TerminalStatusApplicationState = "not-authoritative"
	TerminalApplicationRejected         TerminalStatusApplicationState = "rejected"
)

// TerminalStatusOutbox implements the documented terminal-workarea contract.
type TerminalStatusOutbox struct {
	SchemaVersion    string                         `json:"schemaVersion"`
	TerminalResultID string                         `json:"terminalResultId"`
	ReceiverKey      string                         `json:"receiverKey"`
	BodyBase64       string                         `json:"bodyBase64"`
	BodySHA256       string                         `json:"bodySha256"`
	DeadlineAt       time.Time                      `json:"deadlineAt"`
	DeliveryState    TerminalStatusDeliveryState    `json:"deliveryState"`
	ApplicationState TerminalStatusApplicationState `json:"applicationState"`
	AttemptCount     int64                          `json:"attemptCount"`
	NextAttemptAt    time.Time                      `json:"nextAttemptAt"`
	LastAttemptAt    *time.Time                     `json:"lastAttemptAt"`
	LastError        *string                        `json:"lastError"`
}

// NewTerminalStatusOutbox implements the documented terminal-workarea contract.
func NewTerminalStatusOutbox(terminalResultID, receiverKey string, body []byte, deadlineAt, nextAttemptAt time.Time) TerminalStatusOutbox {
	digest := sha256.Sum256(body)
	return TerminalStatusOutbox{
		SchemaVersion: TerminalStatusOutboxSchemaV1, TerminalResultID: terminalResultID,
		ReceiverKey: receiverKey, BodyBase64: base64.StdEncoding.EncodeToString(body),
		BodySHA256: hex.EncodeToString(digest[:]), DeadlineAt: deadlineAt,
		DeliveryState: TerminalStatusPending, ApplicationState: TerminalApplicationPending,
		NextAttemptAt: nextAttemptAt,
	}
}

// Body implements the documented terminal-workarea contract.
func (o TerminalStatusOutbox) Body() ([]byte, error) {
	body, err := base64.StdEncoding.Strict().DecodeString(o.BodyBase64)
	if err != nil || base64.StdEncoding.EncodeToString(body) != o.BodyBase64 {
		return nil, errors.New("runtime/workarea: bodyBase64 is not canonical padded RFC4648")
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != o.BodySHA256 {
		return nil, errors.New("runtime/workarea: bodySha256 does not match bodyBase64")
	}
	return body, nil
}

// Validate implements the documented terminal-workarea contract.
func (o TerminalStatusOutbox) Validate() error {
	if o.SchemaVersion != TerminalStatusOutboxSchemaV1 {
		return errors.New("runtime/workarea: invalid terminal status outbox schema")
	}
	if err := validateGeneratedID(o.TerminalResultID, "tr_"); err != nil {
		return err
	}
	if err := validateGeneratedID(o.ReceiverKey, "rcv_"); err != nil {
		return err
	}
	if !validLowerHex(o.BodySHA256, 64) {
		return errors.New("runtime/workarea: bodySha256 must be 64 lowercase hexadecimal digits")
	}
	if _, err := o.Body(); err != nil {
		return err
	}
	if _, err := formatCanonicalMillis(o.DeadlineAt); err != nil {
		return err
	}
	if _, err := formatCanonicalMillis(o.NextAttemptAt); err != nil {
		return err
	}
	if o.LastAttemptAt != nil {
		if _, err := formatCanonicalMillis(*o.LastAttemptAt); err != nil {
			return err
		}
	}
	if o.AttemptCount < 0 {
		return errors.New("runtime/workarea: attemptCount must not be negative")
	}
	switch o.DeliveryState {
	case TerminalStatusPending, TerminalStatusAttempting, TerminalStatusDelivered, TerminalStatusDeadLetter:
	default:
		return errors.New("runtime/workarea: invalid deliveryState")
	}
	switch o.ApplicationState {
	case TerminalApplicationPending, TerminalApplicationApplied, TerminalApplicationNotAuthoritative, TerminalApplicationRejected:
	default:
		return errors.New("runtime/workarea: invalid applicationState")
	}
	return nil
}

// MarshalJSON implements the documented terminal-workarea contract.
func (o TerminalStatusOutbox) MarshalJSON() ([]byte, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	deadline, _ := formatCanonicalMillis(o.DeadlineAt)
	next, _ := formatCanonicalMillis(o.NextAttemptAt)
	out := []byte{'{'}
	var err error
	for i, field := range [][2]string{{"schemaVersion", o.SchemaVersion}, {"terminalResultId", o.TerminalResultID}, {"receiverKey", o.ReceiverKey}, {"bodyBase64", o.BodyBase64}, {"bodySha256", o.BodySHA256}, {"deadlineAt", deadline}, {"deliveryState", string(o.DeliveryState)}, {"applicationState", string(o.ApplicationState)}} {
		out, err = appendCanonicalStringField(out, field[0], field[1], i == 0)
		if err != nil {
			return nil, err
		}
	}
	out, _ = appendCanonicalIntegerField(out, "attemptCount", o.AttemptCount, false)
	out, _ = appendCanonicalStringField(out, "nextAttemptAt", next, false)
	var lastAttempt *string
	if o.LastAttemptAt != nil {
		text, _ := formatCanonicalMillis(*o.LastAttemptAt)
		lastAttempt = &text
	}
	out, err = appendCanonicalNullableStringField(out, "lastAttemptAt", lastAttempt, false)
	if err != nil {
		return nil, err
	}
	out, err = appendCanonicalNullableStringField(out, "lastError", o.LastError, false)
	if err != nil {
		return nil, err
	}
	return append(out, '}'), nil
}

// UnmarshalJSON implements the documented terminal-workarea contract.
func (o *TerminalStatusOutbox) UnmarshalJSON(data []byte) error {
	values, err := parseCanonicalJSONObject(data)
	if err != nil {
		return err
	}
	fields := []string{"schemaVersion", "terminalResultId", "receiverKey", "bodyBase64", "bodySha256", "deadlineAt", "deliveryState", "applicationState", "attemptCount", "nextAttemptAt", "lastAttemptAt", "lastError"}
	if err := requireClosedFields(values, fields...); err != nil {
		return err
	}
	stringOutputs := []*string{&o.SchemaVersion, &o.TerminalResultID, &o.ReceiverKey, &o.BodyBase64, &o.BodySHA256}
	for i, field := range fields[:5] {
		if *stringOutputs[i], err = requireString(values, field); err != nil {
			return err
		}
	}
	deadline, err := requireString(values, "deadlineAt")
	if err != nil {
		return err
	}
	if o.DeadlineAt, err = parseCanonicalMillis(deadline); err != nil {
		return err
	}
	delivery, err := requireString(values, "deliveryState")
	if err != nil {
		return err
	}
	o.DeliveryState = TerminalStatusDeliveryState(delivery)
	application, err := requireString(values, "applicationState")
	if err != nil {
		return err
	}
	o.ApplicationState = TerminalStatusApplicationState(application)
	if o.AttemptCount, err = requireInteger(values, "attemptCount"); err != nil {
		return err
	}
	next, err := requireString(values, "nextAttemptAt")
	if err != nil {
		return err
	}
	if o.NextAttemptAt, err = parseCanonicalMillis(next); err != nil {
		return err
	}
	lastAttempt, err := requireNullableString(values, "lastAttemptAt")
	if err != nil {
		return err
	}
	if lastAttempt != nil {
		parsed, parseErr := parseCanonicalMillis(*lastAttempt)
		if parseErr != nil {
			return parseErr
		}
		o.LastAttemptAt = &parsed
	} else {
		o.LastAttemptAt = nil
	}
	if o.LastError, err = requireNullableString(values, "lastError"); err != nil {
		return err
	}
	return o.Validate()
}

// QuarantineState implements the documented terminal-workarea contract.
type QuarantineState string

const (
	// QuarantineGuarded and the related constants define the public terminal-workarea contract.
	QuarantineGuarded        QuarantineState = "guarded"
	QuarantineQuarantined    QuarantineState = "quarantined"
	QuarantineCleanupPending QuarantineState = "cleanup-pending"
)

// TerminalWorkareaQuarantine implements the documented terminal-workarea contract.
type TerminalWorkareaQuarantine struct {
	SchemaVersion    string          `json:"schemaVersion"`
	QuarantineID     string          `json:"quarantineId"`
	WorkareaID       string          `json:"workareaId"`
	SessionID        string          `json:"sessionId"`
	TerminalResultID string          `json:"terminalResultId"`
	WorkareaPath     string          `json:"workareaPath"`
	PathSHA256       string          `json:"pathSha256"`
	Reason           string          `json:"reason"`
	State            QuarantineState `json:"state"`
	CreatedAt        time.Time       `json:"createdAt"`
	UpdatedAt        time.Time       `json:"updatedAt"`
	LastError        *string         `json:"lastError"`
}

// Validate implements the documented terminal-workarea contract.
func (q TerminalWorkareaQuarantine) Validate() error {
	if q.SchemaVersion != TerminalWorkareaQuarantineSchemaV1 || q.Reason != "lease-acquisition-failed" {
		return errors.New("runtime/workarea: invalid quarantine schema or reason")
	}
	if err := validateGeneratedID(q.QuarantineID, "twq_"); err != nil {
		return err
	}
	if err := validateGeneratedID(q.WorkareaID, "wa_"); err != nil {
		return err
	}
	if err := validateCanonicalUUID(q.SessionID); err != nil {
		return err
	}
	if err := validateGeneratedID(q.TerminalResultID, "tr_"); err != nil {
		return err
	}
	if !filepath.IsAbs(q.WorkareaPath) {
		return errors.New("runtime/workarea: quarantine workareaPath must be absolute")
	}
	pathDigest := sha256.Sum256([]byte(q.WorkareaPath))
	if q.PathSHA256 != hex.EncodeToString(pathDigest[:]) {
		return errors.New("runtime/workarea: quarantine pathSha256 mismatch")
	}
	if q.State != QuarantineGuarded && q.State != QuarantineQuarantined && q.State != QuarantineCleanupPending {
		return errors.New("runtime/workarea: invalid quarantine state")
	}
	if _, err := formatCanonicalMillis(q.CreatedAt); err != nil {
		return err
	}
	if _, err := formatCanonicalMillis(q.UpdatedAt); err != nil {
		return err
	}
	return nil
}

// MarshalJSON implements the documented terminal-workarea contract.
func (q TerminalWorkareaQuarantine) MarshalJSON() ([]byte, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	created, _ := formatCanonicalMillis(q.CreatedAt)
	updated, _ := formatCanonicalMillis(q.UpdatedAt)
	out := []byte{'{'}
	var err error
	for i, field := range [][2]string{{"schemaVersion", q.SchemaVersion}, {"quarantineId", q.QuarantineID}, {"workareaId", q.WorkareaID}, {"sessionId", q.SessionID}, {"terminalResultId", q.TerminalResultID}, {"workareaPath", q.WorkareaPath}, {"pathSha256", q.PathSHA256}, {"reason", q.Reason}, {"state", string(q.State)}, {"createdAt", created}, {"updatedAt", updated}} {
		out, err = appendCanonicalStringField(out, field[0], field[1], i == 0)
		if err != nil {
			return nil, err
		}
	}
	out, err = appendCanonicalNullableStringField(out, "lastError", q.LastError, false)
	if err != nil {
		return nil, err
	}
	return append(out, '}'), nil
}

// UnmarshalJSON implements the documented terminal-workarea contract.
func (q *TerminalWorkareaQuarantine) UnmarshalJSON(data []byte) error {
	values, err := parseCanonicalJSONObject(data)
	if err != nil {
		return err
	}
	fields := []string{"schemaVersion", "quarantineId", "workareaId", "sessionId", "terminalResultId", "workareaPath", "pathSha256", "reason", "state", "createdAt", "updatedAt", "lastError"}
	if err := requireClosedFields(values, fields...); err != nil {
		return err
	}
	outputs := []*string{&q.SchemaVersion, &q.QuarantineID, &q.WorkareaID, &q.SessionID, &q.TerminalResultID, &q.WorkareaPath, &q.PathSHA256, &q.Reason}
	for i, field := range fields[:8] {
		if *outputs[i], err = requireString(values, field); err != nil {
			return err
		}
	}
	state, err := requireString(values, "state")
	if err != nil {
		return err
	}
	q.State = QuarantineState(state)
	created, err := requireString(values, "createdAt")
	if err != nil {
		return err
	}
	if q.CreatedAt, err = parseCanonicalMillis(created); err != nil {
		return err
	}
	updated, err := requireString(values, "updatedAt")
	if err != nil {
		return err
	}
	if q.UpdatedAt, err = parseCanonicalMillis(updated); err != nil {
		return err
	}
	if q.LastError, err = requireNullableString(values, "lastError"); err != nil {
		return err
	}
	return q.Validate()
}

func marshalCanonicalStringObject(fields [][2]string) ([]byte, error) {
	out := []byte{'{'}
	var err error
	for i, field := range fields {
		out, err = appendCanonicalStringField(out, field[0], field[1], i == 0)
		if err != nil {
			return nil, err
		}
	}
	return append(out, '}'), nil
}

func formatCanonicalMillis(value time.Time) (string, error) {
	value = value.UTC()
	if value.Nanosecond()%int(time.Millisecond) != 0 {
		return "", errors.New("timestamp must have exact millisecond precision")
	}
	return value.Format(canonicalMillisLayout), nil
}

func parseCanonicalMillis(value string) (time.Time, error) {
	if len(value) != len("2006-01-02T15:04:05.000Z") || value[4] != '-' || value[7] != '-' || value[10] != 'T' || value[13] != ':' || value[16] != ':' || value[19] != '.' || value[23] != 'Z' {
		return time.Time{}, errors.New("runtime/workarea: timestamp must use UTC RFC3339 millisecond form")
	}
	parsed, err := time.Parse(canonicalMillisLayout, value)
	if err != nil || parsed.Format(canonicalMillisLayout) != value {
		return time.Time{}, errors.New("runtime/workarea: invalid canonical timestamp")
	}
	return parsed, nil
}

// NewGeneratedID implements the documented terminal-workarea contract.
func NewGeneratedID(prefix string) (string, error) {
	switch prefix {
	case "twl_", "wa_", "tr_", "rcv_", "twq_":
	default:
		return "", fmt.Errorf("runtime/workarea: unsupported generated identity prefix %q", prefix)
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("runtime/workarea: generate identity: %w", err)
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

// NewWorkareaID implements the documented terminal-workarea contract.
func NewWorkareaID() (string, error) { return NewGeneratedID("wa_") }

// IDForPath is retained for source compatibility only. Workarea identity is now
// acquisition-scoped and deliberately not derived from a reusable path.
func IDForPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("runtime/workarea: workarea path required")
	}
	if _, err := filepath.Abs(path); err != nil {
		return "", fmt.Errorf("runtime/workarea: resolve workarea path: %w", err)
	}
	return NewWorkareaID()
}

func validateCanonicalUUID(value string) error {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return errors.New("identity must be a canonical lowercase hyphenated UUID")
	}
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return errors.New("identity must be a canonical lowercase hyphenated UUID")
		}
	}
	if value[14] < '1' || value[14] > '5' {
		return errors.New("UUID version must be 1 through 5")
	}
	if !strings.ContainsRune("89ab", rune(value[19])) {
		return errors.New("UUID must use the RFC4122 variant")
	}
	return nil
}

func validateGeneratedID(value, prefix string) error {
	if len(value) != len(prefix)+32 || !strings.HasPrefix(value, prefix) || !validLowerHex(value[len(prefix):], 32) {
		return fmt.Errorf("identity must be %s followed by 32 lowercase hexadecimal digits", prefix)
	}
	return nil
}

func validLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, c := range value {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// CanonicalBytes implements the documented terminal-workarea contract.
func CanonicalBytes(value any) ([]byte, error) {
	if marshaler, ok := value.(json.Marshaler); ok {
		return marshaler.MarshalJSON()
	}
	return json.Marshal(value)
}

// Descriptor implements the documented terminal-workarea contract.
func (l TerminalLease) Descriptor() TerminalLeaseDescriptor {
	return TerminalLeaseDescriptor{
		SchemaVersion: TerminalLeaseSchemaV1, LeaseID: l.LeaseID, SessionID: l.SessionID,
		TerminalResultID: l.TerminalResultID, WorkareaID: l.WorkareaID,
		AcquiredAt: l.AcquiredAt, ExpiresAt: l.ExpiresAt, SettlementBudgetMS: SettlementBudgetMS,
	}
}

// ValidateDescriptor implements the documented terminal-workarea contract.
func (l TerminalLease) ValidateDescriptor(desc TerminalLeaseDescriptor, now time.Time, minRemaining time.Duration) error {
	if err := desc.Validate(); err != nil {
		return err
	}
	if desc != l.Descriptor() {
		return fmt.Errorf("%w: lease %s", ErrLeaseConflict, l.LeaseID)
	}
	if l.State != LeaseActive {
		return fmt.Errorf("runtime/workarea: terminal lease %s is %s", l.LeaseID, l.State)
	}
	remainingMS := l.ExpiresAt.UnixMilli() - now.UTC().UnixMilli()
	if remainingMS <= minRemaining.Milliseconds() {
		return fmt.Errorf("runtime/workarea: terminal lease has %dms remaining; need more than %dms", remainingMS, minRemaining.Milliseconds())
	}
	return nil
}
