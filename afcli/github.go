package afcli

// af github — GitHub Issues operations.
//
// This file mirrors the `af linear` surface for GitHub Issues. Each subcommand
// has a direct conceptual equivalent; JSON output shapes are intentionally
// stable for automation compatibility.
//
// Auth strategy:
//   1. GITHUB_TOKEN env var → direct calls to api.github.com with a
//      personal access token (classic or fine-grained) or a GitHub App
//      installation token. This is the preferred path for local dev and
//      worker-fleet operation.
//   2. No env key + ds() returns an authenticated *afclient.Client (rsk_
//      token + base URL) → platform proxy mode via the platform's
//      /api/cli/github/rest route. The platform unwraps the rsk_ token,
//      looks up the org's stored GitHub App installation credential, and
//      forwards the REST request under that credential.
//   3. Neither → hard error with an actionable message.
//
// Env-var wins precedence is deliberate: it preserves the worker-fleet
// behavior without conditional logic and matches the posture every other
// afcli command uses.
//
// Owner/repo resolution:
//   Subcommands that target a specific repo accept --owner and --repo flags.
//   Both default to the GITHUB_OWNER / GITHUB_REPO env vars respectively, so
//   callers in a fleet context rarely need to set them explicitly. Some
//   subcommands (list-issues, get-issue, …) also accept the shorthand
//   --repo owner/repo to set both in one flag.

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
	gh "github.com/RenseiAI/agentfactory-tui/internal/github"
)

// ─── top-level command factory ────────────────────────────────────────────────

// newGitHubCmd constructs the `github` parent command with all sub-commands.
// It is wired into RegisterCommands so that `<binary> github <sub>` works.
//
// `ds` is the platform-DataSource factory; subcommands call it lazily to
// pick up rsk_ credentials when running embedded under rensei. The factory
// may be nil — subcommands degrade to the GITHUB_TOKEN env path.
func newGitHubCmd(ds func() afclient.DataSource) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "github",
		Short: "GitHub Issues operations",
		Long: `GitHub Issues operations.

Mirrors the 'af linear' surface adapted to GitHub Issues vocabulary.
Outputs JSON to stdout.

Authentication (in order):
  1. GITHUB_TOKEN env var → direct calls to api.github.com (personal access
     token, fine-grained token, or GitHub App installation token).
  2. Otherwise, when running under a logged-in platform session (rsk_ token),
     REST calls are proxied through the platform under the org's connected
     GitHub App installation credential.
  3. Neither set → hard error.

Owner/repo resolution (applied to all repo-scoped subcommands):
  --owner  defaults to GITHUB_OWNER env var.
  --repo   defaults to GITHUB_REPO env var.
  Either flag also accepts the combined 'owner/repo' form.`,
		SilenceUsage: true,
	}

	cmd.AddCommand(newGitHubGetIssueCmd(ds))
	cmd.AddCommand(newGitHubCreateIssueCmd(ds))
	cmd.AddCommand(newGitHubUpdateIssueCmd(ds))
	cmd.AddCommand(newGitHubListIssuesCmd(ds))
	cmd.AddCommand(newGitHubListCommentsCmd(ds))
	cmd.AddCommand(newGitHubCreateCommentCmd(ds))
	cmd.AddCommand(newGitHubAddLabelsCmd(ds))
	cmd.AddCommand(newGitHubSetAssigneesCmd(ds))
	cmd.AddCommand(newGitHubCloseIssueCmd(ds))
	cmd.AddCommand(newGitHubReopenIssueCmd(ds))
	cmd.AddCommand(newGitHubListLabelsCmd(ds))
	cmd.AddCommand(newGitHubGetRepoCmd(ds))

	return cmd
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// githubToken resolves the GitHub token from the environment.
func githubToken() string {
	return os.Getenv("GITHUB_TOKEN")
}

