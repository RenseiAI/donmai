package linearcmd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afcli/internal/cli"
	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/internal/linear"
)

type nativeLabelScope struct {
	teamID  string // empty only for an explicitly selected workspace scope
	teamKey string
}

func requireNativeLabelManager(ctx context.Context, client linear.Linear) (linear.LabelManager, error) {
	manager, ok := client.(linear.LabelManager)
	if !ok {
		return nil, fmt.Errorf("native label groups are unavailable from this Linear client")
	}
	// The proxy capability probe is an authenticated, non-provider OPTIONS
	// request. Every group-flow read/write subsequently carries that policy.
	if concrete, ok := client.(*linear.Client); ok && concrete.ProxyMode {
		if err := concrete.EnableNoFallbackLabelProxy(ctx); err != nil {
			return nil, fmt.Errorf("native label-group proxy preflight: %w", err)
		}
	}
	return manager, nil
}

func resolveNativeLabelScope(ctx context.Context, client linear.Linear, workspace bool, teamRef string) (nativeLabelScope, error) {
	teamRef = strings.TrimSpace(teamRef)
	if workspace == (teamRef != "") {
		return nativeLabelScope{}, fmt.Errorf("choose exactly one scope: --workspace or --team <key|uuid>")
	}
	if workspace {
		return nativeLabelScope{}, nil
	}
	team, err := client.GetTeamByName(ctx, teamRef)
	if err != nil {
		return nativeLabelScope{}, fmt.Errorf("resolve explicit label team %q: %w", teamRef, err)
	}
	if team == nil || team.ID == "" || team.Key == "" ||
		(!strings.EqualFold(teamRef, team.Key) && !strings.EqualFold(teamRef, team.ID)) {
		return nativeLabelScope{}, fmt.Errorf("team scope %q is not an accessible canonical team key or UUID", teamRef)
	}
	return nativeLabelScope{teamID: team.ID, teamKey: team.Key}, nil
}

func listNativeScope(ctx context.Context, manager linear.LabelManager, scope nativeLabelScope) ([]linear.LabelInfo, error) {
	if scope.teamID == "" {
		return manager.ListLabelDetails(ctx)
	}
	return manager.ListLabelDetailsForTeam(ctx, scope.teamKey)
}

func inExactLabelScope(label linear.LabelInfo, scope nativeLabelScope) bool {
	return label.TeamID == scope.teamID
}

func nativeLabelByID(labels []linear.LabelInfo, id string) (linear.LabelInfo, bool) {
	for _, label := range labels {
		if label.ID == id {
			return label, true
		}
	}
	return linear.LabelInfo{}, false
}

func requireSingleSelectGroup(group linear.LabelInfo) error {
	if !group.IsGroup || group.ParentID != "" {
		return fmt.Errorf("label %q (%s) is not a native label group", group.Name, group.ID)
	}
	// Linear IssueLabel.groupType documents null for regular labels and the
	// single-select default for older groups without an explicit type. The
	// isGroup guard above is therefore essential before accepting empty here.
	if group.GroupType != "" && group.GroupType != "singleSelect" {
		return fmt.Errorf("label group %q (%s) is %s; this command requires native singleSelect", group.Name, group.ID, group.GroupType)
	}
	return nil
}

func readBackNativeLabel(ctx context.Context, manager linear.LabelManager, scope nativeLabelScope, id string) (linear.LabelInfo, error) {
	labels, err := listNativeScope(ctx, manager, scope)
	if err != nil {
		return linear.LabelInfo{}, fmt.Errorf("read back native label: %w", err)
	}
	label, found := nativeLabelByID(labels, id)
	if !found || !inExactLabelScope(label, scope) {
		return linear.LabelInfo{}, fmt.Errorf("native label %s did not read back in its requested scope", id)
	}
	return label, nil
}

