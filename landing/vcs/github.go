package vcs

import (
	"context"
	"fmt"
)

// GitHubCapabilities is the capability declaration for the GitHub adapter: git
// three-way text merge, full PR + merge-queue support, attestation faked via
// commit trailers.
//
// Ported from GITHUB_VCS_CAPABILITIES in donmai-libraries vcs/github.ts.
var GitHubCapabilities = Capabilities{
	MergeModel:          "three-way-text",
	ConflictGranularity: "line",
	PatchModel:          "commit-graph",

	HasPullRequests:   true,
	HasReviewWorkflow: true,
	HasMergeQueue:     true,

	SupportsBranches: true,
	SupportsRebase:   true,

	ProvenanceNative: false,
	SupportsAttest:   true,

	SupportsBinary:            true,
	SupportsStructuredContent: false,
	SupportsLargeFiles:        false,
}

// GitHubProvider implements Provider using the git CLI for content operations
// and the gh CLI for proposal operations. Attestation is faked via commit
// trailers (X-Donmai-*).
//
// Ported from GitHubVCSProvider in donmai-libraries vcs/github.ts.
type GitHubProvider struct {
	caps Capabilities
}

// compile-time assertion.
var _ Provider = (*GitHubProvider)(nil)

// GitHubOpts configures NewGitHubProvider.
type GitHubOpts struct {
	// HasMergeQueueOverride, when non-nil, overrides HasMergeQueue (set false for
	// repos without a provider-native merge queue enabled).
	HasMergeQueueOverride *bool
}

// NewGitHubProvider returns a GitHub provider with default capabilities, applying
// any override.
func NewGitHubProvider(opts GitHubOpts) *GitHubProvider {
	caps := GitHubCapabilities
	if opts.HasMergeQueueOverride != nil {
		caps.HasMergeQueue = *opts.HasMergeQueueOverride
	}
	return &GitHubProvider{caps: caps}
}

// Name returns the provider id.
func (p *GitHubProvider) Name() string { return "github" }

// Capabilities returns the provider's capabilities.
func (p *GitHubProvider) Capabilities() Capabilities { return p.caps }

// Clone — stub: not yet ported.
func (p *GitHubProvider) Clone(ctx context.Context, uri, dst string, opts CloneOpts) (Workspace, error) {
	_ = ctx
	_ = uri
	_ = dst
	_ = opts
	return Workspace{}, fmt.Errorf("GitHubProvider.Clone: %w", errNotImplemented)
}

// RecordChange — stub: not yet ported.
func (p *GitHubProvider) RecordChange(ctx context.Context, ws Workspace, change ChangeRequest) (ChangeRef, error) {
	_ = ctx
	_ = ws
	_ = change
	return ChangeRef{}, fmt.Errorf("GitHubProvider.RecordChange: %w", errNotImplemented)
}

// Push — stub: not yet ported.
func (p *GitHubProvider) Push(ctx context.Context, ws Workspace, target PushTarget) (PushResult, error) {
	_ = ctx
	_ = ws
	_ = target
	return PushResult{}, fmt.Errorf("GitHubProvider.Push: %w", errNotImplemented)
}

// Pull — stub: not yet ported.
func (p *GitHubProvider) Pull(ctx context.Context, ws Workspace, source PullSource) (MergeResult, error) {
	_ = ctx
	_ = ws
	_ = source
	return MergeResult{}, fmt.Errorf("GitHubProvider.Pull: %w", errNotImplemented)
}

// OpenProposal — stub: not yet ported. Gate: HasPullRequests.
func (p *GitHubProvider) OpenProposal(ctx context.Context, ws Workspace, opts ProposalOpts) (ProposalRef, error) {
	if err := AssertCapability(p.caps, "HasPullRequests", p.Name()); err != nil {
		return ProposalRef{}, err
	}
	_ = ctx
	_ = ws
	_ = opts
	return ProposalRef{}, fmt.Errorf("GitHubProvider.OpenProposal: %w", errNotImplemented)
}

// MergeProposal — stub: not yet ported. Gate: HasPullRequests.
func (p *GitHubProvider) MergeProposal(ctx context.Context, ref ProposalRef, strategy string) (MergeResult, error) {
	if err := AssertCapability(p.caps, "HasPullRequests", p.Name()); err != nil {
		return MergeResult{}, err
	}
	_ = ctx
	_ = ref
	_ = strategy
	return MergeResult{}, fmt.Errorf("GitHubProvider.MergeProposal: %w", errNotImplemented)
}

// EnqueueForMerge — stub: not yet ported. Gate: HasMergeQueue.
func (p *GitHubProvider) EnqueueForMerge(ctx context.Context, ref ProposalRef, opts QueueOpts) (QueueTicket, error) {
	if err := AssertCapability(p.caps, "HasMergeQueue", p.Name()); err != nil {
		return QueueTicket{}, err
	}
	_ = ctx
	_ = ref
	_ = opts
	return QueueTicket{}, fmt.Errorf("GitHubProvider.EnqueueForMerge: %w", errNotImplemented)
}

// Attest — stub: not yet ported. Gate: SupportsAttest.
func (p *GitHubProvider) Attest(ctx context.Context, ws Workspace, meta SessionAttestation) (AttestationRef, error) {
	if err := AssertCapability(p.caps, "SupportsAttest", p.Name()); err != nil {
		return AttestationRef{}, err
	}
	_ = ctx
	_ = ws
	_ = meta
	return AttestationRef{}, fmt.Errorf("GitHubProvider.Attest: %w", errNotImplemented)
}