// newGitHubClient resolves a GitHub client following the auth strategy in
// the package header doc-comment.
//
// When githubTestBaseURL is non-empty (set by tests via setGitHubTestBaseURL),
// the direct-path client's BaseURL is overridden to point at the test server.
func newGitHubClient(ds func() afclient.DataSource) (gh.GitHub, error) {
	// Path 1: env-var direct.
	if token := githubToken(); token != "" {
		c := gh.NewClient(token)
		if githubTestBaseURL != "" {
			c.BaseURL = githubTestBaseURL
		}
		return c, nil
	}

	// Path 2: platform proxy (rsk_ session).
	if ds != nil {
		if baseURL, token, ok := afclient.CredentialsFromDataSource(ds()); ok {
			return gh.NewProxiedClient(baseURL, token), nil
		}
	}

	// Path 3: neither — actionable error.
	return nil, fmt.Errorf(
		"github access requires either a GITHUB_TOKEN env var or a " +
			"logged-in platform session: set the env var, or run `af login` " +
			"then connect a GitHub integration",
	)
}

// splitOwnerRepo parses "owner/repo" into (owner, repo).
// If the string has no slash it is treated as the repo name only.
func splitOwnerRepo(ownerRepo string) (owner, repo string) {
	if idx := strings.Index(ownerRepo, "/"); idx >= 0 {
		return ownerRepo[:idx], ownerRepo[idx+1:]
	}
	return "", ownerRepo
}

// resolveOwnerRepo resolves owner and repo from flags/env.
// --owner / GITHUB_OWNER and --repo / GITHUB_REPO.
// The --repo flag may carry "owner/repo" form.
func resolveOwnerRepo(ownerFlag, repoFlag string) (owner, repo string, err error) {
	owner = ownerFlag
	if owner == "" {
		owner = os.Getenv("GITHUB_OWNER")
	}
	repo = repoFlag
	if repo == "" {
		repo = os.Getenv("GITHUB_REPO")
	}
	// If repo flag has the combined form, split it and let it override.
	if strings.Contains(repo, "/") {
		o, r := splitOwnerRepo(repo)
		if o != "" {
			owner = o
		}
		repo = r
	}
	if owner == "" {
		return "", "", fmt.Errorf("owner is required: set --owner or GITHUB_OWNER env var (or pass owner/repo to --repo)")
	}
	if repo == "" {
		return "", "", fmt.Errorf("repo is required: set --repo or GITHUB_REPO env var")
	}
	return owner, repo, nil
}

// labelNames returns label names from a slice of Label.
func ghLabelNames(labels []gh.Label) []string {
	names := make([]string, len(labels))
	for i, l := range labels {
		names[i] = l.Name
	}
	return names
}

// assigneeLogins returns login names from a slice of User.
func assigneeLogins(users []gh.User) []string {
	logins := make([]string, len(users))
	for i, u := range users {
		logins[i] = u.Login
	}
	return logins
}

// readFileIfSet returns the contents of path if path is non-empty;
// otherwise returns inline.
func ghResolveFileArg(inline, path string) (string, error) {
	if path == "" {
		return inline, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: user-supplied path, intentional CLI flag
	if err != nil {
		return "", fmt.Errorf("read file %q: %w", path, err)
	}
	return string(data), nil
}

// issueJSON renders an Issue as the stable output map.
func issueJSON(iss *gh.Issue) map[string]any {
	var milestoneTitle any
	if iss.Milestone != nil {
		milestoneTitle = iss.Milestone.Title
	}
	var user string
	if iss.User != nil {
		user = iss.User.Login
	}
	return map[string]any{
		"number":    iss.Number,
		"title":     iss.Title,
		"body":      iss.Body,
		"state":     iss.State,
		"url":       iss.HTMLURL,
		"labels":    ghLabelNames(iss.Labels),
		"assignees": assigneeLogins(iss.Assignees),
		"author":    user,
		"milestone": milestoneTitle,
		"comments":  iss.Comments,
		"createdAt": iss.CreatedAt,
		"updatedAt": iss.UpdatedAt,
	}
}

// ─── get-issue ────────────────────────────────────────────────────────────────

func newGitHubGetIssueCmd(ds func() afclient.DataSource) *cobra.Command {
	ownerF, repoF := new(string), new(string)
	var number int

	cmd := &cobra.Command{
		Use:   "get-issue",
		Short: "Get issue details",
		Long: `Get details of a GitHub issue.

Example:
  af github get-issue --repo owner/repo --number 42`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if number <= 0 {
				return fmt.Errorf("usage: af github get-issue --repo owner/repo --number 42")
			}
			client, err := newGitHubClient(ds)
			if err != nil {
				return err
			}
			owner, repo, err := resolveOwnerRepo(*ownerF, *repoF)
			if err != nil {
				return err
			}
			issue, err := client.GetIssue(cmd.Context(), owner, repo, number)
			if err != nil {
				return fmt.Errorf("get issue: %w", err)
			}
			return writeJSON(cmd.OutOrStdout(), issueJSON(issue))
		},
	}

	cmd.Flags().StringVar(ownerF, "owner", "", "GitHub org or user name (or set GITHUB_OWNER)")
	cmd.Flags().StringVar(repoF, "repo", "", "Repository name, or 'owner/repo' (or set GITHUB_REPO)")
	cmd.Flags().IntVar(&number, "number", 0, "Issue number (required)")

	return cmd
}

