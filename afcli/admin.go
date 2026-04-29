package afcli

// af admin — operational admin commands.
//
// Subcommands:
//   af admin cleanup          — prune orphaned git worktrees + stale branches
//   af admin queue            — inspect / mutate the Redis work queue
//   af admin merge-queue      — inspect / mutate the Redis merge queue
//
// Design notes
// ─────────────────────────────────────────────────────────────────────────────
//   • All subcommands output JSON to stdout.
//   • Destructive operations (cleanup, queue drop/requeue, merge-queue dequeue/
//     force-merge) require interactive confirmation unless --yes is passed.
//   • Confirmation uses bufio.Scanner on os.Stdin so there is no additional
//     dependency.
//   • Redis URL is read from REDIS_URL env var (same as the TS runner).
//   • Worktree/branch cleanup shells out to git (same model as TS cleanup-runner).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	adminqueue "github.com/RenseiAI/agentfactory-tui/afclient/queue"
)

// ──────────────────────────────────────────────────────────────────────────────
// Top-level admin command
// ──────────────────────────────────────────────────────────────────────────────

func newAdminCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Operational admin commands (cleanup, queue, merge-queue)",
		Long: `Operational admin commands for AgentFactory.

Subcommands:
  cleanup      Prune orphaned git worktrees and stale local branches
  queue        Inspect and mutate the Redis work queue
  merge-queue  Inspect and mutate the Redis merge queue

Environment:
  REDIS_URL    Redis connection URL (required for queue / merge-queue)`,
		SilenceUsage: true,
	}
	cmd.AddCommand(newAdminCleanupCmd())
	cmd.AddCommand(newAdminQueueCmd())
	cmd.AddCommand(newAdminMergeQueueCmd())
	return cmd
}

// ──────────────────────────────────────────────────────────────────────────────
// Confirmation helper
// ──────────────────────────────────────────────────────────────────────────────

// confirm prompts the user for y/n confirmation on the given message.
// Returns true when the user answers "y" or "yes" (case-insensitive).
// When yes is true the prompt is skipped and true is returned immediately.
func confirm(cmd *cobra.Command, msg string, yes bool) bool {
	if yes {
		return true
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s [y/N]: ", msg)
	sc := bufio.NewScanner(os.Stdin)
	if sc.Scan() {
		answer := strings.TrimSpace(strings.ToLower(sc.Text()))
		return answer == "y" || answer == "yes"
	}
	return false
}

// ──────────────────────────────────────────────────────────────────────────────
// newAdminQueueCmd — redis work queue inspection + mutation
// ──────────────────────────────────────────────────────────────────────────────

func newAdminQueueCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "queue",
		Short:        "Inspect and mutate the Redis work queue",
		SilenceUsage: true,
	}
	cmd.AddCommand(newAdminQueueListCmd())
	cmd.AddCommand(newAdminQueuePeekCmd())
	cmd.AddCommand(newAdminQueueRequeueCmd())
	cmd.AddCommand(newAdminQueueDropCmd())
	return cmd
}

func redisAdminClient() (*adminqueue.AdminClient, error) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		return nil, adminqueue.ErrRedisURLRequired
	}
	return adminqueue.NewAdminClient(url)
}

func newAdminQueueListCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "list",
		Short:        "List queued work items, sessions, and workers",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := redisAdminClient()
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			ctx := cmd.Context()

			items, err := client.ListWorkItems(ctx)
			if err != nil {
				return fmt.Errorf("list work items: %w", err)
			}
			sessions, err := client.ListSessions(ctx)
			if err != nil {
				return fmt.Errorf("list sessions: %w", err)
			}
			workers, err := client.ListWorkers(ctx)
			if err != nil {
				return fmt.Errorf("list workers: %w", err)
			}

			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"items":    items,
				"sessions": sessions,
				"workers":  workers,
			})
		},
	}
}

func newAdminQueuePeekCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "peek",
		Short:        "Show the next item in the work queue without removing it",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := redisAdminClient()
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			item, err := client.PeekWorkItem(cmd.Context())
			if err != nil {
				return fmt.Errorf("peek: %w", err)
			}
			return writeJSON(cmd.OutOrStdout(), item)
		},
	}
}

func newAdminQueueRequeueCmd() *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:          "requeue <session-id>",
		Short:        "Reset a session from running/claimed back to pending",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			partialID := args[0]

			if !confirm(cmd, fmt.Sprintf("Requeue session matching %q (sets status back to pending)?", partialID), yes) {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "aborted")
				return nil
			}

			client, err := redisAdminClient()
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			n, err := client.RequeueSession(cmd.Context(), partialID)
			if err != nil {
				return fmt.Errorf("requeue: %w", err)
			}

			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"requeued": n,
				"selector": partialID,
			})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip confirmation prompt")
	return cmd
}

