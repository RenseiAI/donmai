package codex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Startup trust seeding for the interactive (PTY) spawn mode.
//
// # The failure this exists to prevent
//
// The codex CLI's own TUI holds MODAL startup reviews before it will read a
// single keystroke of the seeded prompt. Two of them are reachable purely from
// configuration state, and a session launched by this runner hits both on its
// first run in a freshly provisioned workspace:
//
//  1. "Do you trust the contents of this directory?" — codex has never seen the
//     session's working directory before, because the runner just created it.
//  2. "Hooks need review — N hooks are new or changed." — the checked-out repo
//     ships a `.codex/hooks.json`, and codex will not run hooks it has not been
//     told to trust ("Hooks can run outside the sandbox after you trust them").
//
// Neither modal times out. An attended terminal answers them in two keystrokes;
// an unattended session sits on them until its wall-clock timeout kills it,
// having done no work and produced no output that explains why. A hang at
// startup is the worst available outcome — worse than a refusal, which at least
// names what is missing — so this leg removes both by CONFIGURING the answers
// the platform is entitled to give, before the child starts.
//
// # What may be pre-answered, and what may not
//
// The rule is provenance, not convenience: the platform may pre-answer a trust
// question about something the PLATFORM ITSELF provisioned, and nothing else.
//
//   - The session working directory IS platform-provisioned — the runner
//     created it and placed its contents there — so it is pre-trusted.
//   - Requested MCP servers ARE platform-provisioned, and they need no trust
//     entry at all: codex starts every server named in effective configuration
//     without an approval step (measured against codex-cli 0.146.0; a server
//     that fails to start, including one answering 401, degrades to a warning
//     line and the session continues).
//   - Hooks discovered in the workspace are NOT platform-provisioned. They are
//     repo content — whoever wrote the checked-out tree chose them — and
//     trusting them grants arbitrary command execution OUTSIDE the sandbox for
//     the life of the session. This leg therefore never marks a hook trusted.
//     It instead takes the same answer the third option of codex's own modal
//     offers ("Continue without trusting — hooks won't run") and takes it
//     deterministically, by turning the hooks feature off for this process. The
//     session starts, no unreviewed repo code runs outside the sandbox, and the
//     decision is visible in argv rather than implied by a keystroke nobody
//     made.
//
// # Why overrides rather than writes
//
// The on-platform interactive spawn mode runs against a private,
// process-owned CODEX_HOME; file-backed authentication is hard-linked into
// that boundary without ever copying the operator's config.toml. Every seed
// here is still a process-local `--config` override — a `sessionFlags`
// configuration layer in codex's own vocabulary — so a trust decision the
// platform made for one session cannot outlive that session or accumulate in a
// file the operator owns. Standalone interactive use retains its historical
// ambient configuration.
//
// One consequence is deliberate and worth stating: the `projects` override
// SHADOWS the ambient projects table for this process, so the session's trusted
// set is exactly the workspace the platform provisioned. That is narrower than
// what the operator's own configuration may grant (a broad entry such as `/`
// stops applying inside the session), never broader.
//
// # The headless app-server lane is deliberately NOT seeded
//
// The app-server lane cannot hit either modal — it has no UI, and its approval
// traffic rides the JSON-RPC approval bridge (approval.go), where a decision is
// computed rather than typed. Pre-trusting its working directory would also
// have a cost the interactive lane does not pay: a trusted directory makes
// codex load that directory's `.codex/config.toml` as a `project` configuration
// layer, and any `mcp_servers` declared there would enter the effective
// configuration the isolated `CODEX_HOME` boundary exists to keep exclusive
// (config_boundary.go, and the readback proof in Provider.verifyMCPConfig).
// Staying untrusted there costs nothing and keeps that proof intact.

const (
	// codexTrustLevelTrusted is codex's own value for a directory whose
	// contents may be loaded: `[projects."<dir>"] trust_level = "trusted"`.
	codexTrustLevelTrusted = "trusted"

	// codexHooksEnv selects what happens to hooks codex discovers for the
	// session. It exists for the attended case only; see codexHooksPolicy.
	codexHooksEnv = "DONMAI_CODEX_HOOKS"

	// codexHooksOff is the default: the hooks feature is turned off for this
	// process, so an unreviewed hook neither runs nor blocks startup.
	codexHooksOff = "off"

	// codexHooksInherit leaves codex's own hook handling alone. An attended
	// terminal can then review and trust hooks interactively — and an
	// UNATTENDED session started this way can block on that review, which is
	// exactly the hang this file otherwise prevents.
	codexHooksInherit = "inherit"
)