// ─── create-issue ─────────────────────────────────────────────────────────────

func newGitHubCreateIssueCmd(ds func() afclient.DataSource) *cobra.Command {
	var (
		ownerF, repoF   string
		title           string
		body            string
		bodyFile        string
		labels          string
		assignees       string
	)

	cmd := &cobra.Command{
		Use:   "create-issue",
		Short: "Create a new issue",
		Long: `Create a new GitHub issue.

Example:
  af github create-issue --repo owner/repo --title "Bug: something broken" \
    --body "Description" --labels "bug,needs-triage"`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if title == "" {
				return fmt.Errorf("usage: af github create-issue --repo owner/repo --title \"Title\" [--body \"...\"] ...")
			}
			client, err := newGitHubClient(ds)
			if err != nil {
				return err
			}
			owner, repo, err := resolveOwnerRepo(ownerF, repoF)
			if err != nil {
				return err
			}

			resolvedBody, err := ghResolveFileArg(body, bodyFile)
			if err != nil {
				return err
			}

			input := gh.CreateIssueInput{
				Title:     title,
				Body:      resolvedBody,
				Labels:    parseCSV(labels),
				Assignees: parseCSV(assignees),
			}

			issue, err := client.CreateIssue(cmd.Context(), owner, repo, input)
			if err != nil {
				return fmt.Errorf("create issue: %w", err)
			}
			return writeJSON(cmd.OutOrStdout(), issueJSON(issue))
		},
	}

	cmd.Flags().StringVar(&ownerF, "owner", "", "GitHub org or user name (or set GITHUB_OWNER)")
	cmd.Flags().StringVar(&repoF, "repo", "", "Repository name, or 'owner/repo' (or set GITHUB_REPO)")
	cmd.Flags().StringVar(&title, "title", "", "Issue title (required)")
	cmd.Flags().StringVar(&body, "body", "", "Issue body")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "Path to file containing issue body")
	cmd.Flags().StringVar(&labels, "labels", "", "Comma-separated label names (e.g. 'bug,enhancement')")
	cmd.Flags().StringVar(&assignees, "assignees", "", "Comma-separated GitHub usernames to assign")

	return cmd
}

// ─── update-issue ─────────────────────────────────────────────────────────────

func newGitHubUpdateIssueCmd(ds func() afclient.DataSource) *cobra.Command {
	var (
		ownerF, repoF string
		number        int
		title         string
		body          string
		bodyFile      string
		labels        string
		assignees     string
		state         string
	)

	cmd := &cobra.Command{
		Use:   "update-issue",
		Short: "Update an existing issue",
		Long: `Update an existing GitHub issue.

Example:
  af github update-issue --repo owner/repo --number 42 --state closed`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if number <= 0 {
				return fmt.Errorf("usage: af github update-issue --repo owner/repo --number 42 [--title ...] [--state open|closed]")
			}
			client, err := newGitHubClient(ds)
			if err != nil {
				return err
			}
			owner, repo, err := resolveOwnerRepo(ownerF, repoF)
			if err != nil {
				return err
			}

			resolvedBody, err := ghResolveFileArg(body, bodyFile)
			if err != nil {
				return err
			}

			input := gh.UpdateIssueInput{
				Title:     title,
				Body:      resolvedBody,
				State:     state,
				Labels:    parseCSV(labels),
				Assignees: parseCSV(assignees),
			}

			issue, err := client.UpdateIssue(cmd.Context(), owner, repo, number, input)
			if err != nil {
				return fmt.Errorf("update issue: %w", err)
			}
			return writeJSON(cmd.OutOrStdout(), issueJSON(issue))
		},
	}

	cmd.Flags().StringVar(&ownerF, "owner", "", "GitHub org or user name (or set GITHUB_OWNER)")
	cmd.Flags().StringVar(&repoF, "repo", "", "Repository name, or 'owner/repo' (or set GITHUB_REPO)")
	cmd.Flags().IntVar(&number, "number", 0, "Issue number (required)")
	cmd.Flags().StringVar(&title, "title", "", "New title")
	cmd.Flags().StringVar(&body, "body", "", "New body")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "Path to file containing new body")
	cmd.Flags().StringVar(&labels, "labels", "", "Comma-separated label names (replaces current labels)")
	cmd.Flags().StringVar(&assignees, "assignees", "", "Comma-separated GitHub usernames (replaces current assignees)")
	cmd.Flags().StringVar(&state, "state", "", "New state: 'open' or 'closed'")

	return cmd
}

