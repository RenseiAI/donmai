// Package localqueue persists one host-local, single-writer session authority.
// It contains no provider credentials and exports no remote queue protocol.
package localqueue

import (
	"encoding/json"
	"errors"

	"github.com/RenseiAI/donmai/executioncell"
)

// Closed event and dispatch states understood by the local store.
const (
	EventSessionAdmitted     = "session.admitted"
	DispatchPending          = "pending"
	DispatchClaimed          = "claimed"
	DispatchLaunched         = "launched"
	DispatchUnknown          = "launch_outcome_unknown"
	DispatchTerminal         = "terminal"
	PublicationGitHubPR      = "github_pr"
	PublicationGitHubComment = "github_comment"
)

// Typed store errors distinguish conflicts, invalid input, corruption and
// ambiguous durability without exposing private payload bytes.
var (
	ErrConflict            = errors.New("localqueue: revision conflict")
	ErrBindingMismatch     = errors.New("localqueue: configured host binding mismatch")
	ErrAlreadyAdmitted     = errors.New("localqueue: source issue already admitted")
	ErrUnknownSession      = errors.New("localqueue: unknown session")
	ErrAttemptMismatch     = errors.New("localqueue: attempt fence mismatch")
	ErrInvalidEnvelope     = errors.New("localqueue: invalid admission envelope")
	ErrCorruptStore        = errors.New("localqueue: corrupt journal")
	ErrWriterLocked        = errors.New("localqueue: writer already locked")
	ErrAmbiguousCommit     = errors.New("localqueue: commit durability is ambiguous")
	ErrUnsupportedPlatform = errors.New("localqueue: kernel writer lock unsupported on this platform")
)

// Revision is the total order of durable local transactions. Revision one
// persists the configured authority; every mutation advances it by one.
type Revision uint64

// ConfiguredHostBinding is supplied by the trusted local daemon process.
// Selectors is live validation input and is never written to the queue.
type ConfiguredHostBinding struct {
	ScopeID   string
	HostID    string
	WorkerID  string
	Selectors *executioncell.ExecutionSelectorRegistry
}

// Authority is the durable local resource identity, not a commercial tenant.
type Authority struct {
	ScopeID   string `json:"scopeId"`
	HostID    string `json:"hostId"`
	WorkerID  string `json:"workerId"`
	CreatedAt string `json:"createdAt"`
}

// GitHubSource keys deduplication to immutable repository identity and issue
// number. OwnerRepo and IssueURL are checked correlation, never authority.
type GitHubSource struct {
	RepositoryID int64  `json:"repositoryId"`
	OwnerRepo    string `json:"ownerRepo"`
	IssueNumber  int    `json:"issueNumber"`
	IssueURL     string `json:"issueUrl"`
}

// LifecycleEvent is a closed, secret-free initial event. No issue text,
// credential, environment, or terminal body belongs in this payload.
type LifecycleEvent struct {
	Type       string `json:"type"`
	ScopeID    string `json:"scopeId"`
	SessionID  string `json:"sessionId"`
	ReceiptID  string `json:"receiptId"`
	RecordedAt string `json:"recordedAt"`
}

// DispatchOutbox is committed with admission, before any launch. Digest is
// filled and verified by Admit against the immutable envelope evidence.
type DispatchOutbox struct {
	Key            string `json:"key"`
	State          string `json:"state"`
	EnvelopeSHA256 string `json:"envelopeSha256"`
}

// AdmissionEnvelope carries only validated, canonical execution evidence.
// CredentialHandle names separate protected storage, never bearer material.
type AdmissionEnvelope struct {
	ScopeID            string                       `json:"scopeId"`
	Source             GitHubSource                 `json:"source"`
	Session            executioncell.SessionRef     `json:"session"`
	Intent             executioncell.DispatchIntent `json:"intent"`
	Receipt            json.RawMessage              `json:"receipt"`
	EffectiveCell      json.RawMessage              `json:"effectiveCell"`
	RuntimeBinding     json.RawMessage              `json:"runtimeBinding"`
	OperationalPayload json.RawMessage              `json:"operationalPayload"`
	HostPreflight      json.RawMessage              `json:"hostPreflight"`
	ProducerEvidence   json.RawMessage              `json:"producerEvidence"`
	InitialEvent       LifecycleEvent               `json:"initialEvent"`
	Dispatch           DispatchOutbox               `json:"dispatch"`
	CredentialHandle   string                       `json:"credentialHandle"`
}

