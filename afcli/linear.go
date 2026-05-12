package afcli

// af linear — Linear issue-tracker operations.
//
// This file ports the TypeScript `af-linear` CLI surface to Go. Each subcommand
// mirrors the TS implementation in packages/linear/src/tools/linear-runner.ts.
// JSON output shapes are intentionally preserved for automation compatibility.
//
// Auth strategy (per ADR-2026-05-12-cli-linear-proxy):
//   1. LINEAR_API_KEY (or LINEAR_ACCESS_TOKEN) env var → direct GraphQL calls
//      to api.linear.app. Preserves the standalone `af` path AND the
//      worker-fleet path where `af agent run` injects the env var into the
//      in-session shell.
//   2. No env key + ds() returns an authenticated *afclient.Client (rsk_ token
//      + base URL) → platform proxy mode via `linear.NewProxiedClient`. The
//      platform unwraps the rsk_, looks up the org's stored Linear OAuth
//      credential, and forwards the GraphQL under that credential. This is
//      the rensei case.
//   3. Neither → hard error with an actionable message.
//
// Env wins precedence is deliberate: it preserves the worker-fleet behavior
// without conditional logic and matches the explicit-env-overrides-default
// posture every other afcli command uses.
//
// check-deployment is NOT ported here: it depends on Vercel/GitHub APIs and
// lives conceptually outside the Linear package. Callers needing it should
// continue using `pnpm af-linear check-deployment`.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
	"github.com/RenseiAI/agentfactory-tui/internal/linear"
)

// ─── top-level command factory ────────────────────────────────────────────────

// newLinearCmd constructs the `linear` parent command with all sub-commands.
// It is wired into RegisterCommands so that `<binary> linear <sub>` works.
//
// `ds` is the platform-DataSource factory; subcommands call it lazily to
// pick up rsk_ credentials when running embedded under rensei. The factory
// may be nil (e.g. minimal test embedders) — subcommands degrade to the
// LINEAR_API_KEY env path.
func newLinearCmd(ds func() afclient.DataSource) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "linear",
		Short: "Linear issue-tracker operations",
		Long: `Linear issue-tracker operations.

Mirrors the TypeScript 'pnpm af-linear' surface. Outputs JSON to stdout.

Authentication (in order):
  1. LINEAR_API_KEY (or LINEAR_ACCESS_TOKEN) env var → direct calls to api.linear.app.
  2. Otherwise, when running under rensei with a stored login session (rsk_
     token), GraphQL is proxied through the platform's /api/cli/linear/graphql
     route under the org's connected Linear OAuth credential.
  3. Neither set → hard error.

LINEAR_TEAM_NAME can be set to provide a default team for create-issue.`,
		SilenceUsage: true,
	}

	cmd.AddCommand(newLinearGetIssueCmd(ds))
	cmd.AddCommand(newLinearCreateIssueCmd(ds))
	cmd.AddCommand(newLinearUpdateIssueCmd(ds))
	cmd.AddCommand(newLinearListCommentsCmd(ds))
	cmd.AddCommand(newLinearCreateCommentCmd(ds))
	cmd.AddCommand(newLinearAddRelationCmd(ds))
	cmd.AddCommand(newLinearListRelationsCmd(ds))
	cmd.AddCommand(newLinearRemoveRelationCmd(ds))
	cmd.AddCommand(newLinearListSubIssuesCmd(ds))
	cmd.AddCommand(newLinearListSubIssueStatusesCmd(ds))
	cmd.AddCommand(newLinearUpdateSubIssueCmd(ds))
	cmd.AddCommand(newLinearListIssuesCmd(ds))
	cmd.AddCommand(newLinearCheckBlockedCmd(ds))
	cmd.AddCommand(newLinearListBacklogIssuesCmd(ds))
	cmd.AddCommand(newLinearListUnblockedBacklogCmd(ds))
	cmd.AddCommand(newLinearCreateBlockerCmd(ds))

	return cmd
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// apiKey resolves the Linear API key from the environment.
// Accepts both LINEAR_API_KEY and LINEAR_ACCESS_TOKEN.
func apiKey() string {
	if v := os.Getenv("LINEAR_API_KEY"); v != "" {
		return v
	}
	return os.Getenv("LINEAR_ACCESS_TOKEN")
}

// newLinearClient resolves a Linear client following the auth strategy in
// the package header doc-comment:
//
//  1. LINEAR_API_KEY / LINEAR_ACCESS_TOKEN env → direct path.
//  2. Authenticated DataSource (rsk_ token + platform base URL) → proxy
//     mode via `linear.NewProxiedClient`.
//  3. Neither → friendly error.
//
// When linearTestBaseURL is non-empty (set by tests via setTestBaseURL),
// the direct-path client's BaseURL is overridden to point at the test
// HTTP server. The proxy path is exercised by tests via injecting a
// MockClient-equivalent DataSource that returns an *afclient.Client whose
// BaseURL points at the test server.
func newLinearClient(ds func() afclient.DataSource) (linear.Linear, error) {
	// Path 1: env-var direct.
	if key := apiKey(); key != "" {
		c, err := linear.NewClient(key)
		if err != nil {
			return nil, err
		}
		if linearTestBaseURL != "" {
			c.BaseURL = linearTestBaseURL
		}
		return c, nil
	}

	// Path 2: platform proxy (rensei login session).
	if ds != nil {
		if baseURL, token, ok := afclient.CredentialsFromDataSource(ds()); ok {
			return linear.NewProxiedClient(baseURL, token)
		}
	}

	// Path 3: neither — actionable error.
	return nil, fmt.Errorf(
		"Linear access requires either a `LINEAR_API_KEY` env var or a " +
			"logged-in platform session. Set the env var, or run `rensei login` " +
			"+ `rensei project trackers connect-linear`.",
	)
}