// ─── list-issues ──────────────────────────────────────────────────────────────

func newGitHubListIssuesCmd(ds func() afclient.DataSource) *cobra.Command {
	var (
		ownerF, repoF string
		state         string
		labels        string
		assignee      string
		creator       string
		milestone     string
		sort          string
		direction     string
		since         string
		limit         int
	)

	cmd := &cobra.Command{
		Use:   "list-issues",
		Short: "List issues with flexible filters",
		Long: `List GitHub issues with optional filters.

Example:
  af github list-issues --repo owner/repo --state open --labels "bug" --limit 20`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newGitHubClient(ds)
			if err != nil {
				return err
			}
			owner, repo, err := resolveOwnerRepo(ownerF, repoF)
			if err != nil {
				return err
			}

			if limit <= 0 {
				limit = 30
			}
			if limit > 100 {
				limit = 100
			}

			issues, err := client.ListIssues(cmd.Context(), owner, repo, gh.ListIssuesOptions{
				State:     state,
				Labels:    labels,
				Assignee:  assignee,
				Creator:   creator,
				Milestone: milestone,
				Sort:      sort,
				Direction: direction,
				Since:     since,
				PerPage:   limit,
			})
			if err != nil {
				return fmt.Errorf("list issues: %w", err)
			}

			out := make([]map[string]any, len(issues))
			for i, iss := range issues {
				out[i] = issueJSON(&iss)
			}
			return writeJSON(cmd.OutOrStdout(), out)
		},
	}

	cmd.Flags().StringVar(&ownerF, "owner", "", "GitHub org or user name (or set GITHUB_OWNER)")
	cmd.Flags().StringVar(&repoF, "repo", "", "Repository name, or 'owner/repo' (or set GITHUB_REPO)")
	cmd.Flags().StringVar(&state, "state", "open", "Issue state: open, closed, or all")
	cmd.Flags().StringVar(&labels, "labels", "", "Comma-separated label names to filter by")
	cmd.Flags().StringVar(&assignee, "assignee", "", "Filter by assignee login, 'none', or '*'")
	cmd.Flags().StringVar(&creator, "creator", "", "Filter by issue creator login")
	cmd.Flags().StringVar(&milestone, "milestone", "", "Filter by milestone number, '*', or 'none'")
	cmd.Flags().StringVar(&sort, "sort", "created", "Sort by: created, updated, or comments")
	cmd.Flags().StringVar(&direction, "direction", "desc", "Sort direction: asc or desc")
	cmd.Flags().StringVar(&since, "since", "", "Only issues updated after this ISO 8601 timestamp")
	cmd.Flags().IntVar(&limit, "limit", 30, "Maximum number of issues to return (max 100)")

	return cmd
}

// ─── list-comments ────────────────────────────────────────────────────────────