func newAdminQueueDropCmd() *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:          "drop <session-id>",
		Short:        "Remove a session (and its queue/claim entries) permanently",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			partialID := args[0]

			if !confirm(cmd, fmt.Sprintf("Permanently drop session matching %q?", partialID), yes) {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "aborted")
				return nil
			}

			client, err := redisAdminClient()
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			n, err := client.DropSession(cmd.Context(), partialID)
			if err != nil {
				return fmt.Errorf("drop: %w", err)
			}

			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"dropped":  n,
				"selector": partialID,
			})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip confirmation prompt")
	return cmd
}

// ──────────────────────────────────────────────────────────────────────────────
// newAdminMergeQueueCmd — merge queue inspection + mutation
// ──────────────────────────────────────────────────────────────────────────────

func newAdminMergeQueueCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "merge-queue",
		Short:        "Inspect and mutate the Redis merge queue",
		SilenceUsage: true,
	}
	cmd.AddCommand(newAdminMergeQueueListCmd())
	cmd.AddCommand(newAdminMergeQueueDequeueCmd())
	cmd.AddCommand(newAdminMergeQueueForceMergeCmd())
	return cmd
}

func newAdminMergeQueueListCmd() *cobra.Command {
	var repoID string

	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List all merge-queue entries for a repo",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if repoID == "" {
				repoID = "default"
			}

			client, err := redisAdminClient()
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			snap, err := client.ListMergeQueue(cmd.Context(), repoID)
			if err != nil {
				return fmt.Errorf("list merge-queue: %w", err)
			}
			return writeJSON(cmd.OutOrStdout(), snap)
		},
	}
	cmd.Flags().StringVar(&repoID, "repo", "default", "Repository ID (e.g. my-org/my-repo)")
	return cmd
}

func newAdminMergeQueueDequeueCmd() *cobra.Command {
	var (
		repoID string
		yes    bool
	)

	cmd := &cobra.Command{
		Use:          "dequeue <pr-number>",
		Short:        "Remove a PR from the merge queue permanently",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			prNum, err := parsePRNumber(args[0])
			if err != nil {
				return err
			}
			if repoID == "" {
				repoID = "default"
			}

			if !confirm(cmd, fmt.Sprintf("Permanently dequeue PR #%d from merge queue for repo %q?", prNum, repoID), yes) {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "aborted")
				return nil
			}

			client, err := redisAdminClient()
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			if err := client.DequeueEntry(cmd.Context(), repoID, prNum); err != nil {
				return fmt.Errorf("dequeue: %w", err)
			}

			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"dequeued": true,
				"prNumber": prNum,
				"repoId":   repoID,
			})
		},
	}
	cmd.Flags().StringVar(&repoID, "repo", "default", "Repository ID")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip confirmation prompt")
	return cmd
}

func newAdminMergeQueueForceMergeCmd() *cobra.Command {
	var (
		repoID string
		yes    bool
	)

	cmd := &cobra.Command{
		Use:          "force-merge <pr-number>",
		Short:        "Move a failed/blocked PR back to the head of the merge queue",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			prNum, err := parsePRNumber(args[0])
			if err != nil {
				return err
			}
			if repoID == "" {
				repoID = "default"
			}

			if !confirm(cmd, fmt.Sprintf("Force-retry PR #%d in merge queue for repo %q?", prNum, repoID), yes) {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "aborted")
				return nil
			}

			client, err := redisAdminClient()
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			if err := client.ForceRetry(cmd.Context(), repoID, prNum); err != nil {
				return fmt.Errorf("force-merge: %w", err)
			}

			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"retried":  true,
				"prNumber": prNum,
				"repoId":   repoID,
			})
		},
	}
	cmd.Flags().StringVar(&repoID, "repo", "default", "Repository ID")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip confirmation prompt")
	return cmd
}

// parsePRNumber converts a string argument to a positive integer.
func parsePRNumber(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid PR number %q: must be a positive integer", s)
	}
	return n, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// newAdminCleanupCmd — worktree + branch pruning
// ──────────────────────────────────────────────────────────────────────────────

// CleanupResult is the JSON shape emitted by `af admin cleanup`.
type CleanupResult struct {
	DryRun    bool              `json:"dryRun"`
	Worktrees WorktreeResult    `json:"worktrees"`
	Branches  BranchCleanResult `json:"branches"`
}

