package afcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	engine "github.com/RenseiAI/donmai/afclient/codeintel"
	eval "github.com/RenseiAI/donmai/eval/codeintel"
)

// newEvalCmd constructs the hidden `donmai eval` command group. It hosts the
// code-intelligence A/B eval harness — the GA acceptance vehicle that measures
// agent-success delta WITH vs WITHOUT the code-intel MCP surface. Hidden because
// it is an operator/CI harness, not an everyday command.
func newEvalCmd(cfg Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "eval",
		Short:  "Evaluation harnesses (code-intel A/B, ...)",
		Hidden: true,
	}
	cmd.AddCommand(newEvalCodeIntelCmd(cfg))
	return cmd
}

type evalCodeIntelOpts struct {
	trials       int
	dry          bool
	executor     string
	advertise    string
	benchmarkDir string
	donmaiBin    string
	repoRoots    []string
	task         string
	platformURL  string
	platformTok  string
	platformPath string
	orgID        string
	projectID    string
	datasetID    string
	datasetName  string
	maxTurns     int
	maxTokens    int64
	keepWA       bool
	jsonOut      bool
	timeout      time.Duration
}

func newEvalCodeIntelCmd(cfg Config) *cobra.Command {
	bin := binaryName(cfg)
	opts := &evalCodeIntelOpts{}
	cmd := &cobra.Command{
		Use:   "codeintel",
		Short: "Run the code-intelligence A/B (WITH vs WITHOUT) eval benchmark",
		Long: `Run the code-intelligence A/B eval: for each benchmark task, provision two
fresh identical workareas at the pinned repo@sha, run an agent on each —
WITHOUT (` + bin + ` stripped from PATH, the mandatory contamination guard) and
WITH (the real af-code-intelligence MCP surface) — grade both, and report the
success delta, tokens-to-solution ratio, and tool adoption.

Two executors are available via --executor:

  plumbing (default) — a deterministic, no-LLM agent that proves the two-arm
    execution end to end (provisioning, PATH strip, a REAL MCP round-trip,
    transcript capture). Hermetic; keeps CI green. NOT a statistical GA result.

  claude — the live agent: spawns the real ` + "`claude`" + ` CLI in headless
    stream-json mode on each arm and captures the actual tool calls, turns,
    tokens (including cache-read), and final answer. A live run needs:
      * ` + "`claude`" + ` on PATH (claude 2.x, headless stream-json).
      * for the WITH arm, the af-code-intelligence MCP server reachable via the
        donmai binary (--donmai-bin / on PATH) so --mcp-config resolves.
      * VOYAGE_API_KEY and/or COHERE_API_KEY in env when the code-intel engine
        runs in hybrid (embedding) mode.

Use --dry to also dump the WITH/WITHOUT transcripts for one task.

Examples:
  ` + bin + ` eval codeintel --dry --trials 1 --task codeintel-find-symbol-donmai-001
  ` + bin + ` eval codeintel --executor claude --trials 3 --advertise mcp --repo-root RenseiAI/donmai=.
  ` + bin + ` eval codeintel --executor claude --trials 3 --platform-url https://... --platform-token rsk_...`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEvalCodeIntel(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.IntVar(&opts.trials, "trials", 3, "Repeated trials per arm (default 3; use 1 for a fast plumbing run)")
	f.BoolVar(&opts.dry, "dry", false, "Dump the WITH/WITHOUT transcripts for the first task (works with any executor)")
	f.StringVar(&opts.executor, "executor", "plumbing", "Arm executor: plumbing (default, hermetic no-LLM) or claude (live claude CLI)")
	f.StringVar(&opts.advertise, "advertise", "mcp", "WITH-arm advertisement mechanism: mcp (default) or prompt-help")
	f.StringVar(&opts.benchmarkDir, "benchmark-dir", "", "Benchmark JSONL dir (default: afclient/codeintel/testdata/eval-benchmark under the git root)")
	f.StringVar(&opts.donmaiBin, "donmai-bin", "", "Path to the donmai binary the WITH arm uses (default: this executable)")
	f.StringArrayVar(&opts.repoRoots, "repo-root", nil, "Repo clone source as slug=path (e.g. RenseiAI/donmai=/path/to/donmai); repeatable")
	f.StringVar(&opts.task, "task", "", "Only run cases whose id or family contains this substring")
	f.StringVar(&opts.platformURL, "platform-url", "", "Platform base URL for reporting eval_runs/eval_traces (empty = offline)")
	f.StringVar(&opts.platformTok, "platform-token", "", "Bearer token (rsk_...) for the platform reporting bridge")
	f.StringVar(&opts.platformPath, "platform-path", eval.DefaultBridgePath, "Platform ingest path")
	f.StringVar(&opts.orgID, "org", "", "Org id for the eval_runs rows")
	f.StringVar(&opts.projectID, "project", "", "Project id for the eval_runs rows")
	f.StringVar(&opts.datasetID, "dataset-id", "", "Dataset id for the eval_runs rows")
	f.StringVar(&opts.datasetName, "dataset-name", "codeintel-benchmark-v1", "Dataset name label")
	f.IntVar(&opts.maxTurns, "max-turns", 12, "Equal max-turns budget for BOTH arms")
	f.Int64Var(&opts.maxTokens, "max-tokens", 200000, "Equal max-tokens budget for BOTH arms")
	f.BoolVar(&opts.keepWA, "keep-workareas", false, "Leave provisioned workareas on disk (debugging)")
	f.BoolVar(&opts.jsonOut, "json", false, "Emit the full report as JSON")
	f.DurationVar(&opts.timeout, "timeout", 10*time.Minute, "Overall run timeout")
	return cmd
}

func runEvalCodeIntel(cmd *cobra.Command, opts *evalCodeIntelOpts) error {
	out := cmd.OutOrStdout()
	advertise, err := eval.ParseAdvertiseMode(opts.advertise)
	if err != nil {
		return err
	}

	benchDir := opts.benchmarkDir
	if benchDir == "" {
		benchDir = defaultBenchmarkDir()
	}
	cases, err := eval.LoadCasesDir(benchDir)
	if err != nil {
		return fmt.Errorf("load benchmark: %w", err)
	}
	if opts.task != "" {
		cases = filterCases(cases, opts.task)
	}
	if len(cases) == 0 {
		return fmt.Errorf("no benchmark cases selected (dir=%s task=%q)", benchDir, opts.task)
	}

	donmaiBin := opts.donmaiBin
	if donmaiBin == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve donmai binary: %w (pass --donmai-bin)", err)
		}
		donmaiBin = exe
	}

	repoRoots, err := resolveRepoRoots(opts.repoRoots)
	if err != nil {
		return err
	}

	var bridge *eval.Bridge
	if opts.platformURL != "" {
		bridge = eval.NewBridge(opts.platformURL, opts.platformTok, opts.platformPath)
	}

	executor, err := selectExecutor(opts.executor)
	if err != nil {
		return err
	}

	driver, err := eval.NewDriver(eval.Config{
		Trials:        opts.trials,
		Advertise:     advertise,
		DonmaiBin:     donmaiBin,
		RepoRoots:     repoRoots,
		Budget:        eval.Budget{MaxTurns: opts.maxTurns, MaxTokens: opts.maxTokens},
		Executor:      executor,
		Bridge:        bridge,
		OrgID:         opts.orgID,
		ProjectID:     opts.projectID,
		DatasetID:     opts.datasetID,
		DatasetName:   opts.datasetName,
		KeepWorkareas: opts.keepWA,
		Logf: func(format string, args ...any) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[eval] "+format+"\n", args...)
		},
	})
	if err != nil {
		return err
	}

	baseCtx := cmd.Context()
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(baseCtx, opts.timeout)
	defer cancel()

	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[eval] running %d case(s) x %d trial(s) x 2 arms (executor=%s, advertise=%s, donmai-bin=%s)\n",
		len(cases), opts.trials, executor.Name(), advertise, donmaiBin)
	rep, err := driver.Run(ctx, cases)
	if err != nil {
		return fmt.Errorf("eval run: %w", err)
	}

	_, _ = fmt.Fprint(out, rep.Summary())

	if opts.dry {
		if err := dumpTranscripts(out, rep); err != nil {
			return err
		}
	}
	if opts.jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	}
	if bridge == nil {
		note := "[eval] NOTE: no --platform-url; results captured locally only (offline). "
		if executor.Name() == "plumbing" {
			note += "This proves the two-arm plumbing; a live platform + the --executor claude agent are needed for a GA statistical result."
		} else {
			note += "A live platform (--platform-url) is still needed to persist the eval_runs/eval_traces for a GA statistical result."
		}
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), note)
	}
	return nil
}

