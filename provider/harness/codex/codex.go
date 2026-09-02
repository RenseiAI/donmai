package codex

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// mcpConfigCleanupTimeout bounds the terminal empty-config write/readback.
// Tool-using turns can leave app-server work queued briefly after the terminal
// notification; two seconds produced false cleanup poison on the real executor
// even though the same request completed immediately afterward. Five seconds
// remains bounded beneath the ordinary RPC timeout while covering that drain.
const mcpConfigCleanupTimeout = 5 * time.Second

// Provider is the agent.Provider implementation backed by a long-lived
// `codex app-server` subprocess. One Provider instance owns one
// subprocess; sessions multiplex via JSON-RPC `thread/start` calls.
//
// Concurrency: Spawn / Resume / Shutdown are safe to call concurrently.
// Inflight Handles share the same Client and read goroutine; the
// Provider tracks them so Shutdown can fail them all if the app-server
// dies.
type Provider struct {
	opts Options

	cmd          *exec.Cmd
	codexBin     string
	client       *Client
	stdin        io.WriteCloser
	stdout       io.ReadCloser
	stderr       io.ReadCloser
	config       *codexConfigBoundary
	hostAuthFile string
	processDone  chan error
	startMu      sync.Mutex
	startErr     error
	started      bool

	// appServerStderr is the bounded, redaction-aware capture of this
	// shared app-server child's stderr (see appserver_stderr.go). Nil until
	// startLocked's real-subprocess branch runs; Excerpt() on a nil buffer
	// is safe and returns "", which is what every skipProcess-based test
	// (no real subprocess spawned) sees.
	appServerStderr *boundedBuffer

	// pinnedSessionEnv is the per-session environment layer that was frozen
	// into the app-server child at start, and sessionEnvPinned records that a
	// start happened at all (a nil layer is a real, distinct value: it means
	// the child carries NO session layer). Both are guarded by startMu and
	// exist so a later Spawn/Resume can be checked against what the running
	// child actually received — see checkSessionEnvLocked.
	pinnedSessionEnv map[string]string
	sessionEnvPinned bool

	// mcpMu serializes app-server-global config changes. mcpUsers counts
	// live handles holding the current config digest: equal configs may share
	// it, while an incompatible config is denied until all prior handles have
	// closed so no session can observe another session's MCP set.
	mcpMu           sync.Mutex
	mcpConfigDigest [sha256.Size]byte
	mcpConfigKnown  bool
	mcpManagedNames map[string]struct{}
	mcpUsers        int
	mcpPoisoned     bool

	// handlesMu / handles tracks live Handles so we can fail them
	// all when the shared app-server crashes.
	handlesMu sync.Mutex
	handles   map[*Handle]struct{}

	// shutdown is closed once Shutdown has been initiated; protected
	// by closeOnce so multiple callers see consistent state.
	closeOnce sync.Once
	shutdown  chan struct{}
}

// Options configures Provider construction. Most fields are optional;
// the empty value runs `codex` from PATH against the parent process'
// environment.
type Options struct {
	// CodexBin is the codex binary path. Defaults to $CODEX_BIN, then
	// "codex" looked up via $PATH.
	CodexBin string

	// Args overrides the subcommand args; defaults to ["app-server"].
	Args []string

	// Cwd is the working directory for the codex app-server child.
	// Defaults to os.Getwd(). Sessions still pass per-thread Cwd via
	// thread/start params; this is just where the app-server itself
	// runs.
	Cwd string

	// Env is merged into the parent process environment for the
	// subprocess. Use to inject OPENAI_API_KEY.
	Env map[string]string

	// HostSessionAuth permits a selected headless Codex host-session route to
	// project the host's existing CLI login into the otherwise isolated
	// CODEX_HOME. Construction pins file-backed auth but does not deliver the
	// credential or start app-server; Spawn/Resume hard-links auth.json only
	// after harness preparation, then initializes app-server, while config.toml
	// and MCP servers remain private.
	HostSessionAuth bool

	// HandshakeTimeout caps the JSON-RPC initialize handshake.
	// Defaults to 30s.
	HandshakeTimeout time.Duration

	// RPCTimeout is forwarded to handles for per-request timeouts.
	// Defaults to 30s.
	RPCTimeout time.Duration

	// stdoutOverride / stdinOverride are test seams. Production code
	// leaves them nil and the Provider spawns a real subprocess.
	stdoutOverride io.ReadCloser
	stdinOverride  io.WriteCloser
	stderrOverride io.ReadCloser
	skipProcess    bool // when true, no real codex is spawned (tests)
	// verifyMCPReadback makes protocol fakes exercise the production
	// config/read and initialized-inventory activation proof. Real processes
	// always verify it.
	verifyMCPReadback bool
	configTempDir     string
	// interactiveMCPInventoryRunner is a test seam for the pre-PTY effective
	// config readback. Production executes the selected Codex binary's own
	// `mcp list --json` semantics.
	interactiveMCPInventoryRunner interactiveMCPInventoryRunner
	// interactiveAuthSeeder is a test seam for projecting an explicit
	// environment credential into the private, ephemeral session auth store.
	interactiveAuthSeeder interactiveCodexAuthSeeder
	// interactiveNameServerStarted observes the exact runner-owned bootstrap
	// endpoint once its handshake completes (and, for an explicit
	// attach-to-existing session, once thread/resume has proven the target
	// exists) — always before the PTY is spawned. Test-only; production
	// leaves it nil.
	interactiveNameServerStarted func(remoteURL string)
}

