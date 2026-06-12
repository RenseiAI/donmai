package credentials

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// wizardEnv bundles the I/O surface + external-command runner the wizard
// uses, so tests can substitute a scripted stdin, a captured stdout/stderr,
// and a stub `op` binary.
//
// All fields are mandatory; the zero value is unusable.
type wizardEnv struct {
	// In is the source of interactive answers. Production wiring uses
	// os.Stdin; tests inject a bytes.Buffer.
	In io.Reader

	// Out receives prompt + status lines.
	Out io.Writer

	// Err receives non-fatal warnings.
	Err io.Writer

	// CWD is the working directory the wizard treats as the starting
	// point for the git-root walk. Tests inject a temp dir; production
	// wiring passes os.Getwd().
	CWD string

	// LookPath mirrors exec.LookPath and is overridable in tests so
	// "op not on PATH" is deterministic.
	LookPath func(name string) (string, error)

	// RunOp runs the `op` binary with the given args and returns
	// (stdout, error). Production wiring shells out; tests script
	// responses per (args[0], args[1]) tuple.
	//
	// The wizard never echoes the OP_OUTPUT — it only inspects exit
	// status / parses the small JSON whoami envelope.
	RunOp func(ctx context.Context, args ...string) ([]byte, error)
}

// newSetupCmd builds the `donmai creds setup` Cobra command.
//
// The wizard is interactive but the function under test (runSetup) takes
// an explicit wizardEnv so the test suite can drive it without spawning
// a TTY.
func newSetupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Interactive wizard for standalone donmai credentials",
		Long: "Walk through optional 1Password CLI detection and write a sample\n" +
			"${gitRoot}/.env.local for donmai to read at startup. The wizard never\n" +
			"writes secret values — generated entries are commented-out\n" +
			"op://vault/item/field placeholders that the operator fills in.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("getwd: %w", err)
			}
			env := &wizardEnv{
				In:       cmd.InOrStdin(),
				Out:      cmd.OutOrStdout(),
				Err:      cmd.ErrOrStderr(),
				CWD:      cwd,
				LookPath: exec.LookPath,
				RunOp: func(ctx context.Context, args ...string) ([]byte, error) {
					//nolint:gosec // G204: args are hard-coded subcommand
					// names selected by the wizard ("--version", "whoami",
					// "signin", "vault"); no operator-supplied tokens
					// reach exec.Command.
					return exec.CommandContext(ctx, "op", args...).Output()
				},
			}
			return runSetup(cmd.Context(), env)
		},
	}
	return cmd
}

