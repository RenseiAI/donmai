package afcli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient/codeintel"
)

// newArchCmd constructs the `af arch` command tree.
//
// Architecture: shell-out bridge to `pnpm af-arch` (TS implementation).
// See afclient/codeintel/runner.go for the full rationale.
func newArchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "arch",
		Short: "Architectural intelligence — drift detection for PRs and commits",
		Long: `Architectural intelligence commands powered by @renseiai/architectural-intelligence.

Detects deviations between a PR/commit and the stored architectural baseline.
All commands output JSON to stdout by default.

Exit codes (assess subcommand):
  0  Clean — no deviations or gate not triggered
  1  Gated — threshold exceeded per policy
  2  Error — invalid args, network failure, parse error

Environment:
  ANTHROPIC_API_KEY     Enables live LLM drift assessment (required for real detection)
  RENSEI_DRIFT_GATE     Gate policy: none | no-severity-high | zero-deviations | max:N
  RENSEI_ARCH_DB        SQLite DB path (default: .agentfactory/arch-intelligence/db.sqlite)

Binary resolution (in order):
  1. AGENTFACTORY_ARCH_BIN env var (explicit override)
  2. af-arch on PATH (npm install -g @renseiai/agentfactory-cli)
  3. pnpm af-arch (monorepo dev)`,
		SilenceUsage: true,
	}

	cmd.AddCommand(newArchAssessCmd())

	return cmd
}

// newArchAssessCmd constructs `af arch assess`.
func newArchAssessCmd() *cobra.Command {
	var (
		repository string
		prNumber   int
		gatePolicy string
		scopeLevel string
		projectID  string
		db         string
		summary    bool
	)

	cmd := &cobra.Command{
		Use:   "assess [pr-url]",
		Short: "Assess a PR or commit for architectural drift",
		Long: `Runs a drift assessment against the stored architectural baseline.

Provide either a full GitHub PR URL as a positional argument, or use
--repository + --pr to specify the PR explicitly.

Gate policy controls the exit code:
  none               Never return exit code 1
  no-severity-high   Block on high-severity deviations (default)
  zero-deviations    Block on any deviation
  max:N              Block when total deviations > N

Without ANTHROPIC_API_KEY, the CLI uses a stub adapter that returns an empty
DriftReport with a notice — useful for testing the pipeline without API credits.

Examples:
  af arch assess https://github.com/org/repo/pull/123
  af arch assess --repository github.com/org/repo --pr 123
  af arch assess https://github.com/org/repo/pull/123 --gate-policy zero-deviations
  af arch assess https://github.com/org/repo/pull/123 --summary`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			r := codeintel.New(cwd())
			if !r.IsArchAvailable() {
				return fmt.Errorf("%w", codeintel.ErrArchNotAvailable)
			}

			opts := codeintel.ArchAssessOptions{
				Repository: repository,
				PrNumber:   prNumber,
				GatePolicy: gatePolicy,
				ScopeLevel: scopeLevel,
				ProjectID:  projectID,
				DB:         db,
				Summary:    summary,
			}
			if len(args) == 1 {
				opts.PrURL = args[0]
			}

			out, err := r.ArchAssess(opts)
			if err != nil {
				fmt.Fprintln(os.Stderr, "Error:", err)
				os.Exit(2)
			}

			if summary {
				// In summary mode, the output map contains summaryText (plain text).
				if m, ok := out.(map[string]any); ok {
					if text, _ := m["summaryText"].(string); text != "" {
						fmt.Print(text)
						if m["gated"] == true {
							os.Exit(1)
						}
						return nil
					}
				}
			}

			if err := printJSON(out); err != nil {
				return fmt.Errorf("encode output: %w", err)
			}

			// Mirror TS exit code 1 when the gate was triggered.
			if m, ok := out.(map[string]any); ok {
				if m["gated"] == true {
					os.Exit(1)
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&repository, "repository", "", "Repository identifier (e.g. github.com/org/repo)")
	cmd.Flags().IntVar(&prNumber, "pr", 0, "PR number within the repository")
	cmd.Flags().StringVar(&gatePolicy, "gate-policy", "", "Gate policy: none | no-severity-high | zero-deviations | max:N")
	cmd.Flags().StringVar(&scopeLevel, "scope-level", "", "Scope level: project | org | tenant | global")
	cmd.Flags().StringVar(&projectID, "project-id", "", "Project ID for scope")
	cmd.Flags().StringVar(&db, "db", "", "Path to SQLite DB (overrides RENSEI_ARCH_DB)")
	cmd.Flags().BoolVar(&summary, "summary", false, "Output human-readable summary instead of JSON")

	return cmd
}