func newGitHubListCommentsCmd(ds func() afclient.DataSource) *cobra.Command {
	var (
		ownerF, repoF string
		number        int
	)

	cmd := &cobra.Command{
		Use:   "list-comments",
		Short: "List comments on an issue",
		Long: `List comments on a GitHub issue.

Example:
  af github list-comments --repo owner/repo --number 42`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if number <= 0 {
				return fmt.Errorf("usage: af github list-comments --repo owner/repo --number 42")
			}
			client, err := newGitHubClient(ds)
			if err != nil {
				return err
			}
			owner, repo, err := resolveOwnerRepo(ownerF, repoF)
			if err != nil {
				return err
			}

			comments, err := client.ListIssueComments(cmd.Context(), owner, repo, number)
			if err != nil {
				return fmt.Errorf("list comments: %w", err)
			}

			out := make([]map[string]any, len(comments))
			for i, c := range comments {
				var userLogin any
				if c.User != nil {
					userLogin = c.User.Login
				}
				out[i] = map[string]any{
					"id":        c.ID,
					"body":      c.Body,
					"author":    userLogin,
					"url":       c.HTMLURL,
					"createdAt": c.CreatedAt,
					"updatedAt": c.UpdatedAt,
				}
			}
			return writeJSON(cmd.OutOrStdout(), out)
		},
	}

	cmd.Flags().StringVar(&ownerF, "owner", "", "GitHub org or user name (or set GITHUB_OWNER)")
	cmd.Flags().StringVar(&repoF, "repo", "", "Repository name, or 'owner/repo' (or set GITHUB_REPO)")
	cmd.Flags().IntVar(&number, "number", 0, "Issue number (required)")

	return cmd
}

// ─── create-comment ───────────────────────────────────────────────────────────

func newGitHubCreateCommentCmd(ds func() afclient.DataSource) *cobra.Command {
	var (
		ownerF, repoF string
		number        int
		body          string
		bodyFile      string
	)

	cmd := &cobra.Command{
		Use:   "create-comment",
		Short: "Create a comment on an issue",
		Long: `Post a comment on a GitHub issue.

Example:
  af github create-comment --repo owner/repo --number 42 --body "Looks good!"`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if number <= 0 {
				return fmt.Errorf("usage: af github create-comment --repo owner/repo --number 42 --body \"...\"")
			}
			resolvedBody, err := ghResolveFileArg(body, bodyFile)
			if err != nil {
				return err
			}
			if resolvedBody == "" {
				return fmt.Errorf("usage: af github create-comment --repo owner/repo --number 42 --body \"Comment text\" or --body-file /path")
			}

			client, err := newGitHubClient(ds)
			if err != nil {
				return err
			}
			owner, repo, err := resolveOwnerRepo(ownerF, repoF)
			if err != nil {
				return err
			}

			comment, err := client.CreateIssueComment(cmd.Context(), owner, repo, number, resolvedBody)
			if err != nil {
				return fmt.Errorf("create comment: %w", err)
			}

			var userLogin any
			if comment.User != nil {
				userLogin = comment.User.Login
			}
			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"id":        comment.ID,
				"body":      comment.Body,
				"author":    userLogin,
				"url":       comment.HTMLURL,
				"createdAt": comment.CreatedAt,
			})
		},
	}

	cmd.Flags().StringVar(&ownerF, "owner", "", "GitHub org or user name (or set GITHUB_OWNER)")
	cmd.Flags().StringVar(&repoF, "repo", "", "Repository name, or 'owner/repo' (or set GITHUB_REPO)")
	cmd.Flags().IntVar(&number, "number", 0, "Issue number (required)")
	cmd.Flags().StringVar(&body, "body", "", "Comment body text")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "Path to file containing comment body")

	return cmd
}

// ─── add-labels ───────────────────────────────────────────────────────────────

func newGitHubAddLabelsCmd(ds func() afclient.DataSource) *cobra.Command {
	var (
		ownerF, repoF string
		number        int
		labels        string
	)

	cmd := &cobra.Command{
		Use:   "add-labels",
		Short: "Add labels to an issue (non-destructive)",
		Long: `Add labels to a GitHub issue. Existing labels are preserved.

Example:
  af github add-labels --repo owner/repo --number 42 --labels "bug,priority:high"`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if number <= 0 || labels == "" {
				return fmt.Errorf("usage: af github add-labels --repo owner/repo --number 42 --labels \"label1,label2\"")
			}
			client, err := newGitHubClient(ds)
			if err != nil {
				return err
			}
			owner, repo, err := resolveOwnerRepo(ownerF, repoF)
			if err != nil {
				return err
			}

			added, err := client.AddLabels(cmd.Context(), owner, repo, number, parseCSV(labels))
			if err != nil {
				return fmt.Errorf("add labels: %w", err)
			}

			names := make([]string, len(added))
			for i, l := range added {
				names[i] = l.Name
			}
			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"number": number,
				"labels": names,
			})
		},
	}

	cmd.Flags().StringVar(&ownerF, "owner", "", "GitHub org or user name (or set GITHUB_OWNER)")
	cmd.Flags().StringVar(&repoF, "repo", "", "Repository name, or 'owner/repo' (or set GITHUB_REPO)")
	cmd.Flags().IntVar(&number, "number", 0, "Issue number (required)")
	cmd.Flags().StringVar(&labels, "labels", "", "Comma-separated label names to add (required)")

	return cmd
}

