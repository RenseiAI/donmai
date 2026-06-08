package afcli

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient/codeintel"
	"github.com/RenseiAI/donmai/agent"
)

// archLaneHarnessPreference is the harness preference order for the arch-intel
// drift lane. The stub is deliberately excluded: it produces deterministic
// non-LLM output, useless for real deviation detection. We pick the first
// available real harness so the lane inherits the host's resolved provider.
var archLaneHarnessPreference = []agent.ProviderName{
	agent.ProviderClaude,
	agent.ProviderCodex,
	agent.ProviderAGYCLI,
	agent.ProviderGemini,
	agent.ProviderAmp,
	agent.ProviderOpenCode,
	agent.ProviderOllama,
}

// resolveArchLaneAdapter builds the arch-intel drift ModelAdapter by resolving
// the first available real harness from the agent-run registry. Returns
// (nil, false) when no real harness is available (e.g. no API key, no CLI on
// PATH) — the caller then runs the diff-only native path. This is the single
// place the provider registry meets the codeintel pipeline; codeintel itself
// never imports the registry (it consumes the injected ModelAdapter).
func resolveArchLaneAdapter() (codeintel.ModelAdapter, bool) {
	reg := BuildAgentRunRegistry(slog.Default())
	for _, name := range archLaneHarnessPreference {
		p, err := reg.Resolve(name)
		if err != nil {
			continue
		}
		hp, ok := p.(agent.HarnessProvider)
		if !ok {
			continue
		}
		return codeintel.LaneAdapter{Harness: hp}, true
	}
	return nil, false
}

// newArchCmd constructs the `donmai arch` command tree.
//
// The assess subcommand uses a three-tier implementation (most-capable first):
//  1. Exec-shim (when DONMAI_ARCH_BIN or donmai-arch on PATH): the
//     @donmai/architectural-intelligence TS package — byte-identical legacy path.
//  2. Native LLM pipeline ("mode":"native"): when an LLM harness is available
//     (and --no-llm is not set) the assess routes through the OSS one-shot lane
//     (agent.Complete) against the SQLite observation graph — real deviation
//     detection + materialized Deviation nodes. Requires a baseline in the graph.
//  3. Native diff-only ("mode":"native-diff-only"): pure-Go regex diff/gate
//     analysis. No LLM, no baseline required. The fallback when no harness is
//     available or --no-llm is set.
//
// Tiers 2 and 3 both fetch the REAL PR diff via the GitHub CLI (`gh`).
//
// See afclient/codeintel/{runner.go,arch_native.go,arch_assess_full.go,arch_difffetch.go}.
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

When none of the above are available, the native pipeline runs instead: it
fetches the PR diff via the GitHub CLI (gh) and, when an LLM harness is
available, runs real deviation detection against the SQLite baseline
("mode":"native"). Without a harness (or with --no-llm) it falls back to
pure-regex diff/gate analysis ("mode":"native-diff-only").

Requires the GitHub CLI (gh) on PATH for diff fetch; without it the native path
degrades to PR-metadata-only analysis.`,
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
		noLLM      bool
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

			// When no external donmai-arch binary is present, wire the native
			// LLM lane (unless --no-llm). The lane runs the full
			// assess-against-baseline pipeline against the SQLite graph; without
			// a real harness it falls through to diff-only.
			if !r.IsArchBinAvailable() {
				if noLLM {
					fmt.Fprintln(os.Stderr,
						"notice: --no-llm set — running native diff/gate analysis "+
							"(pure regex, no LLM, no deviation detection).")
				} else if adapter, ok := resolveArchLaneAdapter(); ok {
					r = r.WithArchAdapter(adapter)
					fmt.Fprintln(os.Stderr,
						"notice: DONMAI_ARCH_BIN not set — running native LLM drift "+
							"assessment against the SQLite baseline (mode: native).")
				} else {
					fmt.Fprintln(os.Stderr,
						"notice: no LLM harness available (no API key / CLI on PATH) — "+
							"running native diff/gate analysis (no LLM, no deviation detection). "+
							"Set DONMAI_ARCH_BIN or configure a provider for full drift detection.")
				}
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
	cmd.Flags().BoolVar(&noLLM, "no-llm", false, "Force diff-only native analysis (skip the LLM deviation lane)")

	return cmd
}
