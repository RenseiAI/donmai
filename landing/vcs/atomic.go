package vcs

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

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

	HasPullRequests:   false,
	HasReviewWorkflow: false,
	HasMergeQueue:     false,

	SupportsBranches: true,
	SupportsRebase:   false,

	ProvenanceNative: true,
	SupportsAttest:   true,

	SupportsBinary:            true,
	SupportsStructuredContent: false,
	SupportsLargeFiles:        false,
}

// AtomicProvider implements Provider for the Atomic commutative VCS. Because
// pushes commute, the landing serializer skips the queue and lands proposals
// directly. Atomic has no PR/proposal concept today, so OpenProposal /
// MergeProposal return *UnsupportedOperationError (HasPullRequests = false).
//
// Ported from AtomicVCSProvider in donmai-libraries vcs/atomic.ts.
type AtomicProvider struct {
	caps   Capabilities
	runner commandRunner
	now    func() time.Time
}

// compile-time assertion.
var _ Provider = (*AtomicProvider)(nil)

// NewAtomicProvider returns an Atomic provider with default capabilities.
func NewAtomicProvider() *AtomicProvider {
	return &AtomicProvider{
		caps:   AtomicCapabilities,
		runner: defaultRunner,
		now:    time.Now,
	}
}

// Name returns the provider id.
func (p *AtomicProvider) Name() string { return "atomic" }

// Capabilities returns the provider's capabilities.
func (p *AtomicProvider) Capabilities() Capabilities { return p.caps }

func (p *AtomicProvider) nowISO() string {
	return p.now().UTC().Format(time.RFC3339)
}

// Clone clones an Atomic repository to dst. Atomic refs are patch-set hashes;
// resolveHead retrieves the current HEAD patch-set id after clone.
func (p *AtomicProvider) Clone(ctx context.Context, uri, dst string, opts CloneOpts) (Workspace, error) {
	args := []string{"clone"}
	if opts.Ref != "" {
		args = append(args, "--branch", opts.Ref)
	}
	args = append(args, "--", uri, dst)

	if _, err := p.runner.run(ctx, "", nil, "atomic", args...); err != nil {
		return Workspace{}, fmt.Errorf("AtomicProvider.Clone: %w", err)
	}

	headRef, err := p.resolveHead(ctx, dst)
	if err != nil {
		return Workspace{}, fmt.Errorf("AtomicProvider.Clone: resolve HEAD: %w", err)
	}

	return Workspace{
		ID:         "atomic:" + uri,
		ProviderID: p.Name(),
		Path:       dst,
		HeadRef:    headRef,
	}, nil
}

// RecordChange stages paths (atomic add) and records a change (atomic record).
// When the attestation carries a signing identity it is passed natively via
// --author rather than as a trailer.
func (p *AtomicProvider) RecordChange(ctx context.Context, ws Workspace, change ChangeRequest) (ChangeRef, error) {
	if len(change.Paths) > 0 {
		addArgs := append([]string{"add", "--"}, change.Paths...)
		if _, err := p.runner.run(ctx, ws.Path, nil, "atomic", addArgs...); err != nil {
			return ChangeRef{}, fmt.Errorf("AtomicProvider.RecordChange: stage: %w", err)
		}
	}

	recordArgs := []string{"record", "-m", change.Message}
	if change.Attestation != nil && change.Attestation.SignedBy != "" {
		recordArgs = append(recordArgs, "--author", change.Attestation.SignedBy)
	}
	if _, err := p.runner.run(ctx, ws.Path, nil, "atomic", recordArgs...); err != nil {
		return ChangeRef{}, fmt.Errorf("AtomicProvider.RecordChange: record: %w", err)
	}

	newRef, err := p.resolveHead(ctx, ws.Path)
	if err != nil {
		return ChangeRef{}, fmt.Errorf("AtomicProvider.RecordChange: resolve HEAD: %w", err)
	}

	return ChangeRef{
		Ref:        newRef,
		RecordedAt: p.nowISO(),
		Summary:    change.Message,
	}, nil
}

// Push pushes local changes. Atomic pushes are commutative — they normally
// succeed against an unstable trunk (no non-fast-forward rejection). Errors are
// classified into a PushRejected reason.
func (p *AtomicProvider) Push(ctx context.Context, ws Workspace, target PushTarget) (PushResult, error) {
	if _, err := p.runner.run(ctx, ws.Path, nil, "atomic", "push", target.Remote, "--branch", target.Ref); err != nil {
		return PushResult{
			Kind:    PushRejected,
			Reason:  classifyAtomicPushError(err.Error()),
			Details: err.Error(),
		}, nil
	}

	headRef, err := p.resolveHead(ctx, ws.Path)
	if err != nil {
		return PushResult{}, fmt.Errorf("AtomicProvider.Push: resolve HEAD: %w", err)
	}
	return PushResult{
		Kind: PushPushed,
		Ref: ChangeRef{
			Ref:        headRef,
			RecordedAt: p.nowISO(),
		},
	}, nil
}

