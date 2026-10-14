package muse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
	"github.com/google/uuid"
)

// MuseBinaryDefault is the binary name resolved via PATH when
// Options.MuseBin is empty.
const MuseBinaryDefault = "muse"

// MinVersion is the lowest host version the MSP surface in types.go is
// known against. Verified live against 1.4.6; older hosts predate the
// schema export and are refused at construction.
const MinVersion = "1.4.6"

// PinnedVersion is the exact version this adapter was verified against.
const PinnedVersion = "1.4.6"

// VerifiedAgainst is the highest host version this adapter has actually
// been run against.
const VerifiedAgainst = "1.4.6"

// DefaultHandshakeTimeout caps the initialize handshake.
const DefaultHandshakeTimeout = 30 * time.Second

// DefaultRPCTimeout caps a single JSON-RPC request.
const DefaultRPCTimeout = 30 * time.Second

// Provider is the agent.Provider implementation backed by one
// `muse serve` subprocess per Handle. It holds no shared child: New
// only probes the binary, and Spawn starts a fresh host per session.
//
// Concurrency: Spawn / Resume / Shutdown are safe to call concurrently.
type Provider struct {
	opts     Options
	museBin  string
	verified bool

	mu       sync.Mutex
	handles  map[*Handle]struct{}
	shutdown bool
}

// Options configures Provider construction.
type Options struct {
	// MuseBin is the host binary path. Defaults to $MUSE_BIN, then
	// "muse" looked up via $PATH.
	MuseBin string

	// Args overrides the serve subcommand args; defaults to ["serve"].
	Args []string

	// Cwd is the working directory for the serve child. Defaults to
	// os.Getwd(). Sessions still pass the canonical workspace root
	// via session/start params; this is just where the host runs.
	Cwd string

	// Env is merged into the parent process environment for the
	// subprocess.
	Env map[string]string

	// HandshakeTimeout caps the JSON-RPC initialize handshake.
	// Defaults to 30s.
	HandshakeTimeout time.Duration

	// RPCTimeout is forwarded to handles for per-request timeouts.
	// Defaults to 30s.
	RPCTimeout time.Duration

	// TrustWorkspace passes --trust-workspace (load workspace skills
	// and rules). Sandbox posture is fixed at host construction and
	// applies to every session the host loads.
	TrustWorkspace bool

	// DisableSandbox passes --disable-sandbox. Prefer the read-only
	// lane (SandboxLevel read-only → --disable-shell --disable-write)
	// over disabling the sandbox outright.
	DisableSandbox bool

	// stdoutOverride / stdinOverride / stderrOverride are test seams.
	stdoutOverride io.ReadCloser
	stdinOverride  io.WriteCloser
	stderrOverride io.ReadCloser
	skipProcess    bool // when true, no real host is spawned (tests)
}

// New constructs the Provider. Binary availability and the minimum
// version are probed here — a missing or too-old binary returns
// agent.ErrProviderUnavailable — but no host process starts until the
// first Spawn/Resume, which owns exactly one `muse serve` child per
// Handle.
func New(opts Options) (*Provider, error) {
	if opts.HandshakeTimeout == 0 {
		opts.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if opts.RPCTimeout == 0 {
		opts.RPCTimeout = DefaultRPCTimeout
	}
	if len(opts.Args) == 0 {
		opts.Args = []string{"serve"}
	}
	if opts.skipProcess {
		return &Provider{opts: opts, museBin: "muse-test", verified: true, handles: make(map[*Handle]struct{})}, nil
	}
	bin, err := resolveMuseBinary(opts.MuseBin)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrProviderUnavailable, err)
	}
	verified, err := checkMuseVersion(bin)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrProviderUnavailable, err)
	}
	return &Provider{opts: opts, museBin: bin, verified: verified, handles: make(map[*Handle]struct{})}, nil
}

// Name implements agent.Provider.
func (p *Provider) Name() agent.ProviderName { return agent.ProviderMuse }

// Capabilities implements agent.Provider.
func (p *Provider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		SupportsMessageInjection:            true, // turn/steer into the running turn
		SupportsSessionResume:               true, // session/resume
		SupportsToolPlugins:                 true,
		NeedsBaseInstructions:               true, // leading text part (no system channel)
		NeedsPermissionConfig:               true, // approval bridge consumes it
		SupportsCodeIntelligenceEnforcement: false,
		EmitsSubagentEvents:                 true, // subagent items map to SubagentEvent
		SupportsReasoningEffort:             true, // turn/start.reasoningEffort
		ToolPermissionFormat:                "muse",
		AcceptsAllowedToolsList:             false,
		AcceptsMcpServerSpec:                true,
		HumanLabel:                          "Muse",
		SupportsTurnInputContext:            true,
	}
}