// ─── set-assignees ────────────────────────────────────────────────────────────

func newGitHubSetAssigneesCmd(ds func() afclient.DataSource) *cobra.Command {
	var (
		ownerF, repoF string
		number        int
		assignees     string
	)

	cmd := &cobra.Command{
		Use:   "set-assignees",
		Short: "Replace assignees on an issue",
		Long: `Replace the assignee list on a GitHub issue.
Passing an empty --assignees clears all assignees.

Example:
  af github set-assignees --repo owner/repo --number 42 --assignees "alice,bob"`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if number <= 0 {
				return fmt.Errorf("usage: af github set-assignees --repo owner/repo --number 42 --assignees \"alice,bob\"")
			}
			client, err := newGitHubClient(ds)
			if err != nil {
				return err
			}
			owner, repo, err := resolveOwnerRepo(ownerF, repoF)
			if err != nil {
				return err
			}

			issue, err := client.SetAssignees(cmd.Context(), owner, repo, number, parseCSV(assignees))
			if err != nil {
				return fmt.Errorf("set assignees: %w", err)
			}
			return writeJSON(cmd.OutOrStdout(), issueJSON(issue))
		},
	}

	cmd.Flags().StringVar(&ownerF, "owner", "", "GitHub org or user name (or set GITHUB_OWNER)")
	cmd.Flags().StringVar(&repoF, "repo", "", "Repository name, or 'owner/repo' (or set GITHUB_REPO)")
	cmd.Flags().IntVar(&number, "number", 0, "Issue number (required)")
	cmd.Flags().StringVar(&assignees, "assignees", "", "Comma-separated GitHub usernames (empty to clear)")

	return cmd
}

// ─── close-issue ──────────────────────────────────────────────────────────────

func newGitHubCloseIssueCmd(ds func() afclient.DataSource) *cobra.Command {
	var (
		ownerF, repoF string
		number        int
		comment       string
	)

	cmd := &cobra.Command{
		Use:   "close-issue",
		Short: "Close an issue (optionally with a comment)",
		Long: `Close a GitHub issue, optionally posting a closing comment.

Example:
  af github close-issue --repo owner/repo --number 42 --comment "Resolved in v2.0"`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if number <= 0 {
				return fmt.Errorf("usage: af github close-issue --repo owner/repo --number 42 [--comment \"...\"]")
			}
			client, err := newGitHubClient(ds)
			if err != nil {
				return err
			}
			owner, repo, err := resolveOwnerRepo(ownerF, repoF)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			if comment != "" {
				if _, err := client.CreateIssueComment(ctx, owner, repo, number, comment); err != nil {
					return fmt.Errorf("create comment: %w", err)
				}
			}

			issue, err := client.UpdateIssue(ctx, owner, repo, number, gh.UpdateIssueInput{State: "closed"})
			if err != nil {
				return fmt.Errorf("close issue: %w", err)
			}
			return writeJSON(cmd.OutOrStdout(), issueJSON(issue))
		},
	}

	cmd.Flags().StringVar(&ownerF, "owner", "", "GitHub org or user name (or set GITHUB_OWNER)")
	cmd.Flags().StringVar(&repoF, "repo", "", "Repository name, or 'owner/repo' (or set GITHUB_REPO)")
	cmd.Flags().IntVar(&number, "number", 0, "Issue number (required)")
	cmd.Flags().StringVar(&comment, "comment", "", "Optional closing comment")

	return cmd
}

// ─── reopen-issue ─────────────────────────────────────────────────────────────

