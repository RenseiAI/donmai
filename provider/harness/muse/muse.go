package muse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// noAutoUpdateEnv pins the serve child's update check off. The host
// must never self-update under a running session.
const noAutoUpdateEnv = "MUSE_NO_AUTO_UPDATE"

// apiKeyEnv is stripped from the serve child's environment unless the
// Spec sets it: the key takes priority over the host login, so an
// ambient key would silently reroute auth.
const apiKeyEnv = "META_API_KEY" //nolint:gosec // G101: an env-var NAME, not a credential.

// Provider is the agent.Provider implementation backed by one
// `muse serve` subprocess per session. Unlike codex (one shared
// app-server, N threads), each Spawn starts a fresh host and each
// Handle owns it for the session lifetime.
//
// Concurrency: Spawn / Resume / Shutdown are safe to call
// concurrently. The Provider tracks live Handles so Shutdown can fail
// them all if their hosts die.
type Provider struct {
	opts Options

	museBin string

	// handlesMu / handles tracks live Handles so Shutdown can fail
	// them all.
	handlesMu sync.Mutex
	handles   map[*Handle]struct{}

	// shutdown is closed once Shutdown has been initiated.
	closeOnce sync.Once
	shutdown  chan struct{}
}

// Options configures Provider construction. Most fields are optional;
// the empty value runs `muse` from PATH against the parent process'
// environment.
type Options struct {
	// MuseBin is the muse binary path. Defaults to $MUSE_BIN, then
	// "muse" looked up via $PATH.
	MuseBin string

	// Env is merged into the parent process environment for the
	// subprocess (before the META_API_KEY strip + MUSE_NO_AUTO_UPDATE
	// pin).
	Env map[string]string

	// HandshakeTimeout caps the JSON-RPC initialize handshake.
	// Defaults to 30s.
	HandshakeTimeout time.Duration

	// RPCTimeout is forwarded to handles for per-request timeouts.
	// Defaults to 30s.
	RPCTimeout time.Duration

	// stdoutOverride / stdinOverride / stderrOverride are test seams.
	// Production code leaves them nil and the Provider spawns a real
	// subprocess.
	stdoutOverride io.ReadCloser
	stdinOverride  io.WriteCloser
	skipProcess    bool // when true, no real muse is spawned (tests)
}

// New constructs the Provider WITHOUT starting any host. Binary
// availability is probed here — a missing binary returns
// agent.ErrProviderUnavailable — but process start and the initialize
// handshake are per-session (one host per Handle) in Spawn/Resume.
func New(opts Options) (*Provider, error) {
	if opts.HandshakeTimeout == 0 {
		opts.HandshakeTimeout = 30 * time.Second
	}
	if opts.RPCTimeout == 0 {
		opts.RPCTimeout = 30 * time.Second
	}
	museBin := ""
	if !opts.skipProcess {
		var err error
		museBin, err = resolveMuseBinary(opts.MuseBin)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", agent.ErrProviderUnavailable, err)
		}
	}
	return &Provider{
		opts:     opts,
		museBin:  museBin,
		shutdown: make(chan struct{}),
		handles:  make(map[*Handle]struct{}),
	}, nil
}

// Name implements agent.Provider.
func (p *Provider) Name() agent.ProviderName { return agent.ProviderMuse }

// Capabilities implements agent.Provider.
func (p *Provider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		SupportsMessageInjection:            true, // turn/steer into the running turn
		SupportsSessionResume:               true, // session/resume
		SupportsToolPlugins:                 true,
		NeedsBaseInstructions:               false, // MSP has no system-prompt channel; instructions ride leading text parts
		NeedsPermissionConfig:               true,  // approval bridge consumes it
		SupportsCodeIntelligenceEnforcement: false,
		EmitsSubagentEvents:                 true, // subagent items map to SubagentEvent
		SupportsReasoningEffort:             true, // turn/start.reasoningEffort
		ToolPermissionFormat:                "muse",
		AcceptsAllowedToolsList:             false,
		AcceptsMcpServerSpec:                true, // inline config.mcpServers
		HumanLabel:                          "Muse",
		SupportsTurnInputContext:            true, // InitialContext rides the first turn's input, not a re-sent prefix
	}
}

// Spawn implements agent.Provider: starts one `muse serve` host,
// handshakes, opens a session, and starts the first turn.
func (p *Provider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	var err error
	spec, err = agent.PrepareHarness(spec, p.Manifest())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	select {
	case <-p.shutdown:
		return nil, fmt.Errorf("%w: muse provider already shut down", agent.ErrSpawnFailed)
	default:
	}

	workspaceRoot, err := canonicalWorkspaceRoot(spec.Cwd)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	plan := NewSpawnPlan(spec, workspaceRoot)

	proc, err := p.startHost(ctx, spec, plan.ReadOnly, workspaceRoot)
	if err != nil {
		return nil, err
	}
	h := newHandle(p, proc.client, spec, HandleOptions{RPCTimeout: p.opts.RPCTimeout})
	h.provider = p
	proc.attachHandle(h)
	p.registerHandle(h)

	if err := h.start(ctx, plan); err != nil {
		p.unregisterHandle(h)
		proc.close()
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}

	if spec.OnProcessSpawned != nil {
		if pid := proc.pid(); pid > 0 {
			spec.OnProcessSpawned(pid)
		}
	}
	return h, nil
}