// New constructs the Provider WITHOUT starting the codex app-server.
// Binary availability is still probed here — a missing binary returns
// agent.ErrProviderUnavailable wrapped with context, so the registry keeps
// skipping an uninstalled codex — but process start and the initialize
// handshake are deferred to the first headless Spawn/Resume.
//
// The deferral is a correctness requirement, not an optimization: the
// app-server child's environment is composed ONCE at start, and the runner's
// per-session agent.Spec.Env (the canonical DONMAI_API_URL among it) does not
// exist yet at construction time. Starting eagerly would pin the child to the
// ambient os.Environ, letting a host- or credential-snapshot-injected value
// outlive and override the runner-owned one for every session this Provider
// serves. HostSessionAuth additionally needs the deferral so the selected
// credential is projected before Codex caches an unauthenticated startup
// state. See startLocked / ensureHeadlessReady.
func New(opts Options) (*Provider, error) {
	if opts.HandshakeTimeout == 0 {
		opts.HandshakeTimeout = 30 * time.Second
	}
	if opts.RPCTimeout == 0 {
		opts.RPCTimeout = 30 * time.Second
	}
	if len(opts.Args) == 0 {
		opts.Args = []string{"app-server"}
	}

	hostAuthFile := ""
	if opts.HostSessionAuth {
		var err error
		hostAuthFile, err = resolveHostSessionAuthFile()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", agent.ErrProviderUnavailable, err)
		}
	}
	codexBin := ""
	if !opts.skipProcess {
		var err error
		codexBin, err = resolveCodexBinary(opts.CodexBin)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", agent.ErrProviderUnavailable, err)
		}
	}
	p := &Provider{
		opts:         opts,
		codexBin:     codexBin,
		shutdown:     make(chan struct{}),
		handles:      make(map[*Handle]struct{}),
		hostAuthFile: hostAuthFile,
	}
	boundary, err := newCodexConfigBoundary(opts.configTempDir, opts.HostSessionAuth)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrProviderUnavailable, err)
	}
	p.config = boundary
	// Seed this session's isolated home from the host-level warm cache of
	// Codex's own cache/ subtree (see plugin_cache.go) so the app-server's
	// bootstrap network fetch of the vendor plugin catalog is a cache hit
	// after the first session on this host. Best-effort and non-fallible —
	// it never returns an error, preserving the invariant below.
	boundary.enablePluginCacheReuse(resolveCodexPluginCacheDir(""))
	// No fallible step follows: the boundary is the last thing New allocates,
	// and the app-server start that used to run here (and could leak it on a
	// failed handshake) is now deferred to ensureHeadlessReady, whose failure
	// path removes the boundary via failStartLocked → terminateLocked.
	return p, nil
}

// ensureStarted serializes app-server initialization with Shutdown for callers
// that have no per-session Spec in hand (package-internal probes and tests).
// Production headless spawns go through ensureHeadlessReady so the session
// environment layer is not silently dropped.
func (p *Provider) ensureStarted() error {
	p.startMu.Lock()
	defer p.startMu.Unlock()
	return p.startLocked(nil)
}