func nativeLabelJSON(label linear.LabelInfo) map[string]any {
	var teamID, teamKey, groupType, parentID, parentName any
	if label.TeamID != "" {
		teamID, teamKey = label.TeamID, label.TeamKey
	}
	if label.GroupType != "" {
		groupType = label.GroupType
	}
	if label.ParentID != "" {
		parentID, parentName = label.ParentID, label.ParentName
	}
	scope := "workspace"
	if label.TeamID != "" {
		scope = "team"
	}
	return map[string]any{
		"id": label.ID, "name": label.Name, "scope": scope,
		"teamId": teamID, "teamKey": teamKey, "isGroup": label.IsGroup,
		"groupType": groupType, "parentId": parentID, "parentName": parentName,
	}
}

func newLinearCreateLabelGroupCmd(ds func() afclient.DataSource, bin string) *cobra.Command {
	var name, team string
	var workspace bool
	cmd := &cobra.Command{
		Use: "create-label-group", Short: "Create or reuse a native single-select label group",
		Long:         "Create or reuse a native group in an explicit scope. A colon in a flat label name never makes it a group.\n\nExample: " + bin + " linear create-label-group --name Type --workspace",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("--name is required")
			}
			client, err := newLinearClient(ds, bin)
			if err != nil {
				return err
			}
			manager, err := requireNativeLabelManager(cmd.Context(), client)
			if err != nil {
				return err
			}
			scope, err := resolveNativeLabelScope(cmd.Context(), client, workspace, team)
			if err != nil {
				return err
			}
			catalog, err := listNativeScope(cmd.Context(), manager, scope)
			if err != nil {
				return fmt.Errorf("list native labels before group create: %w", err)
			}
			matching := make([]linear.LabelInfo, 0)
			for _, label := range catalog {
				if inExactLabelScope(label, scope) && strings.EqualFold(label.Name, strings.TrimSpace(name)) {
					matching = append(matching, label)
				}
			}
			if len(matching) > 1 {
				return fmt.Errorf("group name %q is ambiguous in the selected scope; use stable IDs", name)
			}
			if len(matching) == 1 {
				if err := requireSingleSelectGroup(matching[0]); err != nil {
					return err
				}
				return cli.WriteJSON(cmd.OutOrStdout(), map[string]any{"label": nativeLabelJSON(matching[0]), "reused": true})
			}
			created, createErr := manager.CreateNativeLabel(cmd.Context(), linear.NativeLabelCreateInput{
				Name: strings.TrimSpace(name), TeamID: scope.teamID, IsGroup: true, GroupType: "singleSelect",
			})
			if createErr != nil {
				if errors.Is(createErr, linear.ErrUnauthorized) || errors.Is(createErr, linear.ErrForbidden) {
					return fmt.Errorf("create native label group: %w", createErr)
				}
				// One concurrent/manual create may have won. Re-read; never retry
				// the mutation after an ambiguous transport result.
				catalog, err = listNativeScope(cmd.Context(), manager, scope)
				if err == nil {
					matching = matching[:0]
					for _, label := range catalog {
						if inExactLabelScope(label, scope) && strings.EqualFold(label.Name, strings.TrimSpace(name)) {
							matching = append(matching, label)
						}
					}
					if len(matching) > 1 {
						return fmt.Errorf("group name %q became ambiguous after create refusal", name)
					}
					if len(matching) == 1 {
						if err := requireSingleSelectGroup(matching[0]); err != nil {
							return err
						}
						return cli.WriteJSON(cmd.OutOrStdout(), map[string]any{"label": nativeLabelJSON(matching[0]), "reused": true})
					}
				}
				return fmt.Errorf("create native label group: %w", createErr)
			}
			verified, err := readBackNativeLabel(cmd.Context(), manager, scope, created.ID)
			if err != nil {
				return err
			}
			if !strings.EqualFold(verified.Name, strings.TrimSpace(name)) || requireSingleSelectGroup(verified) != nil {
				return fmt.Errorf("created label group %s did not read back as the requested single-select group", created.ID)
			}
			return cli.WriteJSON(cmd.OutOrStdout(), map[string]any{"label": nativeLabelJSON(verified), "reused": false})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Native group name")
	cmd.Flags().BoolVar(&workspace, "workspace", false, "Create/reuse at workspace scope")
	cmd.Flags().StringVar(&team, "team", "", "Create/reuse in the specified team key or UUID")
	return cmd
}

