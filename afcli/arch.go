package afcli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient/codeintel"
)

// newArchCmd constructs the `donmai arch` command tree.
//
// The assess subcommand uses a two-tier implementation:
//  1. Primary (always available): native Go diff/gate analysis.
//     No external binary, LLM, or DB required.
//     Output is tagged with "mode":"native-diff-only".
//  2. Full pipeline (when DONMAI_ARCH_BIN or donmai-arch on PATH):
//     exec-shim to the @donmai/architectural-intelligence TS package.
//     Provides LLM-backed drift detection and SQLite observation graph.
//
// See afclient/codeintel/runner.go and arch_native.go for details.
func newArchCmd(cfg Config) *cobra.Command {
	bin := binaryName(cfg)
	cmd := &cobra.Command{
		Use:   "arch",
		Short: "Architectural intelligence — drift detection for PRs and commits",
		Long: `Architectural intelligence commands powered by @donmai/architectural-intelligence.

Detects deviations between a PR/commit and the stored architectural baseline.
All commands output JSON to stdout by default.

Exit codes (assess subcommand):
  0  Clean — no deviations or gate not triggered
  1  Gated — threshold exceeded per policy
  2  Error — invalid args, network failure, parse error

Environment:
  ANTHROPIC_API_KEY     Enables live LLM drift assessment (required for full detection)
  DONMAI_DRIFT_GATE     Gate policy: none | no-severity-high | zero-deviations | max:N
  DONMAI_ARCH_DB        SQLite DB path (default: .donmai/arch-intelligence/db.sqlite)

Binary resolution for full LLM pipeline (in order):
  1. DONMAI_ARCH_BIN env var — explicit binary override
  2. donmai-arch on PATH (npm install -g @donmai/cli)
  3. pnpm donmai-arch (monorepo dev)

When none of the above are available, the native diff/gate path runs instead.
Set DONMAI_ARCH_BIN for full drift detection including LLM deviation analysis.`,
		SilenceUsage: true,
	}

	cmd.AddCommand(newArchAssessCmd(bin))

	return cmd
}

// newArchAssessCmd constructs `donmai arch assess`.
func newArchAssessCmd(bin string) *cobra.Command {
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

Without DONMAI_ARCH_BIN or donmai-arch on PATH, the native diff/gate path
runs instead. It performs pure-regex diff analysis and gate evaluation without
LLM or database access. Output includes "mode":"native-diff-only" to indicate
the path used. Set DONMAI_ARCH_BIN for full LLM-backed drift assessment.

Examples:
  ` + bin + ` arch assess https://github.com/org/repo/pull/123
  ` + bin + ` arch assess --repository github.com/org/repo --pr 123
  ` + bin + ` arch assess https://github.com/org/repo/pull/123 --gate-policy zero-deviations
  ` + bin + ` arch assess https://github.com/org/repo/pull/123 --summary`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			r := codeintel.New(cwd())

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

			// Warn when using native-only mode (no arch binary available).
			if !r.IsArchBinAvailable() {
				fmt.Fprintln(os.Stderr,
					"notice: DONMAI_ARCH_BIN not set and donmai-arch not on PATH — "+
						"running native diff/gate analysis (no LLM, no SQLite graph). "+
						"Set DONMAI_ARCH_BIN for full drift detection.")
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
	cmd.Flags().StringVar(&db, "db", "", "Path to SQLite DB (overrides DONMAI_ARCH_DB)")
	cmd.Flags().BoolVar(&summary, "summary", false, "Output human-readable summary instead of JSON")

	return cmd
}