// startLocked starts and initializes the app-server. startMu must be held.
//
// sessionEnv is the agent.Spec.Env of the session whose Spawn/Resume triggered
// this start. It is overlaid on the inherited parent environment through the
// canonical composition in runtime/env, so a runner-owned key beats whatever
// the ambient process environment happens to carry. The app-server is shared
// by every session this Provider serves, so only the FIRST start applies an
// overlay — the runner composes one canonical value per box, and a Provider is
// never reused across boxes.
//
// That last sentence is an invariant, not a hope: a successful start PINS the
// layer it applied, and ensureHeadlessReady refuses a later Spawn/Resume whose
// layer materially differs rather than silently serving it the first session's
// DONMAI_SESSION_ID and DONMAI_API_URL. See checkSessionEnvLocked.
func (p *Provider) startLocked(sessionEnv map[string]string) error {
	if p.startErr != nil {
		return p.startErr
	}
	if p.started {
		return nil
	}
	select {
	case <-p.shutdown:
		return fmt.Errorf("%w: codex provider already shut down", agent.ErrProviderUnavailable)
	default:
	}

	if p.opts.skipProcess {
		// Test path: caller wired stdin/stdout via overrides.
		p.stdin = p.opts.stdinOverride
		p.stdout = p.opts.stdoutOverride
		p.stderr = p.opts.stderrOverride
	} else {
		// nolint:gosec // bin is sourced from explicit Options/env, not user input.
		cmd := exec.Command(p.codexBin, p.opts.Args...)
		cmd.Dir = p.opts.Cwd
		cmd.Env = mergeEnv(p.opts.Env, sessionEnv, p.config.home)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return p.failStartLocked(fmt.Errorf("%w: codex stdin pipe: %v", agent.ErrProviderUnavailable, err))
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			_ = stdin.Close()
			return p.failStartLocked(fmt.Errorf("%w: codex stdout pipe: %v", agent.ErrProviderUnavailable, err))
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			_ = stdin.Close()
			_ = stdout.Close()
			return p.failStartLocked(fmt.Errorf("%w: codex stderr pipe: %v", agent.ErrProviderUnavailable, err))
		}
		if err := cmd.Start(); err != nil {
			_ = stdin.Close()
			_ = stdout.Close()
			_ = stderr.Close()
			return p.failStartLocked(fmt.Errorf("%w: codex spawn: %v", agent.ErrProviderUnavailable, err))
		}
		p.cmd = cmd
		p.stdin = stdin
		p.stdout = stdout
		p.stderr = stderr
		// Pin the app-server's own verified process identity (PID + OS-
		// reported start time) against its isolated home so a later orphan
		// sweep (orphan_sweep.go) can identify and terminate it specifically
		// — never by bare PID alone, which PID reuse on a host churning
		// thousands of codex spawns can make point at an unrelated process.
		pinDonmaiChildIdentity(p.config.home, cmd.Process.Pid)
		// Drain stderr into a bounded, redacted capture so the child never
		// deadlocks on a full pipe and a crash leaves a forensic excerpt
		// instead of nothing — see appserver_stderr.go and failStartLocked /
		// watchExit / onClientClose / checkAlive below, which are what
		// actually surface it.
		p.processDone = make(chan error, 1)
		p.appServerStderr = captureAppServerStderr(stderr)
	}

	p.client = NewClient(p.stdin, p.stdout)
	p.client.SetOnClose(p.onClientClose)
	if p.cmd != nil {
		go p.watchExit()
	}

	hctx, cancel := context.WithTimeout(context.Background(), p.opts.HandshakeTimeout)
	defer cancel()
	initRaw, err := p.client.Request(hctx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "donmai",
			"title":   "Donmai Orchestrator",
			"version": "0.5.0",
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	}, p.opts.HandshakeTimeout)
	if err != nil {
		return p.failStartLocked(fmt.Errorf("%w: codex initialize handshake: %v", agent.ErrProviderUnavailable, err))
	}
	if !p.opts.skipProcess {
		var initResp struct {
			CodexHome string `json:"codexHome"`
		}
		if err := json.Unmarshal(initRaw, &initResp); err != nil || initResp.CodexHome == "" || !sameResolvedPath(initResp.CodexHome, p.config.home) {
			return p.failStartLocked(fmt.Errorf("%w: codex app-server did not confirm its isolated config home", agent.ErrProviderUnavailable))
		}
	}
	if err := p.client.Notify("initialized", map[string]any{}); err != nil {
		return p.failStartLocked(fmt.Errorf("%w: codex initialized notification: %v", agent.ErrProviderUnavailable, err))
	}
	p.started = true
	// Freeze the layer the child actually received. maps.Clone defends against
	// a caller mutating its own Spec.Env map after the Spawn returns, which
	// would otherwise make a later divergence check compare against a value the
	// child never saw. A nil layer clones to nil and stays a distinct pin.
	p.pinnedSessionEnv = maps.Clone(sessionEnv)
	p.sessionEnvPinned = true
	return nil
}

func (p *Provider) failStartLocked(err error) error {
	if excerpt := p.appServerStderr.Excerpt(); excerpt != "" {
		err = fmt.Errorf("%w (app-server stderr: %s)", err, excerpt)
	}
	p.startErr = errors.Join(err, p.terminateLocked(context.Background()))
	return p.startErr
}

// Name implements agent.Provider.
func (p *Provider) Name() agent.ProviderName { return agent.ProviderCodex }