// Pull pulls remote changes. Patch-theory auto-resolves overlapping token-range
// edits; the result is MergeClean, MergeAutoResolved, or MergeConflict.
// Auto-resolutions MUST be surfaced — they are audit-chain evidence.
func (p *AtomicProvider) Pull(ctx context.Context, ws Workspace, source PullSource) (MergeResult, error) {
	out, err := p.runner.run(ctx, ws.Path, nil, "atomic", "pull", source.Remote, "--branch", source.Ref)
	if err != nil {
		msg := err.Error()
		// Unresolvable structural conflict (rare with patch-theory).
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "conflict") || strings.Contains(lower, "unresolvable") {
			return MergeResult{Kind: MergeConflict, Conflicts: parseAtomicConflicts(msg)}, nil
		}
		return MergeResult{}, fmt.Errorf("AtomicProvider.Pull: %w", err)
	}
	return parseAtomicPullOutput(out), nil
}

// OpenProposal is unsupported by Atomic today (no PR concept). Gate:
// HasPullRequests — returns *UnsupportedOperationError. Early adopters coordinate
// review via Slack / Linear until Atomic ships a proposal concept.
func (p *AtomicProvider) OpenProposal(ctx context.Context, ws Workspace, opts ProposalOpts) (ProposalRef, error) {
	_ = ctx
	_ = ws
	_ = opts
	return ProposalRef{}, AssertCapability(p.caps, "HasPullRequests", p.Name())
}

// MergeProposal is unsupported by Atomic today. Gate: HasPullRequests — returns
// *UnsupportedOperationError.
func (p *AtomicProvider) MergeProposal(ctx context.Context, ref ProposalRef, strategy string) (MergeResult, error) {
	_ = ctx
	_ = ref
	_ = strategy
	return MergeResult{}, AssertCapability(p.caps, "HasPullRequests", p.Name())
}

// EnqueueForMerge always returns an *UnsupportedOperationError: Atomic is
// commutative (HasMergeQueue = false), so there is no queue to enqueue into.
func (p *AtomicProvider) EnqueueForMerge(ctx context.Context, ref ProposalRef, opts QueueOpts) (QueueTicket, error) {
	_ = ctx
	_ = ref
	_ = opts
	return QueueTicket{}, AssertCapability(p.caps, "HasMergeQueue", p.Name())
}

// Attest records an attestation-only commit. For Atomic, attestation is
// first-class (ProvenanceNative = true): `atomic record --signed` embeds the
// Ed25519 identity + session metadata natively, NOT via trailers.
func (p *AtomicProvider) Attest(ctx context.Context, ws Workspace, meta SessionAttestation) (AttestationRef, error) {
	if err := AssertCapability(p.caps, "SupportsAttest", p.Name()); err != nil {
		return AttestationRef{}, err
	}

	message := buildAtomicAttestationMessage(meta)
	recordArgs := []string{"record", "--signed", "--allow-empty", "-m", message}
	if meta.SignedBy != "" {
		recordArgs = append(recordArgs, "--author", meta.SignedBy)
	}
	if _, err := p.runner.run(ctx, ws.Path, nil, "atomic", recordArgs...); err != nil {
		return AttestationRef{}, fmt.Errorf("AtomicProvider.Attest: %w", err)
	}

	attestedRef, err := p.resolveHead(ctx, ws.Path)
	if err != nil {
		return AttestationRef{}, fmt.Errorf("AtomicProvider.Attest: resolve HEAD: %w", err)
	}

	return AttestationRef{
		ID:          attestedRef,
		StorageKind: "native",
		AttestedAt:  p.nowISO(),
	}, nil
}