// Spawn implements agent.Provider. Starts one `muse serve` child,
// handshakes, opens a session, and starts the first turn.
func (p *Provider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	var err error
	spec, err = agent.PrepareHarness(spec, p.Manifest())
	if err != nil {
		return nil, fmt.Errorf("muse spawn: prepare harness: %w", errors.Join(agent.ErrSpawnFailed, err))
	}
	p.mu.Lock()
	if p.shutdown {
		p.mu.Unlock()
		return nil, fmt.Errorf("%w: muse provider already shut down", agent.ErrSpawnFailed)
	}
	p.mu.Unlock()

	h, err := p.spawnHandle(ctx, spec, "")
	if err != nil {
		return nil, err
	}
	return h, nil
}

// Resume implements agent.Provider. Starts one `muse serve` child and
// re-attaches it to the named session via session/resume.
func (p *Provider) Resume(ctx context.Context, sessionID string, spec agent.Spec) (agent.Handle, error) {
	var err error
	spec, err = agent.PrepareHarness(spec, p.Manifest())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	if sessionID == "" {
		return nil, agent.ErrSessionNotFound
	}
	if _, parseErr := uuid.Parse(sessionID); parseErr != nil {
		return nil, fmt.Errorf("%w: invalid muse session id: %v", agent.ErrSessionNotFound, parseErr)
	}
	p.mu.Lock()
	if p.shutdown {
		p.mu.Unlock()
		return nil, fmt.Errorf("%w: muse provider already shut down", agent.ErrSpawnFailed)
	}
	p.mu.Unlock()

	h, err := p.spawnHandle(ctx, spec, sessionID)
	if err != nil {
		return nil, err
	}
	return h, nil
}

// spawnHandle owns the full per-session bring-up: canonical cwd, child
// env, process start, handshake, session start/resume, first turn.
func (p *Provider) spawnHandle(ctx context.Context, spec agent.Spec, resumeSessionID string) (*Handle, error) {
	workspaceRoot, err := canonicalWorkspaceRoot(spec.Cwd)
	if err != nil {
		return nil, fmt.Errorf("%w: muse workspace root: %v", agent.ErrSpawnFailed, err)
	}
	plan := NewSpawnPlan(spec, workspaceRoot)

	h := newHandle(p, spec, plan, HandleOptions{RPCTimeout: p.opts.RPCTimeout})

	if err := h.spawnChild(workspaceRoot, spec); err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	// From here on the child is live; failures must stop it.
	if err := h.handshake(ctx); err != nil {
		_ = h.shutdownChild("handshake failed")
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	if resumeSessionID != "" {
		if err := h.resumeSession(ctx, resumeSessionID); err != nil {
			h.signalClosed()
			h.closeEvents()
			_ = h.shutdownChild("resume failed")
			if IsSessionGone(err) {
				return nil, fmt.Errorf("%w: %v", agent.ErrSessionNotFound, err)
			}
			return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
		}
	} else {
		if err := h.startSession(ctx); err != nil {
			h.signalClosed()
			h.closeEvents()
			_ = h.shutdownChild("session start failed")
			return nil, p.classifyStartError(err)
		}
	}
	if err := h.startTurn(ctx, plan.PromptInput); err != nil {
		h.signalClosed()
		h.closeEvents()
		_ = h.shutdownChild("first turn failed")
		return nil, p.classifyStartError(err)
	}

	p.registerHandle(h)
	//nolint:gosec // G118: session-lifetime goroutine, ctx is honored
	go h.forward(ctx)
	return h, nil
}

// classifyStartError maps a first-turn/session failure to a typed
// error. model/list answers from cache even when logged out, so auth
// failures surface here: classify them loudly instead of reporting a
// generic spawn failure. There is no login-status command on the host
// (login is a browser device-code flow and model/list answers from
// cache when logged out), so the first turn is the auth probe:
//
//   - "does not exist or you lack access" (and kind-bearing
//     unauthorized/unauthenticated failures) classify as
//     ErrModelNotEntitled: the account may not use this model id.
//     A logged-out subscriber login surfaces the same way (the
//     host cannot tell a lapsed login from a missing grant), hence
//     the typed split below on unauthenticated/logout signals.
//   - explicit logged-out / unauthenticated signals classify as
//     ErrNotAuthenticated.
//
// Both wrap ErrSpawnFailed too, so existing spawn-failure handling
// keeps working while auth-aware callers match the typed cause with
// errors.Is.
func (p *Provider) classifyStartError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unauthenticated") ||
		strings.Contains(msg, "not logged in") ||
		strings.Contains(msg, "logged out") ||
		strings.Contains(msg, "login required") ||
		strings.Contains(msg, "no login"):
		return fmt.Errorf("muse host is not authenticated: %w: %w", errors.Join(agent.ErrSpawnFailed, agent.ErrNotAuthenticated), err)
	case strings.Contains(msg, "does not exist") || strings.Contains(msg, "lack access") ||
		strings.Contains(msg, "unauthorized") || strings.Contains(msg, "not entitled") ||
		strings.Contains(msg, "forbidden"):
		return fmt.Errorf("muse host is not entitled to this model: %w: %w", errors.Join(agent.ErrSpawnFailed, agent.ErrModelNotEntitled), err)
	}
	return fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
}

