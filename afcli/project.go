package afcli

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// configReaderWriter abstracts the daemon.yaml read/write operations so tests
// can inject a mock without touching the filesystem.
type configReaderWriter interface {
	ReadConfig() (*afclient.DaemonYAML, error)
	WriteConfig(cfg *afclient.DaemonYAML) error
}

// fileConfigRW is the production implementation — reads/writes daemon.yaml.
type fileConfigRW struct {
	path string
}

func (f *fileConfigRW) ReadConfig() (*afclient.DaemonYAML, error) {
	return afclient.ReadDaemonYAML(f.path)
}

func (f *fileConfigRW) WriteConfig(cfg *afclient.DaemonYAML) error {
	return afclient.WriteDaemonYAML(f.path, cfg)
}

// defaultConfigRW returns the production configReaderWriter.
func defaultConfigRW() configReaderWriter {
	return &fileConfigRW{path: afclient.DefaultDaemonYAMLPath()}
}

// ── newProjectCmd ─────────────────────────────────────────────────────────────

// newProjectCmd constructs the `project` parent command. It holds no logic of
// its own; it dispatches to subcommands that manage the daemon's project
// allowlist and per-project credentials in ~/.rensei/daemon.yaml.
func newProjectCmd() *cobra.Command {
	return newProjectCmdWithRW(defaultConfigRW())
}

// newProjectCmdWithRW is the injectable variant used in tests.
func newProjectCmdWithRW(rw configReaderWriter) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Manage the daemon's project allowlist and credentials",
		Long: "Manage the local rensei-daemon's project allowlist.\n\n" +
			"Projects must be explicitly allowed before the daemon will accept work\n" +
			"for them. Credentials can be configured interactively or later via\n" +
			"`af project credentials`.\n\n" +
			"Config is written to ~/.rensei/daemon.yaml atomically.\n" +
			"The daemon reloads on SIGHUP or restart.",
		SilenceUsage: true,
	}

	cmd.AddCommand(newProjectAllowCmd(rw))
	cmd.AddCommand(newProjectCredentialsCmd(rw))
	cmd.AddCommand(newProjectListCmd(rw))
	cmd.AddCommand(newProjectRemoveCmd(rw))

	return cmd
}

// ── allow ─────────────────────────────────────────────────────────────────────

func newProjectAllowCmd(rw configReaderWriter) *cobra.Command {
	var (
		noCredentials  bool
		nonInteractive bool
		cloneStrategy  string
	)

	cmd := &cobra.Command{
		Use:   "allow <repo-url>",
		Short: "Add a project to the daemon allowlist",
		Long: "Add a project to the daemon's project allowlist in ~/.rensei/daemon.yaml.\n\n" +
			"By default, an interactive prompt selects the credential helper.\n" +
			"Pass --no-credentials to skip credential configuration; the daemon will\n" +
			"refuse work for this project until `af project credentials` is run.\n" +
			"Pass --non-interactive to suppress all prompts (for CI/scripts).",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			repoURL := args[0]
			if repoURL == "" {
				return fmt.Errorf("repo-url must not be empty")
			}

			strategy := afclient.CloneStrategy(cloneStrategy)
			if strategy == "" {
				strategy = afclient.CloneShallow
			}

			cfg, err := rw.ReadConfig()
			if err != nil {
				return fmt.Errorf("read daemon config: %w", err)
			}

			entry := afclient.ProjectEntry{
				RepoURL:       repoURL,
				CloneStrategy: strategy,
			}

			switch {
			case noCredentials:
				// Explicit opt-out: leave CredentialHelper nil.
				entry.CredentialHelper = nil
			case nonInteractive:
				// Non-interactive with no --no-credentials: use nil (warn user).
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
					"warning: --non-interactive set without --no-credentials; "+
						"project added without credential helper. "+
						"Run `af project credentials "+repoURL+"` to configure.",
				)
				entry.CredentialHelper = nil
			default:
				helper, promptErr := promptCredentialHelper(cmd.InOrStdin(), cmd.OutOrStdout(), repoURL)
				if promptErr != nil {
					return fmt.Errorf("credential helper prompt: %w", promptErr)
				}
				entry.CredentialHelper = helper
			}

			cfg.AddOrUpdateProject(entry)

			if err := rw.WriteConfig(cfg); err != nil {
				return fmt.Errorf("write daemon config: %w", err)
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "project allowed: %s\n", repoURL)
			if entry.CredentialHelper == nil {
				_, _ = fmt.Fprintln(out,
					"  No credentials configured — daemon will refuse work until credentials are added.\n"+
						"  Run: af project credentials "+repoURL,
				)
			} else {
				_, _ = fmt.Fprintf(out, "  credential helper: %s\n", entry.CredentialHelper.Kind)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&noCredentials, "no-credentials", false,
		"Allow the project without configuring credentials (daemon will refuse work until credentials added)")
	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false,
		"Suppress all interactive prompts (for CI/scripts)")
	cmd.Flags().StringVar(&cloneStrategy, "clone-strategy", string(afclient.CloneShallow),
		"Clone strategy: shallow (default), full, reference-clone")

	return cmd
}

