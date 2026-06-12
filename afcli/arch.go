package afcli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient/codeintel"
)

// newArchCmd constructs the `donmai arch` command tree.
//
// The assess subcommand is implemented NATIVELY in Go (Layer 1) - no external
// binary required. It fetches the REAL PR diff via the GitHub CLI (`gh`) and
// runs pure-Go regex diff/gate analysis ("mode":"native-diff-only"). No LLM and
// no datastore are involved.
//
// A DEPRECATED exec-shim (legacy TS arch implementation) can be opted into via
// DONMAI_ARCH_BIN or af-arch on PATH; it emits a one-time deprecation notice
// and will be removed in a future release.
//
// NOTE: the Layer-2 arch-intelligence pipeline (learned baseline + LLM deviation
// detection) is platform-owned per ADR-2026-06-07 and is NOT part
// of this OSS surface.
//
// See afclient/codeintel/{runner.go,arch_native.go,arch_difffetch.go}.
func newArchCmd(cfg Config) *cobra.Command {
	bin := binaryName(cfg)
	cmd := &cobra.Command{
		Use:   "arch",
		Short: "Architectural intelligence: native Layer-1 drift detection for PRs",
		Long: `Architectural intelligence - native Go arch-intel (Layer 1).

Detects architectural drift in a PR/commit entirely in-process: it fetches the
PR diff via the GitHub CLI (gh), indexes the change (Layer 1), and runs pure-Go
regex diff/gate analysis. No external binary, LLM, or datastore is required. All
commands output JSON to stdout by default.

Exit codes (assess subcommand):
  0  Clean - no deviations or gate not triggered
  1  Gated - threshold exceeded per policy
  2  Error - invalid args, network failure, parse error

Environment:
  DONMAI_DRIFT_GATE     Gate policy: none | no-severity-high | zero-deviations | max:N
  DONMAI_ARCH_BIN       DEPRECATED - opt into the legacy TS shim (see below)

The native pipeline fetches the PR diff via the GitHub CLI (gh) and performs
pure-regex diff/gate analysis ("mode":"native-diff-only"). Without gh on PATH it
degrades to PR-metadata-only analysis.

The Layer-2 arch-intelligence pipeline (learned baseline + LLM deviation
detection) is platform-owned and not part of this OSS surface.

DEPRECATED shim: set DONMAI_ARCH_BIN to force the legacy TS arch shim.
This path is deprecated, emits a one-time notice, and will be removed once
the native pipeline is the sole supported path.`,
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

This runs natively in Go (no external binary): it fetches the PR diff via the
GitHub CLI (gh) and performs pure-regex diff/gate analysis
("mode":"native-diff-only") — no LLM and no datastore. The legacy TS shim
(DEPRECATED) can still be opted into via DONMAI_ARCH_BIN or af-arch on PATH.

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

			// When the deprecated TS shim is not opted into (the common case),
			// run the native Go Layer-1 diff/gate analysis over the real PR diff.
			if !r.IsArchBinAvailable() {
				fmt.Fprintln(os.Stderr,
					"notice: running native diff/gate analysis (Layer 1: real PR diff "+
						"via gh, pure regex, no LLM, no datastore). The Layer-2 LLM "+
						"deviation pipeline is platform-owned. Set DONMAI_ARCH_BIN to "+
						"opt into the legacy (DEPRECATED) TS shim.")
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
