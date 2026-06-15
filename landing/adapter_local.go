package landing

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ghCLITimeout bounds gh CLI calls used for proposal eligibility / state checks.
const ghCLITimeout = 15 * time.Second

// defaultPriority is the priority assigned to proposals enqueued via the local
// adapter when an orchestrator does not override it (lower = higher precedence).
const defaultPriority = 3

// LocalAdapter is the self-hosted landing-serializer adapter: it uses the
// built-in Worker + Redis Storage instead of an external provider-native merge
// queue. Proposal eligibility / state is probed via the `gh` CLI (no GraphQL).
//
// It is the default adapter — it works with any GitHub repository without the
// provider's paid merge-queue feature. The adapter handles queue management only
// (enqueue/dequeue/status); conflict resolution lives in the Worker pipeline.
//
// FD-4: the legacy adapter keyed Redis on repoId alone. This port carries an
// OrgID so the (orgId, repoId) composite isolates tenants that share or fork the
// same owner/repo.
//
// Ported from LocalMergeQueueAdapter in
// donmai-libraries merge-queue/adapters/local.ts.
type LocalAdapter struct {
	// orgID scopes every storage key to a tenant (FD-4).
	orgID   string
	storage Storage
	// runner runs the gh CLI; tests inject a fake.
	runner commandRunner
	// now stamps EnqueuedAt; tests stub it.
	now func() time.Time
}

// compile-time assertion that LocalAdapter satisfies Adapter.
var _ Adapter = (*LocalAdapter)(nil)

// NewLocalAdapter returns a LocalAdapter scoped to orgID and backed by storage.
func NewLocalAdapter(orgID string, storage Storage) *LocalAdapter {
	return &LocalAdapter{orgID: orgID, storage: storage, runner: defaultRunner, now: time.Now}
}

func (a *LocalAdapter) run() commandRunner {
	if a.runner == nil {
		return defaultRunner
	}
	return a.runner
}

func (a *LocalAdapter) clock() time.Time {
	if a.now == nil {
		return time.Now()
	}
	return a.now()
}

// key builds the (orgId, repoId) key for an owner/repo, with repoId = owner/repo.
func (a *LocalAdapter) key(owner, repo string) Key {
	return Key{OrgID: a.orgID, RepoID: owner + "/" + repo}
}

// Name returns the provider name identifier.
func (a *LocalAdapter) Name() string { return "local" }

// ghProposal is the subset of `gh pr view` JSON the adapter consumes.
type ghProposal struct {
	State       string `json:"state"`
	HeadRefName string `json:"headRefName"`
	URL         string `json:"url"`
	Title       string `json:"title"`
}

// viewProposal runs `gh pr view` with the given JSON fields and decodes the
// result. The context deadline bounds the call.
func (a *LocalAdapter) viewProposal(ctx context.Context, owner, repo string, proposal int, fields string) (ghProposal, error) {
	runCtx, cancel := context.WithTimeout(ctx, ghCLITimeout)
	defer cancel()
	out, err := a.run().run(runCtx, "", nil,
		"gh", "pr", "view", fmt.Sprintf("%d", proposal),
		"--repo", owner+"/"+repo, "--json", fields)
	if err != nil {
		return ghProposal{}, fmt.Errorf("gh pr view %d: %w", proposal, err)
	}
	var pr ghProposal
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return ghProposal{}, fmt.Errorf("gh pr view %d decode: %w", proposal, err)
	}
	return pr, nil
}

// CanEnqueue reports whether a proposal is eligible for queue entry: it must be
// OPEN. A conflict-free state is NOT required — the Worker handles rebasing. A gh
// CLI failure yields (false, nil), matching the TS source's swallow-and-deny.
func (a *LocalAdapter) CanEnqueue(ctx context.Context, owner, repo string, proposal int) (bool, error) {
	pr, err := a.viewProposal(ctx, owner, repo, proposal, "state,headRefName")
	if err != nil {
		return false, nil
	}
	return strings.EqualFold(pr.State, "OPEN"), nil
}