// dumpTranscripts prints the WITH and WITHOUT EvalTrace envelopes for the first
// case — the plumbing proof that both arms produced a transcript of the expected
// shape.
func dumpTranscripts(out io.Writer, rep eval.Report) error {
	if len(rep.Records) == 0 {
		return nil
	}
	firstCase := rep.Records[0].CaseID
	_, _ = fmt.Fprintf(out, "\n=== transcripts for %s (WITH + WITHOUT) ===\n", firstCase)
	for _, arm := range []eval.Arm{eval.ArmWithout, eval.ArmWith} {
		for _, rec := range rep.Records {
			if rec.CaseID != firstCase || rec.Arm != arm {
				continue
			}
			_, _ = fmt.Fprintf(out, "\n--- arm=%s pass=%v ---\n", rec.Arm, rec.Pass)
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			if err := enc.Encode(rec.Envelope.Trace); err != nil {
				return err
			}
			break
		}
	}
	return nil
}

// selectExecutor maps the --executor flag to an eval.Executor. Plumbing is the
// default so `donmai eval codeintel` stays hermetic (no live LLM) in CI and the
// existing --dry path is unchanged; claude selects the live agent executor.
func selectExecutor(name string) (eval.Executor, error) {
	switch strings.TrimSpace(name) {
	case "", "plumbing":
		return eval.NewPlumbingExecutor(), nil
	case "claude":
		return eval.NewClaudeExecutor(), nil
	default:
		return nil, fmt.Errorf("unknown --executor %q (want plumbing or claude)", name)
	}
}

