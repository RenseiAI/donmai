package afcli

// af logs analyze — port of the legacy TypeScript af-analyze-logs tool.
// Parses agent log files (path or stdin), detects known failure signatures,
// and optionally drafts a Linear issue via the internal/linear package.
//
// Usage:
//
//	af logs analyze [--input <path>] [--config <path>]
//	                [--dry-run] [--json] [--team <team>] [--project <project>]
//
// Signature catalog:
//
//	Defaults are compiled from the TS PATTERN_RULES.
//	Override or extend via ~/.config/af/log-signatures.yaml.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient/logsignatures"
	"github.com/RenseiAI/agentfactory-tui/internal/linear"
)

// ─── top-level logs command ────────────────────────────────────────────────────

func newLogsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "logs",
		Short:        "Agent log operations",
		SilenceUsage: true,
	}
	cmd.AddCommand(newLogsAnalyzeCmd())
	return cmd
}

// ─── analyze subcommand ────────────────────────────────────────────────────────

func newLogsAnalyzeCmd() *cobra.Command {
	var (
		inputPath   string
		configPath  string
		dryRun      bool
		jsonOutput  bool
		teamName    string
		projectName string
	)

	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Analyze agent log files for failure patterns",
		Long: `Analyze agent log files for known failure signatures.

On a match, a Linear issue is drafted with title and description derived from
the detected pattern.  Without --dry-run, the issue is posted via the Linear
API (LINEAR_API_KEY must be set).

Signature catalog:
  Default signatures are built in (ported from the TypeScript reference).
  Override or extend via ~/.config/af/log-signatures.yaml or --config.

Examples:
  # Analyze a log file (human-readable output)
  af logs analyze --input /path/to/agent.log

  # Pipe from stdin
  cat agent.log | af logs analyze

  # Dry-run: print what would be filed without posting
  af logs analyze --input agent.log --dry-run

  # Machine-readable JSON output
  af logs analyze --input agent.log --json

  # Post a Linear issue to a specific team
  af logs analyze --input agent.log --team "Engineering" --project "Agent"`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogsAnalyze(cmd, inputPath, configPath, dryRun, jsonOutput, teamName, projectName)
		},
	}

	cmd.Flags().StringVarP(&inputPath, "input", "i", "", "Path to log file (default: stdin)")
	cmd.Flags().StringVar(&configPath, "config", defaultSignatureCatalogPath(), "Path to YAML signature catalog override")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print what would be filed without posting to Linear")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON for machine consumption")
	cmd.Flags().StringVar(&teamName, "team", os.Getenv("LINEAR_TEAM_NAME"), "Linear team name for issue creation")
	cmd.Flags().StringVar(&projectName, "project", "", "Linear project name for issue creation")

	return cmd
}

// defaultSignatureCatalogPath returns ~/.config/af/log-signatures.yaml.
func defaultSignatureCatalogPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "af", "log-signatures.yaml")
}

// ─── core analysis logic ───────────────────────────────────────────────────────

// AnalysisResult is the top-level result produced by the analyze command.
type AnalysisResult struct {
	LinesScanned  int            `json:"linesScanned"`
	ErrorLines    int            `json:"errorLines"`
	Matches       []PatternMatch `json:"matches"`
	DraftedIssues []DraftedIssue `json:"draftedIssues"`
	AnalyzedAt    time.Time      `json:"analyzedAt"`
}

// PatternMatch records a single matched line.
type PatternMatch struct {
	SignatureID string                    `json:"signatureId"`
	Type        logsignatures.PatternType `json:"type"`
	Severity    logsignatures.Severity    `json:"severity"`
	Title       string                    `json:"title"`
	Occurrences int                       `json:"occurrences"`
	Examples    []string                  `json:"examples"`
}