// Capabilities implements agent.Provider. The values are locked per
// F.1.1 §3.2.
func (p *Provider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		SupportsMessageInjection:            false, // codex CLI cannot inject mid-session
		SupportsSessionResume:               true,
		SupportsToolPlugins:                 true,
		NeedsBaseInstructions:               true,
		NeedsPermissionConfig:               true,
		SupportsCodeIntelligenceEnforcement: false,
		EmitsSubagentEvents:                 false,
		SupportsReasoningEffort:             true,
		ToolPermissionFormat:                "codex",
		// Tool-use surface (002 v2):
		//   - MCPServers IS wired via `config/batchWrite` mcp_servers
		//     keyPath (see spec_translation.go::mcpServersConfig).
		//   - AllowedTools is NOT wired: codex routes per-tool
		//     permission through the approval-bridge grammar
		//     (Spec.PermissionConfig). Flat allow/deny lists are
		//     dropped with a SpecFieldNote in NewSpawnPlan.
		AcceptsAllowedToolsList: false,
		AcceptsMcpServerSpec:    true,
		HumanLabel:              "Codex",
		// Codex re-includes thread-level developer/base instructions in the
		// model prompt on every internal turn; turn/start input is sent
		// once and then lives in cached conversation history. Declaring
		// SupportsTurnInputContext lets the runner route large, volatile
		// context (recalled agent memory) through Spec.InitialContext so
		// it rides the first turn's input instead of inflating the
		// re-sent instruction prefix.
		SupportsTurnInputContext: true,
	}
}

// Spawn implements agent.Provider. Translates the Spec to JSON-RPC
// params, acquires the exact MCP server config for this session, opens
// a thread, and starts the first turn.
func (p *Provider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	var err error
	spec, err = agent.PrepareHarness(spec, p.Manifest())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	// Interactive spawn mode (W4): independent of this Provider's shared
	// headless app-server state — see interactive.go.
	// Capability-gated on the live manifest so a future edit that flips
	// SupportsInteractivePTY back to false silently falls through to the
	// headless app-server path instead of a hardcoded branch always firing.
	if spec.Interactive != nil && p.Manifest().Caps.SupportsInteractivePTY {
		return spawnInteractivePrepared(ctx, p.opts, spec)
	}

	if err := p.ensureHeadlessReady(spec); err != nil {
		return nil, err
	}
	if err := p.checkAlive(); err != nil {
		return nil, err
	}
	if err := validateCodexCLIMCPServers(spec.MCPServers); err != nil {
		return nil, fmt.Errorf("%w: codex configure mcp servers: %w", agent.ErrSpawnFailed, persistHeadlessMCPApplicationDenial(spec, err))
	}
	plan := NewSpawnPlan(spec)
	releaseMCP, err := p.acquireMCPConfig(ctx, plan.MCPConfig, spec.Cwd)
	if err != nil {
		return nil, fmt.Errorf("%w: codex configure mcp servers: %w", agent.ErrSpawnFailed, persistHeadlessMCPApplicationDenial(spec, err))
	}

	h := newHandle(p, p.client, spec, HandleOptions{
		RPCTimeout: p.opts.RPCTimeout,
	})
	h.mcpRelease = releaseMCP
	p.registerHandle(h)

	if err := h.start(ctx, plan, ""); err != nil {
		p.unregisterHandle(h)
		cleanupErr := h.releaseMCPConfig()
		return nil, errors.Join(fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err), cleanupErr)
	}

	if spec.OnProcessSpawned != nil && p.cmd != nil && p.cmd.Process != nil {
		spec.OnProcessSpawned(p.cmd.Process.Pid)
	}
	return h, nil
}

// Resume implements agent.Provider.
func (p *Provider) Resume(ctx context.Context, sessionID string, spec agent.Spec) (agent.Handle, error) {
	var err error
	spec, err = agent.PrepareHarness(spec, p.Manifest())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	if err := p.ensureHeadlessReady(spec); err != nil {
		return nil, err
	}
	if err := p.checkAlive(); err != nil {
		return nil, err
	}
	if err := validateCodexCLIMCPServers(spec.MCPServers); err != nil {
		return nil, fmt.Errorf("%w: codex configure mcp servers: %w", agent.ErrSpawnFailed, persistHeadlessMCPApplicationDenial(spec, err))
	}
	if sessionID == "" {
		return nil, agent.ErrSessionNotFound
	}
	plan := NewSpawnPlan(spec)
	releaseMCP, err := p.acquireMCPConfig(ctx, plan.MCPConfig, spec.Cwd)
	if err != nil {
		return nil, fmt.Errorf("%w: codex configure mcp servers: %w", agent.ErrSpawnFailed, persistHeadlessMCPApplicationDenial(spec, err))
	}

	h := newHandle(p, p.client, spec, HandleOptions{
		RPCTimeout: p.opts.RPCTimeout,
	})
	h.mcpRelease = releaseMCP
	p.registerHandle(h)

	if err := h.start(ctx, plan, sessionID); err != nil {
		p.unregisterHandle(h)
		cleanupErr := h.releaseMCPConfig()
		return nil, errors.Join(fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err), cleanupErr)
	}

	if spec.OnProcessSpawned != nil && p.cmd != nil && p.cmd.Process != nil {
		spec.OnProcessSpawned(p.cmd.Process.Pid)
	}
	return h, nil
}