func newLinearCreateGroupLabelCmd(ds func() afclient.DataSource, bin string) *cobra.Command {
	var name, groupID, team string
	var workspace bool
	cmd := &cobra.Command{
		Use: "create-group-label", Short: "Create or reuse a child in a native label group",
		Long:         "The group is selected by stable ID and must be in the explicit scope. A flat colon-prefixed label is never reused as a child.\n\nExample: " + bin + " linear create-group-label --group-id <uuid> --name Bug --workspace",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(name) == "" || strings.TrimSpace(groupID) == "" {
				return fmt.Errorf("--name and --group-id are required")
			}
			client, err := newLinearClient(ds, bin)
			if err != nil {
				return err
			}
			manager, err := requireNativeLabelManager(cmd.Context(), client)
			if err != nil {
				return err
			}
			scope, err := resolveNativeLabelScope(cmd.Context(), client, workspace, team)
			if err != nil {
				return err
			}
			catalog, err := listNativeScope(cmd.Context(), manager, scope)
			if err != nil {
				return fmt.Errorf("list native labels before child create: %w", err)
			}
			group, found := nativeLabelByID(catalog, strings.TrimSpace(groupID))
			if !found || !inExactLabelScope(group, scope) {
				return fmt.Errorf("group id %q is not visible in the selected scope", groupID)
			}
			if err := requireSingleSelectGroup(group); err != nil {
				return err
			}
			var existing *linear.LabelInfo
			for _, label := range catalog {
				if !inExactLabelScope(label, scope) || !strings.EqualFold(label.Name, strings.TrimSpace(name)) {
					continue
				}
				if label.ParentID != group.ID {
					return fmt.Errorf("label %q already exists in this scope outside group %s; use reparent-label with its stable ID", name, group.ID)
				}
				if existing != nil {
					return fmt.Errorf("child label %q is ambiguous in group %s", name, group.ID)
				}
				selectedLabel := label
				existing = &selectedLabel
			}
			if existing != nil {
				return cli.WriteJSON(cmd.OutOrStdout(), map[string]any{"label": nativeLabelJSON(*existing), "reused": true})
			}
			created, createErr := manager.CreateNativeLabel(cmd.Context(), linear.NativeLabelCreateInput{
				Name: strings.TrimSpace(name), TeamID: scope.teamID, ParentID: group.ID,
			})
			if createErr != nil {
				if errors.Is(createErr, linear.ErrUnauthorized) || errors.Is(createErr, linear.ErrForbidden) {
					return fmt.Errorf("create group child: %w", createErr)
				}
				catalog, err = listNativeScope(cmd.Context(), manager, scope)
				if err == nil {
					var winner *linear.LabelInfo
					for _, label := range catalog {
						if inExactLabelScope(label, scope) && strings.EqualFold(label.Name, strings.TrimSpace(name)) {
							if label.ParentID != group.ID {
								return fmt.Errorf("label %q appeared outside group %s after create refusal", name, group.ID)
							}
							if winner != nil {
								return fmt.Errorf("child label %q became ambiguous after create refusal", name)
							}
							selectedLabel := label
							winner = &selectedLabel
						}
					}
					if winner != nil {
						return cli.WriteJSON(cmd.OutOrStdout(), map[string]any{"label": nativeLabelJSON(*winner), "reused": true})
					}
				}
				return fmt.Errorf("create group child: %w", createErr)
			}
			verified, err := readBackNativeLabel(cmd.Context(), manager, scope, created.ID)
			if err != nil {
				return err
			}
			if verified.IsGroup || verified.ParentID != group.ID || !strings.EqualFold(verified.Name, strings.TrimSpace(name)) {
				return fmt.Errorf("created child %s did not read back in group %s", created.ID, group.ID)
			}
			return cli.WriteJSON(cmd.OutOrStdout(), map[string]any{"label": nativeLabelJSON(verified), "reused": false})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Child label name")
	cmd.Flags().StringVar(&groupID, "group-id", "", "Existing native group UUID")
	cmd.Flags().BoolVar(&workspace, "workspace", false, "Create/reuse at workspace scope")
	cmd.Flags().StringVar(&team, "team", "", "Create/reuse in the specified team key or UUID")
	return cmd
}

func newLinearReparentLabelCmd(ds func() afclient.DataSource, bin string) *cobra.Command {
	var labelID, groupID, team string
	var workspace bool
	cmd := &cobra.Command{
		Use: "reparent-label", Short: "Move an existing label into a native single-select group without changing its ID",
		Long:         "Both stable IDs must be in the explicit scope; no label is created or deleted.\n\nExample: " + bin + " linear reparent-label --label-id <uuid> --group-id <uuid> --team ENG",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(labelID) == "" || strings.TrimSpace(groupID) == "" {
				return fmt.Errorf("--label-id and --group-id are required")
			}
			client, err := newLinearClient(ds, bin)
			if err != nil {
				return err
			}
			manager, err := requireNativeLabelManager(cmd.Context(), client)
			if err != nil {
				return err
			}
			scope, err := resolveNativeLabelScope(cmd.Context(), client, workspace, team)
			if err != nil {
				return err
			}
			catalog, err := listNativeScope(cmd.Context(), manager, scope)
			if err != nil {
				return fmt.Errorf("list native labels before reparent: %w", err)
			}
			label, found := nativeLabelByID(catalog, strings.TrimSpace(labelID))
			if !found || !inExactLabelScope(label, scope) {
				return fmt.Errorf("label id %q is not visible in the selected scope", labelID)
			}
			if label.IsGroup {
				return fmt.Errorf("label %q is a group and cannot be nested", label.Name)
			}
			group, found := nativeLabelByID(catalog, strings.TrimSpace(groupID))
			if !found || !inExactLabelScope(group, scope) {
				return fmt.Errorf("group id %q is not visible in the selected scope", groupID)
			}
			if err := requireSingleSelectGroup(group); err != nil {
				return err
			}
			if label.ParentID == group.ID {
				return cli.WriteJSON(cmd.OutOrStdout(), map[string]any{"label": nativeLabelJSON(label), "reused": true})
			}
			updated, err := manager.ReparentLabel(cmd.Context(), label.ID, group.ID)
			if err != nil {
				return fmt.Errorf("reparent label: %w", err)
			}
			if updated.ID != label.ID {
				return fmt.Errorf("reparent changed label identity from %s to %s", label.ID, updated.ID)
			}
			verified, err := readBackNativeLabel(cmd.Context(), manager, scope, label.ID)
			if err != nil {
				return err
			}
			if verified.ParentID != group.ID || verified.IsGroup {
				return fmt.Errorf("label %s did not read back as child of group %s", label.ID, group.ID)
			}
			return cli.WriteJSON(cmd.OutOrStdout(), map[string]any{"label": nativeLabelJSON(verified), "reused": false})
		},
	}
	cmd.Flags().StringVar(&labelID, "label-id", "", "Existing label UUID to move")
	cmd.Flags().StringVar(&groupID, "group-id", "", "Destination native group UUID")
	cmd.Flags().BoolVar(&workspace, "workspace", false, "Require both labels at workspace scope")
	cmd.Flags().StringVar(&team, "team", "", "Require both labels in the specified team key or UUID")
	return cmd
}

