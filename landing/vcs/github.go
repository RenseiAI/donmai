package vcs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
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

// ErrGitHubUnavailable is returned by GitHubProvider verbs when the provider was
// constructed without being advertised as available (the default). It is the
// landing-serializer analogue of the daemon's substrate-capability gate: a
// skew-safe, default-off switch so a GitHub provider compiled into a binary that
// has not declared GitHub support never silently shells out to git/gh.
//
// Pattern mirror: the daemon advertises substrate capabilities (provides[]) to
// the platform at registration time and is default-off for kinds it has not
// detected. Here the consumer opts the provider in via GitHubOpts.Available
// (e.g. from a daemon-advertised capability) before any verb runs.
var ErrGitHubUnavailable = errors.New(
	"github provider is not advertised as available; construct with GitHubOpts.Available=true " +
		"(default-off, skew-safe) before invoking verbs",
)

// GitHubProvider implements Provider using the git CLI for content operations
// and the gh CLI for proposal operations. Attestation is faked via commit
// trailers (X-Donmai-*).
//
// Ported from GitHubVCSProvider in donmai-libraries vcs/github.ts.
type GitHubProvider struct {
	caps Capabilities
	// available gates every verb. Default-off, skew-safe (donmai #151 pattern):
	// a provider that was not explicitly advertised refuses to run.
	available bool
	// runner is the CLI seam; tests inject a fake.
	runner commandRunner
	// now supplies recorded-at / attested-at timestamps; tests stub it.
	now func() time.Time
}

// compile-time assertion.
var _ Provider = (*GitHubProvider)(nil)

// GitHubOpts configures NewGitHubProvider.
type GitHubOpts struct {
	// Available opts the provider in. It is FALSE by default so a GitHub provider
	// linked into a binary that has not advertised GitHub support is inert
	// (default-off, skew-safe). Set true from the daemon-advertised capability.
	Available bool
	// HasMergeQueueOverride, when non-nil, overrides HasMergeQueue (set false for
	// repos without a provider-native merge queue enabled).
	HasMergeQueueOverride *bool
}

// NewGitHubProvider returns a GitHub provider with default capabilities, applying
// any override. The returned provider is gated by opts.Available: verbs return
// ErrGitHubUnavailable until it is advertised.
func NewGitHubProvider(opts GitHubOpts) *GitHubProvider {
	caps := GitHubCapabilities
	if opts.HasMergeQueueOverride != nil {
		caps.HasMergeQueue = *opts.HasMergeQueueOverride
	}
	return &GitHubProvider{
		caps:      caps,
		available: opts.Available,
		runner:    defaultRunner,
		now:       time.Now,
	}
}

// Name returns the provider id.
func (p *GitHubProvider) Name() string { return "github" }

// Capabilities returns the provider's capabilities.
func (p *GitHubProvider) Capabilities() Capabilities { return p.caps }

// gate returns ErrGitHubUnavailable when the provider has not been advertised.
// Every verb calls it first.
func (p *GitHubProvider) gate(op string) error {
	if !p.available {
		return fmt.Errorf("GitHubProvider.%s: %w", op, ErrGitHubUnavailable)
	}
	return nil
}

func (p *GitHubProvider) nowISO() string {
	return p.now().UTC().Format(time.RFC3339)
}

// Clone clones a repository to dst. Supports Depth, Ref, and SingleBranch.
func (p *GitHubProvider) Clone(ctx context.Context, uri, dst string, opts CloneOpts) (Workspace, error) {
	if err := p.gate("Clone"); err != nil {
		return Workspace{}, err
	}

	args := []string{"clone"}
	if opts.Depth > 0 {
		args = append(args, fmt.Sprintf("--depth=%d", opts.Depth))
	}
	if opts.SingleBranch {
		args = append(args, "--single-branch")
	}
	if opts.Ref != "" {
		args = append(args, "--branch="+opts.Ref)
	}
	args = append(args, "--", uri, dst)

	if _, err := p.runner.run(ctx, "", nil, "git", args...); err != nil {
		return Workspace{}, fmt.Errorf("GitHubProvider.Clone: %w", err)
	}

	headRef, err := p.runner.run(ctx, dst, nil, "git", "rev-parse", "HEAD")
	if err != nil {
		return Workspace{}, fmt.Errorf("GitHubProvider.Clone: resolve HEAD: %w", err)
	}

	return Workspace{
		ID:         "github:" + uri,
		ProviderID: p.Name(),
		Path:       dst,
		HeadRef:    strings.TrimSpace(headRef),
	}, nil
}

