// Package executionevent is the journal-first normalized runtime execution
// event source. It owns only the OSS source contract and delivery mechanics;
// the hosted platform owns durable event storage and routing.
package executionevent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// RecordVersion and BatchVersion identify the strict normalized source wire.
	RecordVersion = "donmai.execution-event-source/v1alpha1"
	// BatchVersion identifies the bounded source batch wire.
	BatchVersion = "donmai.execution-event-source-batch/v1alpha1"

	// MaxBatchRecords is the maximum records per transport batch.
	MaxBatchRecords = 100
	// MaxRecordBytes is the maximum compact record size.
	MaxRecordBytes = 64 * 1024
	// MaxBatchBytes is the maximum compact batch size.
	MaxBatchBytes = 1024 * 1024
	// MaxTransportByte is the request body cap enforced by the route.
	MaxTransportByte = 1024 * 1024
	// MaxSafeInteger is the JSON number precision ceiling.
	MaxSafeInteger = 9007199254740991

	// DefaultEndpointPath is the platform runtime-ingest route template.
	DefaultEndpointPath = "/api/daemon/sessions/{id}/execution-events"
)

// Evidence describes source completeness without exposing provider payloads.
type Evidence struct {
	Kind         string `json:"kind"`
	Completeness string `json:"completeness,omitempty"`
	Partial      bool   `json:"partial,omitempty"`
	Replay       bool   `json:"replay,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`
}

// Causation carries bounded event ancestry when a source has it.
type Causation struct {
	RootEventID      string   `json:"rootEventId,omitempty"`
	ParentEventID    string   `json:"parentEventId,omitempty"`
	SubscriptionPath []string `json:"subscriptionPath,omitempty"`
	Depth            int      `json:"depth,omitempty"`
}

// Record is the closed source record accepted by the platform runtime ingest
// route. Payloads are intentionally topic-specific and secret-free.
type Record struct {
	Version           string         `json:"version"`
	EventID           string         `json:"eventId"`
	StructuredSeq     uint64         `json:"structuredSeq"`
	ObservedAt        string         `json:"observedAt"`
	EventType         string         `json:"eventType"`
	PersistencePolicy string         `json:"persistencePolicy"`
	Evidence          Evidence       `json:"evidence"`
	Causation         *Causation     `json:"causation,omitempty"`
	Payload           map[string]any `json:"payload"`
}

// Batch is one bounded source upload for a single session.
type Batch struct {
	Version   string   `json:"version"`
	SessionID string   `json:"sessionId"`
	Records   []Record `json:"records"`
}

// MarshalCompact encodes stable, non-indented JSON without HTML escaping.
func MarshalCompact(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "")
	if err := enc.Encode(value); err != nil {
		return nil, fmt.Errorf("executionevent: encode compact JSON: %w", err)
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
	return append([]byte(nil), b...), nil
}

// RuntimeSourceEventID derives the deterministic source identity required by
// the platform ingest contract.
func RuntimeSourceEventID(sessionID string, structuredSeq uint64) string {
	h := sha256.New()
	_, _ = h.Write([]byte(RecordVersion))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(sessionID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strconv.FormatUint(structuredSeq, 10)))
	return "evt_" + hex.EncodeToString(h.Sum(nil))
}

// NewRecord constructs and validates one durable normalized source record.
func NewRecord(sessionID string, seq uint64, observedAt time.Time, eventType string, payload map[string]any) (Record, error) {
	if strings.TrimSpace(sessionID) == "" || strings.Contains(sessionID, "\x00") {
		return Record{}, errors.New("executionevent: invalid session id")
	}
	if seq == 0 {
		return Record{}, errors.New("executionevent: structuredSeq must be positive")
	}
	if observedAt.IsZero() {
		return Record{}, errors.New("executionevent: observedAt is required")
	}
	if strings.TrimSpace(eventType) == "" {
		return Record{}, errors.New("executionevent: eventType is required")
	}
	if payload == nil {
		return Record{}, errors.New("executionevent: payload is required")
	}
	r := Record{
		Version: RecordVersion, EventID: RuntimeSourceEventID(sessionID, seq),
		StructuredSeq: seq, ObservedAt: observedAt.UTC().Format(time.RFC3339Nano),
		EventType: eventType, PersistencePolicy: "durable",
		Evidence: Evidence{Kind: "native", Completeness: "complete"}, Payload: payload,
	}
	if err := ValidateRecord(sessionID, r, seq-1); err != nil {
		return Record{}, err
	}
	return r, nil
}

// ValidateRecord validates one record and its contiguous predecessor.
func ValidateRecord(sessionID string, r Record, previousSeq uint64) error {
	if err := validateRecordShape(sessionID, r); err != nil {
		return err
	}
	if r.StructuredSeq != previousSeq+1 {
		return fmt.Errorf("executionevent: structuredSeq must be contiguous after %d", previousSeq)
	}
	return validateRecordBytes(r)
}

