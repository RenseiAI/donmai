package afcli

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient/orchestrator"
	"github.com/RenseiAI/agentfactory-tui/afclient/repoconfig"
)

// orchestratorFlags holds the parsed flag values for `af orchestrator`.
// Factored out so unit tests can inspect defaults without executing RunE.
type orchestratorFlags struct {
	project   string
	single    string
	max       int
	dryRun    bool
	repo      string
	templates string
	linearKey string
	debug     bool
	quiet     bool
}

// newOrchestratorCmd constructs the `af orchestrator` command.
//
// Flags match the legacy `pnpm af-orchestrator` entrypoint:
//
//	--project    Filter issues by Linear project name
//	--single     Process exactly one issue by ID / identifier
//	--max        Maximum concurrent agents (default: 3)
//	--dry-run    Print what would be dispatched without spawning agents
//	--repo       Git remote URL pattern to validate against origin
//	--templates  Custom workflow template directory path
func newOrchestratorCmd() *cobra.Command {
	flags := &orchestratorFlags{}

	cmd := &cobra.Command{
		Use:   "orchestrator",
		Short: "Local orchestrator — pick Linear backlog issues and dispatch agents",
		Long: `orchestrator is the local entrypoint for OSS users without a coordinator
daemon.  It loads .agentfactory/config.yaml, validates the git remote, picks
Linear backlog issues from the configured project(s), and dispatches agents
(Claude / Codex) to work on them.

Environment:
  LINEAR_API_KEY     Required — Linear API key for authentication

Examples:
  # Process up to 3 backlog issues from a project
  af orchestrator --project MyProject

  # Process a specific issue
  af orchestrator --single REN-42

  # Preview what would be dispatched without running
  af orchestrator --project MyProject --dry-run

  # Restrict to a specific git repository
  af orchestrator --project MyProject --repo github.com/org/repo

  # Use custom workflow templates
  af orchestrator --project MyProject --templates .agentfactory/templates`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runOrchestrator(cmd, flags)
		},
	}

	cmd.Flags().StringVar(&flags.project, "project", "", "Filter backlog issues by Linear project name")
	cmd.Flags().StringVar(&flags.single, "single", "", "Process exactly one issue (by ID or identifier)")
	cmd.Flags().IntVar(&flags.max, "max", 3, "Maximum concurrent agent dispatches")
	cmd.Flags().BoolVar(&flags.dryRun, "dry-run", false, "Print what would be dispatched without spawning agents")
	cmd.Flags().StringVar(&flags.repo, "repo", "", "Git remote URL pattern to validate against origin (e.g. github.com/org/repo)")
	cmd.Flags().StringVar(&flags.templates, "templates", "", "Custom workflow template directory path")
	cmd.Flags().StringVar(&flags.linearKey, "linear-key", "", "Linear API key (default: $LINEAR_API_KEY)")
	cmd.Flags().BoolVar(&flags.debug, "debug", false, "Enable debug logging")
	cmd.Flags().BoolVar(&flags.quiet, "quiet", false, "Suppress non-error output")

	return cmd
}

// runOrchestrator is the body of RunE.  Extracted to keep the cobra wiring
// small and to let tests drive it with synthetic flags.
func runOrchestrator(_ *cobra.Command, flags *orchestratorFlags) error {
	// Configure logging.
	logLevel := slog.LevelInfo
	if flags.debug {
		logLevel = slog.LevelDebug
	} else if flags.quiet {
		logLevel = slog.LevelError
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))

	// Resolve API key.
	apiKey := flags.linearKey
	if apiKey == "" {
		apiKey = os.Getenv("LINEAR_API_KEY")
	}
	if apiKey == "" {
		return fmt.Errorf("orchestrator: LINEAR_API_KEY is required (set the env var or pass --linear-key)")
	}

	cfg := orchestrator.Config{
		LinearAPIKey: apiKey,
		Project:      flags.project,
		Single:       flags.single,
		Max:          flags.max,
		DryRun:       flags.dryRun,
		Repository:   flags.repo,
		TemplateDir:  flags.templates,
		Logger:       logger,
	}

	o, err := orchestrator.New(cfg)
	if err != nil {
		return err
	}

	// Banner — mirrors the TS orchestrator output.
	if !flags.quiet {
		_, _ = fmt.Fprintf(os.Stdout, "AgentFactory Orchestrator\n")
		_, _ = fmt.Fprintf(os.Stdout, "=========================\n")
		projectLabel := flags.project
		if projectLabel == "" {
			projectLabel = "All (from config.yaml)"
		}
		_, _ = fmt.Fprintf(os.Stdout, "Project: %s\n", projectLabel)
		if flags.single != "" {
			_, _ = fmt.Fprintf(os.Stdout, "Single:  %s\n", flags.single)
		}
		_, _ = fmt.Fprintf(os.Stdout, "Max concurrent: %d\n", flags.max)
		repoLabel := flags.repo
		if repoLabel == "" {
			repoLabel = "Any"
		}
		_, _ = fmt.Fprintf(os.Stdout, "Repo: %s\n", repoLabel)
		_, _ = fmt.Fprintf(os.Stdout, "Dry run: %v\n\n", flags.dryRun)
	}

	// Check if config.yaml is present and list allowed projects.
	if !flags.quiet {
		rc, loadErr := repoconfig.Load(resolveGitRoot())
		if loadErr == nil && rc != nil {
			if allowed := rc.GetEffectiveAllowedProjects(); len(allowed) > 0 {
				logger.Debug("allowed projects from config", "projects", allowed)
			}
		}
	}

	ctx := interruptContext()
	result, err := o.Run(ctx)
	if err != nil {
		return err
	}

	// Print summary.
	if !flags.quiet {
		_, _ = fmt.Fprintf(os.Stdout, "\n")
		if flags.dryRun {
			_, _ = fmt.Fprintf(os.Stdout, "[DRY RUN] Would dispatch %d issue(s):\n", len(result.Dispatched))
		} else {
			_, _ = fmt.Fprintf(os.Stdout, "Dispatched %d agent(s):\n", len(result.Dispatched))
		}
		for _, d := range result.Dispatched {
			statusLabel := string(d.Status)
			if d.Status == orchestrator.DispatchCompleted {
				dur := d.CompletedAt.Sub(d.StartedAt).Round(time.Second)
				statusLabel = fmt.Sprintf("completed (%s)", dur)
			}
			_, _ = fmt.Fprintf(os.Stdout, "  %s — %s [%s]\n", d.Identifier, d.Title, statusLabel)
		}
		if len(result.Errors) > 0 {
			_, _ = fmt.Fprintf(os.Stdout, "\nErrors (%d):\n", len(result.Errors))
			for _, e := range result.Errors {
				_, _ = fmt.Fprintf(os.Stdout, "  %s\n", e)
			}
		}
	}

	if len(result.Errors) > 0 {
		return fmt.Errorf("orchestrator: %d error(s) during run", len(result.Errors))
	}

	return nil
}

// resolveGitRoot returns the git repository root via git rev-parse.
// Returns the current working directory on failure (best-effort).
func resolveGitRoot() string {
	out, err := runGitCommand("rev-parse", "--show-toplevel")
	if err != nil {
		wd, _ := os.Getwd()
		return wd
	}
	return out
}
