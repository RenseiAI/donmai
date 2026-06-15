// Package vcs defines the VersionControlProvider abstraction used by the landing
// serializer, plus GitHub and Atomic adapter stubs.
//
// Design:
//   - Required verbs (Clone, RecordChange, Push, Pull) every provider implements.
//   - Optional verbs (OpenProposal, MergeProposal, EnqueueForMerge, Attest) are
//     gated by capabilities; calling an unsupported verb returns an
//     *UnsupportedOperationError.
//   - Capabilities are declared up front (flat struct) so a scheduler can reason
//     about candidates without loading the implementation.
//   - MergeResult is a tagged union: clean | auto-resolved | conflict.
//     Auto-resolved MUST be surfaced (never swallowed) — it is audit-chain
//     evidence.
//
// Ported from donmai-libraries vcs/types.ts.
package vcs

import (
	"context"
	"fmt"
)

// Capabilities declares a provider's merge model, proposal/review support, and
// trust scheme up front so callers can gate optional verbs without loading the
// implementation.
//
// Ported from VersionControlProviderCapabilities in
// donmai-libraries providers/base.ts (VCS subset).
type Capabilities struct {
	// MergeModel names the merge strategy family: "three-way-text",
	// "patch-theory", "crdt", "last-write-wins", "object-version", "cell-merge".
	MergeModel string
	// ConflictGranularity: "line", "token", "object", "cell", "none".
	ConflictGranularity string
	// PatchModel: "commit-graph", "patch-theoretic", "object-version",
	// "cell-based".
	PatchModel string

	// HasPullRequests gates OpenProposal / MergeProposal.
	HasPullRequests bool
	// HasReviewWorkflow indicates a review concept exists.
	HasReviewWorkflow bool
	// HasMergeQueue gates EnqueueForMerge. False ⇒ commutative VCS (pushes
	// commute by construction); the worker lands directly without queuing.
	HasMergeQueue bool

	// SupportsBranches / SupportsRebase describe branching semantics.
	SupportsBranches bool
	SupportsRebase   bool

	// ProvenanceNative is true when attestation is native (e.g. Atomic Ed25519)
	// rather than faked via commit trailers / object metadata.
	ProvenanceNative bool
	// SupportsAttest gates Attest.
	SupportsAttest bool

	// SupportsBinary / SupportsStructuredContent / SupportsLargeFiles describe
	// content shape support.
	SupportsBinary            bool
	SupportsStructuredContent bool
	SupportsLargeFiles        bool
}

// Workspace is a materialized working copy.
type Workspace struct {
	ID         string
	ProviderID string
	// Path is the local working-copy path.
	Path string
	// HeadRef is the commit / patch-set / object version.
	HeadRef string
}

// CloneOpts configures Clone.
type CloneOpts struct {
	// Ref is the target ref (branch/tag/SHA) to check out after cloning.
	Ref string
	// Depth is the shallow clone depth (git); 0 means full clone.
	Depth int
	// SingleBranch clones only a single branch.
	SingleBranch bool
}

// CellChange is a cell-level update for structured content (sheets, Notion).
type CellChange struct {
	// Address is the cell address (e.g. "A1", "Sheet1!B3").
	Address string
	// Value is the new cell value.
	Value any
}

// KitProviderID identifies a kit + version for attestation.
type KitProviderID struct {
	ID      string
	Version string
}

// WorkareaSnapshotRef references a workarea snapshot for attestation.
type WorkareaSnapshotRef struct {
	Ref        string
	ProviderID string
}

// ModelRef identifies the model used in a session.
type ModelRef struct {
	Provider string
	Model    string
}

// SessionAttestation is provenance metadata attached to a recorded change.
type SessionAttestation struct {
	AgentID             string
	Model               ModelRef
	SessionID           string
	KitIDs              []KitProviderID
	WorkareaSnapshotRef *WorkareaSnapshotRef
	ReviewerHints       []string
	StartedAt           string
	// SignedBy is the signing identity (keypair URL / DID), when supported.
	SignedBy string
}

// ChangeRequest describes a change to record.
type ChangeRequest struct {
	// Message is the commit message or equivalent.
	Message string
	// Paths are content-based paths (git, atomic, s3).
	Paths []string
	// Cells are cell-level updates for structured content.
	Cells []CellChange
	// Attestation is optional provenance metadata.
	Attestation *SessionAttestation
}

// ChangeRef references a recorded change.
type ChangeRef struct {
	// Ref is the commit SHA / patch ID / object version / row revision.
	Ref string
	// RecordedAt is the ISO timestamp of the recorded change.
	RecordedAt string
	// Summary is a human-readable summary.
	Summary string
}

// PushTarget identifies where to push.
type PushTarget struct {
	Remote string
	Ref    string
}

// PullSource identifies where to pull from.
type PullSource struct {
	Remote string
	Ref    string
}

// ProposalOpts configures OpenProposal.
type ProposalOpts struct {
	Title string
	Body  string
	// BaseRef is the base branch to land the proposal into.
	BaseRef   string
	Reviewers []string
	Labels    []string
}

// ProposalRef references an opened proposal.
type ProposalRef struct {
	// ID is the provider-specific identifier (PR number, MR IID, etc.).
	ID string
	// URL is the human-readable URL.
	URL string
	// State is one of "open", "merged", "closed".
	State string
}

// QueueOpts configures EnqueueForMerge.
type QueueOpts struct {
	// RequiredLabels must be present to enqueue.
	RequiredLabels []string
	// Priority within the queue.
	Priority int
}

// QueueTicket references an enqueued proposal.
type QueueTicket struct {
	ID string
	// Position at time of enqueue (1-based), or 0 if unknown.
	Position int
	// EnqueuedAt is the ISO timestamp when enqueued.
	EnqueuedAt string
}

