package afcli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/internal/views/hostwatch"
)

// hostWatchEnvDaemonURL overrides the daemon base URL for `host watch`,
// mirroring the same env var the rest of the daemon CLI surface honours.
const hostWatchEnvDaemonURL = "DONMAI_DAEMON_URL"

// hostWatchSource is the minimal daemon-client surface the host-watch engine
// needs. *afclient.DaemonClient satisfies it; tests inject a fake.
type hostWatchSource interface {
	GetSessions() ([]afclient.DaemonSessionHandle, error)
	GetStatus() (*afclient.DaemonStatusResponse, error)
	GetStats(withPool, byMachine bool) (*afclient.DaemonStatsResponse, error)
}

// newHostWatchCmd constructs the `host watch` live dashboard command — the
// live view of the agent sessions running on THIS host, which is why it belongs
// under the `host` noun. A composing downstream binary grafts the same factory
// under its own `host` parent.
//
// The dashboard is a PURE LOCAL READER: it polls the localhost daemon control
// API for the live session index and tails each session's on-disk
// `.agent/events.jsonl` for the stream — it never round-trips a control plane,
// spawns no child, and its death is invisible to the daemon (out-of-band from
// execution).
func newHostWatchCmd() *cobra.Command {
	return newHostWatchCmdWithSource(nil)
}

// newFleetWatchAliasCmd returns the hidden, deprecated top-level `fleet-watch`
// alias of `host watch`. It is a fresh tree (cobra commands carry a single
// parent), behaviourally identical to the `host watch` instance.
func newFleetWatchAliasCmd(cfg Config) *cobra.Command {
	bin := binaryName(cfg)
	cmd := newHostWatchCmdWithSource(nil)
	cmd.Use = "fleet-watch"
	cmd.Short = "Deprecated alias for `" + bin + " host watch`"
	cmd.Hidden = true
	deprecateTree(cmd, bin+" host watch", hostAliasRemovalVersion)
	return cmd
}

// newHostWatchCmdWithSource is the injectable variant. factory may be nil to
// use the production daemon client.
func newHostWatchCmdWithSource(factory func(afclient.DaemonConfig) hostWatchSource) *cobra.Command {
	var (
		projectFlag string
		allFlag     bool
		replayFlag  bool
		plainFlag   bool
		daemonURL   string
	)
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Live dashboard of the agent sessions running on this host",
		Long: "Launch a per-project live dashboard of all agent work on this host, sourced\n" +
			"entirely from LOCAL data: the local daemon's session index joined with each\n" +
			"session's on-disk .agent/events.jsonl stream. It is a pure reader — killing it\n" +
			"never affects the daemon or any running agent.\n\n" +
			"With no --project/--all, the scope auto-detects from the current repo's git\n" +
			"remote (the \"one tab per project\" ergonomic). --all shows every session on the\n" +
			"host.\n\n" +
			"The header leads with the host, then the sessions in scope, queue depth,\n" +
			"the host's occupied/total session slots, uptime and version. Session cards\n" +
			"flow across the full width in equal-size columns, each showing the issue and\n" +
			"title, project and work type, model and harness, state with elapsed time\n" +
			"(plus turns and cost when the harness reports them), and the last activity.\n" +
			"Unreported values read unknown, never zero.\n\n" +
			"Keys: up/down or j/k select a card; enter swaps the stream for the selected\n" +
			"session's full detail and esc returns; [ and ] move the cards/stream split\n" +
			"(50/50 by default), 0 resets it, and the ratio survives terminal resizes;\n" +
			"f pauses or resumes the stream, g jumps to its tail; q quits.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := afclient.DefaultDaemonConfig()
			var client hostWatchSource
			switch {
			case factory != nil:
				client = factory(cfg)
			default:
				client = newHostWatchClient(daemonURL, cfg)
			}

			repoScope := ""
			if !allFlag {
				repoScope = strings.TrimSpace(projectFlag)
				if repoScope == "" {
					repoScope = detectRepoScope()
				}
			}

			src := hostwatch.NewSource(client, nil, repoScope)
			label := scopeLabel(repoScope, allFlag)
			interactive := isInteractiveTTY()
			plain := plainFlag || !interactive

			return runHostWatch(cmd.Context(), cmd.OutOrStdout(), !interactive, hostwatch.Options{
				Source:       src,
				ProjectLabel: label,
				HostLabel:    hostname(),
				Plain:        plain,
				Replay:       replayFlag,
			})
		},
	}
	cmd.Flags().StringVar(&projectFlag, "project", "", "Scope to a project/repo (default: auto-detect from CWD git remote)")
	cmd.Flags().BoolVar(&allFlag, "all", false, "Show every session on this host")
	cmd.Flags().BoolVar(&replayFlag, "replay", false, "Include history from each session's events.jsonl (scroll-back)")
	cmd.Flags().BoolVar(&plainFlag, "plain", false, "Plain (no color / box) output for non-TTY / CI")
	cmd.Flags().StringVar(&daemonURL, "daemon-url", "", "Daemon control URL (default: $DONMAI_DAEMON_URL or http://127.0.0.1:7734)")
	return cmd
}