// RecordChange stages the given paths and creates a commit. When an attestation
// is included it appends commit trailers so the provenance metadata lands in git
// history.
func (p *GitHubProvider) RecordChange(ctx context.Context, ws Workspace, change ChangeRequest) (ChangeRef, error) {
	if err := p.gate("RecordChange"); err != nil {
		return ChangeRef{}, err
	}

	if len(change.Paths) > 0 {
		addArgs := append([]string{"add", "--"}, change.Paths...)
		if _, err := p.runner.run(ctx, ws.Path, nil, "git", addArgs...); err != nil {
			return ChangeRef{}, fmt.Errorf("GitHubProvider.RecordChange: stage: %w", err)
		}
	}

	message := change.Message
	if change.Attestation != nil {
		message = BuildCommitMessageWithTrailers(change.Message, *change.Attestation)
	}

	// Pass the message via -m. Multiple -m segments are joined with blank lines
	// by git, so split on the trailer separator to preserve the body/trailer
	// boundary without a temp file (the temp-file dance in the TS source exists
	// only to dodge shell escaping, which the arg-vector runner makes moot).
	commitArgs := []string{"commit"}
	for _, seg := range splitMessageSegments(message) {
		commitArgs = append(commitArgs, "-m", seg)
	}
	if _, err := p.runner.run(ctx, ws.Path, nil, "git", commitArgs...); err != nil {
		return ChangeRef{}, fmt.Errorf("GitHubProvider.RecordChange: commit: %w", err)
	}

	newSHA, err := p.runner.run(ctx, ws.Path, nil, "git", "rev-parse", "HEAD")
	if err != nil {
		return ChangeRef{}, fmt.Errorf("GitHubProvider.RecordChange: resolve HEAD: %w", err)
	}
	summary, err := p.runner.run(ctx, ws.Path, nil, "git", "log", "-1", "--pretty=format:%s")
	if err != nil {
		return ChangeRef{}, fmt.Errorf("GitHubProvider.RecordChange: read summary: %w", err)
	}

	return ChangeRef{
		Ref:        strings.TrimSpace(newSHA),
		RecordedAt: p.nowISO(),
		Summary:    strings.TrimSpace(summary),
	}, nil
}

// Push pushes the local HEAD to the remote. Returns PushPushed on success or
// PushRejected (with a classified Reason) on non-fast-forward, auth, or policy
// failure.
func (p *GitHubProvider) Push(ctx context.Context, ws Workspace, target PushTarget) (PushResult, error) {
	if err := p.gate("Push"); err != nil {
		return PushResult{}, err
	}

	headSHA, err := p.runner.run(ctx, ws.Path, nil, "git", "rev-parse", "HEAD")
	if err != nil {
		return PushResult{}, fmt.Errorf("GitHubProvider.Push: resolve HEAD: %w", err)
	}

	_, pushErr := p.runner.run(ctx, ws.Path, nil, "git", "push", target.Remote, "HEAD:"+target.Ref)
	if pushErr != nil {
		return PushResult{
			Kind:    PushRejected,
			Reason:  classifyPushError(pushErr.Error()),
			Details: pushErr.Error(),
		}, nil
	}

	return PushResult{
		Kind: PushPushed,
		Ref: ChangeRef{
			Ref:        strings.TrimSpace(headSHA),
			RecordedAt: p.nowISO(),
		},
	}, nil
}

