package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/ptycli"
	"github.com/RenseiAI/donmai/runtime/confinement"
)

// museAuthPathEnv overrides the credential-file location the host login
// check reads, exactly as the launcher resolves it for the child:
// $MUSE_AUTH_PATH, then $XDG_CONFIG_HOME/muse/auth.json, then
// $HOME/.config/muse/auth.json.
const museAuthPathEnv = "MUSE_AUTH_PATH"

// hostLoginProbeTimeout bounds the native login probe below. Both probe
// outcomes arrive without network (a missing login fails local
// credential resolution; a present login fails local model validation),
// so the bound only ever fires on a wedged child.
const hostLoginProbeTimeout = 30 * time.Second

// hostLoginProbeModel is a model id no catalog can resolve. Probing
// `exec` with it proves the host got PAST authentication (the failure is
// the model's, not the login's) without running a real turn: no prompt
// is processed, no tokens are spent, and --no-session-log keeps it out
// of the session index.
const hostLoginProbeModel = "donmai-login-probe-nonexistent-model"

// errHostSessionLogin is the untyped refusal the host login check
// returns. Callers wrap it with ErrSpawnFailed + ErrNotAuthenticated so
// auth-aware supervisors match the typed cause (the same split the
// headless first-turn classifier in muse.go draws).
var errHostSessionLogin = fmt.Errorf("muse host-session login is unavailable")

// CheckInteractiveSessionLogin verifies the operator login an interactive
// PTY session inherits before it spawns. It performs no model turn and
// spends nothing:
//
//  1. File evidence (offline): the credential file the launcher itself
//     resolves (MUSE_AUTH_PATH → XDG → HOME) must exist, parse, and carry
//     a login for the session's provider. An explicit session API key
//     satisfies this leg on its own: serveEnv strips an ambient
//     META_API_KEY the child would never see, so only a key the Spec
//     sets (which the child inherits) counts.
//  2. Native probe (no network on either path): `muse exec` with an
//     unresolvable model id. A present login fails local model
//     validation ("does not exist or you lack access"); a missing login
//     fails local credential resolution ("missing meta credentials").
//
// This is local login evidence, not online model entitlement — the same
// contract the headless lane's first-turn classifier (classifyStartError)
// enforces online. No credential bytes are returned to callers.
//
// This is the INTERACTIVE leg of the host-session auth story, distinct
// from CheckHostSessionLogin (the install/version evidence the daemon
// quota poller runs): spawnInteractive calls that check first, then this
// one — an uninstalled CLI surfaces ErrProviderUnavailable, a login-less
// host surfaces ErrNotAuthenticated.
func CheckInteractiveSessionLogin(ctx context.Context, binary string) error {
	return checkHostSessionLogin(ctx, binary, "")
}

// checkHostSessionLogin is CheckInteractiveSessionLogin parameterized by
// session's explicit API key: a non-empty key is the session's route and
// the file + probe legs are skipped. An ambient META_API_KEY never
// counts — serveEnv strips it unless the Spec sets it, so the child it
// would authenticate never sees it.
func checkHostSessionLogin(ctx context.Context, binary string, apiKey string) error {
	if strings.TrimSpace(apiKey) != "" {
		return nil
	}
	if ctx == nil || strings.TrimSpace(binary) == "" {
		return errHostSessionLogin
	}
	if err := ctx.Err(); err != nil {
		return errHostSessionLogin
	}
	if !filepath.IsAbs(binary) {
		return errHostSessionLogin
	}
	if err := checkHostSessionLoginFile(); err != nil {
		return err
	}
	return probeHostSessionLogin(ctx, binary)
}

// resolveMuseAuthPath applies the launcher's own credential resolution:
// $MUSE_AUTH_PATH, then $XDG_CONFIG_HOME/muse/auth.json, then
// $HOME/.config/muse/auth.json. Empty means no location resolves (no
// HOME to anchor the default), which is itself a refusal.
func resolveMuseAuthPath(getenv func(string) string) string {
	if p := strings.TrimSpace(getenv(museAuthPathEnv)); p != "" {
		return p
	}
	if xdg := strings.TrimSpace(getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "muse", "auth.json")
	}
	if home := strings.TrimSpace(getenv("HOME")); home != "" {
		return filepath.Join(home, ".config", "muse", "auth.json")
	}
	return ""
}