// Shutdown implements agent.Provider. Stops every live handle.
// Idempotent.
func (p *Provider) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	if p.shutdown {
		p.mu.Unlock()
		return nil
	}
	p.shutdown = true
	live := make([]*Handle, 0, len(p.handles))
	for h := range p.handles {
		live = append(live, h)
	}
	p.handles = make(map[*Handle]struct{})
	p.mu.Unlock()

	var errs []error
	for _, h := range live {
		if err := h.Stop(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *Provider) registerHandle(h *Handle) {
	p.mu.Lock()
	p.handles[h] = struct{}{}
	p.mu.Unlock()
}

func (p *Provider) unregisterHandle(h *Handle) {
	p.mu.Lock()
	delete(p.handles, h)
	p.mu.Unlock()
}

// resolveMuseBinary applies the Options → $MUSE_BIN → "muse" fallback
// chain and resolves the result via exec.LookPath.
func resolveMuseBinary(bin string) (string, error) {
	if bin == "" {
		bin = os.Getenv("MUSE_BIN")
	}
	if bin == "" {
		bin = MuseBinaryDefault
	}
	full, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("muse binary %q not on PATH: %w", bin, err)
	}
	return full, nil
}

// checkMuseVersion probes "<binary> --version" and enforces the
// minimum. Returns whether the version is within the verified range
// (older-than-verified labels the session; newer is allowed).
func checkMuseVersion(bin string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// nolint:gosec // G204: bin is the resolved-from-PATH path New() execs.
	cmd := exec.CommandContext(ctx, bin, "--version")
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("muse version probe: %w", err)
	}
	ver := parseMuseVersion(strings.TrimSpace(string(out)))
	if ver == "" {
		return false, fmt.Errorf("muse version probe: could not parse version from %q", strings.TrimSpace(string(out)))
	}
	if compareVersions(ver, MinVersion) < 0 {
		return false, fmt.Errorf("muse version %s is below the minimum %s", ver, MinVersion)
	}
	return compareVersions(ver, VerifiedAgainst) <= 0, nil
}

// parseMuseVersion extracts the dotted version from "<name> <version>"
// probe output (e.g. "Muse Code 1.4.6 (1.4.6-R6000.1)" → "1.4.6").
func parseMuseVersion(out string) string {
	for _, field := range strings.Fields(out) {
		trimmed := strings.Trim(field, "()")
		if trimmed == "" || trimmed[0] < '0' || trimmed[0] > '9' {
			continue
		}
		dot := strings.Index(trimmed, ".")
		if dot < 0 {
			continue
		}
		end := 0
		for end < len(trimmed) && (trimmed[end] == '.' || (trimmed[end] >= '0' && trimmed[end] <= '9')) {
			end++
		}
		if end > 0 {
			return trimmed[:end]
		}
	}
	return ""
}

// compareVersions compares dotted version strings. Returns -1/0/+1.
func compareVersions(a, b string) int {
	ap, bp := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(ap) || i < len(bp); i++ {
		var ai, bi int
		if i < len(ap) {
			_, _ = fmt.Sscanf(ap[i], "%d", &ai)
		}
		if i < len(bp) {
			_, _ = fmt.Sscanf(bp[i], "%d", &bi)
		}
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
	}
	return 0
}