// Enqueue adds a proposal to the queue and returns its status. Already-queued
// proposals are a no-op (the existing entry is left untouched). Proposal details
// are resolved via the gh CLI, falling back to defaults when unavailable.
func (a *LocalAdapter) Enqueue(ctx context.Context, owner, repo string, proposal int) (Status, error) {
	key := a.key(owner, repo)
	if err := validateKey(key); err != nil {
		return Status{}, err
	}

	already, err := a.storage.IsEnqueued(ctx, key, proposal)
	if err != nil {
		return Status{}, fmt.Errorf("LocalAdapter.Enqueue is-enqueued: %w", err)
	}
	if already {
		return a.GetStatus(ctx, owner, repo, proposal)
	}

	// Resolve proposal details for the queue entry; fall back to defaults.
	sourceBranch := fmt.Sprintf("pr-%d", proposal)
	proposalURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, proposal)
	var title string
	if pr, verr := a.viewProposal(ctx, owner, repo, proposal, "headRefName,url,title"); verr == nil {
		if pr.HeadRefName != "" {
			sourceBranch = pr.HeadRefName
		}
		if pr.URL != "" {
			proposalURL = pr.URL
		}
		title = pr.Title
	}

	// Resolve the originating issue id so the Worker can bubble results back.
	// Convention: agents prefix branches / titles with the issue id. Fall back
	// to PR-N when neither yields a parseable identifier.
	issueID := ExtractIssueID(sourceBranch)
	if issueID == "" {
		issueID = ExtractIssueID(title)
	}
	if issueID == "" {
		issueID = fmt.Sprintf("PR-%d", proposal)
	}

	entry := Entry{
		OrgID:        a.orgID,
		RepoID:       key.RepoID,
		Proposal:     proposal,
		ProposalURL:  proposalURL,
		IssueID:      issueID,
		Priority:     defaultPriority,
		SourceBranch: sourceBranch,
		TargetBranch: "main",
		EnqueuedAt:   a.clock(),
	}
	if err := a.storage.Enqueue(ctx, entry); err != nil {
		return Status{}, fmt.Errorf("LocalAdapter.Enqueue: %w", err)
	}
	return a.GetStatus(ctx, owner, repo, proposal)
}

// GetStatus returns the current queue status for a proposal, consulting (in
// order) queue position, failed reason, blocked reason, then the provider-side
// merged state.
func (a *LocalAdapter) GetStatus(ctx context.Context, owner, repo string, proposal int) (Status, error) {
	key := a.key(owner, repo)
	if err := validateKey(key); err != nil {
		return Status{}, err
	}

	pos, err := a.storage.Position(ctx, key, proposal)
	if err != nil {
		return Status{}, fmt.Errorf("LocalAdapter.GetStatus position: %w", err)
	}
	if pos > 0 {
		state := StateQueued
		if pos == 1 {
			state = StateMerging
		}
		return Status{State: state, Position: pos}, nil
	}

	failed, err := a.storage.FailedReason(ctx, key, proposal)
	if err != nil {
		return Status{}, fmt.Errorf("LocalAdapter.GetStatus failed-reason: %w", err)
	}
	if failed != "" {
		return Status{State: StateFailed, FailureReason: failed}, nil
	}

	blocked, err := a.storage.BlockedReason(ctx, key, proposal)
	if err != nil {
		return Status{}, fmt.Errorf("LocalAdapter.GetStatus blocked-reason: %w", err)
	}
	if blocked != "" {
		return Status{State: StateBlocked, FailureReason: blocked}, nil
	}

	// Provider-side merged check (best-effort).
	if pr, verr := a.viewProposal(ctx, owner, repo, proposal, "state"); verr == nil {
		if strings.EqualFold(pr.State, "MERGED") {
			return Status{State: StateMerged}, nil
		}
	}

	return Status{State: StateNotQueued}, nil
}

// Dequeue removes a proposal from the queue.
func (a *LocalAdapter) Dequeue(ctx context.Context, owner, repo string, proposal int) error {
	key := a.key(owner, repo)
	if err := validateKey(key); err != nil {
		return err
	}
	if err := a.storage.Remove(ctx, key, proposal); err != nil {
		return fmt.Errorf("LocalAdapter.Dequeue: %w", err)
	}
	return nil
}

// IsEnabled reports whether the serializer is enabled for a repository. The
// local adapter is always available (no external service dependency).
func (a *LocalAdapter) IsEnabled(ctx context.Context, owner, repo string) (bool, error) {
	_ = ctx
	_ = owner
	_ = repo
	return true, nil
}