// ── credentials ───────────────────────────────────────────────────────────────

func newProjectCredentialsCmd(rw configReaderWriter) *cobra.Command {
	var nonInteractive bool

	cmd := &cobra.Command{
		Use:   "credentials <repo-url>",
		Short: "Configure credentials for an allowed project",
		Long: "Interactively configure the git credential helper for a project that is\n" +
			"already in the allowlist. Choices:\n\n" +
			"  1. osxkeychain — macOS Keychain (macOS only)\n" +
			"  2. ssh         — path to an SSH private key\n" +
			"  3. pat         — env-var name holding a Personal Access Token\n" +
			"  4. gh          — delegate to `gh auth` (GitHub CLI)\n\n" +
			"The project must already be in the allowlist; run `af project allow` first.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			repoURL := args[0]

			cfg, err := rw.ReadConfig()
			if err != nil {
				return fmt.Errorf("read daemon config: %w", err)
			}

			idx := cfg.FindProject(repoURL)
			if idx < 0 {
				return fmt.Errorf(
					"project %q is not in the allowlist — run `af project allow %s` first",
					repoURL, repoURL,
				)
			}

			if nonInteractive {
				return fmt.Errorf(
					"--non-interactive requires --no-credentials flag; " +
						"use `af project allow --non-interactive --no-credentials` for scripted onboarding",
				)
			}

			helper, promptErr := promptCredentialHelper(cmd.InOrStdin(), cmd.OutOrStdout(), repoURL)
			if promptErr != nil {
				return fmt.Errorf("credential helper prompt: %w", promptErr)
			}

			cfg.Projects[idx].CredentialHelper = helper

			if err := rw.WriteConfig(cfg); err != nil {
				return fmt.Errorf("write daemon config: %w", err)
			}

			// helper is nil when the user selects "None (configure later)";
			// the entry persists with no credential helper and the daemon
			// will refuse to clone until credentials are added.
			if helper == nil {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"credentials cleared for %s — daemon will refuse work until credentials are added\n",
					repoURL,
				)
			} else {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"credentials updated for %s: %s\n", repoURL, helper.Kind,
				)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false,
		"Suppress all interactive prompts (for CI/scripts)")

	return cmd
}

// ── list ──────────────────────────────────────────────────────────────────────

func newProjectListCmd(rw configReaderWriter) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all allowed projects",
		Long: "List the projects in the daemon's allowlist with repo URL, clone strategy,\n" +
			"and credential helper. Data is read from ~/.rensei/daemon.yaml.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := rw.ReadConfig()
			if err != nil {
				return fmt.Errorf("read daemon config: %w", err)
			}

			out := cmd.OutOrStdout()

			if len(cfg.Projects) == 0 {
				_, _ = fmt.Fprintln(out,
					"No projects in the allowlist.\n"+
						"  Add one with: af project allow <repo-url>",
				)
				return nil
			}

			return writeProjectTable(out, cfg.Projects)
		},
	}
}

// writeProjectTable renders a tabwriter table of projects.
func writeProjectTable(w io.Writer, projects []afclient.ProjectEntry) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "  REPO URL\tCLONE STRATEGY\tCREDENTIAL HELPER"); err != nil {
		return fmt.Errorf("write table header: %w", err)
	}
	for _, p := range projects {
		strategy := string(p.CloneStrategy)
		if strategy == "" {
			strategy = string(afclient.CloneShallow)
		}
		helperStr := credentialHelperString(p.CredentialHelper)
		if _, err := fmt.Fprintf(tw, "  %s\t%s\t%s\n", p.RepoURL, strategy, helperStr); err != nil {
			return fmt.Errorf("write table row: %w", err)
		}
	}
	return tw.Flush()
}

// credentialHelperString formats a *CredentialHelper for table display.
func credentialHelperString(h *afclient.CredentialHelper) string {
	if h == nil || h.Kind == "" {
		return "(none — credentials required)"
	}
	switch h.Kind {
	case afclient.CredentialHelperSSH:
		if h.SSHKeyPath != "" {
			return fmt.Sprintf("ssh (%s)", h.SSHKeyPath)
		}
		return "ssh"
	case afclient.CredentialHelperPAT:
		if h.EnvVarName != "" {
			return fmt.Sprintf("pat ($%s)", h.EnvVarName)
		}
		return "pat"
	default:
		return string(h.Kind)
	}
}

// ── remove ────────────────────────────────────────────────────────────────────

