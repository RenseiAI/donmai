package afcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/tempdoctor"
	"github.com/RenseiAI/donmai/tempown"
)

// doctorTempIO is the terminal seam of `doctor temp`: whether stdin is a
// terminal, and where answers are read from. Tests replace it.
type doctorTempIO struct {
	isTTY func() bool
	in    io.Reader
}

func defaultDoctorTempIO() doctorTempIO {
	return doctorTempIO{
		isTTY: func() bool {
			fi, err := os.Stdin.Stat()
			return err == nil && fi.Mode()&os.ModeCharDevice != 0
		},
		in: os.Stdin,
	}
}

// newDaemonDoctorTempCmd builds `<bin> host doctor temp`: a report of the
// user temp folder by producer, and --fix to remove what donmai left behind.
// The work is tempdoctor.Scan and tempdoctor.Fix, which an embedding product
// can wrap as its own command.
func newDaemonDoctorTempCmd(factory daemonClientFactory, bin string) *cobra.Command {
	return newDaemonDoctorTempCmdWithIO(factory, bin, defaultDoctorTempIO())
}

func newDaemonDoctorTempCmdWithIO(factory daemonClientFactory, bin string, tio doctorTempIO) *cobra.Command {
	var (
		jsonOut           bool
		fix               bool
		includeToolCaches bool
		yes               bool
		root              string
		timeout           time.Duration
		grace             time.Duration
		port              int
		host              string
	)
	cmd := &cobra.Command{
		Use:   "temp",
		Short: "Report what fills the user temp folder and remove donmai's own leftovers",
		Long: "Report the user temp folder by producer: donmai's own artefacts (with their\n" +
			"janitor verdicts), codex homes, donmai caches, shared tool caches and everything\n" +
			"else, with counts and sizes, warnings for flat directories with huge entry\n" +
			"counts, and an estimate of what emptying the folder at boot costs.\n\n" +
			"Counts are sampled and the scan stops at --timeout, printing partial results.\n\n" +
			"--fix removes only donmai artefacts whose recorded owner process is gone and\n" +
			"that sat idle past the grace period, plus codex homes no codex process ever\n" +
			"used. --fix --include-tool-caches also removes the shared tool caches\n" +
			"(" + strings.Join(tempdoctor.ToolCachePatterns(), ", ") + ") after the daemon\n" +
			"confirms no seat is running; it asks first, and without a terminal it refuses\n" +
			"unless --yes is given.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if includeToolCaches && !fix {
				return errors.New("--include-tool-caches requires --fix")
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			out := cmd.OutOrStdout()
			var fixResult *tempdoctor.FixResult
			var fixErr error
			if fix {
				opts := tempdoctor.FixOptions{Root: root, IncludeToolCaches: includeToolCaches, Grace: grace}
				if root != "" {
					// An explicit folder scopes the codex sweep too; only the
					// default run covers every codex home root.
					opts.CodexRoots = []string{root}
				}
				if includeToolCaches {
					cfg := afclient.DefaultDaemonConfig()
					if host != "" {
						cfg.Host = host
					}
					if port != 0 {
						cfg.Port = port
					}
					opts.SeatsRunning = func(context.Context) (int, error) {
						status, err := factory(cfg).GetStatus()
						if err != nil {
							return 0, fmt.Errorf("daemon status: %w", err)
						}
						return status.ActiveSessions, nil
					}
					opts.Confirm = doctorTempConfirm(cmd.ErrOrStderr(), tio, yes)
				}
				res, err := tempdoctor.Fix(ctx, opts)
				fixResult, fixErr = &res, err
				if fixErr != nil && !errors.Is(fixErr, tempdoctor.ErrToolCachesRefused) {
					return fmt.Errorf("doctor temp: %w", fixErr)
				}
			}
			report, err := tempdoctor.Scan(ctx, tempdoctor.Options{Root: root, Timeout: timeout, Grace: grace})
			if err != nil {
				return fmt.Errorf("doctor temp: %w", err)
			}
			if jsonOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(struct {
					Report *tempdoctor.Report    `json:"report"`
					Fix    *tempdoctor.FixResult `json:"fix,omitempty"`
				}{report, fixResult}); err != nil {
					return err
				}
				return fixErr
			}
			if fixResult != nil {
				if err := fixResult.WriteText(out); err != nil {
					return err
				}
				_, _ = fmt.Fprintln(out)
			}
			if err := report.WriteText(out); err != nil {
				return err
			}
			if !fix {
				if n := report.Reclaimable(); n > 0 {
					_, _ = fmt.Fprintf(out, "\n%d donmai artefact%s can be removed: run `%s host doctor temp --fix`.\n", n, plural1(n), bin)
				}
			}
			return fixErr
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output the report (and fix result) as JSON")
	cmd.Flags().BoolVar(&fix, "fix", false, "Remove donmai artefacts whose owner is gone, and never-used codex homes")
	cmd.Flags().BoolVar(&includeToolCaches, "include-tool-caches", false, "With --fix, also remove shared tool caches once no seat is running")
	cmd.Flags().BoolVar(&yes, "yes", false, "Do not ask before removing shared tool caches (required without a terminal)")
	cmd.Flags().StringVar(&root, "root", "", "Temp folder to inspect (default: the system temp folder)")
	cmd.Flags().DurationVar(&timeout, "timeout", tempdoctor.DefaultTimeout, "Stop scanning after this long and print partial results")
	cmd.Flags().DurationVar(&grace, "grace", tempown.DefaultGrace, "How long a dead owner's artefact must sit idle before --fix removes it")
	cmd.Flags().IntVar(&port, "port", 0, "Daemon HTTP port for the seat check (default from daemon.yaml: 7734)")
	cmd.Flags().StringVar(&host, "host", "", "Daemon HTTP host for the seat check (default: 127.0.0.1)")
	return cmd
}

// doctorTempConfirm asks on a terminal, accepts with --yes, and refuses
// without either.
func doctorTempConfirm(prompt io.Writer, tio doctorTempIO, yes bool) func(string) (bool, error) {
	return func(question string) (bool, error) {
		if yes {
			return true, nil
		}
		if !tio.isTTY() {
			return false, errors.New("stdin is not a terminal; pass --yes to confirm")
		}
		_, _ = fmt.Fprintf(prompt, "%s [y/N]: ", question)
		sc := bufio.NewScanner(tio.in)
		if !sc.Scan() {
			return false, nil
		}
		answer := strings.ToLower(strings.TrimSpace(sc.Text()))
		return answer == "y" || answer == "yes", nil
	}
}

func plural1(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