func newLinearSelectGroupLabelCmd(ds func() afclient.DataSource, bin string) *cobra.Command {
	var labelID string
	cmd := &cobra.Command{
		Use: "select-group-label <issue-id>", Short: "Select one child of a native single-select label group",
		Long: "Use the child label's stable ID. Linear atomically replaces only the prior child in that group; this command reads back membership and refuses a surprising result.\n\nExample: " + bin + " linear select-group-label ENG-1 --label-id <uuid>",
		Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(labelID) == "" {
				return fmt.Errorf("--label-id is required")
			}
			client, err := newLinearClient(ds, bin)
			if err != nil {
				return err
			}
			manager, err := requireNativeLabelManager(cmd.Context(), client)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			issue, err := client.GetIssue(ctx, args[0])
			if err != nil {
				return fmt.Errorf("get issue: %w", err)
			}
			if issue == nil || issue.Team.ID == "" || issue.Team.Key == "" {
				return fmt.Errorf("issue team is required for grouped label selection")
			}
			catalog, err := manager.ListLabelDetailsForTeam(ctx, issue.Team.Key)
			if err != nil {
				return fmt.Errorf("list issue-team labels: %w", err)
			}
			target, found := nativeLabelByID(catalog, strings.TrimSpace(labelID))
			if !found || target.IsGroup || target.ParentID == "" || (target.TeamID != "" && target.TeamID != issue.Team.ID) {
				return fmt.Errorf("label id %q is not an applicable native group child for team %s", labelID, issue.Team.Key)
			}
			group, found := nativeLabelByID(catalog, target.ParentID)
			if !found || group.TeamID != target.TeamID {
				return fmt.Errorf("group %s is not visible in the child's scope", target.ParentID)
			}
			if err := requireSingleSelectGroup(group); err != nil {
				return err
			}
			priorLabels, err := manager.ListIssueLabels(ctx, issue.ID)
			if err != nil {
				return fmt.Errorf("list complete prior issue labels: %w", err)
			}
			prior := make(map[string]linear.Label, len(priorLabels))
			priorSiblings := make([]string, 0)
			alreadyApplied := false
			for _, label := range priorLabels {
				prior[label.ID] = label
				if label.ID == target.ID {
					alreadyApplied = true
				}
				if label.ParentID == group.ID && label.ID != target.ID {
					priorSiblings = append(priorSiblings, label.ID)
				}
			}
			if !alreadyApplied {
				if _, err := client.AddIssueLabel(ctx, issue.ID, target.ID); err != nil {
					return fmt.Errorf("select grouped label: %w", err)
				}
			}
			updated, err := client.GetIssue(ctx, issue.ID)
			if err != nil {
				return fmt.Errorf("read back grouped selection: %w", err)
			}
			if updated == nil || updated.ID != issue.ID {
				return fmt.Errorf("grouped selection readback did not return the same issue")
			}
			updatedLabels, err := manager.ListIssueLabels(ctx, issue.ID)
			if err != nil {
				return fmt.Errorf("list complete updated issue labels: %w", err)
			}
			present := make(map[string]bool, len(updatedLabels))
			for _, label := range updatedLabels {
				present[label.ID] = true
			}
			if !present[target.ID] {
				return fmt.Errorf("grouped label %s did not read back on issue", target.ID)
			}
			for _, siblingID := range priorSiblings {
				if present[siblingID] {
					return fmt.Errorf("prior group selection %s remains after selecting %s", siblingID, target.ID)
				}
			}
			for id, label := range prior {
				if id == target.ID {
					continue
				}
				if label.ParentID == group.ID {
					continue
				}
				if !present[id] {
					return fmt.Errorf("unrelated issue label %s was lost during group selection", id)
				}
			}
			sort.Strings(priorSiblings)
			return cli.WriteJSON(cmd.OutOrStdout(), map[string]any{
				"id": updated.ID, "identifier": updated.Identifier, "appliedLabelId": target.ID,
				"appliedLabel": target.Name, "groupId": group.ID, "replacedLabelIds": priorSiblings,
				"alreadyApplied": alreadyApplied, "labels": labelNames(updatedLabels),
			})
		},
	}
	cmd.Flags().StringVar(&labelID, "label-id", "", "Exact child label UUID; name-only selection is intentionally unavailable")
	return cmd
}