func newProjectRemoveCmd(rw configReaderWriter) *cobra.Command {
	var (
		yes            bool
		nonInteractive bool
	)

	cmd := &cobra.Command{
		Use:   "remove <repo-url>",
		Short: "Remove a project from the daemon allowlist",
		Long: "Remove a project from the daemon's project allowlist in ~/.rensei/daemon.yaml.\n\n" +
			"By default, a confirmation prompt is shown. Pass --yes to skip the prompt.\n" +
			"The daemon will refuse new work for this project after the next reload.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			repoURL := args[0]

			cfg, err := rw.ReadConfig()
			if err != nil {
				return fmt.Errorf("read daemon config: %w", err)
			}

			if cfg.FindProject(repoURL) < 0 {
				return fmt.Errorf("project %q not found in allowlist", repoURL)
			}

			if !yes && !nonInteractive {
				confirmed, confirmErr := promptConfirm(
					cmd.InOrStdin(), cmd.OutOrStdout(),
					fmt.Sprintf("Remove project %q from the allowlist? [y/N]: ", repoURL),
				)
				if confirmErr != nil {
					return fmt.Errorf("confirm prompt: %w", confirmErr)
				}
				if !confirmed {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "aborted.")
					return nil
				}
			}

			cfg.RemoveProject(repoURL)

			if err := rw.WriteConfig(cfg); err != nil {
				return fmt.Errorf("write daemon config: %w", err)
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "project removed: %s\n", repoURL)
			return nil
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")
	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false,
		"Suppress all interactive prompts (same as --yes for remove)")

	return cmd
}

// ── interactive prompt helpers ────────────────────────────────────────────────

// credentialHelperMenu is the list of choices presented in the interactive prompt.
var credentialHelperMenu = []struct {
	label string
	kind  afclient.CredentialHelperKind
}{
	{"macOS Keychain (osxkeychain)", afclient.CredentialHelperOSXKeychain},
	{"SSH key (path to private key)", afclient.CredentialHelperSSH},
	{"Personal Access Token (env-var name)", afclient.CredentialHelperPAT},
	{"GitHub CLI (gh auth)", afclient.CredentialHelperGH},
}

// promptCredentialHelper presents the credential-helper menu and returns the
// configured CredentialHelper. Returns nil if the user selects "none".
func promptCredentialHelper(in io.Reader, out io.Writer, repoURL string) (*afclient.CredentialHelper, error) {
	r := bufio.NewReader(in)

	_, _ = fmt.Fprintf(out, "\nCredential helper for %s:\n", repoURL)
	for i, item := range credentialHelperMenu {
		_, _ = fmt.Fprintf(out, "  %d. %s\n", i+1, item.label)
	}
	_, _ = fmt.Fprintf(out, "  5. None (configure later)\n")
	_, _ = fmt.Fprint(out, "Choice [1]: ")

	line, err := readLineReader(r)
	if err != nil {
		return nil, fmt.Errorf("read choice: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		line = "1"
	}

	switch line {
	case "1":
		return &afclient.CredentialHelper{Kind: afclient.CredentialHelperOSXKeychain}, nil
	case "2":
		_, _ = fmt.Fprint(out, "SSH key path [~/.ssh/id_ed25519]: ")
		keyPath, err := readLineReader(r)
		if err != nil {
			return nil, fmt.Errorf("read ssh key path: %w", err)
		}
		keyPath = strings.TrimSpace(keyPath)
		if keyPath == "" {
			keyPath = "~/.ssh/id_ed25519"
		}
		return &afclient.CredentialHelper{
			Kind:       afclient.CredentialHelperSSH,
			SSHKeyPath: keyPath,
		}, nil
	case "3":
		_, _ = fmt.Fprint(out, "Env-var name holding the PAT [GITHUB_TOKEN]: ")
		envVar, err := readLineReader(r)
		if err != nil {
			return nil, fmt.Errorf("read env-var name: %w", err)
		}
		envVar = strings.TrimSpace(envVar)
		if envVar == "" {
			envVar = "GITHUB_TOKEN"
		}
		return &afclient.CredentialHelper{
			Kind:       afclient.CredentialHelperPAT,
			EnvVarName: envVar,
		}, nil
	case "4":
		return &afclient.CredentialHelper{Kind: afclient.CredentialHelperGH}, nil
	case "5":
		return nil, nil //nolint:nilnil // intentional: caller treats nil as "no credentials"
	default:
		return nil, fmt.Errorf("invalid choice %q — expected 1–5", line)
	}
}

// promptConfirm reads a y/N confirmation from in and returns true for 'y'/'Y'.
func promptConfirm(in io.Reader, out io.Writer, prompt string) (bool, error) {
	_, _ = fmt.Fprint(out, prompt)
	line, err := readLineReader(bufio.NewReader(in))
	if err != nil {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes", nil
}

// readLineReader reads a single line from br, stripping the trailing newline.
// Unlike bufio.Scanner, a single bufio.Reader instance can be reused across
// multiple sequential readLineReader calls without consuming ahead.
func readLineReader(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	// Trim trailing newline / carriage-return regardless of error.
	line = strings.TrimRight(line, "\r\n")
	if err != nil {
		// io.EOF after reading some bytes is fine (last line without newline).
		if len(line) > 0 {
			return line, nil
		}
		return "", nil
	}
	return line, nil
}
