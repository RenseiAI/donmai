package codex

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

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

	cmd         *exec.Cmd
	client      *Client
	stdin       io.WriteCloser
	stdout      io.ReadCloser
	stderr      io.ReadCloser
	config      *codexConfigBoundary
	processDone chan error

	// mcpMu serializes app-server-global config changes. mcpUsers counts
	// live handles holding the current config digest: equal configs may share
	// it, while an incompatible config is denied until all prior handles have
	// closed so no session can observe another session's MCP set.
	mcpMu           sync.Mutex
	mcpConfigDigest [sha256.Size]byte
	mcpConfigKnown  bool
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
	// config/read activation proof. Real processes always verify it.
	verifyMCPReadback bool
	configTempDir     string
}

// New constructs the Provider, spawning the codex app-server
// subprocess and completing the JSON-RPC initialize handshake. Returns
// agent.ErrProviderUnavailable wrapped with context if the binary is
// missing or the handshake fails.
func New(opts Options) (_ *Provider, resultErr error) {
	if opts.HandshakeTimeout == 0 {
		opts.HandshakeTimeout = 30 * time.Second
	}
	if opts.RPCTimeout == 0 {
		opts.RPCTimeout = 30 * time.Second
	}
	if len(opts.Args) == 0 {
		opts.Args = []string{"app-server"}
	}

	p := &Provider{
		opts:     opts,
		shutdown: make(chan struct{}),
		handles:  make(map[*Handle]struct{}),
	}
	boundary, err := newCodexConfigBoundary(opts.configTempDir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrProviderUnavailable, err)
	}
	p.config = boundary
	constructed := false
	defer func() {
		if !constructed {
			resultErr = errors.Join(resultErr, boundary.remove())
		}
	}()

	if opts.skipProcess {
		// Test path: caller wired stdin/stdout via overrides.
		p.stdin = opts.stdinOverride
		p.stdout = opts.stdoutOverride
		p.stderr = opts.stderrOverride
	} else {
		full, err := resolveCodexBinary(opts.CodexBin)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", agent.ErrProviderUnavailable, err)
		}
		// nolint:gosec // bin is sourced from explicit Options/env, not user input.
		cmd := exec.Command(full, opts.Args...)
		cmd.Dir = opts.Cwd
		cmd.Env = mergeEnv(opts.Env, boundary.home)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("%w: codex stdin pipe: %v", agent.ErrProviderUnavailable, err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("%w: codex stdout pipe: %v", agent.ErrProviderUnavailable, err)
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return nil, fmt.Errorf("%w: codex stderr pipe: %v", agent.ErrProviderUnavailable, err)
		}
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("%w: codex spawn: %v", agent.ErrProviderUnavailable, err)
		}
		p.cmd = cmd
		p.stdin = stdin
		p.stdout = stdout
		p.stderr = stderr
		// Drain stderr to a sink so the child does not deadlock
		// when the buffer fills. Logs go to the parent's stderr.
		p.processDone = make(chan error, 1)
		go drainStderr(stderr)
	}

	p.client = NewClient(p.stdin, p.stdout)
	p.client.SetOnClose(p.onClientClose)
	if p.cmd != nil {
		go p.watchExit()
	}

	// Initialize handshake.
	hctx, cancel := context.WithTimeout(context.Background(), opts.HandshakeTimeout)
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
	}, opts.HandshakeTimeout)
	if err != nil {
		_ = p.terminate(context.Background())
		return nil, fmt.Errorf("%w: codex initialize handshake: %v", agent.ErrProviderUnavailable, err)
	}
	if !opts.skipProcess {
		var initResp struct {
			CodexHome string `json:"codexHome"`
		}
		if err := json.Unmarshal(initRaw, &initResp); err != nil || initResp.CodexHome == "" || !sameResolvedPath(initResp.CodexHome, boundary.home) {
			_ = p.terminate(context.Background())
			return nil, fmt.Errorf("%w: codex app-server did not confirm its isolated config home", agent.ErrProviderUnavailable)
		}
	}
	if err := p.client.Notify("initialized", map[string]any{}); err != nil {
		_ = p.terminate(context.Background())
		return nil, fmt.Errorf("%w: codex initialized notification: %v", agent.ErrProviderUnavailable, err)
	}
	constructed = true
	return p, nil
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
	// Interactive spawn mode (W4): completely independent of the app-server
	// JSON-RPC subprocess this Provider otherwise drives — see interactive.go.
	// Capability-gated on the live manifest so a future edit that flips
	// SupportsInteractivePTY back to false silently falls through to the
	// headless app-server path instead of a hardcoded branch always firing.
	if spec.Interactive != nil && p.Manifest().Caps.SupportsInteractivePTY {
		return SpawnInteractive(ctx, p.opts, spec)
	}
	var err error
	spec, err = agent.PrepareHarness(spec, p.Manifest())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
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

// Shutdown implements agent.Provider. Idempotent.
func (p *Provider) Shutdown(ctx context.Context) error {
	return p.terminate(ctx)
}

// acquireMCPConfig reserves the app-server-global MCP config for one handle.
// The write targets only this Provider's private CODEX_HOME/config.toml and is
// read back before thread/start. Different configs never overlap; the last
// release replaces the config with an empty baseline.
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
	// plus read-back is what proves ambient MCP did not enter this process.
	if changed {
		if err := p.applyMCPConfig(ctx, desired, cwd, true); err != nil {
			return nil, p.poisonMCPApplication(err)
		}
	} else if err := p.verifyMCPConfig(ctx, desired, cwd); err != nil {
		return nil, p.poisonMCPApplication(err)
	}
	p.mcpConfigDigest = digest
	p.mcpConfigKnown = true
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
				cleanupCtx, cancel := context.WithTimeout(context.Background(), min(p.opts.RPCTimeout, 2*time.Second))
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
	return p.verifyMCPConfig(ctx, desired, cwd)
}

func (p *Provider) verifyMCPConfig(ctx context.Context, desired map[string]any, cwd string) error {
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
	return nil
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
	for _, key := range []string{"command", "url"} {
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
	var rpc *RPCError
	if errors.As(err, &rpc) {
		method := "config RPC"
		if rpc.Method == "config/batchWrite" || rpc.Method == "config/read" {
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
	cause := err
	if cause == nil {
		cause = errors.New("codex app-server exited")
	}
	if p.client != nil {
		p.client.Stop(cause)
	}
}

// terminate is the internal shutdown path. Idempotent.
func (p *Provider) terminate(ctx context.Context) error {
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
			return fmt.Errorf("%w: %v", agent.ErrProviderUnavailable, err)
		}
	}
	return nil
}

// resolveCodexBinary applies the shared CodexBin → $CODEX_BIN → "codex"
// fallback chain and resolves the result via exec.LookPath. Used by New (the
// app-server subprocess) and by SpawnInteractive (interactive.go, the bare
// TUI spawn mode) — both need the same codex binary, resolved the same way,
// even though they never share a process. Returns the raw LookPath error
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

func mergeEnv(extra map[string]string, codexHome string) []string {
	return runtimeenv.ComposeChildEnv(os.Environ(), extra, map[string]string{"CODEX_HOME": codexHome})
}

func drainStderr(r io.ReadCloser) {
	defer func() { _ = r.Close() }()
	buf := make([]byte, 4096)
	for {
		_, err := r.Read(buf)
		if err != nil {
			return
		}
		// Discard. Future iteration could plumb this to slog if
		// useful for codex debugging.
	}
}