// defaultBenchmarkDir resolves the in-repo benchmark location from the git root.
func defaultBenchmarkDir() string {
	root, ok := engine.FindGitRoot(cwd())
	if !ok {
		root = cwd()
	}
	return filepath.Join(root, "afclient", "codeintel", "testdata", "eval-benchmark")
}

// resolveRepoRoots maps benchmark repo slugs to on-disk clone roots. The
// enclosing git root (this repo) is registered under its own slug by default;
// any additional dogfood repo is supplied at eval time via --repo-root
// slug=path (repeatable), so the OSS harness never hard-codes another repo.
func resolveRepoRoots(pairs []string) (map[string]string, error) {
	roots := map[string]string{}
	if root, ok := engine.FindGitRoot(cwd()); ok {
		roots["RenseiAI/donmai"] = root
	}
	for _, p := range pairs {
		slug, path, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("--repo-root %q must be slug=path", p)
		}
		roots[strings.TrimSpace(slug)] = strings.TrimSpace(path)
	}
	return roots, nil
}

// filterCases keeps cases whose id or family contains sub.
func filterCases(cases []eval.Case, sub string) []eval.Case {
	var out []eval.Case
	for _, c := range cases {
		if strings.Contains(c.ID, sub) || strings.Contains(string(c.Family()), sub) {
			out = append(out, c)
		}
	}
	return out
}
