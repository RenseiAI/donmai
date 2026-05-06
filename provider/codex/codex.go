package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
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

	cmd    *exec.Cmd
	client *Client
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	mcpOnce      sync.Once
	mcpConfigErr error

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
}

// New constructs the Provider, spawning the codex app-server
// subprocess and completing the JSON-RPC initialize handshake. Returns
// agent.ErrProviderUnavailable wrapped with context if the binary is
// missing or the handshake fails.
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

	p := &Provider{
		opts:     opts,
		shutdown: make(chan struct{}),
		handles:  make(map[*Handle]struct{}),
	}

	if opts.skipProcess {
		// Test path: caller wired stdin/stdout via overrides.
		p.stdin = opts.stdinOverride
		p.stdout = opts.stdoutOverride
		p.stderr = opts.stderrOverride
	} else {
		bin := opts.CodexBin
		if bin == "" {
			bin = os.Getenv("CODEX_BIN")
		}
		if bin == "" {
			bin = "codex"
		}
		full, err := exec.LookPath(bin)
		if err != nil {
			return nil, fmt.Errorf("%w: codex binary %q not on PATH (install: brew install codex or follow https://developers.openai.com/codex/)", agent.ErrProviderUnavailable, bin)
		}
		// nolint:gosec // bin is sourced from explicit Options/env, not user input.
		cmd := exec.Command(full, opts.Args...)
		cmd.Dir = opts.Cwd
		cmd.Env = mergeEnv(opts.Env)
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
		go drainStderr(stderr)
		go p.watchExit()
	}

	p.client = NewClient(p.stdin, p.stdout)
	p.client.SetOnClose(p.onClientClose)

	// Initialize handshake.
	hctx, cancel := context.WithTimeout(context.Background(), opts.HandshakeTimeout)
	defer cancel()
	if _, err := p.client.Request(hctx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "agentfactory",
			"title":   "AgentFactory Orchestrator",
			"version": "0.5.0",
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	}, opts.HandshakeTimeout); err != nil {
		_ = p.terminate(context.Background())
		return nil, fmt.Errorf("%w: codex initialize handshake: %v", agent.ErrProviderUnavailable, err)
	}
	if err := p.client.Notify("initialized", map[string]any{}); err != nil {
		_ = p.terminate(context.Background())
		return nil, fmt.Errorf("%w: codex initialized notification: %v", agent.ErrProviderUnavailable, err)
	}
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
		//   - MCPServers IS wired via `config/batchWrite` mcpServers
		//     keyPath (see spec_translation.go::mcpServersConfig).
		//   - AllowedTools is NOT wired: codex routes per-tool
		//     permission through the approval-bridge grammar
		//     (Spec.PermissionConfig). Flat allow/deny lists are
		//     dropped with a SpecFieldNote in NewSpawnPlan.
		AcceptsAllowedToolsList: false,
		AcceptsMcpServerSpec:    true,
		HumanLabel:              "Codex",
	}
}

// Spawn implements agent.Provider. Translates the Spec to JSON-RPC
// params, pushes MCP server config (once per Provider instance), opens
// a thread, and starts the first turn.
func (p *Provider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	if err := p.checkAlive(); err != nil {
		return nil, err
	}
	plan := NewSpawnPlan(spec)
	if err := p.configureMCP(ctx, plan.MCPConfig); err != nil {
		return nil, fmt.Errorf("codex: configure mcp servers: %w", err)
	}

	h := newHandle(p, p.client, spec, HandleOptions{
		RPCTimeout: p.opts.RPCTimeout,
	})
	p.registerHandle(h)

	if err := h.start(ctx, plan, ""); err != nil {
		p.unregisterHandle(h)
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}

	if spec.OnProcessSpawned != nil && p.cmd != nil && p.cmd.Process != nil {
		spec.OnProcessSpawned(p.cmd.Process.Pid)
	}
	return h, nil
}

// Resume implements agent.Provider.
func (p *Provider) Resume(ctx context.Context, sessionID string, spec agent.Spec) (agent.Handle, error) {
	if err := p.checkAlive(); err != nil {
		return nil, err
	}
	if sessionID == "" {
		return nil, agent.ErrSessionNotFound
	}
	plan := NewSpawnPlan(spec)
	if err := p.configureMCP(ctx, plan.MCPConfig); err != nil {
		return nil, fmt.Errorf("codex: configure mcp servers: %w", err)
	}

	h := newHandle(p, p.client, spec, HandleOptions{
		RPCTimeout: p.opts.RPCTimeout,
	})
	p.registerHandle(h)

	if err := h.start(ctx, plan, sessionID); err != nil {
		p.unregisterHandle(h)
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
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

// configureMCP pushes the MCP server config once per Provider via
// `config/batchWrite`. Subsequent calls are no-ops; the first call's
// error is cached (matches mcpConfigured semantics in the legacy TS).
func (p *Provider) configureMCP(ctx context.Context, mcpConfig map[string]any) error {
	if mcpConfig == nil {
		return nil
	}
	p.mcpOnce.Do(func() {
		_, err := p.client.RequestWithRetry(ctx, "config/batchWrite", map[string]any{
			"edits": []map[string]any{
				{"keyPath": "mcpServers", "mergeStrategy": "replace", "value": mcpConfig},
			},
		}, p.opts.RPCTimeout)
		// Some codex versions do not implement config/batchWrite.
		// Treat -32601 (Method not found) as a soft failure so the
		// Provider still works against older builds — sessions just
		// run without tool plugins. The caller decides whether to
		// hard-fail.
		if err != nil {
			var rpc *RPCError
			if errors.As(err, &rpc) && rpc.Code == -32601 {
				return
			}
			p.mcpConfigErr = err
		}
	})
	return p.mcpConfigErr
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
}

// watchExit observes the codex subprocess exit and stops the client
// so live Handles get failed via onClientClose.
func (p *Provider) watchExit() {
	if p.cmd == nil {
		return
	}
	err := p.cmd.Wait()
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
			done := make(chan error, 1)
			go func() { done <- p.cmd.Wait() }()
			grace := 5 * time.Second
			if dl, ok := ctx.Deadline(); ok {
				if remaining := time.Until(dl); remaining < grace && remaining > 0 {
					grace = remaining
				}
			}
			select {
			case <-done:
			case <-time.After(grace):
				_ = p.cmd.Process.Kill()
				<-done
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

func mergeEnv(extra map[string]string) []string {
	parent := os.Environ()
	if len(extra) == 0 {
		return parent
	}
	// Build a map from the parent env so extra keys override
	// existing values.
	merged := make(map[string]string, len(parent)+len(extra))
	for _, e := range parent {
		for i := 0; i < len(e); i++ {
			if e[i] == '=' {
				merged[e[:i]] = e[i+1:]
				break
			}
		}
	}
	for k, v := range extra {
		merged[k] = v
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
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