// Pull pulls a remote ref into the working copy. git does not auto-resolve — any
// conflict is surfaced as MergeConflict; unexpected errors are returned.
func (p *GitHubProvider) Pull(ctx context.Context, ws Workspace, source PullSource) (MergeResult, error) {
	if err := p.gate("Pull"); err != nil {
		return MergeResult{}, err
	}

	if _, err := p.runner.run(ctx, ws.Path, nil, "git", "pull", source.Remote, source.Ref); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "CONFLICT") || strings.Contains(msg, "Automatic merge failed") {
			conflicts := p.extractConflictFiles(ctx, ws.Path)
			return MergeResult{Kind: MergeConflict, Conflicts: conflicts}, nil
		}
		return MergeResult{}, fmt.Errorf("GitHubProvider.Pull: %w", err)
	}
	return MergeResult{Kind: MergeClean}, nil
}

// OpenProposal opens a GitHub Pull Request via the gh CLI. Gate: HasPullRequests.
func (p *GitHubProvider) OpenProposal(ctx context.Context, ws Workspace, opts ProposalOpts) (ProposalRef, error) {
	if err := p.gate("OpenProposal"); err != nil {
		return ProposalRef{}, err
	}
	if err := AssertCapability(p.caps, "HasPullRequests", p.Name()); err != nil {
		return ProposalRef{}, err
	}

	args := []string{
		"pr", "create",
		"--base", opts.BaseRef,
		"--title", opts.Title,
		"--body", opts.Body,
	}
	if len(opts.Reviewers) > 0 {
		args = append(args, "--reviewer", strings.Join(opts.Reviewers, ","))
	}
	if len(opts.Labels) > 0 {
		args = append(args, "--label", strings.Join(opts.Labels, ","))
	}

	out, err := p.runner.run(ctx, ws.Path, nil, "gh", args...)
	if err != nil {
		return ProposalRef{}, fmt.Errorf("GitHubProvider.OpenProposal: %w", err)
	}

	url := strings.TrimSpace(out)
	prNumber, err := parsePRNumberFromURL(url)
	if err != nil {
		return ProposalRef{}, fmt.Errorf("GitHubProvider.OpenProposal: %w", err)
	}

	return ProposalRef{
		ID:    prNumber,
		URL:   url,
		State: "open",
	}, nil
}

// MergeProposal merges a GitHub Pull Request. Gate: HasPullRequests.
func (p *GitHubProvider) MergeProposal(ctx context.Context, ref ProposalRef, strategy string) (MergeResult, error) {
	if err := p.gate("MergeProposal"); err != nil {
		return MergeResult{}, err
	}
	if err := AssertCapability(p.caps, "HasPullRequests", p.Name()); err != nil {
		return MergeResult{}, err
	}

	args := []string{"pr", "merge", ref.ID, githubMergeFlag(strategy), "--delete-branch"}
	if _, err := p.runner.run(ctx, "", nil, "gh", args...); err != nil {
		msg := err.Error()
		if strings.Contains(strings.ToLower(msg), "conflict") {
			return MergeResult{
				Kind:      MergeConflict,
				Conflicts: []Conflict{{FilePath: "(unknown — check PR diff)", Detail: msg}},
			}, nil
		}
		return MergeResult{}, fmt.Errorf("GitHubProvider.MergeProposal: %w", err)
	}
	return MergeResult{Kind: MergeClean}, nil
}