// Conflict describes an unresolved conflict.
type Conflict struct {
	// FilePath is relative to the workspace.
	FilePath string
	// Detail is conflict detail (diff output, etc.).
	Detail string
}

// AutoResolution describes a conflict the provider resolved automatically.
type AutoResolution struct {
	FilePath string
	// Strategy used (e.g. "patch-theory", "three-way").
	Strategy string
}

// PushResultKind is the kind of a PushResult.
type PushResultKind string

const (
	// PushPushed — the push succeeded.
	PushPushed PushResultKind = "pushed"
	// PushRejected — the push was rejected by the remote.
	PushRejected PushResultKind = "rejected"
)

// PushResult is the result of a push.
type PushResult struct {
	Kind PushResultKind
	// Ref is the new remote HEAD (when Kind == PushPushed).
	Ref ChangeRef
	// Reason is one of "non-fast-forward", "auth", "policy" (when Kind ==
	// PushRejected).
	Reason  string
	Details string
}

// MergeResultKind is the kind of a MergeResult.
type MergeResultKind string

const (
	// MergeClean — no overlapping changes; fast-forward or trivial merge.
	MergeClean MergeResultKind = "clean"
	// MergeAutoResolved — provider resolved conflicts automatically. MUST be
	// surfaced to the caller, never swallowed.
	MergeAutoResolved MergeResultKind = "auto-resolved"
	// MergeConflict — human or agent intervention is required.
	MergeConflict MergeResultKind = "conflict"
)

// MergeResult is the result of a merge or pull, as a tagged union.
type MergeResult struct {
	Kind MergeResultKind
	// Resolutions is set when Kind == MergeAutoResolved.
	Resolutions []AutoResolution
	// Conflicts is set when Kind == MergeConflict.
	Conflicts []Conflict
}

// AttestationRef references a stored attestation.
type AttestationRef struct {
	ID string
	// StorageKind: "commit-trailer", "native", "object-metadata".
	StorageKind string
	// AttestedAt is the ISO timestamp.
	AttestedAt string
}

// Provider is the VCS family interface. Required verbs must be implemented by
// every provider; optional verbs are gated by Capabilities and return an
// *UnsupportedOperationError when unsupported.
//
// Ported from VersionControlProvider in donmai-libraries vcs/types.ts.
type Provider interface {
	// Name is the provider id (e.g. "github", "atomic").
	Name() string
	// Capabilities returns the provider's declared capabilities.
	Capabilities() Capabilities

	// Clone materializes a working copy at dst.
	Clone(ctx context.Context, uri, dst string, opts CloneOpts) (Workspace, error)
	// RecordChange records a change (stage+commit for git, record for Atomic).
	RecordChange(ctx context.Context, ws Workspace, change ChangeRequest) (ChangeRef, error)
	// Push pushes local changes to a remote.
	Push(ctx context.Context, ws Workspace, target PushTarget) (PushResult, error)
	// Pull pulls remote changes into the local working copy.
	Pull(ctx context.Context, ws Workspace, source PullSource) (MergeResult, error)

	// OpenProposal opens a proposal for review/merge. Gate: HasPullRequests.
	OpenProposal(ctx context.Context, ws Workspace, opts ProposalOpts) (ProposalRef, error)
	// MergeProposal merges a proposal. Gate: HasPullRequests.
	MergeProposal(ctx context.Context, ref ProposalRef, strategy string) (MergeResult, error)
	// EnqueueForMerge enqueues a proposal for serialized merge. Gate:
	// HasMergeQueue.
	EnqueueForMerge(ctx context.Context, ref ProposalRef, opts QueueOpts) (QueueTicket, error)
	// Attest attaches provenance metadata to a change. Gate: SupportsAttest.
	Attest(ctx context.Context, ws Workspace, meta SessionAttestation) (AttestationRef, error)
}

// UnsupportedOperationError is returned when a capability-gated optional verb is
// called on a provider that does not support that capability.
//
// Ported from UnsupportedOperationError in donmai-libraries vcs/types.ts.
type UnsupportedOperationError struct {
	// Capability is the capability flag name.
	Capability string
	// ProviderID is the provider id.
	ProviderID string
}

// Error implements error.
func (e *UnsupportedOperationError) Error() string {
	return fmt.Sprintf(
		"provider %q does not support capability %q; check Capabilities().%s before calling this verb",
		e.ProviderID, e.Capability, e.Capability,
	)
}

// AssertCapability returns an *UnsupportedOperationError if the named boolean
// capability is not true. Use at the top of every capability-gated verb.
//
// Ported from assertCapability in donmai-libraries vcs/types.ts.
func AssertCapability(caps Capabilities, capability, providerID string) error {
	var ok bool
	switch capability {
	case "HasPullRequests":
		ok = caps.HasPullRequests
	case "HasReviewWorkflow":
		ok = caps.HasReviewWorkflow
	case "HasMergeQueue":
		ok = caps.HasMergeQueue
	case "SupportsBranches":
		ok = caps.SupportsBranches
	case "SupportsRebase":
		ok = caps.SupportsRebase
	case "SupportsAttest":
		ok = caps.SupportsAttest
	case "SupportsBinary":
		ok = caps.SupportsBinary
	case "SupportsStructuredContent":
		ok = caps.SupportsStructuredContent
	case "SupportsLargeFiles":
		ok = caps.SupportsLargeFiles
	case "ProvenanceNative":
		ok = caps.ProvenanceNative
	default:
		ok = false
	}
	if !ok {
		return &UnsupportedOperationError{Capability: capability, ProviderID: providerID}
	}
	return nil
}