// Resume implements agent.Provider: starts one `muse serve` host,
// handshakes, and re-attaches the named session.
func (p *Provider) Resume(ctx context.Context, sessionID string, spec agent.Spec) (agent.Handle, error) {
	var err error
	spec, err = agent.PrepareHarness(spec, p.Manifest())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	if sessionID == "" {
		return nil, agent.ErrSessionNotFound
	}
	select {
	case <-p.shutdown:
		return nil, fmt.Errorf("%w: muse provider already shut down", agent.ErrSpawnFailed)
	default:
	}

	workspaceRoot, err := canonicalWorkspaceRoot(spec.Cwd)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	plan := NewSpawnPlan(spec, workspaceRoot)

	proc, err := p.startHost(ctx, spec, plan.ReadOnly, workspaceRoot)
	if err != nil {
		return nil, err
	}
	h := newHandle(p, proc.client, spec, HandleOptions{RPCTimeout: p.opts.RPCTimeout})
	h.provider = p
	proc.attachHandle(h)
	p.registerHandle(h)

	if err := h.resume(ctx, sessionID, sessionResumeParams(sessionID, newUUIDv7(), spec)); err != nil {
		p.unregisterHandle(h)
		proc.close()
		if errors.Is(err, agent.ErrSessionNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}

	if spec.OnProcessSpawned != nil {
		if pid := proc.pid(); pid > 0 {
			spec.OnProcessSpawned(pid)
		}
	}
	return h, nil
}

// Shutdown implements agent.Provider: fails every live Handle and
// releases provider-level resources. Idempotent.
func (p *Provider) Shutdown(_ context.Context) error {
	p.closeOnce.Do(func() { close(p.shutdown) })
	p.handlesMu.Lock()
	live := make([]*Handle, 0, len(p.handles))
	for h := range p.handles {
		live = append(live, h)
	}
	p.handles = map[*Handle]struct{}{}
	p.handlesMu.Unlock()
	for _, h := range live {
		h.failNow(errors.New("muse provider shutting down"))
	}
	return nil
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

// canonicalWorkspaceRoot resolves the session cwd to its realpath. The
// host rejects non-canonical roots (notably macOS /tmp → /private/tmp),
// so every session/start workspaceRoot goes through EvalSymlinks. Empty
// falls back to the process cwd.
func canonicalWorkspaceRoot(cwd string) (string, error) {
	if strings.TrimSpace(cwd) == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve workspace cwd: %w", err)
		}
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve workspace cwd: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("canonicalize workspace root: %w", err)
	}
	return canonical, nil
}

// serveEnv composes the host child environment: the parent environment
// plus Options.Env plus the session layer, with MUSE_NO_AUTO_UPDATE=1
// pinned and META_API_KEY stripped unless the Spec sets it. The key
// takes priority over the host login, so an ambient key would silently
// reroute auth — stripping is the safe default, and an explicit Spec
// value is honored verbatim. exec.Cmd applies last-entry-wins, so the
// session layer's explicit key (appended after the parent by
// ComposeChildEnv) is the effective one; the strip removes the ambient
// parent entry that would otherwise linger as a duplicate.
func serveEnv(parent []string, extra, session map[string]string, specSetsAPIKey bool) []string {
	if !specSetsAPIKey {
		parent = stripEnvKey(parent, apiKeyEnv)
		extra = stripEnvKeyMap(extra, apiKeyEnv)
	}
	composed := runtimeenv.ComposeChildEnv(parent, extra, session, nil)
	out := composed
	if !hasEnvKey(out, noAutoUpdateEnv) {
		out = append(out, noAutoUpdateEnv+"=1")
	}
	return out
}

func stripEnvKey(env []string, key string) []string {
	prefix := key + "="
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return out
}

func stripEnvKeyMap(env map[string]string, key string) map[string]string {
	if _, ok := env[key]; !ok {
		return env
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		if k != key {
			out[k] = v
		}
	}
	return out
}

func hasEnvKey(env []string, key string) bool {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

func specSetsKey(spec agent.Spec, key string) bool {
	if spec.Env == nil {
		return false
	}
	_, ok := spec.Env[key]
	return ok
}

// resolveMuseBinary applies the MuseBin → $MUSE_BIN → "muse" fallback
// chain and resolves the result via exec.LookPath.
func resolveMuseBinary(bin string) (string, error) {
	if bin == "" {
		bin = os.Getenv("MUSE_BIN")
	}
	if bin == "" {
		bin = "muse"
	}
	full, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("muse binary %q not on PATH: %w", bin, err)
	}
	return full, nil
}

// handshake performs initialize + initialized and requires schema
// version 1. It also checks the sessionMcp grant when the session will
// carry inline MCP servers.
func handshake(ctx context.Context, client *Client, timeout time.Duration, needMCP bool) error {
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := client.Request(hctx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    ClientName,
			"version": "0.5.0",
		},
		"capabilities": map[string]any{
			"requestedCapabilities": RequestedCapabilities,
		},
	}, timeout)
	if err != nil {
		return fmt.Errorf("muse initialize handshake: %w", err)
	}
	var res InitializeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("muse initialize handshake: decode result: %w", err)
	}
	if res.Schema.Version != SchemaVersion {
		return fmt.Errorf("muse initialize handshake: unsupported schema version %d (require %d)", res.Schema.Version, SchemaVersion)
	}
	if needMCP && !granted(res.GrantedCapabilities, CapabilitySessionMCP) {
		return fmt.Errorf("muse initialize handshake: host did not grant %q but the session carries MCP servers", CapabilitySessionMCP)
	}
	// Anything sent before the handshake completes is dropped by the
	// host, so initialized MUST precede session/start.
	if err := client.Notify("initialized", map[string]any{}); err != nil {
		return fmt.Errorf("muse initialized notification: %w", err)
	}
	return nil
}

func granted(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}