// EnqueueForMerge enqueues a PR for GitHub's native merge queue via the GraphQL
// enqueuePullRequest mutation. Gate: HasMergeQueue.
func (p *GitHubProvider) EnqueueForMerge(ctx context.Context, ref ProposalRef, opts QueueOpts) (QueueTicket, error) {
	if err := p.gate("EnqueueForMerge"); err != nil {
		return QueueTicket{}, err
	}
	if err := AssertCapability(p.caps, "HasMergeQueue", p.Name()); err != nil {
		return QueueTicket{}, err
	}
	_ = opts

	prNodeID, err := p.getPRNodeID(ctx, ref.ID)
	if err != nil {
		return QueueTicket{}, fmt.Errorf("GitHubProvider.EnqueueForMerge: %w", err)
	}

	const mutation = `mutation($prId: ID!) { enqueuePullRequest(input: { pullRequestId: $prId }) { mergeQueueEntry { state position enqueuedAt } } }`
	out, err := p.runner.run(ctx, "", nil, "gh", "api", "graphql", "-f", "query="+mutation, "-F", "prId="+prNodeID)
	if err != nil {
		return QueueTicket{}, fmt.Errorf("GitHubProvider.EnqueueForMerge: %w", err)
	}

	entry, err := parseEnqueueResponse(out)
	if err != nil {
		return QueueTicket{}, fmt.Errorf("GitHubProvider.EnqueueForMerge: %w", err)
	}

	enqueuedAt := entry.EnqueuedAt
	if enqueuedAt == "" {
		enqueuedAt = p.nowISO()
	}
	return QueueTicket{
		ID:         ref.ID + ":queue",
		Position:   entry.Position,
		EnqueuedAt: enqueuedAt,
	}, nil
}

// Attest creates an empty attestation commit carrying all provenance trailers.
// Gate: SupportsAttest.
func (p *GitHubProvider) Attest(ctx context.Context, ws Workspace, meta SessionAttestation) (AttestationRef, error) {
	if err := p.gate("Attest"); err != nil {
		return AttestationRef{}, err
	}
	if err := AssertCapability(p.caps, "SupportsAttest", p.Name()); err != nil {
		return AttestationRef{}, err
	}

	trailers := BuildAttestationTrailers(meta)
	message := fmt.Sprintf("attestation: session %s\n\n%s", meta.SessionID, trailers)

	commitArgs := []string{"commit", "--allow-empty"}
	for _, seg := range splitMessageSegments(message) {
		commitArgs = append(commitArgs, "-m", seg)
	}
	if _, err := p.runner.run(ctx, ws.Path, nil, "git", commitArgs...); err != nil {
		return AttestationRef{}, fmt.Errorf("GitHubProvider.Attest: commit: %w", err)
	}

	sha, err := p.runner.run(ctx, ws.Path, nil, "git", "rev-parse", "HEAD")
	if err != nil {
		return AttestationRef{}, fmt.Errorf("GitHubProvider.Attest: resolve HEAD: %w", err)
	}

	return AttestationRef{
		ID:          strings.TrimSpace(sha),
		StorageKind: "commit-trailer",
		AttestedAt:  p.nowISO(),
	}, nil
}

// getPRNodeID resolves the GraphQL node id for a PR number.
func (p *GitHubProvider) getPRNodeID(ctx context.Context, prNumber string) (string, error) {
	const query = `query($prNumber: Int!) { repository { pullRequest(number: $prNumber) { id } } }`
	out, err := p.runner.run(ctx, "", nil, "gh", "api", "graphql", "-f", "query="+query, "-F", "prNumber="+prNumber)
	if err != nil {
		return "", err
	}
	return parsePRNodeIDResponse(out)
}