// resolveHead returns the current HEAD patch-set id (atomic log --limit=1
// --format=%H).
func (p *AtomicProvider) resolveHead(ctx context.Context, dir string) (string, error) {
	out, err := p.runner.run(ctx, dir, nil, "atomic", "log", "--limit=1", "--format=%H")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ── parsing helpers ─────────────────────────────────────────────────────────

var (
	atomicAutoResolveCountRe = regexp.MustCompile(`(?i)auto-resolved\s+(\d+)\s+patch`)
	atomicAutoResolveFileRe  = regexp.MustCompile(`(?i)auto-resolved\s+(\S+)\s+\(([^)]+)\)`)
	atomicConflictRe         = regexp.MustCompile(`(?i)conflict in\s+([^\s:]+)(?::\s*(.+))?`)
)

// parseAtomicPullOutput classifies `atomic pull` stdout into a MergeResult.
//
// Ported from parseAtomicPullOutput in donmai-libraries vcs/atomic.ts.
func parseAtomicPullOutput(stdout string) MergeResult {
	lower := strings.ToLower(stdout)

	if atomicAutoResolveCountRe.MatchString(stdout) {
		return MergeResult{Kind: MergeAutoResolved, Resolutions: parseAutoResolutions(stdout)}
	}

	if strings.Contains(lower, "conflict in") || strings.Contains(lower, "unresolvable") {
		return MergeResult{Kind: MergeConflict, Conflicts: parseAtomicConflicts(stdout)}
	}

	return MergeResult{Kind: MergeClean}
}

// parseAutoResolutions extracts per-file auto-resolution details. When the count
// marker is present but no per-file entries match, it synthesizes a summary
// entry so callers always have something to log (the audit chain needs evidence).
//
// Ported from parseAutoResolutions in donmai-libraries vcs/atomic.ts.
func parseAutoResolutions(output string) []AutoResolution {
	var resolutions []AutoResolution
	for _, m := range atomicAutoResolveFileRe.FindAllStringSubmatch(output, -1) {
		strategy := m[2]
		if strategy == "" {
			strategy = "patch-theory"
		}
		resolutions = append(resolutions, AutoResolution{FilePath: m[1], Strategy: strategy})
	}

	if len(resolutions) == 0 && atomicAutoResolveCountRe.MatchString(output) {
		resolutions = append(resolutions, AutoResolution{
			FilePath: "(multiple files — see atomic log)",
			Strategy: "patch-theory",
		})
	}

	return resolutions
}

// parseAtomicConflicts extracts conflict entries from `atomic pull` error output.
// Falls back to a single unknown-file conflict so callers never get an empty
// MergeConflict.
//
// Ported from parseAtomicConflicts in donmai-libraries vcs/atomic.ts.
func parseAtomicConflicts(output string) []Conflict {
	var conflicts []Conflict
	for _, m := range atomicConflictRe.FindAllStringSubmatch(output, -1) {
		conflicts = append(conflicts, Conflict{FilePath: m[1], Detail: m[2]})
	}

	if len(conflicts) == 0 {
		detail := output
		if len(detail) > 200 {
			detail = detail[:200]
		}
		conflicts = append(conflicts, Conflict{
			FilePath: "(unknown — check atomic log)",
			Detail:   detail,
		})
	}

	return conflicts
}

// classifyAtomicPushError classifies an `atomic push` error into a PushRejected
// reason. Patch-theory makes non-fast-forward structurally impossible, so it is
// only reported when explicitly indicated; otherwise auth, else policy.
//
// Ported from classifyAtomicPushError in donmai-libraries vcs/atomic.ts.
func classifyAtomicPushError(message string) string {
	lower := strings.ToLower(message)
	if strings.Contains(lower, "authentication") ||
		strings.Contains(lower, "permission denied") ||
		strings.Contains(lower, "auth") {
		return "auth"
	}
	if strings.Contains(lower, "non-fast-forward") {
		return "non-fast-forward"
	}
	return "policy"
}

// buildAtomicAttestationMessage builds the attestation message for `atomic record
// --signed`. Metadata is stored natively in the Atomic patch object, NOT as git
// trailers.
//
// Ported from buildAtomicAttestationMessage in donmai-libraries vcs/atomic.ts
// (the legacy private-brand metadata keys are emitted as Donmai-* for OSS).
func buildAtomicAttestationMessage(attestation SessionAttestation) string {
	lines := []string{
		fmt.Sprintf("attestation: session %s", attestation.SessionID),
		"",
		fmt.Sprintf("Donmai-Agent-Id: %s", attestation.AgentID),
		fmt.Sprintf("Donmai-Session-Id: %s", attestation.SessionID),
		fmt.Sprintf("Donmai-Model: %s/%s", attestation.Model.Provider, attestation.Model.Model),
		fmt.Sprintf("Donmai-Started-At: %s", attestation.StartedAt),
	}

	if len(attestation.KitIDs) > 0 {
		kits := make([]string, len(attestation.KitIDs))
		for i, k := range attestation.KitIDs {
			kits[i] = k.ID + "@" + k.Version
		}
		lines = append(lines, "Donmai-Kit-Set: "+strings.Join(kits, ","))
	}

	if attestation.WorkareaSnapshotRef != nil {
		lines = append(lines, "Donmai-Workarea-Snapshot: "+attestation.WorkareaSnapshotRef.Ref)
	}

	if attestation.SignedBy != "" {
		lines = append(lines, "Donmai-Signed-By: "+attestation.SignedBy)
	}

	if len(attestation.ReviewerHints) > 0 {
		lines = append(lines, "Donmai-Reviewer-Hints: "+strings.Join(attestation.ReviewerHints, ","))
	}

	return strings.Join(lines, "\n")
}