// writeJSON writes v as indented JSON to w.
// Use cmd.OutOrStdout() as w so tests can capture output.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// readFile reads the content of path. Used for --description-file / --body-file.
func readFile(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: user-supplied file path is intentional; CLI flag
	if err != nil {
		return "", fmt.Errorf("read file %q: %w", path, err)
	}
	return string(data), nil
}

// resolveFileArg returns fileContent (if filePath is set) or the inline value.
func resolveFileArg(inline, filePath string) (string, error) {
	if filePath != "" {
		content, err := readFile(filePath)
		if err != nil {
			return "", err
		}
		return content, nil
	}
	return inline, nil
}

// splitLabels parses a comma-separated label string or JSON array into []string.
func splitLabels(raw string) []string {
	if raw == "" {
		return nil
	}
	// Try JSON array first: --labels '["Bug","Feature"]'
	if strings.HasPrefix(raw, "[") {
		var labels []string
		if err := json.Unmarshal([]byte(raw), &labels); err == nil {
			return labels
		}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// resolveStateID looks up the state ID for a state name in a team.
func resolveStateID(ctx context.Context, client linear.Linear, teamID, stateName string) (string, error) {
	states, err := client.ListWorkflowStates(ctx, teamID)
	if err != nil {
		return "", fmt.Errorf("list workflow states: %w", err)
	}
	id, ok := states[stateName]
	if !ok {
		return "", fmt.Errorf("state %q not found in team", stateName)
	}
	return id, nil
}

// resolveLabelIDs maps label names to their IDs.
func resolveLabelIDs(ctx context.Context, client linear.Linear, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	allLabels, err := client.ListLabels(ctx)
	if err != nil {
		return nil, fmt.Errorf("list labels: %w", err)
	}
	var ids []string
	for _, name := range names {
		for labelName, id := range allLabels {
			if strings.EqualFold(labelName, name) {
				ids = append(ids, id)
				break
			}
		}
	}
	return ids, nil
}

// getBlockingIssues returns issues that are blocking the given issue (non-Accepted).
func getBlockingIssues(ctx context.Context, client linear.Linear, issueID string) ([]map[string]any, error) {
	relations, err := client.GetIssueRelations(ctx, issueID)
	if err != nil {
		return nil, err
	}
	var blockers []map[string]any
	for _, rel := range relations.InverseRelations {
		if rel.Type != "blocks" {
			continue
		}
		blocker, err := client.GetIssue(ctx, rel.IssueID)
		if err != nil {
			continue // best-effort
		}
		if blocker.State.Name == "Accepted" {
			continue
		}
		blockers = append(blockers, map[string]any{
			"identifier": blocker.Identifier,
			"title":      blocker.Title,
			"status":     blocker.State.Name,
		})
	}
	return blockers, nil
}

// labelNames extracts label names from an Issue's Labels slice.
func labelNames(labels []linear.Label) []string {
	names := make([]string, len(labels))
	for i, l := range labels {
		names[i] = l.Name
	}
	return names
}

// ─── get-issue ────────────────────────────────────────────────────────────────

func newLinearGetIssueCmd(ds func() afclient.DataSource) *cobra.Command {
	return &cobra.Command{
		Use:          "get-issue <id>",
		Short:        "Get issue details",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newLinearClient(ds)
			if err != nil {
				return err
			}
			issue, err := client.GetIssue(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("get issue: %w", err)
			}

			var projectName *string
			if issue.Project.Name != "" {
				s := issue.Project.Name
				projectName = &s
			}

			out := map[string]any{
				"id":          issue.ID,
				"identifier":  issue.Identifier,
				"title":       issue.Title,
				"description": issue.Description,
				"url":         issue.URL,
				"status":      issue.State.Name,
				"team":        issue.Team.Name,
				"project":     projectName,
				"labels":      labelNames(issue.Labels),
				"createdAt":   issue.CreatedAt,
				"updatedAt":   issue.UpdatedAt,
			}
			return writeJSON(cmd.OutOrStdout(), out)
		},
	}
}

// ─── create-issue ─────────────────────────────────────────────────────────────

func newLinearCreateIssueCmd(ds func() afclient.DataSource) *cobra.Command {
	var (
		title           string
		team            string
		description     string
		descriptionFile string
		project         string
		labels          string
		state           string
		parentID        string
	)

	cmd := &cobra.Command{
		Use:   "create-issue",
		Short: "Create a new issue",
		Long: `Create a new issue.

  --team is required unless LINEAR_TEAM_NAME env var is set.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Resolve team
			if team == "" {
				team = os.Getenv("LINEAR_TEAM_NAME")
			}
			if title == "" || team == "" {
				return fmt.Errorf(
					"usage: af linear create-issue --title \"Title\" --team \"Team\" [--description \"...\"] ...\n" +
						"Tip: Set LINEAR_TEAM_NAME env var to provide a default team",
				)
			}

			client, err := newLinearClient(ds)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			desc, err := resolveFileArg(description, descriptionFile)
			if err != nil {
				return err
			}

			// Resolve team
			t, err := client.GetTeamByName(ctx, team)
			if err != nil {
				return fmt.Errorf("resolve team: %w", err)
			}

			input := linear.CreateIssueInput{
				TeamID:      t.ID,
				Title:       title,
				Description: desc,
			}

			// Optional project
			if project != "" {
				proj, err := client.GetProjectByName(ctx, project)
				if err != nil {
					return fmt.Errorf("resolve project: %w", err)
				}
				input.ProjectID = proj.ID
			}

			// Optional state
			if state != "" {
				stateID, err := resolveStateID(ctx, client, t.ID, state)
				if err != nil {
					return fmt.Errorf("resolve state: %w", err)
				}
				input.StateID = stateID
			}

			// Optional labels
			if labels != "" {
				labelList := splitLabels(labels)
				ids, err := resolveLabelIDs(ctx, client, labelList)
				if err != nil {
					return fmt.Errorf("resolve labels: %w", err)
				}
				input.LabelIDs = ids
			}

			// Optional parent
			if parentID != "" {
				parent, err := client.GetIssue(ctx, parentID)
				if err != nil {
					return fmt.Errorf("resolve parent issue: %w", err)
				}
				input.ParentID = parent.ID
			}

			issue, err := client.CreateIssue(ctx, input)
			if err != nil {
				return fmt.Errorf("create issue: %w", err)
			}

			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"id":         issue.ID,
				"identifier": issue.Identifier,
				"title":      issue.Title,
				"url":        issue.URL,
			})
		},
	}

	cmd.Flags().StringVar(&title, "title", "", "Issue title (required)")
	cmd.Flags().StringVar(&team, "team", "", "Team name or key (or set LINEAR_TEAM_NAME)")
	cmd.Flags().StringVar(&description, "description", "", "Issue description")
	cmd.Flags().StringVar(&descriptionFile, "description-file", "", "Path to file containing description")
	cmd.Flags().StringVar(&project, "project", "", "Project name")
	cmd.Flags().StringVar(&labels, "labels", "", "Comma-separated label names (e.g. 'Bug,Feature')")
	cmd.Flags().StringVar(&state, "state", "", "Initial state name (e.g. 'Backlog')")
	cmd.Flags().StringVar(&parentID, "parentId", "", "Parent issue ID or identifier")

	return cmd
}

// ─── update-issue ─────────────────────────────────────────────────────────────

func newLinearUpdateIssueCmd(ds func() afclient.DataSource) *cobra.Command {
	var (
		title           string
		description     string
		descriptionFile string
		state           string
		labels          string
		parentID        string
	)

	cmd := &cobra.Command{
		Use:          "update-issue <id>",
		Short:        "Update an existing issue",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newLinearClient(ds)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			issueID := args[0]

			// Fetch the issue (needed for team ID to resolve state)
			issue, err := client.GetIssue(ctx, issueID)
			if err != nil {
				return fmt.Errorf("get issue: %w", err)
			}

			desc, err := resolveFileArg(description, descriptionFile)
			if err != nil {
				return err
			}

			input := linear.UpdateIssueInput{
				Title:       title,
				Description: desc,
			}

			// Optional state
			if state != "" {
				stateID, err := resolveStateID(ctx, client, issue.Team.ID, state)
				if err != nil {
					return fmt.Errorf("resolve state: %w", err)
				}
				input.StateID = stateID
			}

			// Optional labels
			if labels != "" {
				labelList := splitLabels(labels)
				ids, err := resolveLabelIDs(ctx, client, labelList)
				if err != nil {
					return fmt.Errorf("resolve labels: %w", err)
				}
				input.LabelIDs = ids
			}

			// Optional parentId: "null" clears the parent
			if cmd.Flags().Changed("parentId") {
				if parentID == "null" {
					empty := ""
					input.ParentID = &empty
				} else {
					parent, err := client.GetIssue(ctx, parentID)
					if err != nil {
						return fmt.Errorf("resolve parent issue: %w", err)
					}
					input.ParentID = &parent.ID
				}
			}

			updated, err := client.UpdateIssue(ctx, issue.ID, input)
			if err != nil {
				return fmt.Errorf("update issue: %w", err)
			}

			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"id":         updated.ID,
				"identifier": updated.Identifier,
				"title":      updated.Title,
				"status":     updated.State.Name,
				"url":        updated.URL,
			})
		},
	}

	cmd.Flags().StringVar(&title, "title", "", "New title")
	cmd.Flags().StringVar(&description, "description", "", "New description")
	cmd.Flags().StringVar(&descriptionFile, "description-file", "", "Path to file containing description")
	cmd.Flags().StringVar(&state, "state", "", "New state name (e.g. 'Finished')")
	cmd.Flags().StringVar(&labels, "labels", "", "Comma-separated label names")
	cmd.Flags().StringVar(&parentID, "parentId", "", "New parent issue ID ('null' to clear)")

	return cmd
}

// ─── list-comments ────────────────────────────────────────────────────────────

func newLinearListCommentsCmd(ds func() afclient.DataSource) *cobra.Command {
	return &cobra.Command{
		Use:          "list-comments <issue-id>",
		Short:        "List comments on an issue",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newLinearClient(ds)
			if err != nil {
				return err
			}
			comments, err := client.GetIssueComments(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("list comments: %w", err)
			}

			out := make([]map[string]any, len(comments))
			for i, c := range comments {
				out[i] = map[string]any{
					"id":        c.ID,
					"body":      c.Body,
					"createdAt": c.CreatedAt,
				}
			}
			return writeJSON(cmd.OutOrStdout(), out)
		},
	}
}

// ─── create-comment ───────────────────────────────────────────────────────────

func newLinearCreateCommentCmd(ds func() afclient.DataSource) *cobra.Command {
	var (
		body     string
		bodyFile string
	)

	cmd := &cobra.Command{
		Use:          "create-comment <issue-id>",
		Short:        "Create a comment on an issue",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedBody, err := resolveFileArg(body, bodyFile)
			if err != nil {
				return err
			}
			if resolvedBody == "" {
				return fmt.Errorf("usage: af linear create-comment <issue-id> --body \"Comment text\" or --body-file /path/to/file")
			}

			client, err := newLinearClient(ds)
			if err != nil {
				return err
			}
			comment, err := client.CreateComment(cmd.Context(), args[0], resolvedBody)
			if err != nil {
				return fmt.Errorf("create comment: %w", err)
			}

			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"id":        comment.ID,
				"body":      comment.Body,
				"createdAt": comment.CreatedAt,
			})
		},
	}

	cmd.Flags().StringVar(&body, "body", "", "Comment body text")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "Path to file containing comment body")

	return cmd
}

// ─── add-relation ─────────────────────────────────────────────────────────────

func newLinearAddRelationCmd(ds func() afclient.DataSource) *cobra.Command {
	var relType string

	cmd := &cobra.Command{
		Use:          "add-relation <issue-id> <related-issue-id>",
		Short:        "Create a relation between two issues",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isValidRelationType(relType) {
				return fmt.Errorf("usage: af linear add-relation <issue-id> <related-issue-id> --type <related|blocks|duplicate>")
			}

			client, err := newLinearClient(ds)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			issueID := args[0]
			relatedID := args[1]

			// Resolve identifiers to UUIDs if needed
			issue, err := client.GetIssue(ctx, issueID)
			if err != nil {
				return fmt.Errorf("get issue: %w", err)
			}
			related, err := client.GetIssue(ctx, relatedID)
			if err != nil {
				return fmt.Errorf("get related issue: %w", err)
			}

			relationID, success, err := client.CreateRelation(ctx, issue.ID, related.ID, relType)
			if err != nil {
				return fmt.Errorf("add relation: %w", err)
			}

			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"success":        success,
				"relationId":     relationID,
				"issueId":        issueID,
				"relatedIssueId": relatedID,
				"type":           relType,
			})
		},
	}

	cmd.Flags().StringVar(&relType, "type", "", "Relation type: related, blocks, or duplicate (required)")

	return cmd
}

func isValidRelationType(t string) bool {
	return t == "related" || t == "blocks" || t == "duplicate"
}

// ─── list-relations ───────────────────────────────────────────────────────────

func newLinearListRelationsCmd(ds func() afclient.DataSource) *cobra.Command {
	return &cobra.Command{
		Use:          "list-relations <issue-id>",
		Short:        "List relations for an issue",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newLinearClient(ds)
			if err != nil {
				return err
			}
			result, err := client.GetIssueRelations(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("list relations: %w", err)
			}

			relations := make([]map[string]any, len(result.Relations))
			for i, r := range result.Relations {
				relatedIssue := r.RelatedIssueIdentifier
				if relatedIssue == "" {
					relatedIssue = r.RelatedIssueID
				}
				relations[i] = map[string]any{
					"id":           r.ID,
					"type":         r.Type,
					"relatedIssue": relatedIssue,
					"createdAt":    r.CreatedAt,
				}
			}

			inverseRelations := make([]map[string]any, len(result.InverseRelations))
			for i, r := range result.InverseRelations {
				sourceIssue := r.IssueIdentifier
				if sourceIssue == "" {
					sourceIssue = r.IssueID
				}
				inverseRelations[i] = map[string]any{
					"id":          r.ID,
					"type":        r.Type,
					"sourceIssue": sourceIssue,
					"createdAt":   r.CreatedAt,
				}
			}

			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"issueId":          args[0],
				"relations":        relations,
				"inverseRelations": inverseRelations,
			})
		},
	}
}

// ─── remove-relation ──────────────────────────────────────────────────────────

func newLinearRemoveRelationCmd(ds func() afclient.DataSource) *cobra.Command {
	return &cobra.Command{
		Use:          "remove-relation <relation-id>",
		Short:        "Remove a relation by ID",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newLinearClient(ds)
			if err != nil {
				return err
			}
			if err := client.DeleteRelation(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("remove relation: %w", err)
			}

			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"success":    true,
				"relationId": args[0],
			})
		},
	}
}

// ─── list-sub-issues ──────────────────────────────────────────────────────────

func newLinearListSubIssuesCmd(ds func() afclient.DataSource) *cobra.Command {
	return &cobra.Command{
		Use:          "list-sub-issues <parent-issue-id>",
		Short:        "List sub-issues of a parent issue",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newLinearClient(ds)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			// Resolve parent issue to get its UUID
			parent, err := client.GetIssue(ctx, args[0])
			if err != nil {
				return fmt.Errorf("get parent issue: %w", err)
			}

			children, err := client.ListSubIssues(ctx, parent.ID)
			if err != nil {
				return fmt.Errorf("list sub-issues: %w", err)
			}

			subIssues := make([]map[string]any, len(children))
			for i, c := range children {
				subIssues[i] = map[string]any{
					"id":         c.ID,
					"identifier": c.Identifier,
					"title":      c.Title,
					"status":     c.State.Name,
					"priority":   c.Priority,
					"labels":     labelNames(c.Labels),
					"url":        c.URL,
					"blockedBy":  []any{},
					"blocks":     []any{},
				}
			}

			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"parentId":         parent.ID,
				"parentIdentifier": parent.Identifier,
				"subIssueCount":    len(children),
				"subIssues":        subIssues,
			})
		},
	}
}

// ─── list-sub-issue-statuses ──────────────────────────────────────────────────

var terminalStatuses = map[string]bool{
	"Finished":  true,
	"Delivered": true,
	"Accepted":  true,
	"Canceled":  true,
}

func newLinearListSubIssueStatusesCmd(ds func() afclient.DataSource) *cobra.Command {
	return &cobra.Command{
		Use:          "list-sub-issue-statuses <parent-issue-id>",
		Short:        "List sub-issue statuses (lightweight)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newLinearClient(ds)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			parent, err := client.GetIssue(ctx, args[0])
			if err != nil {
				return fmt.Errorf("get parent issue: %w", err)
			}

			children, err := client.ListSubIssues(ctx, parent.ID)
			if err != nil {
				return fmt.Errorf("list sub-issues: %w", err)
			}

			statuses := make([]map[string]any, len(children))
			for i, c := range children {
				statuses[i] = map[string]any{
					"id":         c.ID,
					"identifier": c.Identifier,
					"title":      c.Title,
					"status":     c.State.Name,
				}
			}

			allDone := true
			var incomplete []map[string]any
			for _, s := range statuses {
				statusName, _ := s["status"].(string)
				if !terminalStatuses[statusName] {
					allDone = false
					incomplete = append(incomplete, s)
				}
			}
			if incomplete == nil {
				incomplete = []map[string]any{}
			}

			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"parentIssue":        args[0],
				"subIssueCount":      len(statuses),
				"subIssues":          statuses,
				"allFinishedOrLater": allDone,
				"incomplete":         incomplete,
			})
		},
	}
}

// ─── update-sub-issue ─────────────────────────────────────────────────────────

func newLinearUpdateSubIssueCmd(ds func() afclient.DataSource) *cobra.Command {
	var (
		state   string
		comment string
	)

	cmd := &cobra.Command{
		Use:          "update-sub-issue <id>",
		Short:        "Update sub-issue status with optional comment",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if state == "" && comment == "" {
				return fmt.Errorf("usage: af linear update-sub-issue <issue-id> --state \"Finished\" [--comment \"...\"]")
			}

			client, err := newLinearClient(ds)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			issue, err := client.GetIssue(ctx, args[0])
			if err != nil {
				return fmt.Errorf("get issue: %w", err)
			}

			if state != "" {
				stateID, err := resolveStateID(ctx, client, issue.Team.ID, state)
				if err != nil {
					return fmt.Errorf("resolve state: %w", err)
				}
				_, err = client.UpdateIssue(ctx, issue.ID, linear.UpdateIssueInput{StateID: stateID})
				if err != nil {
					return fmt.Errorf("update state: %w", err)
				}
			}

			if comment != "" {
				if _, err := client.CreateComment(ctx, issue.ID, comment); err != nil {
					return fmt.Errorf("create comment: %w", err)
				}
			}

			updated, err := client.GetIssue(ctx, issue.ID)
			if err != nil {
				return fmt.Errorf("get updated issue: %w", err)
			}

			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"id":         updated.ID,
				"identifier": updated.Identifier,
				"title":      updated.Title,
				"status":     updated.State.Name,
				"url":        updated.URL,
			})
		},
	}

	cmd.Flags().StringVar(&state, "state", "", "New state name (e.g. 'Finished')")
	cmd.Flags().StringVar(&comment, "comment", "", "Comment to post on the issue")

	return cmd
}

// ─── list-issues ──────────────────────────────────────────────────────────────

func newLinearListIssuesCmd(ds func() afclient.DataSource) *cobra.Command {
	var (
		project  string
		status   string
		label    string
		priority int
		assignee string
		team     string
		limit    int
		orderBy  string
		query    string
	)

	cmd := &cobra.Command{
		Use:          "list-issues",
		Short:        "List issues with flexible filters",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newLinearClient(ds)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			filter := map[string]any{}

			// Project filter
			if project != "" {
				proj, err := client.GetProjectByName(ctx, project)
				if err != nil {
					return fmt.Errorf("resolve project: %w", err)
				}
				filter["project"] = map[string]any{"id": map[string]any{"eq": proj.ID}}
			}

			// Status filter (accepts both --status and --state)
			statusVal := status
			if statusVal == "" {
				statusVal, _ = cmd.Flags().GetString("state")
			}
			if statusVal != "" {
				filter["state"] = map[string]any{"name": map[string]any{"eqIgnoreCase": statusVal}}
			}

			// Label filter
			if label != "" {
				filter["labels"] = map[string]any{"name": map[string]any{"eqIgnoreCase": label}}
			}

			// Priority filter
			if cmd.Flags().Changed("priority") {
				filter["priority"] = map[string]any{"eq": priority}
			}

			// Assignee filter
			if assignee != "" {
				if assignee == "me" {
					viewer, err := client.GetViewer(ctx)
					if err != nil {
						return fmt.Errorf("get viewer: %w", err)
					}
					filter["assignee"] = map[string]any{"id": map[string]any{"eq": viewer.ID}}
				} else {
					user, err := client.GetUserByNameOrEmail(ctx, assignee)
					if err != nil {
						return fmt.Errorf("resolve assignee: %w", err)
					}
					filter["assignee"] = map[string]any{"id": map[string]any{"eq": user.ID}}
				}
			}

			// Team filter
			if team != "" {
				t, err := client.GetTeamByName(ctx, team)
				if err != nil {
					return fmt.Errorf("resolve team: %w", err)
				}
				filter["team"] = map[string]any{"id": map[string]any{"eq": t.ID}}
			}

			// Text search
			if query != "" {
				filter["or"] = []map[string]any{
					{"title": map[string]any{"containsIgnoreCase": query}},
					{"description": map[string]any{"containsIgnoreCase": query}},
				}
			}

			if limit == 0 {
				limit = 50
			}

			gqlOrderBy := "createdAt"
			if orderBy == "updatedAt" {
				gqlOrderBy = "updatedAt"
			}

			issues, err := client.ListIssues(ctx, filter, limit, gqlOrderBy)
			if err != nil {
				return fmt.Errorf("list issues: %w", err)
			}

			// Sort by priority (0 = no priority → goes last, treated as 5)
			sort.Slice(issues, func(i, j int) bool {
				pi := issues[i].Priority
				if pi == 0 {
					pi = 5
				}
				pj := issues[j].Priority
				if pj == 0 {
					pj = 5
				}
				return pi < pj
			})

			out := make([]map[string]any, len(issues))
			for i, iss := range issues {
				var projName any
				if iss.Project.Name != "" {
					projName = iss.Project.Name
				}
				var assigneeName any
				if iss.Assignee != nil {
					assigneeName = iss.Assignee.Name
				}
				out[i] = map[string]any{
					"id":        iss.Identifier,
					"title":     iss.Title,
					"status":    iss.State.Name,
					"priority":  iss.Priority,
					"labels":    labelNames(iss.Labels),
					"project":   projName,
					"assignee":  assigneeName,
					"createdAt": iss.CreatedAt,
				}
			}

			return writeJSON(cmd.OutOrStdout(), out)
		},
	}

	cmd.Flags().StringVar(&project, "project", "", "Filter by project name")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status/state name")
	cmd.Flags().StringVar(&status, "state", "", "Filter by state name (alias for --status)")
	cmd.Flags().StringVar(&label, "label", "", "Filter by label name")
	cmd.Flags().IntVar(&priority, "priority", 0, "Filter by priority (1-4)")
	cmd.Flags().StringVar(&assignee, "assignee", "", "Filter by assignee name, email, or 'me'")
	cmd.Flags().StringVar(&team, "team", "", "Filter by team name")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum number of issues to return")
	cmd.Flags().StringVar(&orderBy, "order-by", "createdAt", "Sort order: createdAt or updatedAt")
	cmd.Flags().StringVar(&query, "query", "", "Text search query")

	return cmd
}

// ─── check-blocked ────────────────────────────────────────────────────────────

func newLinearCheckBlockedCmd(ds func() afclient.DataSource) *cobra.Command {
	return &cobra.Command{
		Use:          "check-blocked <issue-id>",
		Short:        "Check if an issue is blocked",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newLinearClient(ds)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			issue, err := client.GetIssue(ctx, args[0])
			if err != nil {
				return fmt.Errorf("get issue: %w", err)
			}

			blockers, err := getBlockingIssues(ctx, client, issue.ID)
			if err != nil {
				return fmt.Errorf("check blockers: %w", err)
			}
			if blockers == nil {
				blockers = []map[string]any{}
			}

			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"issueId":   args[0],
				"blocked":   len(blockers) > 0,
				"blockedBy": blockers,
			})
		},
	}
}

// ─── list-backlog-issues ──────────────────────────────────────────────────────

func newLinearListBacklogIssuesCmd(ds func() afclient.DataSource) *cobra.Command {
	var project string

	cmd := &cobra.Command{
		Use:          "list-backlog-issues",
		Short:        "List backlog issues for a project",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if project == "" {
				return fmt.Errorf("usage: af linear list-backlog-issues --project \"ProjectName\"")
			}

			client, err := newLinearClient(ds)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			proj, err := client.GetProjectByName(ctx, project)
			if err != nil {
				return fmt.Errorf("resolve project: %w", err)
			}

			issues, err := client.ListBacklogIssues(ctx, proj.ID)
			if err != nil {
				return fmt.Errorf("list backlog issues: %w", err)
			}

			// Sort by priority
			sort.Slice(issues, func(i, j int) bool {
				pi := issues[i].Priority
				if pi == 0 {
					pi = 5
				}
				pj := issues[j].Priority
				if pj == 0 {
					pj = 5
				}
				return pi < pj
			})

			out := make([]map[string]any, len(issues))
			for i, iss := range issues {
				out[i] = map[string]any{
					"id":          iss.ID,
					"identifier":  iss.Identifier,
					"title":       iss.Title,
					"description": iss.Description,
					"url":         iss.URL,
					"priority":    iss.Priority,
					"status":      iss.State.Name,
					"labels":      labelNames(iss.Labels),
				}
			}

			return writeJSON(cmd.OutOrStdout(), out)
		},
	}

	cmd.Flags().StringVar(&project, "project", "", "Project name (required)")

	return cmd
}

// ─── list-unblocked-backlog ───────────────────────────────────────────────────

func newLinearListUnblockedBacklogCmd(ds func() afclient.DataSource) *cobra.Command {
	var project string

	cmd := &cobra.Command{
		Use:          "list-unblocked-backlog",
		Short:        "List unblocked backlog issues for a project",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if project == "" {
				return fmt.Errorf("usage: af linear list-unblocked-backlog --project \"ProjectName\"")
			}

			client, err := newLinearClient(ds)
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			proj, err := client.GetProjectByName(ctx, project)
			if err != nil {
				return fmt.Errorf("resolve project: %w", err)
			}

			issues, err := client.ListBacklogIssues(ctx, proj.ID)
			if err != nil {
				return fmt.Errorf("list backlog issues: %w", err)
			}

			var results []map[string]any
			for _, iss := range issues {
				blockers, err := getBlockingIssues(ctx, client, iss.ID)
				if err != nil {
					blockers = []map[string]any{}
				}
				if blockers == nil {
					blockers = []map[string]any{}
				}
				results = append(results, map[string]any{
					"id":          iss.ID,
					"identifier":  iss.Identifier,
					"title":       iss.Title,
					"description": iss.Description,
					"url":         iss.URL,
					"priority":    iss.Priority,
					"status":      iss.State.Name,
					"labels":      labelNames(iss.Labels),
					"blocked":     len(blockers) > 0,
					"blockedBy":   blockers,
				})
			}

			// Filter to unblocked
			var unblocked []map[string]any
			for _, r := range results {
				if !r["blocked"].(bool) {
					unblocked = append(unblocked, r)
				}
			}

			// Sort by priority
			sort.Slice(unblocked, func(i, j int) bool {
				pi, _ := unblocked[i]["priority"].(int)
				pj, _ := unblocked[j]["priority"].(int)
				if pi == 0 {
					pi = 5
				}
				if pj == 0 {
					pj = 5
				}
				return pi < pj
			})

			if unblocked == nil {
				unblocked = []map[string]any{}
			}

			return writeJSON(cmd.OutOrStdout(), unblocked)
		},
	}

	cmd.Flags().StringVar(&project, "project", "", "Project name (required)")

	return cmd
}

// ─── create-blocker ───────────────────────────────────────────────────────────

func newLinearCreateBlockerCmd(ds func() afclient.DataSource) *cobra.Command {
	var (
		title       string
		description string
		team        string
		project     string
		assignee    string
	)

	cmd := &cobra.Command{
		Use:          "create-blocker <source-issue-id>",
		Short:        "Create a human-needed blocker issue",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if title == "" {
				return fmt.Errorf(
					"usage: af linear create-blocker <source-issue-id> --title \"Title\" " +
						"[--description \"...\"] [--team \"...\"] [--project \"...\"] [--assignee \"user@email.com\"]",
				)
			}

			client, err := newLinearClient(ds)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			sourceIssueID := args[0]

			// 1. Fetch source issue
			sourceIssue, err := client.GetIssue(ctx, sourceIssueID)
			if err != nil {
				return fmt.Errorf("get source issue: %w", err)
			}

			// Resolve team (explicit flag > source issue's team key)
			teamName := team
			if teamName == "" {
				teamName = sourceIssue.Team.Key
				if teamName == "" {
					teamName = sourceIssue.Team.Name
				}
			}
			if teamName == "" {
				return fmt.Errorf("could not resolve team from source issue. Provide --team explicitly")
			}

			// Resolve project name
			projectName := project
			if projectName == "" {
				projectName = sourceIssue.Project.Name
			}

			// 2. Deduplicate: check Icebox + "Needs Human" label
			if projectName != "" {
				proj, projErr := client.GetProjectByName(ctx, projectName)
				if projErr == nil {
					filter := map[string]any{
						"project": map[string]any{"id": map[string]any{"eq": proj.ID}},
						"state":   map[string]any{"name": map[string]any{"eqIgnoreCase": "Icebox"}},
						"labels":  map[string]any{"name": map[string]any{"eqIgnoreCase": "Needs Human"}},
					}
					candidates, _ := client.ListIssues(ctx, filter, 50, "createdAt")
					for _, c := range candidates {
						if strings.EqualFold(c.Title, title) {
							// +1 comment on duplicate
							_, _ = client.CreateComment(ctx, c.ID,
								fmt.Sprintf("+1 — Also needed by %s", sourceIssue.Identifier))
							return writeJSON(cmd.OutOrStdout(), map[string]any{
								"id":           c.ID,
								"identifier":   c.Identifier,
								"title":        c.Title,
								"url":          c.URL,
								"sourceIssue":  sourceIssue.Identifier,
								"relation":     "blocks",
								"deduplicated": true,
							})
						}
					}
				}
			}

			// 3. Resolve team object
			t, err := client.GetTeamByName(ctx, teamName)
			if err != nil {
				return fmt.Errorf("resolve team: %w", err)
			}

			// 4. Build description with source reference
			descParts := []string{}
			if description != "" {
				descParts = append(descParts, description)
			}
			descParts = append(descParts, fmt.Sprintf("\n---\n*Source issue: %s*", sourceIssue.Identifier))
			fullDesc := strings.Join(descParts, "\n\n")

			input := linear.CreateIssueInput{
				TeamID:      t.ID,
				Title:       title,
				Description: fullDesc,
			}

			// State: Icebox
			states, err := client.ListWorkflowStates(ctx, t.ID)
			if err == nil {
				if id, ok := states["Icebox"]; ok {
					input.StateID = id
				}
			}

			// Label: "Needs Human"
			labels, err := client.ListLabels(ctx)
			if err == nil {
				for labelName, id := range labels {
					if strings.EqualFold(labelName, "Needs Human") {
						input.LabelIDs = []string{id}
						break
					}
				}
			}

			// Optional project
			if projectName != "" {
				proj, projErr := client.GetProjectByName(ctx, projectName)
				if projErr == nil {
					input.ProjectID = proj.ID
				}
			}

			// 5. Create blocker issue
			blockerIssue, err := client.CreateIssue(ctx, input)
			if err != nil {
				return fmt.Errorf("create blocker issue: %w", err)
			}

			// 6. Create blocking relation: blocker → source
			_, _, _ = client.CreateRelation(ctx, blockerIssue.ID, sourceIssue.ID, "blocks")

			// 7. Comment on source issue
			_, _ = client.CreateComment(ctx, sourceIssue.ID,
				fmt.Sprintf("🚧 Human blocker created: [%s](%s) — %s",
					blockerIssue.Identifier, blockerIssue.URL, title))

			// 8. Optional: assign
			if assignee != "" {
				user, userErr := client.GetUserByNameOrEmail(ctx, assignee)
				if userErr == nil {
					_, _ = client.UpdateIssue(ctx, blockerIssue.ID, linear.UpdateIssueInput{
						AssigneeID: user.ID,
					})
				}
				// Non-critical: blocker still created without assignee
			}

			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"id":           blockerIssue.ID,
				"identifier":   blockerIssue.Identifier,
				"title":        blockerIssue.Title,
				"url":          blockerIssue.URL,
				"sourceIssue":  sourceIssue.Identifier,
				"relation":     "blocks",
				"deduplicated": false,
			})
		},
	}

	cmd.Flags().StringVar(&title, "title", "", "Blocker issue title (required)")
	cmd.Flags().StringVar(&description, "description", "", "Blocker issue description")
	cmd.Flags().StringVar(&team, "team", "", "Team name or key (defaults to source issue's team)")
	cmd.Flags().StringVar(&project, "project", "", "Project name (defaults to source issue's project)")
	cmd.Flags().StringVar(&assignee, "assignee", "", "Assignee name or email")

	return cmd
}