// extractConflictFiles lists files with unmerged (conflicting) status. Best
// effort: returns nil on error rather than masking the conflict result.
func (p *GitHubProvider) extractConflictFiles(ctx context.Context, dir string) []Conflict {
	out, err := p.runner.run(ctx, dir, nil, "git", "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	var conflicts []Conflict
	for _, f := range splitLines(out) {
		conflicts = append(conflicts, Conflict{FilePath: f})
	}
	return conflicts
}

// ── commit-trailer helpers ──────────────────────────────────────────────────

// trailerCoAuthorEmail is the OSS attestation co-author identity. The legacy TS
// source used a private-brand domain; the OSS port uses the donmai domain.
const trailerCoAuthorEmail = "agent@donmai.dev"

// BuildCommitMessageWithTrailers appends provenance trailers to a commit message
// after a blank-line separator (git trailer convention).
//
// Ported from buildCommitMessageWithTrailers in donmai-libraries vcs/github.ts
// (the legacy private-brand X-* trailers are emitted as X-Donmai-* for OSS).
func BuildCommitMessageWithTrailers(message string, attestation SessionAttestation) string {
	trailers := BuildAttestationTrailers(attestation)
	separator := "\n\n"
	if strings.HasSuffix(message, "\n") {
		separator = "\n"
	}
	return message + separator + trailers
}

// BuildAttestationTrailers renders the provenance trailer block.
//
// Ported from buildAttestationTrailers in donmai-libraries vcs/github.ts
// (the legacy private-brand X-* trailer keys are emitted as X-Donmai-* here).
func BuildAttestationTrailers(attestation SessionAttestation) string {
	lines := []string{
		fmt.Sprintf("Co-Authored-By: %s <%s>", attestation.AgentID, trailerCoAuthorEmail),
		fmt.Sprintf("X-Donmai-Session-Id: %s", attestation.SessionID),
		fmt.Sprintf("X-Donmai-Model: %s/%s", attestation.Model.Provider, attestation.Model.Model),
	}

	if len(attestation.KitIDs) > 0 {
		kits := make([]string, len(attestation.KitIDs))
		for i, k := range attestation.KitIDs {
			kits[i] = k.ID + "@" + k.Version
		}
		lines = append(lines, "X-Donmai-Kit-Set: "+strings.Join(kits, ","))
	}

	if attestation.WorkareaSnapshotRef != nil {
		lines = append(lines, "X-Donmai-Workarea-Snapshot: "+attestation.WorkareaSnapshotRef.Ref)
	}

	if attestation.SignedBy != "" {
		lines = append(lines, "X-Donmai-Signed-By: "+attestation.SignedBy)
	}

	return strings.Join(lines, "\n")
}

// ── internal helpers ────────────────────────────────────────────────────────

var prNumberRe = regexp.MustCompile(`/pull/(\d+)`)

// parsePRNumberFromURL extracts the PR number from a GitHub PR URL.
func parsePRNumberFromURL(url string) (string, error) {
	m := prNumberRe.FindStringSubmatch(url)
	if m == nil {
		return "", fmt.Errorf("could not parse PR number from URL: %q", url)
	}
	return m[1], nil
}

// githubMergeFlag maps a merge strategy to the gh merge flag. Unknown / empty /
// "auto" / "three-way-text" map to --merge.
//
// Ported from githubMergeFlag in donmai-libraries vcs/github.ts.
func githubMergeFlag(strategy string) string {
	switch strategy {
	case "rebase":
		return "--rebase"
	case "squash":
		return "--squash"
	default:
		return "--merge"
	}
}

// classifyPushError classifies a git push error string into a PushResult reason.
// Order matters: non-fast-forward is checked before auth because git's rejection
// text can contain both signals.
//
// Ported from classifyPushError in donmai-libraries vcs/github.ts.
func classifyPushError(message string) string {
	lower := strings.ToLower(message)
	if strings.Contains(lower, "non-fast-forward") || strings.Contains(lower, "[rejected]") {
		return "non-fast-forward"
	}
	if strings.Contains(lower, "authentication") ||
		strings.Contains(lower, "permission denied") ||
		strings.Contains(lower, "auth") {
		return "auth"
	}
	return "policy"
}

// splitMessageSegments splits a commit message into subject+body and trailer
// segments on the blank-line boundary so each can be passed as a separate -m.
// git rejoins multiple -m values with a blank line, reproducing the original
// layout without a temp file.
func splitMessageSegments(message string) []string {
	parts := strings.Split(message, "\n\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.Trim(p, "\n"))
	}
	if len(out) == 0 {
		return []string{message}
	}
	return out
}