// AdmissionRecord is an immutable projection rebuilt from the journal.
type AdmissionRecord struct {
	Revision         Revision          `json:"revision"`
	Envelope         AdmissionEnvelope `json:"envelope"`
	EnvelopeSHA256   string            `json:"envelopeSha256"`
	DispatchState    string            `json:"dispatchState"`
	CurrentAttemptID string            `json:"currentAttemptId,omitempty"`
}

// AttemptRef is local/private ownership correlation. It is never serialized
// into executioncell.RuntimeBinding.ClaimID or a pool ClaimReceipt.
type AttemptRef struct {
	ScopeID    string `json:"scopeId"`
	SessionID  string `json:"sessionId"`
	AttemptID  string `json:"attemptId"`
	WorkerID   string `json:"workerId"`
	Generation uint64 `json:"generation"`
}

// OwnedProcessCorrelation is evidence of an actually observed local launch.
type OwnedProcessCorrelation struct {
	ProcessID           int    `json:"processId"`
	ProcessStart        string `json:"processStart"`
	SessionDetailSHA256 string `json:"sessionDetailSha256"`
}

// ObservationKind is a closed local liveness/stop/denial vocabulary.
type ObservationKind string

// Closed observation kinds accepted from an authenticated exact attempt.
const (
	ObservationLiveness     ObservationKind = "liveness"
	ObservationStopRequest  ObservationKind = "stop_requested"
	ObservationPrestartDeny ObservationKind = "prestart_denied"
)

// BoundedTypedObservation contains no freeform stdout, secrets, or prompt.
type BoundedTypedObservation struct {
	Kind       ObservationKind `json:"kind"`
	ObservedAt string          `json:"observedAt"`
	Evidence   string          `json:"evidenceDigest,omitempty"`
}

// DurableReceipt retains exact request bytes for byte-identical response-loss
// replay. Its public lifecycle event records only BodySHA256 and status.
type DurableReceipt struct {
	Attempt    AttemptRef `json:"attempt"`
	Body       []byte     `json:"body"`
	BodySHA256 string     `json:"bodySha256"`
	Status     string     `json:"status"`
	Revision   Revision   `json:"revision"`
}

// SessionProjection is rebuilt from authoritative transactions, never from a
// mutable cross-file index.
type SessionProjection struct {
	Admission    AdmissionRecord           `json:"admission"`
	Attempt      *AttemptRef               `json:"attempt,omitempty"`
	Launch       *OwnedProcessCorrelation  `json:"launch,omitempty"`
	Observations []BoundedTypedObservation `json:"observations"`
	Terminal     *DurableReceipt           `json:"terminal,omitempty"`
	Publications []PublicationOutbox       `json:"publications"`
}

// PublicationOutbox is a secret-free request to publish a terminal outcome.
type PublicationOutbox struct {
	Key          string `json:"key"`
	SessionID    string `json:"sessionId"`
	ResultSHA256 string `json:"resultSha256"`
	Operation    string `json:"operation"`
	State        string `json:"state"`
	URL          string `json:"url,omitempty"`
	HeadSHA      string `json:"headSha,omitempty"`
	ReadAt       string `json:"readAt,omitempty"`
}

// PublicationEvidence is a bounded, public correlation read back after a
// publication side effect. It must not contain provider credentials.
type PublicationEvidence struct {
	URL     string `json:"url"`
	HeadSHA string `json:"headSha"`
	ReadAt  string `json:"readAt"`
}