func newGitHubReopenIssueCmd(ds func() afclient.DataSource) *cobra.Command {
	var (
		ownerF, repoF string
		number        int
		comment       string
	)

	cmd := &cobra.Command{
		Use:   "reopen-issue",
		Short: "Reopen a closed issue (optionally with a comment)",
		Long: `Reopen a closed GitHub issue, optionally posting a comment.

Example:
  af github reopen-issue --repo owner/repo --number 42`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if number <= 0 {
				return fmt.Errorf("usage: af github reopen-issue --repo owner/repo --number 42 [--comment \"...\"]")
			}
			client, err := newGitHubClient(ds)
			if err != nil {
				return err
			}
			owner, repo, err := resolveOwnerRepo(ownerF, repoF)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			if comment != "" {
				if _, err := client.CreateIssueComment(ctx, owner, repo, number, comment); err != nil {
					return fmt.Errorf("create comment: %w", err)
				}
			}

			issue, err := client.UpdateIssue(ctx, owner, repo, number, gh.UpdateIssueInput{State: "open"})
			if err != nil {
				return fmt.Errorf("reopen issue: %w", err)
			}
			return writeJSON(cmd.OutOrStdout(), issueJSON(issue))
		},
	}

	cmd.Flags().StringVar(&ownerF, "owner", "", "GitHub org or user name (or set GITHUB_OWNER)")
	cmd.Flags().StringVar(&repoF, "repo", "", "Repository name, or 'owner/repo' (or set GITHUB_REPO)")
	cmd.Flags().IntVar(&number, "number", 0, "Issue number (required)")
	cmd.Flags().StringVar(&comment, "comment", "", "Optional comment to post when reopening")

	return cmd
}

// ─── list-labels ──────────────────────────────────────────────────────────────

func newGitHubListLabelsCmd(ds func() afclient.DataSource) *cobra.Command {
	var ownerF, repoF string

	cmd := &cobra.Command{
		Use:   "list-labels",
		Short: "List all labels in a repository",
		Long: `List all labels defined in a GitHub repository.

Example:
  af github list-labels --repo owner/repo`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newGitHubClient(ds)
			if err != nil {
				return err
			}
			owner, repo, err := resolveOwnerRepo(ownerF, repoF)
			if err != nil {
				return err
			}

			labels, err := client.ListLabels(cmd.Context(), owner, repo)
			if err != nil {
				return fmt.Errorf("list labels: %w", err)
			}

			out := make([]map[string]any, len(labels))
			for i, l := range labels {
				out[i] = map[string]any{
					"id":          l.ID,
					"name":        l.Name,
					"color":       l.Color,
					"description": l.Description,
				}
			}
			return writeJSON(cmd.OutOrStdout(), out)
		},
	}

	cmd.Flags().StringVar(&ownerF, "owner", "", "GitHub org or user name (or set GITHUB_OWNER)")
	cmd.Flags().StringVar(&repoF, "repo", "", "Repository name, or 'owner/repo' (or set GITHUB_REPO)")

	return cmd
}

// ─── get-repo ─────────────────────────────────────────────────────────────────

func newGitHubGetRepoCmd(ds func() afclient.DataSource) *cobra.Command {
	var ownerF, repoF string

	cmd := &cobra.Command{
		Use:   "get-repo",
		Short: "Get repository metadata",
		Long: `Get metadata about a GitHub repository.

Example:
  af github get-repo --repo owner/repo`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newGitHubClient(ds)
			if err != nil {
				return err
			}
			owner, repo, err := resolveOwnerRepo(ownerF, repoF)
			if err != nil {
				return err
			}

			r, err := client.GetRepo(cmd.Context(), owner, repo)
			if err != nil {
				return fmt.Errorf("get repo: %w", err)
			}
			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"fullName":    r.FullName,
				"name":        r.Name,
				"description": r.Description,
				"url":         r.HTMLURL,
				"private":     r.Private,
				"openIssues":  r.OpenIssues,
			})
		},
	}

	cmd.Flags().StringVar(&ownerF, "owner", "", "GitHub org or user name (or set GITHUB_OWNER)")
	cmd.Flags().StringVar(&repoF, "repo", "", "Repository name, or 'owner/repo' (or set GITHUB_REPO)")

	return cmd
}

// ─── CSV helper ───────────────────────────────────────────────────────────────

// parseCSV parses a comma-separated string into a slice, trimming spaces.
// Returns nil for empty input.
func parseCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ─── compile-time interface check ────────────────────────────────────────────

// Ensure writeJSON (defined in linear.go) and io.Discard remain usable here.
var _ io.Writer = io.Discard