// WorktreeResult holds per-worktree cleanup stats.
type WorktreeResult struct {
	Scanned  int             `json:"scanned"`
	Orphaned int             `json:"orphaned"`
	Cleaned  int             `json:"cleaned"`
	Skipped  int             `json:"skipped"`
	Errors   []WorktreeError `json:"errors"`
}

// WorktreeError records a failure removing one worktree.
type WorktreeError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// BranchCleanResult holds per-branch cleanup stats.
type BranchCleanResult struct {
	Scanned int           `json:"scanned"`
	Deleted int           `json:"deleted"`
	Errors  []BranchError `json:"errors"`
}

// BranchError records a failure deleting one branch.
type BranchError struct {
	Branch string `json:"branch"`
	Error  string `json:"error"`
}

func newAdminCleanupCmd() *cobra.Command {
	var (
		dryRun        bool
		force         bool
		worktreePath  string
		skipWorktrees bool
		skipBranches  bool
		yes           bool
	)

	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Prune orphaned git worktrees and stale local branches",
		Long: `Prune orphaned git worktrees and stale local branches.

Mirrors the TypeScript 'af-cleanup' + 'af-cleanup-sub-issues' surface.

Orphaned worktrees are identified by:
  - Branch no longer exists (merged/deleted)
  - Not listed in 'git worktree list' (stale directory)

Branch cleanup:
  By default, deletes local branches already merged into main.
  With --force, also deletes branches whose remote tracking branch is gone.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			// Resolve worktree path default
			if worktreePath == "" {
				root, err := gitRoot()
				if err != nil {
					return fmt.Errorf("git root: %w", err)
				}
				worktreePath = filepath.Join(root, "..", filepath.Base(root)+".wt")
			}

			result := CleanupResult{DryRun: dryRun}

			// Confirm destructive ops (unless dry-run or --yes)
			if !dryRun && !yes {
				if !confirm(cmd, "This will remove orphaned worktrees and/or stale branches. Continue?", false) {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "aborted")
					return nil
				}
			}

			if !skipWorktrees {
				wt, err := runWorktreeCleanup(ctx, worktreePath, dryRun, force)
				if err != nil {
					return fmt.Errorf("worktree cleanup: %w", err)
				}
				result.Worktrees = wt
			}

			if !skipBranches {
				br, err := runBranchCleanup(ctx, dryRun, force)
				if err != nil {
					return fmt.Errorf("branch cleanup: %w", err)
				}
				result.Branches = br
			}

			return writeJSON(cmd.OutOrStdout(), result)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be cleaned without removing")
	cmd.Flags().BoolVar(&force, "force", false, "Force removal (includes branches with gone remotes)")
	cmd.Flags().StringVar(&worktreePath, "path", "", "Custom worktrees directory (default: ../<repoName>.wt)")
	cmd.Flags().BoolVar(&skipWorktrees, "skip-worktrees", false, "Skip worktree cleanup")
	cmd.Flags().BoolVar(&skipBranches, "skip-branches", false, "Skip branch cleanup")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip confirmation prompt")

	return cmd
}

// ──────────────────────────────────────────────────────────────────────────────
// Git helpers (shells out, same model as TS cleanup-runner)
// ──────────────────────────────────────────────────────────────────────────────

// gitRoot returns the absolute path to the git repository root.
func gitRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitWorktrees returns a map[path]branch of registered git worktrees.
func gitWorktrees() (map[string]string, error) {
	out, err := exec.Command("git", "worktree", "list", "--porcelain").Output()
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w", err)
	}
	result := map[string]string{}
	var currentPath string
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			currentPath = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch "):
			branch := strings.TrimPrefix(line, "branch ")
			branch = strings.TrimPrefix(branch, "refs/heads/")
			result[currentPath] = branch
		}
	}
	return result, nil
}

// branchExists returns true if the named local branch exists.
func branchExists(name string) bool {
	err := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+name).Run() //nolint:gosec // G204: name comes from git worktree list output, not user input
	return err == nil
}

// reservedDirs are infrastructure dirs inside the worktree root that should
// never be treated as agent worktrees (same set as the TS runner).
var reservedDirs = map[string]bool{
	".patches":         true,
	"__merge-worker__": true,
}

// runWorktreeCleanup scans worktreePath and removes orphaned worktrees.
func runWorktreeCleanup(_ context.Context, worktreePath string, dryRun, force bool) (WorktreeResult, error) {
	result := WorktreeResult{}

	entries, err := os.ReadDir(worktreePath) //nolint:gosec // G304: user-supplied path intentional; CLI flag
	if err != nil {
		if os.IsNotExist(err) {
			// Not an error — the directory just doesn't exist yet
			return result, nil
		}
		return result, fmt.Errorf("read dir %s: %w", worktreePath, err)
	}

	knownWorktrees, _ := gitWorktrees()

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if reservedDirs[entry.Name()] {
			continue
		}

		entryPath := filepath.Join(worktreePath, entry.Name())
		result.Scanned++

		// Determine orphan status
		orphaned := false
		var reason string
		_, isKnown := knownWorktrees[entryPath]
		switch {
		case force:
			orphaned = true
			reason = "force cleanup requested"
		case !isKnown:
			orphaned = true
			reason = "not registered with git worktree"
		case !branchExists(entry.Name()):
			orphaned = true
			reason = fmt.Sprintf("branch %q no longer exists", entry.Name())
		}

		if !orphaned {
			continue
		}
		result.Orphaned++

		if dryRun {
			continue
		}

		// Safety: refuse to remove the main working tree (where .git is a dir)
		dotGit := filepath.Join(entryPath, ".git")
		fi, statErr := os.Stat(dotGit)
		if statErr == nil && fi.IsDir() {
			result.Errors = append(result.Errors, WorktreeError{
				Path:  entryPath,
				Error: "SAFETY: .git is a directory — refusing to remove main working tree",
			})
			result.Skipped++
			continue
		}

		// Try git worktree remove first, then rm -rf
		rmErr := exec.Command("git", "worktree", "remove", entryPath, "--force").Run() //nolint:gosec // G204: path from filesystem read, not user input
		if rmErr != nil {
			rmErr = exec.Command("rm", "-rf", entryPath).Run() //nolint:gosec // G204: path from filesystem read
		}
		if rmErr != nil {
			result.Errors = append(result.Errors, WorktreeError{
				Path:  entryPath,
				Error: fmt.Sprintf("remove failed: %v (reason: %s)", rmErr, reason),
			})
		} else {
			result.Cleaned++
		}
	}

	// Prune git metadata
	if !dryRun {
		_ = exec.Command("git", "worktree", "prune").Run()
	}

	return result, nil
}

// runBranchCleanup deletes merged (and optionally gone) local branches.
func runBranchCleanup(_ context.Context, dryRun, force bool) (BranchCleanResult, error) {
	result := BranchCleanResult{}

	// Prune stale worktree metadata so locked branches can be deleted
	_ = exec.Command("git", "worktree", "prune").Run()

	// Determine base branch
	var baseBranch string
	switch {
	case branchExists("main"):
		baseBranch = "main"
	case branchExists("master"):
		baseBranch = "master"
	default:
		// No base branch found — skip branch cleanup silently
		return result, nil
	}

	// Current branch (never delete)
	currentBranchOut, _ := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	currentBranch := strings.TrimSpace(string(currentBranchOut))

	// Merged branches
	toDelete := map[string]string{} // branch → reason

	mergedOut, err := exec.Command("git", "branch", "--merged", baseBranch).Output() //nolint:gosec // G204: baseBranch is "main" or "master" only
	if err == nil {
		for _, line := range strings.Split(string(mergedOut), "\n") {
			b := strings.TrimLeft(line, "* ")
			b = strings.TrimSpace(b)
			if b == "" || b == baseBranch || b == currentBranch {
				continue
			}
			toDelete[b] = "merged"
		}
	}

	// Gone-remote branches (only when --force)
	if force {
		_ = exec.Command("git", "fetch", "--prune").Run()
		vvOut, vvErr := exec.Command("git", "branch", "-vv").Output()
		if vvErr == nil {
			for _, line := range strings.Split(string(vvOut), "\n") {
				if !strings.Contains(line, ": gone]") {
					continue
				}
				parts := strings.Fields(strings.TrimLeft(line, "* "))
				if len(parts) > 0 {
					b := parts[0]
					if b != baseBranch && b != currentBranch {
						if _, exists := toDelete[b]; !exists {
							toDelete[b] = "remote gone"
						}
					}
				}
			}
		}
	}

	result.Scanned = len(toDelete)

	for branch, reason := range toDelete {
		if dryRun {
			continue
		}
		flag := "-d"
		if reason == "remote gone" {
			flag = "-D"
		}
		if out, err := exec.Command("git", "branch", flag, branch).CombinedOutput(); err != nil { //nolint:gosec // G204: flag is "-d" or "-D"; branch comes from git branch output
			result.Errors = append(result.Errors, BranchError{
				Branch: branch,
				Error:  strings.TrimSpace(string(out)),
			})
		} else {
			result.Deleted++
		}
	}

	return result, nil
}

// writeAdminJSON is a local alias to writeJSON (defined in linear.go).
// We reference it inline; this avoids re-declaring it.
var _ = json.Marshal // ensure encoding/json is used