// newHostWatchClient builds the production daemon client for host watch.
// An explicit daemon URL (flag or env) wins over cfg; either way the client
// carries the operator's control token, so an explicit URL keeps the same
// auth as the default one. The token is attached on mutating requests only.
func newHostWatchClient(daemonURL string, cfg afclient.DaemonConfig) *afclient.DaemonClient {
	if url := resolveHostWatchURL(daemonURL); url != "" {
		client := afclient.NewDaemonClientFromURL(url)
		client.SetControlToken(withControlToken(cfg).ControlToken)
		return client
	}
	return afclient.NewDaemonClient(withControlToken(cfg))
}

// resolveHostWatchURL returns the explicit flag, else the env override, else "".
func resolveHostWatchURL(flag string) string {
	if v := strings.TrimSpace(flag); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv(hostWatchEnvDaemonURL))
}

// scopeLabel produces the header scope label.
func scopeLabel(repoScope string, all bool) string {
	if all {
		return "all projects"
	}
	if repoScope == "" {
		return "all projects"
	}
	return repoScope
}

// detectRepoScope returns a scope string derived from the CWD's git remote,
// or "" when not in a repo (the dashboard then shows all sessions). The
// returned value is the owner/name slug (e.g. "RenseiAI/donmai"), matched
// leniently against each session's repository by the source.
func detectRepoScope() string {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return repoSlugFromRemote(strings.TrimSpace(string(out)))
}

// repoSlugFromRemote normalises a git remote URL to an "owner/name" slug.
// Handles both SSH (git@github.com:owner/name.git) and HTTPS
// (https://github.com/owner/name.git) forms. Returns "" when it cannot parse.
func repoSlugFromRemote(remote string) string {
	if remote == "" {
		return ""
	}
	s := remote
	s = strings.TrimSuffix(s, ".git")
	// SSH form: git@host:owner/name
	if i := strings.Index(s, ":"); i >= 0 && !strings.Contains(s[:i], "/") {
		s = s[i+1:]
	} else {
		// URL form: strip scheme + host.
		if i := strings.Index(s, "://"); i >= 0 {
			s = s[i+3:]
		}
		if i := strings.Index(s, "/"); i >= 0 {
			s = s[i+1:] // drop host
		}
	}
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1]
}

// hostname returns the machine hostname, or "" on error.
func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// isInteractiveTTY reports whether stdout is a terminal (so the AltScreen TUI
// is appropriate). Non-TTY falls back to plain mode.
func isInteractiveTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// runHostWatch launches the host-watch Bubble Tea program. Logging is
// suppressed while the TUI owns the terminal. In plain mode the program still
// runs (it renders without AltScreen/color), so an operator piping output
// gets a usable stream.
func runHostWatch(ctx context.Context, output io.Writer, nonTTY bool, opts hostwatch.Options) error {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)

	// AltScreen is requested from the model's View (v.AltScreen), matching
	// the dashboard command — bubbletea v2 reads it per-view rather than as a
	// program option.
	if nonTTY {
		opts.UnboundedCards = true
	}
	model := hostwatch.New(opts)
	programOptions := []tea.ProgramOption{tea.WithContext(ctx), tea.WithOutput(output)}
	if nonTTY {
		// Bubble Tea v2 otherwise opens /dev/tty for keyboard input even when
		// stdout is a pipe. Keep the renderer and a bounded geometry so the
		// same live model can still write its plain view to that pipe.
		programOptions = append(programOptions, tea.WithInput(nil), tea.WithWindowSize(100, 30))
	}
	var engine tea.Model = model
	var plain *hostWatchPipeModel
	if nonTTY {
		plain = &hostWatchPipeModel{inner: model, output: output}
		engine = plain
		programOptions = append(programOptions, tea.WithoutRenderer())
	}
	p := tea.NewProgram(engine, programOptions...)
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("run host watch: %w", err)
	}
	if plain != nil && plain.err != nil {
		return fmt.Errorf("write host watch: %w", plain.err)
	}

	return nil
}

// hostWatchPipeModel uses the same data/update engine while emitting complete
// plain snapshots. A terminal renderer would clip them to its window height
// and write cursor controls even though its output is a pipe.
type hostWatchPipeModel struct {
	inner  tea.Model
	output io.Writer
	last   string
	err    error
}

func (m *hostWatchPipeModel) Init() tea.Cmd  { return m.inner.Init() }
func (m *hostWatchPipeModel) View() tea.View { return m.inner.View() }
func (m *hostWatchPipeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.inner.Update(msg)
	m.inner = next
	content := m.inner.View().Content
	if m.err == nil && content != "" && content != m.last {
		if _, err := fmt.Fprintln(m.output, content); err != nil {
			m.err = err
			return m, tea.Quit
		}
		m.last = content
	}
	return m, cmd
}