// checkHostSessionLoginFile performs the offline leg: the resolved
// credential file must exist, parse, and carry a login mechanism for the
// meta provider. Values stay transient in this call and are never logged,
// returned, persisted, or used as auth.
func checkHostSessionLoginFile() error {
	return checkHostSessionLoginFileAt(resolveMuseAuthPath(os.Getenv))
}

// checkHostSessionLoginFileAt validates one credential path: bounded read,
// JSON shape, providers.meta with a non-empty login mechanism. A missing
// file, an unreadable one, a non-JSON body, or a body with no meta login
// all refuse — a login that lapsed leaves no usable entry here, and the
// native probe below (plus the first-turn classifier online) is the
// backstop for a stale one.
func checkHostSessionLoginFileAt(path string) error {
	if strings.TrimSpace(path) == "" {
		return errHostSessionLogin
	}
	const maxAuthFileBytes = 1 << 20
	body, err := os.ReadFile(path) //nolint:gosec // path is the launcher-resolved credential file, not caller input
	if err != nil || len(body) == 0 || len(body) > maxAuthFileBytes {
		return errHostSessionLogin
	}
	var auth struct {
		Providers map[string]struct {
			Mechanism *string `json:"mechanism"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(body, &auth); err != nil {
		return errHostSessionLogin
	}
	meta, ok := auth.Providers["meta"]
	if !ok || meta.Mechanism == nil || strings.TrimSpace(*meta.Mechanism) == "" {
		return errHostSessionLogin
	}
	return nil
}

// probeHostSessionLogin performs the native leg: `muse exec --no-session-log`
// with an unresolvable model id, under a scrubbed environment carrying only
// the config locators the login resolution needs. The probe runs in a
// throwaway directory so it never touches a real workspace. A present
// login fails model validation; a missing login fails credential
// resolution — anything else (timeout, crash, unexpected text) refuses.
func probeHostSessionLogin(ctx context.Context, binary string) error {
	return probeHostSessionLoginWithEnv(ctx, binary, hostSessionProbeEnv())
}

// probeHostSessionLoginWithEnv is probeHostSessionLogin with the child
// environment injected, so the credential-resolution leg is table-testable
// without mutating process state.
func probeHostSessionLoginWithEnv(ctx context.Context, binary string, env []string) error {
	probeDir, err := os.MkdirTemp("", "donmai-muse-login-")
	if err != nil {
		return errHostSessionLogin
	}
	defer func() { _ = os.RemoveAll(probeDir) }()
	probeCtx, cancel := context.WithTimeout(ctx, hostLoginProbeTimeout)
	defer cancel()
	// nolint:gosec // G204: binary is the resolved-from-PATH host binary New() execs.
	cmd := exec.CommandContext(probeCtx, binary, "exec", "--no-session-log", "--model", hostLoginProbeModel, "donmai login probe")
	cmd.Dir = probeDir
	cmd.Env = env
	// The probe owns this child in its own process group: a wedged
	// binary (or a wrapper that forks helpers) cannot outlive the
	// probe bound as reparented orphans. CommandContext's default
	// cancel signals only the leader, so override it with a group
	// kill — the same discipline the serve child (configureProcessGroup
	// + signalProcessGroup) already uses.
	configureProcessGroup(cmd)
	cmd.Cancel = func() error {
		signalProcessGroup(cmd, sigKILL())
		return nil
	}
	cmd.WaitDelay = 200 * time.Millisecond
	var stdout, stderr boundedProbeOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil {
		// An unresolvable model id can never succeed: a zero exit
		// means the probe did not run the expected validation.
		return errHostSessionLogin
	}
	if probeCtx.Err() != nil {
		return errHostSessionLogin
	}
	combined := stdout.String() + "\n" + stderr.String()
	// The login-proven verdict requires the probe's OWN model id on the
	// SAME line as the rejection: the host must be rejecting the model
	// THIS probe asked for. A bare "does not exist" (for example a
	// credential-store "keychain item does not exist") proves nothing
	// about the login and refuses below — even when the model id appears
	// on some other line (for example a startup banner naming it), which
	// a package-wide substring match would wrongly accept. The probe
	// model id is unresolvable by construction, so the host can only
	// name it alongside a rejection when it got past authentication to
	// model validation.
	hasLoginProvenLine := false
	for line := range strings.Lines(combined) {
		if !strings.Contains(line, hostLoginProbeModel) {
			continue
		}
		if strings.Contains(line, "does not exist") || strings.Contains(line, "lack access") {
			hasLoginProvenLine = true
			break
		}
	}
	switch {
	case hasLoginProvenLine:
		// The host authenticated and rejected the MODEL: login proven.
		return nil
	case strings.Contains(combined, "missing meta credentials") || strings.Contains(combined, "no login"):
		return errHostSessionLogin
	default:
		return errHostSessionLogin
	}
}

// hostSessionProbeEnv builds the scrubbed environment for the native
// probe: exactly the config locators login resolution reads (HOME, an
// explicit XDG override, an explicit credential-path override), plus a
// minimal PATH and NO_COLOR. Unrelated process credentials are never
// inherited — in particular no ambient META_API_KEY, which serveEnv
// strips from the session child too.
func hostSessionProbeEnv() []string {
	return hostSessionProbeEnvWith(os.Getenv, os.Getenv("PATH"))
}

// hostSessionProbeEnvWith is hostSessionProbeEnv with the lookup
// injected for tests.
func hostSessionProbeEnvWith(getenv func(string) string, path string) []string {
	env := []string{"PATH=" + path, "NO_COLOR=1"}
	if home := strings.TrimSpace(getenv("HOME")); home != "" {
		env = append(env, "HOME="+home)
	}
	for _, name := range []string{"XDG_CONFIG_HOME", museAuthPathEnv} {
		if value := strings.TrimSpace(getenv(name)); value != "" {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// boundedProbeOutput drains probe output up to a bound; overflow refuses
// rather than growing.
type boundedProbeOutput struct {
	buffer bytes.Buffer
}

func (b *boundedProbeOutput) Write(data []byte) (int, error) {
	const limit = 16 << 10
	if len(data) > limit-b.buffer.Len() {
		return 0, fmt.Errorf("muse host login probe exceeded its output bound")
	}
	return b.buffer.Write(data)
}

func (b *boundedProbeOutput) String() string { return b.buffer.String() }

// spawnInteractive opens the muse CLI's OWN interactive TUI — bare `muse`,
// with NO subcommand (`serve` selects the headless MSP host spawnInteractive
// never starts) — under a PTY via the shared ptycli driver, seeded with
// spec.Prompt when set and carrying the session-level flags interactiveArgs
// maps from the Spec.
//
// This is a distinct SPAWN MODE from the default headless loop, not a
// different Transport: Manifest().Caps.Transport stays
// agent.TransportSubprocessRPC (the serve host's transport); PTY is used only
// as a per-Spawn-call mode selected by Spec.Interactive != nil. See
// manifest.go and agent/harness.go's HarnessCaps.SupportsInteractivePTY doc
// comment.
//
// spec is already admitted (Spawn ran PrepareHarness before the
// interactive/headless split), so the interactive prompt + tool-lifecycle
// profiles have already compiled the plan onto the native surface: the
// positional seed below carries SystemPromptAppend + InitialContext +
// Prompt exactly as the receipt records.
//
// The host login check runs FIRST, before any confinement or child state:
// a session with no usable login is refused, never spawned to fail on its
// first render. Confinement then composes exactly as the headless lane's
// (spawnHandle): the wrapped argv runs under the session profile, and the
// plan's tmp/cache bindings ride the override env.
//
// Event semantics are the coarse ptycli contract (program decision D4 — the
// byte-accurate PTY stream is the product): an InitEvent once the PTY child
// is up and a single terminal ResultEvent when the process exits.
func (p *Provider) spawnInteractive(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	// Install evidence FIRST (cheap version check, no credential reads):
	// a host without a usable CLI surfaces ErrProviderUnavailable, not
	// the login refusal below.
	if err := CheckHostSessionLogin(ctx, p.museBin); err != nil {
		return nil, err
	}
	// Login evidence SECOND: the credential-file leg plus the no-cost
	// native probe refuse a login-less host with ErrSpawnFailed +
	// ErrNotAuthenticated before any confinement or child state.
	if err := checkHostSessionLogin(ctx, p.museBin, spec.Env["META_API_KEY"]); err != nil {
		return nil, fmt.Errorf("%w: %w: %v", agent.ErrSpawnFailed, agent.ErrNotAuthenticated, err)
	}
	workspaceRoot, err := canonicalWorkspaceRoot(spec.Cwd)
	if err != nil {
		return nil, fmt.Errorf("%w: muse workspace root: %v", agent.ErrSpawnFailed, err)
	}
	// Decide confinement FIRST, before the child starts: a requested
	// confinement that cannot be met refuses the spawn, never runs
	// unconfined. Process-less (skipProcess) launches spawn nothing and
	// are never confined.
	var confiner *confinement.Confiner
	if !p.opts.skipProcess {
		var cerr error
		confiner, cerr = p.confinerForSession(ctx, spec)
		if cerr != nil {
			return nil, cerr
		}
	}
	// The per-session scratch directory holds the confinement tmp and
	// caches for a confined session; unconfined sessions never create
	// it. Removed when the session stops (the ptycli cleanup below).
	var sessionDir string
	if confiner != nil {
		var derr error
		sessionDir, derr = os.MkdirTemp("", "muse-interactive-")
		if derr != nil {
			return nil, fmt.Errorf("%w: muse interactive session scratch: %v", agent.ErrSpawnFailed, derr)
		}
	}
	cleanupSessionDir := func() {
		if sessionDir != "" {
			_ = os.RemoveAll(sessionDir)
		}
	}
	var cplan *confinement.Plan
	if confiner != nil {
		var perr error
		cplan, perr = p.confineSession(spec, workspaceRoot, sessionDir, confiner)
		if perr != nil {
			cleanupSessionDir()
			return nil, perr
		}
	}
	releasePlan := func() error {
		if cplan != nil {
			return cplan.Release()
		}
		return nil
	}

	argv, cerr := confineMuseArgv(cplan, append([]string{p.museBin}, p.interactiveArgs(spec)...))
	if cerr != nil {
		_ = releasePlan()
		cleanupSessionDir()
		return nil, fmt.Errorf("muse confine interactive command: %w", cerr)
	}
	// Drop the parent's terminal descriptors first: they describe the
	// process that launched the harness, not the PTY the interactive child
	// runs in — the PTY host supplies its own interactive defaults, which
	// only an explicit Spec.Env entry may replace (mirrors pi's
	// withoutTerminalEnv). Without this, serveEnv's parent inheritance
	// would promote the parent TERM into an explicit override.
	childEnv := confineMuseEnv(serveEnv(withoutInteractiveParentTerminal(os.Environ()), p.opts.Env, spec.Env), cplan)

	cleanup := func() error {
		// The rendered profile must stay on disk until the confined child
		// has exited; the release runs once the session is over, and a
		// process already running under the profile stays confined.
		if err := releasePlan(); err != nil {
			cleanupSessionDir()
			return err
		}
		cleanupSessionDir()
		return nil
	}
	handle, err := ptycli.SpawnWithOptions(ctx, argv[0], argv[1:], ptycliSpec(spec, workspaceRoot, childEnv), p.Manifest(), ptycli.SpawnOptions{
		Cleanup: cleanup,
		// childEnv is the child's COMPLETE environment (serveEnv already
		// composed the parent allowlist, the provider/session overlays,
		// and the confinement bindings): the PTY host must not layer it
		// over this process's own environment, or every parent name the
		// allowlist stripped — in particular an ambient META_API_KEY —
		// would ride back in around it (mirrors pi's ExactEnv).
		ExactEnv: true,
	})
	if err != nil {
		_ = releasePlan()
		cleanupSessionDir()
		return nil, err
	}
	return handle, nil
}

// ptycliSpec projects the admitted session Spec onto the ptycli driver's
// input: the positional seed the receipt records rides Spec.Prompt (with
// SystemPromptAppend + InitialContext composed into the argv by
// interactiveArgs below), the composed child environment rides intact,
// and the working directory is the canonical root the host accepts.
func ptycliSpec(spec agent.Spec, workspaceRoot string, childEnv []string) agent.Spec {
	out := spec
	out.Cwd = workspaceRoot
	out.Env = serveEnvMap(childEnv)
	return out
}

// withoutInteractiveParentTerminal drops the parent's TERM/COLORTERM so the
// PTY host's interactive defaults apply unless the Spec sets them
// explicitly (which serveEnv layers back on top).
func withoutInteractiveParentTerminal(parent []string) []string {
	out := make([]string, 0, len(parent))
	for _, entry := range parent {
		if strings.HasPrefix(entry, "TERM=") || strings.HasPrefix(entry, "COLORTERM=") {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// serveEnvMap inverts serveEnv's "KEY=VALUE" slice back into the map the
// ptycli driver takes. The slice is already the child's complete
// environment (no runner-only names — envSlice drops those); last entry
// wins, matching the confinement bindings' override position.
func serveEnvMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}

// interactiveArgs builds the argv for the muse CLI's own interactive TUI.
//
// Spec → CLI mapping (interactive spawn mode):
//
//	Model              → --model <id> (resolveModel default is NOT applied:
//	                   empty leaves the TUI on the CLI's own default model,
//	                   mirroring claude's interactive posture; an id the CLI
//	                   rejects surfaces as its own nonzero exit, never a
//	                   silent fallback)
//	Effort             → --reasoning-effort <tier> (resolveEffort wire
//	                   spelling; unset leaves the host default)
//	Autonomous         → --approval-mode never (mirrors the headless lane's
//	                   allowAll for a policy-free autonomous session; a
//	                   non-autonomous session keeps the CLI default
//	                   on-request so the attached human approves)
//	SandboxLevel       → --disable-shell --disable-write on the read-only
//	                   lane (byte-identical to serveArgs: the read-only lane
//	                   is never widened); provider DisableSandbox →
//	                   --disable-sandbox; TrustWorkspace → --trust-workspace
//	                   (so a launched-then-attached session does not park on
//	                   a trust modal before the human attaches)
//	SystemPromptAppend → prepended to the positional seed (the TUI exposes
//	                   no system-prompt flag; the admission receipt records
//	                   every channel under the same muse_cli_pty_seed
//	                   delivery, so nothing is silently dropped)
//	InitialContext     → prepended to the positional seed after the system
//	                   append (same delivery, same receipt)
//	Prompt             → positional argument, ALWAYS LAST
//
// Deliberately unmapped (admission denies them on this profile — the TUI
// exposes no channel for them): SessionName (resume <ref> attaches to an
// EXISTING session only; producers never set ResumeExisting),
// PermissionConfig (the approval bridge has no RPC channel in PTY mode),
// AllowedTools/DisallowedTools (no flat-list grammar on this CLI),
// MCPServers (no inline-MCP surface outside session/start).
func (p *Provider) interactiveArgs(spec agent.Spec) []string {
	var argv []string
	trustWorkspace := p != nil && p.opts.TrustWorkspace
	disableSandbox := p != nil && p.opts.DisableSandbox
	if trustWorkspace {
		argv = append(argv, "--trust-workspace")
	}
	// The read-only lane wins over a provider-level DisableSandbox:
	// a Spec that asks for read-only is never widened (serveArgs parity).
	if spec.SandboxLevel == agent.SandboxReadOnly {
		argv = append(argv, "--disable-shell", "--disable-write")
	} else if disableSandbox {
		argv = append(argv, "--disable-sandbox")
	}
	if spec.Autonomous {
		argv = append(argv, "--approval-mode", "never")
	}
	if model := strings.TrimSpace(spec.Model); model != "" && model != "default" {
		argv = append(argv, "--model", model)
	}
	if effort := resolveEffort(spec); effort != nil {
		argv = append(argv, "--reasoning-effort", string(*effort))
	}
	// The positional seed is the profile's ONLY user channel: system
	// append, initial context, then the prompt — each already recorded on
	// the receipt under muse_cli_pty_seed. It stays LAST so it is never
	// consumed as the value of a preceding flag.
	seed := spec.Prompt
	if ctxText := strings.TrimSpace(spec.InitialContext); ctxText != "" {
		seed = ctxText + "\n\n" + seed
	}
	if sys := strings.TrimSpace(spec.SystemPromptAppend); sys != "" {
		seed = sys + "\n\n" + seed
	}
	if strings.TrimSpace(seed) != "" {
		argv = append(argv, seed)
	}
	return argv
}