// codexHooksPolicy resolves the hooks posture for one spawn. getenv is injected
// so the resolution is testable without mutating process state.
//
// An unrecognized value is an ERROR rather than a silent fall back to the
// default: the whole point of the knob is to choose between "cannot hang" and
// "may hang", and a typo that silently picked either one would be indefensible
// in the direction it happened to pick.
func codexHooksPolicy(getenv func(string) string) (string, error) {
	raw := ""
	if getenv != nil {
		raw = strings.TrimSpace(getenv(codexHooksEnv))
	}
	switch strings.ToLower(raw) {
	case "":
		return codexHooksOff, nil
	case codexHooksOff:
		return codexHooksOff, nil
	case codexHooksInherit:
		return codexHooksInherit, nil
	default:
		return "", fmt.Errorf("%s=%q is not a recognized hooks policy (want %q or %q)",
			codexHooksEnv, raw, codexHooksOff, codexHooksInherit)
	}
}

// interactiveTrustArgs builds the codex CLI overrides that let the interactive
// TUI reach its prompt with nobody at the keyboard. cwd is the session working
// directory (agent.Spec.Cwd); an empty value means "wherever this process is",
// which is what the PTY child would inherit.
//
// Returning an error here fails the spawn, by design. When the workspace
// directory cannot be resolved to an absolute path there is no trust entry to
// write, codex will raise its directory review, and an unattended session would
// sit on it forever — so the session is refused with a message that names the
// missing trust instead.
func interactiveTrustArgs(cwd string, hooksPolicy string, getwd func() (string, error)) ([]string, error) {
	dirs, err := sessionWorkspaceTrustDirs(cwd, getwd)
	if err != nil {
		return nil, err
	}

	args := []string{"--config", trustedProjectsOverride(dirs)}
	if hooksPolicy == codexHooksOff {
		// Disables codex's hooks feature for this process only. Expressed as a
		// dotted config path rather than the `--disable <feature>` flag on
		// purpose: an unknown feature NAME is fatal to that flag ("Error:
		// Unknown feature flag"), which would turn a codex release that renamed
		// the flag into a harness that cannot spawn at all.
		args = append(args, "--config", "features.hooks=false")
	}
	return args, nil
}

// sessionWorkspaceTrustDirs returns every absolute path that can name the
// session workspace in codex's `projects` table.
//
// Codex matches a project entry against the directory it is running in by exact
// path, so a workspace reached through a symlinked prefix (/tmp on macOS is the
// classic one) needs BOTH the path the caller handed us and its resolved form —
// otherwise the entry we write is not the entry codex looks up, and the review
// appears anyway. Resolution failure is not fatal: a directory that does not
// exist yet still has a usable absolute form.
func sessionWorkspaceTrustDirs(cwd string, getwd func() (string, error)) ([]string, error) {
	dir := strings.TrimSpace(cwd)
	if dir == "" {
		if getwd == nil {
			getwd = os.Getwd
		}
		wd, err := getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve the session workspace to pre-trust: %w", err)
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve the session workspace %q to pre-trust: %w", dir, err)
	}
	if abs == "" {
		return nil, errors.New("resolve the session workspace to pre-trust: empty absolute path")
	}

	dirs := []string{abs}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil && resolved != abs {
		dirs = append(dirs, resolved)
	}
	sort.Strings(dirs)
	return dirs, nil
}

// trustedProjectsOverride renders the `projects` table codex parses out of a
// `--config` value. TOML basic strings are reused from the MCP override path so
// a workspace path containing quotes, backslashes, or non-ASCII characters
// round-trips as one key rather than splitting the dotted path.
func trustedProjectsOverride(dirs []string) string {
	var body strings.Builder
	body.WriteString("projects={")
	for i, dir := range dirs {
		if i > 0 {
			body.WriteByte(',')
		}
		body.WriteString(tomlBasicString(dir))
		body.WriteString("={trust_level=")
		body.WriteString(tomlBasicString(codexTrustLevelTrusted))
		body.WriteByte('}')
	}
	body.WriteByte('}')
	return body.String()
}