// ensureHeadlessReady performs credential delivery at the selected headless
// spawn boundary, never while the registry is merely probing constructors.
// App-server initialization shares the same lifecycle lock and happens after
// the link so Codex cannot cache an unauthenticated startup state.
//
// spec is the prepared session spec: its Env is the runner-owned environment
// layer for this session and is overlaid on the inherited parent before the
// child is started (startLocked). Callers MUST pass the prepared spec — the
// overlay is the only thing that keeps an ambient DONMAI_API_URL from
// outranking the runner's canonical platform origin inside the agent.
//
// The overlay only applies at start, so this is also where the one-session
// invariant is enforced: a divergent later layer is refused BEFORE the host
// auth link and before startLocked, so a denied spawn leaves no side effect.
func (p *Provider) ensureHeadlessReady(spec agent.Spec) error {
	p.startMu.Lock()
	defer p.startMu.Unlock()
	select {
	case <-p.shutdown:
		return fmt.Errorf("%w: %w: codex provider already shut down", agent.ErrSpawnFailed, agent.ErrProviderUnavailable)
	default:
	}
	if err := p.checkSessionEnvLocked(spec.Env); err != nil {
		return err
	}
	if p.hostAuthFile != "" {
		if err := p.config.linkHostSessionAuth(p.hostAuthFile); err != nil {
			return fmt.Errorf("%w: codex host-session auth: %w", agent.ErrSpawnFailed, err)
		}
	}
	if err := p.startLocked(spec.Env); err != nil {
		return fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	return nil
}

// Shutdown implements agent.Provider. Idempotent.
func (p *Provider) Shutdown(ctx context.Context) error {
	return p.terminate(ctx)
}

// acquireMCPConfig reserves the app-server-global MCP config for one handle.
// The write targets only this Provider's private CODEX_HOME/config.toml and is
// read back and proved initialized before thread/start. Different configs
// never overlap; the last release replaces the config with an empty baseline
// and waits for every server previously managed by this Provider to leave the
// inventory. Codex-owned ambient servers are outside this config's authority.
func (p *Provider) acquireMCPConfig(ctx context.Context, mcpConfig map[string]any, cwd string) (func() error, error) {
	desired := mcpConfig
	if desired == nil {
		desired = map[string]any{}
	}
	encoded, err := json.Marshal(desired)
	if err != nil {
		return nil, codexMCPApplicationError("could not canonicalize requested MCP config")
	}
	digest := sha256.Sum256(encoded)

	p.mcpMu.Lock()
	defer p.mcpMu.Unlock()
	if p.mcpPoisoned {
		return nil, codexMCPApplicationError("isolated MCP configuration is no longer safe to reuse")
	}

	changed := !p.mcpConfigKnown || digest != p.mcpConfigDigest
	if changed && p.mcpUsers > 0 {
		return nil, codexMCPApplicationError("requested MCP config conflicts with a live app-server session")
	}

	// The first request always writes, even when empty. That explicit baseline
	// plus exact read-back proves the process's user-config MCP set; the status
	// inventory may separately include Codex-owned ambient servers.
	if changed {
		if err := p.applyMCPConfig(ctx, desired, cwd, true); err != nil {
			return nil, p.poisonMCPApplication(err)
		}
	} else if err := p.verifyMCPConfig(ctx, desired, cwd, nil); err != nil {
		return nil, p.poisonMCPApplication(err)
	}
	p.mcpConfigDigest = digest
	p.mcpConfigKnown = true
	p.mcpManagedNames = mcpServerNames(desired)
	p.mcpUsers++

	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			p.mcpMu.Lock()
			defer p.mcpMu.Unlock()
			if p.mcpUsers > 0 {
				p.mcpUsers--
			}
			if p.mcpUsers == 0 {
				if p.client == nil || p.client.CloseErr() != nil {
					p.mcpConfigKnown = false
					if err := p.config.remove(); err != nil {
						p.mcpPoisoned = true
						releaseErr = codexMCPApplicationError("destroy isolated MCP config after app-server exit")
					}
					return
				}
				cleanupCtx, cancel := context.WithTimeout(context.Background(), min(p.opts.RPCTimeout, mcpConfigCleanupTimeout))
				err := p.applyMCPConfig(cleanupCtx, map[string]any{}, cwd, true)
				cancel()
				if err != nil {
					p.mcpConfigKnown = false
					p.mcpPoisoned = true
					_ = p.config.remove()
					releaseErr = codexMCPApplicationError("clear isolated MCP config: " + codexMCPConfigFailureDetail(err))
				} else {
					empty, _ := json.Marshal(map[string]any{})
					p.mcpConfigDigest = sha256.Sum256(empty)
					p.mcpConfigKnown = true
					p.mcpManagedNames = map[string]struct{}{}
				}
			}
		})
		return releaseErr
	}, nil
}