// DraftedIssue is a Linear issue draft derived from detected patterns.
type DraftedIssue struct {
	Signature   string   `json:"signature"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Labels      []string `json:"labels"`
	// Posted is true when the issue was actually created in Linear.
	Posted     bool   `json:"posted"`
	Identifier string `json:"identifier,omitempty"`
	URL        string `json:"url,omitempty"`
}

// runLogsAnalyze is the implementation of `af logs analyze`.
func runLogsAnalyze(
	cmd *cobra.Command,
	inputPath, configPath string,
	dryRun, jsonOutput bool,
	teamName, projectName string,
) error {
	// ── 1. Load signature catalog ────────────────────────────────────────────
	sigs, err := loadSignatures(configPath)
	if err != nil {
		return err
	}

	// ── 2. Open input ────────────────────────────────────────────────────────
	reader, err := openInput(inputPath)
	if err != nil {
		return err
	}
	if closer, ok := reader.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}

	// ── 3. Scan lines ────────────────────────────────────────────────────────
	matchMap := map[string]*PatternMatch{} // signatureID → match
	var linesScanned, errorLines int

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		linesScanned++

		if !isErrorLine(line) {
			continue
		}
		errorLines++

		mr := logsignatures.Match(line, sigs)
		if mr == nil {
			continue
		}

		pm, exists := matchMap[mr.Signature.ID]
		if !exists {
			pm = &PatternMatch{
				SignatureID: mr.Signature.ID,
				Type:        mr.Signature.Type,
				Severity:    mr.Signature.Severity,
				Title:       mr.Signature.Title,
				Occurrences: 0,
				Examples:    []string{},
			}
			matchMap[mr.Signature.ID] = pm
		}
		pm.Occurrences++
		if len(pm.Examples) < 3 {
			excerpt := line
			if len(excerpt) > 200 {
				excerpt = excerpt[:200]
			}
			pm.Examples = append(pm.Examples, excerpt)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan input: %w", err)
	}

	// ── 4. Build flat matches slice ──────────────────────────────────────────
	var matches []PatternMatch
	for _, pm := range matchMap {
		matches = append(matches, *pm)
	}

	// ── 5. Draft Linear issues ───────────────────────────────────────────────
	drafts := buildDrafts(matches)

	// ── 6. Post to Linear (unless dry-run) ──────────────────────────────────
	if !dryRun && len(drafts) > 0 {
		if err := postDrafts(cmd.Context(), drafts, teamName, projectName); err != nil {
			// Non-fatal: we still print results, just warn.
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to post Linear issues: %v\n", err)
		}
	}

	result := AnalysisResult{
		LinesScanned:  linesScanned,
		ErrorLines:    errorLines,
		Matches:       matches,
		DraftedIssues: drafts,
		AnalyzedAt:    time.Now().UTC(),
	}

	// ── 7. Output ────────────────────────────────────────────────────────────
	if jsonOutput {
		return writeJSON(cmd.OutOrStdout(), result)
	}
	printHumanResult(cmd.OutOrStdout(), result, dryRun)
	return nil
}

// loadSignatures loads the signature catalog, falling back to defaults when the
// override file does not exist.
func loadSignatures(configPath string) ([]logsignatures.Signature, error) {
	if configPath == "" {
		return logsignatures.DefaultSignatures(), nil
	}
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return logsignatures.DefaultSignatures(), nil
	}
	sigs, err := logsignatures.LoadCatalog(configPath)
	if err != nil {
		return nil, fmt.Errorf("load signature catalog: %w", err)
	}
	return sigs, nil
}

// openInput opens the log file at path, or returns os.Stdin when path is "".
func openInput(path string) (io.Reader, error) {
	if path == "" {
		return os.Stdin, nil
	}
	f, err := os.Open(path) //nolint:gosec // G304 -- user-supplied via CLI flag
	if err != nil {
		return nil, fmt.Errorf("open log file %q: %w", path, err)
	}
	return f, nil
}

// isErrorLine heuristically determines whether a log line is an error.
// It matches common error indicators from agent logs.
func isErrorLine(line string) bool {
	lower := strings.ToLower(line)
	return strings.Contains(lower, "error") ||
		strings.Contains(lower, "failed") ||
		strings.Contains(lower, "denied") ||
		strings.Contains(lower, "timeout") ||
		strings.Contains(lower, "enoent") ||
		strings.Contains(lower, "eacces") ||
		strings.Contains(lower, "econnrefused") ||
		strings.Contains(lower, "enotfound") ||
		strings.Contains(lower, "etimedout") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "permission") ||
		strings.Contains(lower, "sandbox") ||
		strings.Contains(lower, "blocked") ||
		strings.Contains(lower, "approval") ||
		strings.Contains(lower, "<tool_use_error>") ||
		strings.Contains(lower, "worktree")
}

// ─── issue drafting ───────────────────────────────────────────────────────────

// buildDrafts converts pattern matches into Linear issue drafts, grouping by
// pattern type and applying the same priority rules as the TS reference.
func buildDrafts(matches []PatternMatch) []DraftedIssue {
	// Group matches by PatternType.
	byType := map[logsignatures.PatternType][]PatternMatch{}
	for _, m := range matches {
		byType[m.Type] = append(byType[m.Type], m)
	}

	var drafts []DraftedIssue
	for pt, typeMatches := range byType {
		totalOccurrences := 0
		for _, m := range typeMatches {
			totalOccurrences += m.Occurrences
		}

		hasHighSeverity := false
		for _, m := range typeMatches {
			if m.Severity == logsignatures.SeverityHigh || m.Severity == logsignatures.SeverityCritical {
				hasHighSeverity = true
				break
			}
		}

		hasMediumWithOccurrences := totalOccurrences >= 2
		hasMultiplePatterns := len(typeMatches) >= 2

		if !hasHighSeverity && !hasMediumWithOccurrences && !hasMultiplePatterns {
			continue
		}

		// Prefer highest-severity match as primary.
		primary := typeMatches[0]
		for _, m := range typeMatches {
			if severityRank(m.Severity) > severityRank(primary.Severity) {
				primary = m
			}
		}

		categoryPrefix, labels := categoryForType(pt)
		title := fmt.Sprintf("%s %s", categoryPrefix, primary.Title)
		sig := logsignatures.GenerateSignatureHash(pt, primary.Title)
		desc := buildDescription(primary, pt, totalOccurrences)

		drafts = append(drafts, DraftedIssue{
			Signature:   sig,
			Title:       title,
			Description: desc,
			Labels:      labels,
		})
	}
	return drafts
}

func severityRank(s logsignatures.Severity) int {
	switch s {
	case logsignatures.SeverityCritical:
		return 4
	case logsignatures.SeverityHigh:
		return 3
	case logsignatures.SeverityMedium:
		return 2
	default:
		return 1
	}
}

func categoryForType(pt logsignatures.PatternType) (prefix string, labels []string) {
	switch pt {
	case logsignatures.PatternToolMisuse:
		return "[Agent Behavior]", []string{"Agent", "Tool Usage"}
	case logsignatures.PatternApprovalRequired:
		return "[Agent Permissions]", []string{"Agent", "Permissions"}
	case logsignatures.PatternPermission:
		return "[Agent Environment]", []string{"Agent", "Sandbox"}
	default:
		return "[Agent Environment]", []string{"Agent", "Infrastructure"}
	}
}

func buildDescription(pm PatternMatch, pt logsignatures.PatternType, totalOccurrences int) string {
	var sb strings.Builder
	sb.WriteString("## Summary\n")
	sb.WriteString(fmt.Sprintf("Detected %s issue: %s\n\n", pt, pm.Title))
	sb.WriteString(fmt.Sprintf("**Occurrences:** %d\n", totalOccurrences))
	sb.WriteString(fmt.Sprintf("**Severity:** %s\n\n", pm.Severity))
	sb.WriteString("## Examples\n")
	for _, ex := range pm.Examples {
		sb.WriteString(fmt.Sprintf("```\n%s\n```\n", ex))
	}
	sb.WriteString("\n## Analysis\n")
	sb.WriteString("This issue was detected by the automated log analyzer.\n")
	sb.WriteString(fmt.Sprintf("Pattern type: %s\n", pt))
	return sb.String()
}

// ─── Linear posting ────────────────────────────────────────────────────────────

// postDrafts creates Linear issues for each draft.
// Operates directly on the internal/linear package.
func postDrafts(ctx context.Context, drafts []DraftedIssue, teamName, projectName string) error {
	key := apiKey()
	if key == "" {
		return fmt.Errorf(
			"LINEAR_API_KEY environment variable is required for issue creation; use --dry-run to skip posting",
		)
	}
	if teamName == "" {
		return fmt.Errorf(
			"--team (or LINEAR_TEAM_NAME env var) is required for issue creation; use --dry-run to skip posting",
		)
	}

	client, err := linear.NewClient(key)
	if err != nil {
		return fmt.Errorf("create linear client: %w", err)
	}
	if logsTestBaseURL != "" {
		client.BaseURL = logsTestBaseURL
	}

	team, err := client.GetTeamByName(ctx, teamName)
	if err != nil {
		return fmt.Errorf("resolve team %q: %w", teamName, err)
	}

	var projectID string
	if projectName != "" {
		proj, err := client.GetProjectByName(ctx, projectName)
		if err != nil {
			return fmt.Errorf("resolve project %q: %w", projectName, err)
		}
		projectID = proj.ID
	}

	// Resolve label IDs.
	allLabels, _ := client.ListLabels(ctx)

	for i := range drafts {
		input := linear.CreateIssueInput{
			TeamID:      team.ID,
			Title:       drafts[i].Title,
			Description: drafts[i].Description,
			ProjectID:   projectID,
		}

		// Map label names → IDs.
		var labelIDs []string
		for _, name := range drafts[i].Labels {
			for lName, id := range allLabels {
				if strings.EqualFold(lName, name) {
					labelIDs = append(labelIDs, id)
					break
				}
			}
		}
		input.LabelIDs = labelIDs

		issue, err := client.CreateIssue(ctx, input)
		if err != nil {
			return fmt.Errorf("create issue %q: %w", drafts[i].Title, err)
		}
		if issue != nil {
			drafts[i].Posted = true
			drafts[i].Identifier = issue.Identifier
			drafts[i].URL = issue.URL
		}
	}
	return nil
}

// ─── human-readable output ────────────────────────────────────────────────────

func printHumanResult(w io.Writer, r AnalysisResult, dryRun bool) {
	_, _ = fmt.Fprintf(w, "\n=== AgentFactory Log Analyzer ===\n\n")
	if dryRun {
		_, _ = fmt.Fprintf(w, "[DRY RUN MODE — No issues will be created]\n\n")
	}
	_, _ = fmt.Fprintf(w, "Lines scanned:   %d\n", r.LinesScanned)
	_, _ = fmt.Fprintf(w, "Error lines:     %d\n", r.ErrorLines)
	_, _ = fmt.Fprintf(w, "Pattern matches: %d\n\n", len(r.Matches))

	if len(r.Matches) == 0 {
		_, _ = fmt.Fprintf(w, "No failure patterns detected.\n")
		return
	}

	_, _ = fmt.Fprintf(w, "Detected Patterns:\n")
	for _, m := range r.Matches {
		_, _ = fmt.Fprintf(w, "  [%s] %s (occurrences: %d)\n", m.Severity, m.Title, m.Occurrences)
	}

	if len(r.DraftedIssues) > 0 {
		_, _ = fmt.Fprintf(w, "\nDrafted Issues:\n")
		for _, d := range r.DraftedIssues {
			switch {
			case d.Posted:
				_, _ = fmt.Fprintf(w, "  [CREATED] %s — %s\n", d.Identifier, d.Title)
			case dryRun:
				_, _ = fmt.Fprintf(w, "  [WOULD CREATE] %s\n", d.Title)
			default:
				_, _ = fmt.Fprintf(w, "  [DRAFT] %s\n", d.Title)
			}
		}
	}

	_, _ = fmt.Fprintf(w, "\nAnalyzed at: %s\n", r.AnalyzedAt.Format(time.RFC3339))
}

// logsTestBaseURL, when non-empty, overrides the Linear API base URL in logs
// tests. Mirrors linearTestBaseURL in linear_test_hook.go.
var logsTestBaseURL string
