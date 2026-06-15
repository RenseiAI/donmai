package vcs

import (
	"context"
	"errors"
	"fmt"
)

// errNotImplemented is returned by stubbed verbs across the vcs package. Wrapped
// with fmt.Errorf so the failing operation is identifiable.
var errNotImplemented = errors.New("not implemented")

// AtomicCapabilities is the capability declaration for the Atomic adapter: a
// commutative, patch-theoretic VCS. Pushes commute by construction, so there is
// no merge queue (HasMergeQueue = false) and conflicts are frequently
// auto-resolved. Attestation is native (Ed25519).
//
// Ported from ATOMIC_VCS_CAPABILITIES in donmai-libraries vcs/atomic.ts.
var AtomicCapabilities = Capabilities{
	MergeModel:          "patch-theory",
	ConflictGranularity: "token",
	PatchModel:          "patch-theoretic",

	HasPullRequests:   true,
	HasReviewWorkflow: true,
	HasMergeQueue:     false,

	SupportsBranches: true,
	SupportsRebase:   false,

	ProvenanceNative: true,
	SupportsAttest:   true,

	SupportsBinary:            true,
	SupportsStructuredContent: false,
	SupportsLargeFiles:        true,
}

// AtomicProvider implements Provider for the Atomic commutative VCS. Because
// pushes commute, the landing serializer skips the queue and lands proposals
// directly via MergeProposal.
//
// Ported from AtomicVCSProvider in donmai-libraries vcs/atomic.ts.
type AtomicProvider struct {
	caps Capabilities
}

// compile-time assertion.
var _ Provider = (*AtomicProvider)(nil)

// NewAtomicProvider returns an Atomic provider with default capabilities.
func NewAtomicProvider() *AtomicProvider {
	return &AtomicProvider{caps: AtomicCapabilities}
}

// Name returns the provider id.
func (p *AtomicProvider) Name() string { return "atomic" }

// Capabilities returns the provider's capabilities.
func (p *AtomicProvider) Capabilities() Capabilities { return p.caps }

// Clone — stub: not yet ported.
func (p *AtomicProvider) Clone(ctx context.Context, uri, dst string, opts CloneOpts) (Workspace, error) {
	_ = ctx
	_ = uri
	_ = dst
	_ = opts
	return Workspace{}, fmt.Errorf("AtomicProvider.Clone: %w", errNotImplemented)
}

// RecordChange — stub: not yet ported.
func (p *AtomicProvider) RecordChange(ctx context.Context, ws Workspace, change ChangeRequest) (ChangeRef, error) {
	_ = ctx
	_ = ws
	_ = change
	return ChangeRef{}, fmt.Errorf("AtomicProvider.RecordChange: %w", errNotImplemented)
}

// Push — stub: not yet ported.
func (p *AtomicProvider) Push(ctx context.Context, ws Workspace, target PushTarget) (PushResult, error) {
	_ = ctx
	_ = ws
	_ = target
	return PushResult{}, fmt.Errorf("AtomicProvider.Push: %w", errNotImplemented)
}

// Pull — stub: not yet ported.
func (p *AtomicProvider) Pull(ctx context.Context, ws Workspace, source PullSource) (MergeResult, error) {
	_ = ctx
	_ = ws
	_ = source
	return MergeResult{}, fmt.Errorf("AtomicProvider.Pull: %w", errNotImplemented)
}

// OpenProposal — stub: not yet ported. Gate: HasPullRequests.
func (p *AtomicProvider) OpenProposal(ctx context.Context, ws Workspace, opts ProposalOpts) (ProposalRef, error) {
	if err := AssertCapability(p.caps, "HasPullRequests", p.Name()); err != nil {
		return ProposalRef{}, err
	}
	_ = ctx
	_ = ws
	_ = opts
	return ProposalRef{}, fmt.Errorf("AtomicProvider.OpenProposal: %w", errNotImplemented)
}

// MergeProposal — stub: not yet ported. Gate: HasPullRequests.
func (p *AtomicProvider) MergeProposal(ctx context.Context, ref ProposalRef, strategy string) (MergeResult, error) {
	if err := AssertCapability(p.caps, "HasPullRequests", p.Name()); err != nil {
		return MergeResult{}, err
	}
	_ = ctx
	_ = ref
	_ = strategy
	return MergeResult{}, fmt.Errorf("AtomicProvider.MergeProposal: %w", errNotImplemented)
}

// EnqueueForMerge always returns an *UnsupportedOperationError: Atomic is
// commutative (HasMergeQueue = false), so there is no queue to enqueue into.
func (p *AtomicProvider) EnqueueForMerge(ctx context.Context, ref ProposalRef, opts QueueOpts) (QueueTicket, error) {
	_ = ctx
	_ = ref
	_ = opts
	return QueueTicket{}, AssertCapability(p.caps, "HasMergeQueue", p.Name())
}

// Attest — stub: not yet ported. Gate: SupportsAttest.
func (p *AtomicProvider) Attest(ctx context.Context, ws Workspace, meta SessionAttestation) (AttestationRef, error) {
	if err := AssertCapability(p.caps, "SupportsAttest", p.Name()); err != nil {
		return AttestationRef{}, err
	}
	_ = ctx
	_ = ws
	_ = meta
	return AttestationRef{}, fmt.Errorf("AtomicProvider.Attest: %w", errNotImplemented)
}
