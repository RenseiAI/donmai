package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

const providerErrorCheckpointTimeout = 20 * time.Second

// checkpointProviderError preserves a failed provider-error session on a WIP
// branch the platform can retry from. Best-effort only: any failure is logged
// and recorded as a warning on res, but the original provider-error result
// stays authoritative.
func (r *Runner) checkpointProviderError(qw QueuedWork, res *Result) {
	if res == nil || res.Status != "failed" || res.FailureMode != FailureProviderError || res.Resumable || res.ResumeCheckpoint != nil {
		return
	}
	target, ok := providerErrorCheckpointTarget(res)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), providerErrorCheckpointTimeout)
	defer cancel()
	checkpoint, err := createProviderErrorCheckpoint(ctx, qw, target)
	if err != nil {
		warning := fmt.Sprintf("provider-error WIP checkpoint failed: %v", err)
		res.PostSessionWarnings = append(res.PostSessionWarnings, warning)
		r.logger.Warn("provider-error WIP checkpoint failed",
			"sessionId", qw.SessionID,
			"path", target.path,
			"err", err,
		)
		return
	}
	res.Resumable = true
	res.ResumeCheckpoint = checkpoint
	r.logger.Info("provider-error WIP checkpoint pushed",
		"sessionId", qw.SessionID,
		"branch", checkpoint.Branch,
		"commitSha", checkpoint.CommitSHA,
	)
}

// providerErrorCheckpointTarget chooses the repository-shaped checkout the WIP
// branch contract can currently represent. The result wire is singular, so a
// multi-repository workarea checkpoints the selected checkout (WorktreePath)
// when it is mutable; a non-selected mutable leaf keeps the existing local
// rescue path until the result contract grows per-repository checkpointing.
func providerErrorCheckpointTarget(res *Result) (rescueTarget, bool) {
	if res == nil || len(res.rescueTargets) == 0 {
		return rescueTarget{}, false
	}
	if res.WorktreePath != "" {
		clean := filepath.Clean(res.WorktreePath)
		for _, target := range res.rescueTargets {
			if filepath.Clean(target.path) == clean {
				return target, true
			}
		}
	}
	if len(res.rescueTargets) == 1 {
		return res.rescueTargets[0], true
	}
	return rescueTarget{}, false
}

func createProviderErrorCheckpoint(ctx context.Context, qw QueuedWork, target rescueTarget) (*agent.ResumeCheckpoint, error) {
	if _, err := os.Stat(target.path); err != nil {
		return nil, fmt.Errorf("stat checkout: %w", err)
	}
	head, err := captureHeadSHA(ctx, target.path)
	if err != nil {
		return nil, err
	}
	branch := "wip/" + qw.SessionID
	checkpointHead := head

	status, err := gitStdout(ctx, target.path, nil,
		"-c", "core.quotePath=false", "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	changed := meaningfulPaths(porcelainPaths(status))
	if len(changed) > 0 {
		commitSHA, commitErr := checkpointDirtyWorktree(ctx, target.path, head, qw)
		if commitErr != nil {
			return nil, commitErr
		}
		if commitSHA != "" {
			checkpointHead = commitSHA
		}
	}
	// A clean checkout still gets a resumable branch so the platform has an
	// explicit retry target even when the lost work was only in conversation.
	if _, err := gitStdout(ctx, target.path, nil, "update-ref", "refs/heads/"+branch, checkpointHead); err != nil {
		return nil, fmt.Errorf("update local WIP branch: %w", err)
	}
	id := gitIdentityFromSession(qw)
	if out, err := runGit(ctx, target.path, id, "push", "origin", "refs/heads/"+branch+":refs/heads/"+branch); err != nil {
		return nil, fmt.Errorf("push WIP branch %q: %w (output: %s)", branch, err, out)
	}
	return &agent.ResumeCheckpoint{Branch: branch, CommitSHA: checkpointHead}, nil
}

func checkpointDirtyWorktree(ctx context.Context, worktreePath, head string, qw QueuedWork) (string, error) {
	indexDir, err := os.MkdirTemp("", "provider-error-checkpoint-")
	if err != nil {
		return "", fmt.Errorf("create scratch index dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(indexDir) }()
	env := append([]string{
		"GIT_INDEX_FILE=" + filepath.Join(indexDir, "index"),
		"GIT_LITERAL_PATHSPECS=1",
	}, gitIdentityEnv(qw)...)
	if _, err := gitStdout(ctx, worktreePath, env, "read-tree", "HEAD"); err != nil {
		return "", fmt.Errorf("seed scratch index: %w", err)
	}
	if _, err := gitStdout(ctx, worktreePath, env, "add", "-A", "--", "."); err != nil {
		return "", fmt.Errorf("stage working state: %w", err)
	}
	staged, err := gitStdout(ctx, worktreePath, env,
		"-c", "core.quotePath=false", "diff", "--cached", "--name-only", "-z", "HEAD")
	if err != nil {
		return "", fmt.Errorf("list staged working state: %w", err)
	}
	var excluded []string
	for _, path := range strings.Split(staged, "\x00") {
		if path != "" && shouldExcludeFromBackstop(path) {
			excluded = append(excluded, path)
		}
	}
	for i := 0; i < len(excluded); i += rescueResetBatch {
		batch := excluded[i:min(i+rescueResetBatch, len(excluded))]
		if _, err := gitStdout(ctx, worktreePath, env, append([]string{"reset", "-q", "HEAD", "--"}, batch...)...); err != nil {
			return "", fmt.Errorf("drop non-work paths from scratch index: %w", err)
		}
	}
	tree, err := gitStdout(ctx, worktreePath, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write working-state tree: %w", err)
	}
	tree = strings.TrimSpace(tree)
	if tree == "" {
		return "", fmt.Errorf("write working-state tree: empty tree id")
	}
	headTree, err := gitStdout(ctx, worktreePath, nil, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return "", fmt.Errorf("resolve HEAD tree: %w", err)
	}
	if tree == strings.TrimSpace(headTree) {
		return "", nil
	}
	commitSHA, err := gitStdout(ctx, worktreePath, env,
		"commit-tree", tree, "-p", head, "-m", "WIP checkpoint")
	if err != nil {
		return "", fmt.Errorf("commit scratch tree: %w", err)
	}
	commitSHA = strings.TrimSpace(commitSHA)
	if !headSHARE.MatchString(commitSHA) {
		return "", fmt.Errorf("commit scratch tree: unexpected output %q", commitSHA)
	}
	return commitSHA, nil
}

func gitIdentityFromSession(qw QueuedWork) gitIdentity {
	env := buildSessionEnv(qw)
	return gitIdentity{Name: env["GIT_AUTHOR_NAME"], Email: env["GIT_AUTHOR_EMAIL"]}
}

func gitIdentityEnv(qw QueuedWork) []string {
	id := gitIdentityFromSession(qw)
	return id.envOverrides()
}