// poisonMCPApplication handles an apply or activation-proof failure while
// mcpMu is held. No session may start from an unproved config. When no earlier
// lease exists, destroying the private home also eliminates any write that may
// have succeeded before config/read failed. Existing leases retain the home
// until their final release can attempt an explicit clear.
func (p *Provider) poisonMCPApplication(applicationErr error) error {
	p.mcpConfigKnown = false
	p.mcpPoisoned = true
	detail := codexMCPConfigFailureDetail(applicationErr)
	if p.mcpUsers == 0 && p.config != nil {
		if err := p.config.remove(); err != nil {
			detail += "; isolated config destruction failed"
		}
	}
	return codexMCPApplicationError(detail)
}

func (p *Provider) applyMCPConfig(ctx context.Context, desired map[string]any, cwd string, verify bool) error {
	if p.config == nil {
		return errors.New("isolated Codex config boundary is missing")
	}
	if err := p.config.validate(); err != nil {
		return err
	}
	params := map[string]any{
		"filePath":         p.config.configPath,
		"reloadUserConfig": true,
		"edits": []map[string]any{{
			"keyPath": codexMCPConfigKeyPath, "mergeStrategy": "replace", "value": desired,
		}},
	}
	if _, err := p.client.RequestWithRetry(ctx, "config/batchWrite", params, p.opts.RPCTimeout); err != nil {
		return err
	}
	if !verify || (p.opts.skipProcess && !p.opts.verifyMCPReadback) {
		return nil
	}
	return p.verifyMCPConfig(ctx, desired, cwd, retiredMCPServerNames(p.mcpManagedNames, desired))
}

func (p *Provider) verifyMCPConfig(ctx context.Context, desired map[string]any, cwd string, retired map[string]struct{}) error {
	if p.opts.skipProcess && !p.opts.verifyMCPReadback {
		return nil
	}
	params := map[string]any{"includeLayers": true}
	if cwd != "" {
		params["cwd"] = cwd
	}
	raw, err := p.client.RequestWithRetry(ctx, "config/read", params, p.opts.RPCTimeout)
	if err != nil {
		return err
	}
	if !mcpConfigReadbackMatches(raw, desired) {
		return errors.New("config/read did not confirm the requested MCP set")
	}
	return p.waitForMCPActivation(ctx, desired, retired)
}