func validateRecordShape(sessionID string, r Record) error {
	if len(sessionID) == 0 || len(sessionID) > 256 {
		return errors.New("executionevent: session id must be 1..256 bytes")
	}
	if r.Version != RecordVersion {
		return fmt.Errorf("executionevent: unsupported record version %q", r.Version)
	}
	if r.StructuredSeq == 0 || r.StructuredSeq > MaxSafeInteger {
		return errors.New("executionevent: structuredSeq must be a safe positive integer")
	}
	if r.EventID != RuntimeSourceEventID(sessionID, r.StructuredSeq) {
		return errors.New("executionevent: eventId does not match session and sequence")
	}
	if _, err := time.Parse(time.RFC3339Nano, r.ObservedAt); err != nil {
		return fmt.Errorf("executionevent: invalid observedAt: %w", err)
	}
	if r.PersistencePolicy != "durable" && r.PersistencePolicy != "coalesced" && r.PersistencePolicy != "raw-pointer" {
		return fmt.Errorf("executionevent: unsupported persistence policy %q", r.PersistencePolicy)
	}
	if r.Evidence.Kind != "native" && r.Evidence.Kind != "inferred" && r.Evidence.Kind != "synthesized" {
		return fmt.Errorf("executionevent: unsupported evidence kind %q", r.Evidence.Kind)
	}
	if r.Evidence.Completeness != "" && r.Evidence.Completeness != "complete" && r.Evidence.Completeness != "partial" && r.Evidence.Completeness != "unknown" {
		return fmt.Errorf("executionevent: unsupported evidence completeness %q", r.Evidence.Completeness)
	}
	if len(r.EventType) == 0 || len(r.EventType) > 128 {
		return errors.New("executionevent: eventType must be 1..128 bytes")
	}
	if r.Causation != nil {
		if len(r.Causation.RootEventID) > 256 || len(r.Causation.ParentEventID) > 256 || len(r.Causation.SubscriptionPath) > 8 || r.Causation.Depth < 0 || r.Causation.Depth > 8 {
			return errors.New("executionevent: causation exceeds contract bounds")
		}
		for _, item := range r.Causation.SubscriptionPath {
			if len(item) == 0 || len(item) > 256 {
				return errors.New("executionevent: invalid causation subscription path")
			}
		}
	}
	return nil
}

func validateRecordBytes(r Record) error {
	if r.Payload == nil {
		return errors.New("executionevent: payload is required")
	}
	b, err := MarshalCompact(r)
	if err != nil {
		return err
	}
	if len(b) > MaxRecordBytes {
		return fmt.Errorf("executionevent: record exceeds %d bytes", MaxRecordBytes)
	}
	return nil
}

// ValidateBatch validates one bounded upload batch.
func ValidateBatch(batch Batch) error {
	if batch.Version != BatchVersion {
		return fmt.Errorf("executionevent: unsupported batch version %q", batch.Version)
	}
	if strings.TrimSpace(batch.SessionID) == "" {
		return errors.New("executionevent: batch sessionId is required")
	}
	if len(batch.Records) == 0 || len(batch.Records) > MaxBatchRecords {
		return fmt.Errorf("executionevent: batch records must be 1..%d", MaxBatchRecords)
	}
	if len(batch.SessionID) > 256 {
		return errors.New("executionevent: batch sessionId must be at most 256 bytes")
	}
	for index, record := range batch.Records {
		if err := validateRecordShape(batch.SessionID, record); err != nil {
			return err
		}
		if err := validateRecordBytes(record); err != nil {
			return err
		}
		if index > 0 && record.StructuredSeq != batch.Records[index-1].StructuredSeq+1 {
			return errors.New("executionevent: structuredSeq must increase contiguously within a batch")
		}
		if index > 0 && record.StructuredSeq <= batch.Records[index-1].StructuredSeq {
			return errors.New("executionevent: structuredSeq must increase strictly within a batch")
		}
	}
	b, err := MarshalCompact(batch)
	if err != nil {
		return err
	}
	if len(b) > MaxBatchBytes {
		return fmt.Errorf("executionevent: batch exceeds %d bytes", MaxBatchBytes)
	}
	return nil
}

// SessionEndpoint resolves the platform runtime-ingest route for one session.
func SessionEndpoint(baseURL, sessionID string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", errors.New("executionevent: BaseURL must be an absolute URL")
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return "", errors.New("executionevent: BaseURL scheme must be http or https")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/api/daemon/sessions/" + url.PathEscape(sessionID) + "/execution-events"
	base.RawQuery, base.Fragment = "", ""
	return base.String(), nil
}