// runSetup is the wizard's pure-Go entry point. It is exported within
// the package (lowercase r — package-private) so tests can drive it
// directly with a scripted wizardEnv. The body is a linear top-to-bottom
// flow with a few explicit branch points; splitting it into helpers makes
// the control flow harder to follow than it helps.
//
//nolint:gocyclo // intentional linear flow; see doc comment above.
func runSetup(ctx context.Context, env *wizardEnv) error {
	if env == nil || env.In == nil || env.Out == nil || env.Err == nil {
		return errors.New("wizardEnv: In/Out/Err must be non-nil")
	}
	reader := bufio.NewReader(env.In)

	// ── Step 1: welcome ────────────────────────────────────────────
	_, _ = fmt.Fprintln(env.Out, "donmai creds setup: standalone credentials wizard")
	_, _ = fmt.Fprintln(env.Out, "")
	_, _ = fmt.Fprintln(env.Out,
		"Sets up the credentials donmai forwards to spawned agents when running outside")
	_, _ = fmt.Fprintln(env.Out,
		"of rensei-tui. Precedence: process environment wins over .env.local;")
	_, _ = fmt.Fprintln(env.Out,
		"missing keys fail open with a redacted warning at spawn time.")
	_, _ = fmt.Fprintln(env.Out, "")

	// ── Step 2: detect `op` ───────────────────────────────────────
	opPath, lookErr := env.LookPath("op")
	opPresent := lookErr == nil && opPath != ""

	var opVersion string
	if opPresent {
		out, err := env.RunOp(ctx, "--version")
		if err != nil {
			// op resolved on PATH but exits non-zero — treat as absent
			// for the rest of the flow, but surface the diagnostic.
			_, _ = fmt.Fprintf(env.Err,
				"warning: `op --version` failed (%v); skipping 1Password integration\n",
				err)
			opPresent = false
		} else {
			opVersion = strings.TrimSpace(string(out))
			_, _ = fmt.Fprintf(env.Out, "Detected 1Password CLI: %s (%s)\n", opVersion, opPath)
		}
	} else {
		_, _ = fmt.Fprintln(env.Out,
			"1Password CLI (`op`) not found in PATH.")
		_, _ = fmt.Fprintln(env.Out,
			"Install instructions: https://1password.com/downloads/command-line/")
		_, _ = fmt.Fprintln(env.Out,
			"Skipping 1Password setup; you can re-run `donmai creds setup` later.")
	}

	// ── Step 3 + 4: sign-in + vault ──────────────────────────────
	vaultName := "Private"
	if opPresent {
		signedIn, accountLabel := opWhoami(ctx, env)
		switch {
		case signedIn:
			_, _ = fmt.Fprintf(env.Out, "Signed in to 1Password as %s\n", accountLabel)
			if !promptYesNo(reader, env.Out,
				"Use this account for donmai credentials?", true) {
				_, _ = fmt.Fprintln(env.Out,
					"Skipping 1Password setup; proceeding to .env.local.")
				opPresent = false
			}
		default:
			_, _ = fmt.Fprintln(env.Out, "1Password CLI present but no active session.")
			if promptYesNo(reader, env.Out,
				"Sign in to 1Password now?", true) {
				if _, err := env.RunOp(ctx, "signin", "--raw"); err != nil {
					_, _ = fmt.Fprintf(env.Err,
						"warning: `op signin` failed (%v); skipping 1Password integration\n",
						err)
					opPresent = false
				} else {
					_, _ = fmt.Fprintln(env.Out, "Signed in.")
				}
			} else {
				_, _ = fmt.Fprintln(env.Out,
					"Skipping 1Password setup; proceeding to .env.local.")
				opPresent = false
			}
		}

		if opPresent {
			vaultName = promptVault(ctx, reader, env)
			if vaultName == "" {
				_, _ = fmt.Fprintln(env.Out,
					"No vault selected; falling back to op://Private/... placeholders.")
				vaultName = "Private"
			}
		}
	}

	// ── Step 5: .env.local ────────────────────────────────────────
	gitRoot, err := findGitRoot(env.CWD)
	if err != nil {
		return err
	}
	envLocalPath := filepath.Join(gitRoot, ".env.local")

	if _, statErr := os.Stat(envLocalPath); statErr == nil {
		_, _ = fmt.Fprintf(env.Out,
			".env.local already exists at %s\n", envLocalPath)
		if !promptYesNo(reader, env.Out,
			"Overwrite?", false) {
			_, _ = fmt.Fprintln(env.Out,
				"Existing .env.local preserved. To add 1Password references, edit it")
			_, _ = fmt.Fprintln(env.Out,
				"manually using `op://vault/item/field` syntax.")
			_, _ = fmt.Fprintln(env.Out, "")
			printFinalMessage(env.Out, envLocalPath, false)
			return nil
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("stat .env.local: %w", statErr)
	}

	content := renderEnvLocal(vaultName, opPresent)
	if err := writeEnvLocalFile(envLocalPath, content); err != nil {
		return fmt.Errorf("write .env.local: %w", err)
	}

	_, _ = fmt.Fprintf(env.Out, "Wrote %s (mode 0600)\n", envLocalPath)
	_, _ = fmt.Fprintln(env.Out, "")
	printFinalMessage(env.Out, envLocalPath, true)
	return nil
}

// opWhoami returns (signedIn, label) where label is a human-readable
// "<email> (<account-name>)" string. A non-zero `op whoami` exit is
// treated as "not signed in".
func opWhoami(ctx context.Context, env *wizardEnv) (bool, string) {
	out, err := env.RunOp(ctx, "whoami", "--format=json")
	if err != nil {
		return false, ""
	}
	var payload struct {
		Email   string `json:"email"`
		Account string `json:"account_uuid"`
		URL     string `json:"url"`
		Name    string `json:"user_uuid"`
	}
	if jsonErr := json.Unmarshal(out, &payload); jsonErr != nil {
		// op may emit a non-JSON one-liner in older versions; fall back
		// to the raw string so the user sees something useful.
		return true, strings.TrimSpace(string(out))
	}
	label := payload.Email
	if payload.URL != "" {
		label = fmt.Sprintf("%s (%s)", payload.Email, payload.URL)
	}
	if label == "" {
		label = strings.TrimSpace(string(out))
	}
	return true, label
}

// promptVault asks for a vault name (defaulting to "Private"), verifies
// it exists via `op vault get`, and loops until the operator either
// confirms an existing vault or supplies an empty answer (meaning
// "skip vault setup").
func promptVault(ctx context.Context, reader *bufio.Reader, env *wizardEnv) string {
	for {
		answer := promptString(reader, env.Out,
			"1Password vault to reference [Private, blank to skip]:",
			"Private")
		if answer == "" {
			return ""
		}
		if _, err := env.RunOp(ctx, "vault", "get", answer); err != nil {
			_, _ = fmt.Fprintf(env.Out,
				"Vault %q not found (op vault get exited non-zero). Try another.\n",
				answer)
			continue
		}
		return answer
	}
}

// promptYesNo prints "<question> [Y/n]" (when defaultYes) or "[y/N]"
// (when !defaultYes) and reads a single line. Empty input returns the
// default.
func promptYesNo(reader *bufio.Reader, out io.Writer, question string, defaultYes bool) bool {
	suffix := "[y/N]"
	if defaultYes {
		suffix = "[Y/n]"
	}
	_, _ = fmt.Fprintf(out, "%s %s ", question, suffix)
	line, _ := reader.ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer == "" {
		return defaultYes
	}
	return answer == "y" || answer == "yes"
}

// promptString prompts for free-form input. Empty input returns
// defaultValue.
func promptString(reader *bufio.Reader, out io.Writer, prompt, defaultValue string) string {
	_, _ = fmt.Fprintf(out, "%s ", prompt)
	line, _ := reader.ReadString('\n')
	answer := strings.TrimSpace(line)
	if answer == "" {
		return defaultValue
	}
	return answer
}

// findGitRoot walks up from start looking for a `.git` entry (file or
// directory). Returns the absolute path of the directory containing
// `.git`, or an error if the walk reaches the filesystem root first.
func findGitRoot(start string) (string, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("absolute path of %q: %w", start, err)
	}
	cur := abs
	for {
		dotgit := filepath.Join(cur, ".git")
		if _, err := os.Stat(dotgit); err == nil {
			return cur, nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat %s: %w", dotgit, err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf(
				"no git repository found at or above %s — run `donmai creds setup` from inside a checkout",
				abs)
		}
		cur = parent
	}
}