// canonicalWorkspaceRoot resolves the workspace to its canonical
// realpath. The host rejects non-canonical roots (e.g. macOS /tmp
// symlinks), so every session start goes through here.
func canonicalWorkspaceRoot(cwd string) (string, error) {
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// serveArgs builds the `muse serve` argv for one session. Sandbox
// posture is fixed here, at host construction, and applies to every
// session the host loads; approval mode travels on the wire instead.
// The read-only lane (SandboxLevel read-only) maps to
// --disable-shell --disable-write.
func serveArgs(spec agent.Spec, trustWorkspace, disableSandbox bool) []string {
	args := []string{"serve"}
	if trustWorkspace {
		args = append(args, "--trust-workspace")
	}
	// The read-only lane wins over a provider-level DisableSandbox:
	// a Spec that asks for read-only is never widened.
	if spec.SandboxLevel == agent.SandboxReadOnly {
		args = append(args, "--disable-shell", "--disable-write")
	} else if disableSandbox {
		args = append(args, "--disable-sandbox")
	}
	return args
}

// serveEnv composes the serve child environment: the inherited parent
// (minus runner-only attach controls and the auth blocklist), overlaid
// with Options.Env and the session Spec.Env. META_API_KEY is stripped
// unless the Spec sets it explicitly; MUSE_NO_AUTO_UPDATE=1 is always
// set so the host never self-updates mid-session.
func serveEnv(parent []string, extra, session map[string]string) []string {
	composed := runtimeenv.ComposeChildEnv(parent, extra, session, nil)
	out := composed[:0:0]
	sessionHasKey := session != nil
	_, sessionSetsKey := session["META_API_KEY"]
	for _, kv := range composed {
		if strings.HasPrefix(kv, "META_API_KEY=") && !(sessionHasKey && sessionSetsKey) {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "MUSE_NO_AUTO_UPDATE=1")
	return out
}

// handshakeParams builds the initialize request params.
func handshakeParams() map[string]any {
	return map[string]any{
		"clientInfo": map[string]any{
			"name":    museClientName,
			"title":   "Donmai Orchestrator",
			"version": "0.5.0",
		},
		"capabilities": map[string]any{
			"requestedCapabilities": requestedCapabilities,
		},
	}
}

// checkHandshakeResult validates the initialize result: schema
// version 1 and the granted sessionMcp capability. The grant is
// hard-required here: sessions that carry inline config.mcpServers
// must never start silently tool-less. checkHandshakeResultForPlan
// relaxes the grant for sessions that never asked for MCP.
func checkHandshakeResult(raw json.RawMessage) (InitializeResult, error) {
	return checkHandshakeResultForPlan(raw, true)
}

// checkHandshakeResultForPlan is checkHandshakeResult parameterized
// by whether the session needs inline MCP: it enforces the
// sessionMcp grant exactly when needsMCP is true (hard-deny), and
// accepts a missing grant otherwise (soft path: the session runs
// without MCP tools because it never asked for any).
func checkHandshakeResultForPlan(raw json.RawMessage, needsMCP bool) (InitializeResult, error) {
	var res InitializeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return res, fmt.Errorf("muse initialize: decode response: %w", err)
	}
	if res.Schema.Version != schemaExportVersion {
		return res, fmt.Errorf("muse initialize: unsupported schema version %d (want %d)", res.Schema.Version, schemaExportVersion)
	}
	if !schemaSurfaceStable() {
		return res, fmt.Errorf("muse initialize: pinned schema export is not the stable surface")
	}
	if res.Schema.Fingerprint != "" && res.Schema.Fingerprint != schemaExportFingerprint {
		slog.Warn("muse: host schema fingerprint differs from the export the wire types were generated from",
			"host", res.Schema.Fingerprint, "generated", schemaExportFingerprint)
	}
	granted := false
	for _, cap := range res.GrantedCapabilities {
		if cap == CapabilitySessionMCP {
			granted = true
			break
		}
	}
	if !granted {
		if !needsMCP {
			slog.Warn("muse: host did not grant the sessionMcp capability; continuing without inline MCP (session carries no mcpServers)",
				"granted", res.GrantedCapabilities)
			return res, nil
		}
		return res, fmt.Errorf("muse initialize: host did not grant the sessionMcp capability (granted=%v)", res.GrantedCapabilities)
	}
	return res, nil
}