func mcpConfigReadbackMatches(raw json.RawMessage, desired map[string]any) bool {
	var response struct {
		Config map[string]any `json:"config"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Config == nil {
		return false
	}
	active, ok := response.Config[codexMCPConfigKeyPath].(map[string]any)
	if !ok {
		return false
	}
	if len(active) != len(desired) {
		return false
	}
	for name, wantRaw := range desired {
		want, wantOK := wantRaw.(map[string]any)
		got, gotOK := active[name].(map[string]any)
		if !wantOK || !gotOK || !mcpServerReadbackMatches(got, want) {
			return false
		}
	}
	return true
}

func mcpServerReadbackMatches(got, want map[string]any) bool {
	for _, key := range []string{"command", "url", "default_tools_approval_mode"} {
		if value, exists := want[key]; exists && got[key] != value {
			return false
		}
	}
	for _, key := range []string{"args", "env", "http_headers"} {
		if value, exists := want[key]; exists && !jsonValuesEqual(got[key], value) {
			return false
		}
	}
	return true
}

func jsonValuesEqual(a, b any) bool {
	left, errLeft := json.Marshal(a)
	right, errRight := json.Marshal(b)
	return errLeft == nil && errRight == nil && string(left) == string(right)
}

// codexMCPConfigFailureDetail intentionally excludes the server's message and
// all request values. App servers may echo rejected configuration in an RPC
// error; only a bounded code/class is safe to surface or persist.
func codexMCPConfigFailureDetail(err error) string {
	var activationDeadline *mcpActivationDeadlineError
	if errors.As(err, &activationDeadline) {
		return activationDeadline.Error()
	}
	var rpc *RPCError
	if errors.As(err, &rpc) {
		method := "config RPC"
		if rpc.Method == "config/batchWrite" || rpc.Method == "config/read" || rpc.Method == "mcpServerStatus/list" {
			method = rpc.Method
		}
		return fmt.Sprintf("%s rejected by JSON-RPC code %d", method, rpc.Code)
	}
	if errors.Is(err, context.Canceled) {
		return "isolated config activation canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "isolated config activation deadline exceeded"
	}
	return "isolated config activation verification failed"
}

func codexMCPApplicationError(detail string) *agent.ToolAdaptationError {
	return &agent.ToolAdaptationError{
		Code:    agent.ToolDenialApplicationFailed,
		Channel: agent.ToolChannelMCPServer,
		Detail:  detail,
	}
}

func persistHeadlessMCPApplicationDenial(spec agent.Spec, applicationErr error) error {
	receipt := agent.ToolLifecycleReceipt{
		ContractVersion: agent.ToolLifecycleContractVersion,
		ProfileID:       "codex/headless/tool-lifecycle-v1",
		Decision:        "denied",
		EvidenceTier:    "unit_verified",
		Entries: []agent.ToolLifecycleEntry{{
			ID:         "mcp-servers",
			Channel:    agent.ToolChannelMCPServer,
			Required:   true,
			Outcome:    agent.ToolOutcomeDenied,
			DenialCode: agent.ToolDenialApplicationFailed,
		}},
	}
	if spec.OnToolLifecycleAdapted != nil {
		if err := spec.OnToolLifecycleAdapted(receipt); err != nil {
			return codexMCPApplicationError("persist denied Codex app-server MCP receipt: " + err.Error())
		}
	}
	return applicationErr
}

func (p *Provider) registerHandle(h *Handle) {
	p.handlesMu.Lock()
	p.handles[h] = struct{}{}
	p.handlesMu.Unlock()
}

func (p *Provider) unregisterHandle(h *Handle) {
	p.handlesMu.Lock()
	delete(p.handles, h)
	p.handlesMu.Unlock()
}

// onClientClose is wired to Client.SetOnClose. Fired exactly once
// when the JSON-RPC read loop exits.
func (p *Provider) onClientClose(cause error) {
	p.handlesMu.Lock()
	live := make([]*Handle, 0, len(p.handles))
	for h := range p.handles {
		live = append(live, h)
	}
	p.handles = map[*Handle]struct{}{}
	p.handlesMu.Unlock()

	if cause == nil {
		cause = errors.New("codex app-server stream closed")
	}
	// Attach the excerpt here, at the one place every live Handle's failure
	// actually flows through, rather than at whichever of watchExit's
	// p.client.Stop(cause) or the JSON-RPC read loop's own EOF-triggered
	// Stop happens to win the race to set the close cause (both call Stop;
	// only the first writes p.client.closeErr). Enriching centrally means
	// the excerpt reaches h.failNow's ErrorEvent regardless of which cause
	// won.
	if excerpt := p.appServerStderr.Excerpt(); excerpt != "" {
		cause = fmt.Errorf("%w (app-server stderr: %s)", cause, excerpt)
	}
	for _, h := range live {
		h.failNow(cause)
	}
	if p.config != nil {
		_ = p.config.remove()
	}
}

// watchExit observes the codex subprocess exit and stops the client
// so live Handles get failed via onClientClose.
func (p *Provider) watchExit() {
	if p.cmd == nil {
		return
	}
	err := p.cmd.Wait()
	if p.processDone != nil {
		p.processDone <- err
	}
	p.logAppServerExit(err)
	cause := err
	if cause == nil {
		cause = errors.New("codex app-server exited")
	}
	if p.client != nil {
		p.client.Stop(cause)
	}
}

// logAppServerExit emits exactly one structured line whenever the shared
// headless app-server process exits, regardless of whether the exit looks
// clean. A zero exit code is not proof nothing went wrong: a downstream
// consumer that only observes a dropped connection can exit 0 itself while
// the app-server's own exit — logged here with whatever redacted stderr
// survived — is sometimes the only place the real cause is ever recorded.
// See appserver_stderr.go.
func (p *Provider) logAppServerExit(waitErr error) {
	level := slog.LevelWarn
	select {
	case <-p.shutdown:
		level = slog.LevelInfo // Shutdown initiated this exit; expected.
	default:
	}
	args := []any{"error", waitErr}
	if excerpt := p.appServerStderr.Excerpt(); excerpt != "" {
		args = append(args, "stderrExcerpt", excerpt)
	}
	slog.Log(context.Background(), level, "codex: app-server process exited", args...)
}

// terminate is the internal shutdown path. Idempotent.
func (p *Provider) terminate(ctx context.Context) error {
	p.startMu.Lock()
	defer p.startMu.Unlock()
	return p.terminateLocked(ctx)
}

// terminateLocked serializes cleanup with deferred app-server startup.
// startMu must be held.
func (p *Provider) terminateLocked(ctx context.Context) error {
	var rerr error
	p.closeOnce.Do(func() {
		close(p.shutdown)

		if p.client != nil {
			p.client.Stop(errors.New("codex provider shutting down"))
		}

		if p.cmd != nil && p.cmd.Process != nil {
			// SIGTERM first, then force-kill after a grace
			// period. Mirrors the legacy TS performShutdown.
			_ = p.cmd.Process.Signal(syscallSIGTERM())
			grace := 5 * time.Second
			if dl, ok := ctx.Deadline(); ok {
				if remaining := time.Until(dl); remaining < grace && remaining > 0 {
					grace = remaining
				}
			}
			select {
			case <-p.processDone:
			case <-time.After(grace):
				_ = p.cmd.Process.Kill()
				<-p.processDone
			}
		}
		if p.stdin != nil {
			_ = p.stdin.Close()
		}
		if p.stdout != nil {
			_ = p.stdout.Close()
		}
		if p.stderr != nil {
			_ = p.stderr.Close()
		}
		if p.config != nil {
			rerr = errors.Join(rerr, p.config.remove())
		}
	})
	return rerr
}

// checkAlive returns ErrProviderUnavailable if the app-server has
// already terminated (and Shutdown has not yet run cleanup).
func (p *Provider) checkAlive() error {
	select {
	case <-p.shutdown:
		return fmt.Errorf("%w: codex provider already shut down", agent.ErrProviderUnavailable)
	default:
	}
	if p.client != nil {
		if err := p.client.CloseErr(); err != nil {
			if excerpt := p.appServerStderr.Excerpt(); excerpt != "" {
				err = fmt.Errorf("%w (app-server stderr: %s)", err, excerpt)
			}
			return fmt.Errorf("%w: %v", agent.ErrProviderUnavailable, err)
		}
	}
	return nil
}

// resolveCodexBinary applies the shared CodexBin → $CODEX_BIN → "codex"
// fallback chain and resolves the result via exec.LookPath. Used by New (the
// app-server subprocess) and by SpawnInteractive (interactive.go, the PTY
// spawn mode) — both need the same codex binary, resolved the same way, even
// though they never share the Provider's headless process. Returns the raw LookPath error
// (unwrapped by any agent sentinel) so each call site can wrap it with the
// sentinel appropriate to when the resolution happens: New's failure is
// probe-time (agent.ErrProviderUnavailable); SpawnInteractive's is per-Spawn
// (agent.ErrSpawnFailed).
func resolveCodexBinary(bin string) (string, error) {
	if bin == "" {
		bin = os.Getenv("CODEX_BIN")
	}
	if bin == "" {
		bin = "codex"
	}
	full, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("codex binary %q not on PATH (install: brew install codex or follow https://developers.openai.com/codex/): %w", bin, err)
	}
	return full, nil
}

// mergeEnv composes the app-server child environment.
//
// Layer order (later wins under exec.Cmd's last-entry-wins semantics):
//
//  1. the inherited parent environment, minus runner-only attach controls and
//     the agent-auth blocklist (runtime/env.ComposeChildEnv);
//  2. extra — Options.Env, the construction-time layer;
//  3. session — the per-session agent.Spec.Env the runner composed for THIS
//     session. It is the canonical owner of session routing values such as the
//     platform API origin, so it must beat any ambient copy;
//  4. the Provider's owned CODEX_HOME, which no caller may override.
func mergeEnv(extra, session map[string]string, codexHome string) []string {
	return runtimeenv.ComposeChildEnv(os.Environ(), extra, session, map[string]string{"CODEX_HOME": codexHome})
}

// errSessionEnvConflict marks a headless Spawn/Resume whose per-session
// environment layer materially differs from the layer already frozen into the
// running app-server child.
//
// Layer 3 of mergeEnv is applied exactly once, at process start. Serving a
// second, differently-composed session from the same child would hand it the
// FIRST session's DONMAI_SESSION_ID and DONMAI_API_URL while every log line and
// receipt claimed otherwise — a silent misroute, which is the same class of bug
// as the ambient origin this provider now overlays away. One codex Provider is
// built per `donmai agent run` process (afcli/agent_run.go), so in production
// this never fires; it exists so an embedder that pools Providers across
// sessions gets a loud failure instead of a wrong answer.
var errSessionEnvConflict = errors.New("codex app-server is already bound to a different session environment")

// checkSessionEnvLocked refuses a session whose environment layer the running
// app-server child cannot actually be carrying. startMu must be held.
//
// Before any start there is nothing to conflict with, so the first caller
// always passes and its layer becomes the pin. Equal layers pass forever after,
// which is what keeps same-session Resume working: the runner recomposes an
// identical Spec.Env from the same QueuedWork.
func (p *Provider) checkSessionEnvLocked(sessionEnv map[string]string) error {
	if !p.sessionEnvPinned {
		return nil
	}
	diverged := divergentSessionEnvKeys(p.pinnedSessionEnv, sessionEnv)
	if len(diverged) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w: %s", agent.ErrSpawnFailed, errSessionEnvConflict, strings.Join(diverged, ", "))
}

// divergentSessionEnvKeys returns the sorted names of the keys that are added,
// removed, or changed between the pinned layer and want. A nil and an empty map
// are the same layer — both mean "no session env" — so a provider started
// without one is not spuriously in conflict with a spec that carries an empty
// map.
//
// It returns NAMES ONLY, never values. The caller renders these into an error
// that reaches stderr, structured logs and session records, and this layer is
// exactly where the runner puts WORKER_AUTH_TOKEN. Naming the keys
// is what makes the failure actionable; quoting them would make it a leak.
func divergentSessionEnvKeys(pinned, want map[string]string) []string {
	var keys []string
	for key, pinnedValue := range pinned {
		if wantValue, ok := want[key]; !ok || wantValue != pinnedValue {
			keys = append(keys, key)
		}
	}
	for key := range want {
		if _, ok := pinned[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}