// renderEnvLocal produces the body of the .env.local file. The
// vaultName parameterizes the op:// placeholders; opPresent toggles
// the header tone (mentions op CLI vs not).
func renderEnvLocal(vaultName string, opPresent bool) string {
	var b strings.Builder
	b.WriteString("# Generated by `donmai creds setup`.\n")
	b.WriteString("#\n")
	b.WriteString("# Precedence: donmai reads this file ONCE at startup. The donmai process\n")
	b.WriteString("# environment ALWAYS wins; values here are the fallback layer.\n")
	b.WriteString("# Missing keys fail open with a redacted stderr warning per\n")
	b.WriteString("# spawned agent.\n")
	b.WriteString("#\n")
	b.WriteString("# Blocklist: a small set of variable names (the daemon's own auth\n")
	b.WriteString("# tokens — see internal/credentials/blocklist.go) is NEVER forwarded\n")
	b.WriteString("# into a child agent regardless of which source they come from.\n")
	b.WriteString("#\n")
	if opPresent {
		b.WriteString("# 1Password references: uncomment the lines below and adjust the\n")
		b.WriteString("# op://vault/item/field path to match your vault layout. Sample\n")
		b.WriteString("# values use the vault you selected during the wizard.\n")
	} else {
		b.WriteString("# 1Password references: you can use op://vault/item/field URIs\n")
		b.WriteString("# once the 1Password CLI is installed; until then, paste raw\n")
		b.WriteString("# values directly on the right-hand side.\n")
	}
	b.WriteString("#\n")
	b.WriteString("# Security:\n")
	b.WriteString("#   - File mode is 0600. Keep it that way.\n")
	b.WriteString("#   - This file is never copied into spawned worktrees.\n")
	b.WriteString("#   - Values never appear in donmai logs (variable names only).\n")
	b.WriteString("\n")

	samples := []struct {
		key  string
		item string
	}{
		{"ANTHROPIC_API_KEY", "Anthropic"},
		// Gemini / Google AI Studio key — used when the gemini provider is selected.
		{"GEMINI_API_KEY", "Google"},
		{"LINEAR_API_KEY", "Linear"},
		{"OPENAI_API_KEY", "OpenAI"},
	}
	for _, s := range samples {
		_, _ = fmt.Fprintf(&b, "# %s=op://%s/%s/credential\n", s.key, vaultName, s.item)
	}
	return b.String()
}

// writeEnvLocalFile writes content to path with mode 0600. The mode is
// set explicitly via os.Chmod after WriteFile so we tolerate platforms
// (or umasks) that would otherwise broaden the mode.
func writeEnvLocalFile(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

// printFinalMessage emits the closing reminder. wrote distinguishes
// between "file written" and "file preserved" so the message reads
// naturally either way.
func printFinalMessage(out io.Writer, path string, wrote bool) {
	if wrote {
		_, _ = fmt.Fprintf(out, "Sample placeholders written to %s\n", path)
	} else {
		_, _ = fmt.Fprintf(out, "Existing file kept at %s\n", path)
	}
	_, _ = fmt.Fprintln(out, "")
	_, _ = fmt.Fprintln(out,
		"Reminder: donmai reads this file ONCE at startup; credentials are never")
	_, _ = fmt.Fprintln(out,
		"copied into spawned worktrees. Blocked variable names (see")
	_, _ = fmt.Fprintln(out,
		"internal/credentials/blocklist.go) are never forwarded regardless of source.")
}
