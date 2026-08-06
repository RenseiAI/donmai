package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/RenseiAI/donmai/agent"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// DefaultBinary is the executable name probed on $PATH at construction.
// OpenCode can be run as a standalone CLI with `opencode run`.
const DefaultBinary = "opencode"

// DefaultEndpoint is the URL probed at construction when no explicit
// endpoint or env var is supplied. Mirrors opencode 1.x's default
// `opencode serve` bind address (127.0.0.1:4096). Override with
// $OPENCODE_ENDPOINT.
const DefaultEndpoint = "http://localhost:4096"

// EnvEndpoint overrides DefaultEndpoint when set.
const EnvEndpoint = "OPENCODE_ENDPOINT"

// EnvAPIKey is the optional bearer-token env var NAME. Forwarded for
// future hosted variants; not required for the default localhost
// server.
const EnvAPIKey = "OPENCODE_API_KEY" //nolint:gosec // G101: env-var name, not a credential

// errExternalAttachConfigUnproven is returned before adaptation/session side
// effects when an attached server would need a project config that this
// Provider instance cannot prove is active.
var errExternalAttachConfigUnproven = errors.New("opencode external attach project config is not provider-owned")

// DefaultProbeTimeout caps the probe HTTP GET at construction when
// not in CLI-binary mode.
const DefaultProbeTimeout = 2 * time.Second

// eventBufferSize matches provider/claude — sized to absorb a burst
// of fan-out events without backpressuring the stdout reader.
const eventBufferSize = 64

// stderrBufferSize caps how many bytes of stderr we retain for
// post-mortem diagnostics on unexpected exits.
const stderrBufferSize = 8 * 1024

// stopGracePeriod is the deadline between SIGTERM and SIGKILL.
const stopGracePeriod = 5 * time.Second

// Options configures Provider construction.
type Options struct {
	// Binary names the opencode CLI executable to invoke. When empty,
	// DefaultBinary is used. Tests inject a fake-CLI script path here.
	// When set (or when DefaultBinary is on PATH), the provider uses
	// CLI-spawn mode (opencode run --format json) rather than the HTTP
	// server mode.
	Binary string

	// Endpoint overrides the OpenCode server URL for HTTP-server mode.
	// Empty falls back to $OPENCODE_ENDPOINT then DefaultEndpoint.
	// Ignored when BinaryMode is true or a binary is found.
	Endpoint string

	// APIKey is an optional bearer token. Empty falls back to
	// $OPENCODE_API_KEY (which may also be empty).
	APIKey string

	// HTTPClient is used for the probe call in HTTP-server mode.
	// Defaults to a client with DefaultProbeTimeout.
	HTTPClient *http.Client

	// Getenv overrides the environment lookup. Defaults to os.Getenv.
	Getenv func(string) string

	// LookPath overrides the binary-resolution function. Defaults to
	// exec.LookPath.
	LookPath func(name string) (string, error)

	// SkipProbe disables the construction-time liveness check in
	// HTTP-server mode. Tests use this when the goal is to assert
	// capability / Spawn behavior without standing up a server.
	SkipProbe bool

	// VersionProbe overrides how the resolved CLI binary's version is
	// determined at construction (§8 probe-time pin enforcement, see
	// probe.go). Defaults to running "<binary> --version". Tests inject
	// a stub here to assert enforcement behavior without a real
	// fake-CLI script.
	VersionProbe func(ctx context.Context, binary string) (string, error)

	// SkipVersionCheck disables the construction-time version-pin
	// enforcement in CLI mode entirely (no probe call at all). Tests use
	// this when the goal is unrelated to version enforcement.
	SkipVersionCheck bool

	// PreferServer forces Lane B (opencode serve + REST/SSE) even for
	// sessions that would otherwise take the one-shot CLI lane. Exposed for
	// tests and operators (07 §2). When the binary is absent and only an
	// endpoint is reachable, Lane B (attach) is used regardless of this flag.
	PreferServer bool

	// ServerClientFactory overrides how the Lane-B serverClient is built for
	// a resolved endpoint. Defaults to newClientV1. Tests inject a stub to
	// exercise the handle against an httptest server without a real child.
	ServerClientFactory func(endpoint, apiKey string) serverClient

	// configTempDir is a test seam for the binary-backed config boundary.
	// Production always uses the resolved system temp directory.
	configTempDir string
}

// Provider is the agent.Provider implementation for OpenCode.
// It supports two execution modes:
//
//  1. CLI mode (preferred): Spawn execs `opencode run --format json`
//     which streams NDJSON events to stdout.
//  2. HTTP-server mode (legacy/fallback): operator runs
//     `opencode serve`; the provider posts tasks to the REST API
//     and streams WebSocket events. Not yet wired — Spawn returns
//     ErrSpawnFailed in this mode until the REST client lands.
type Provider struct {
	binary   string // resolved CLI path, or "" for HTTP-server mode
	endpoint string // HTTP server endpoint (HTTP-server mode only)
	apiKey   string

	// versionUnverified is set at construction (CLI mode only) when the
	// probed binary version could not be confirmed to fall within
	// [MinVersion, VerifiedAgainst] — either because it is above
	// VerifiedAgainst, or because the probe itself couldn't determine a
	// version at all. A version confirmed BELOW MinVersion instead fails
	// construction outright (see checkVersionPin in probe.go). Surfaced
	// once per session as a SystemEvent from spawnCLI.
	versionUnverified bool

	// preferServer forces Lane B (from Options.PreferServer).
	preferServer bool

	// clientFactory builds a Lane-B serverClient for a resolved endpoint.
	clientFactory func(endpoint, apiKey string) serverClient

	// resources tracks each provider-owned Lane-B child together with its
	// isolated config. Registration happens before process start so Shutdown
	// cannot miss an in-flight spawn. Guarded by mu.
	mu            sync.Mutex
	resources     map[*openCodeServerResource]struct{}
	shuttingDown  bool
	configTempDir string
}

func (p *Provider) registerResource(resource *openCodeServerResource) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shuttingDown {
		return errOpenCodeShutdown
	}
	if p.resources == nil {
		p.resources = make(map[*openCodeServerResource]struct{})
	}
	p.resources[resource] = struct{}{}
	return nil
}

func (p *Provider) releaseResource(resource *openCodeServerResource) error {
	err := resource.close()
	p.mu.Lock()
	if err == nil {
		delete(p.resources, resource)
	}
	p.mu.Unlock()
	return err
}

func (p *Provider) checkOpen() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shuttingDown {
		return errOpenCodeShutdown
	}
	return nil
}

// newServerClient builds a Lane-B serverClient, honoring an injected factory.
func (p *Provider) newServerClient(endpoint, apiKey string) serverClient {
	if p.clientFactory != nil {
		return p.clientFactory(endpoint, apiKey)
	}
	return newClientV1(endpoint, apiKey, nil)
}

// New constructs a Provider.
//
// Construction order:
//  1. Probe for the `opencode` CLI binary on PATH. If found, use CLI
//     mode — no HTTP probe needed.
//  2. If the binary is not on PATH, fall back to HTTP-server mode:
//     probe the OpenCode HTTP server at the configured endpoint.
//     Connection refused or 5xx → wrapped agent.ErrProviderUnavailable.
func New(opts Options) (*Provider, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = defaultGetenv
	}
	lookup := opts.LookPath
	if lookup == nil {
		lookup = exec.LookPath
	}

	apiKey := opts.APIKey
	if apiKey == "" {
		apiKey = getenv(EnvAPIKey)
	}

	// Try CLI binary first.
	binary := opts.Binary
	if binary == "" {
		binary = DefaultBinary
	}
	resolved, lookErr := lookup(binary)
	if lookErr == nil {
		// CLI binary is available — use CLI mode. Enforce the version
		// pin (§8) before handing back a usable Provider: a confirmed
		// below-MinVersion binary is rejected outright; anything else
		// unconfirmed-but-plausible proceeds labeled (DEC-2).
		unverified := false
		if !opts.SkipVersionCheck {
			probe := opts.VersionProbe
			if probe == nil {
				probe = defaultVersionProbe
			}
			vctx, cancel := context.WithTimeout(context.Background(), DefaultVersionProbeTimeout)
			var err error
			unverified, err = checkVersionPin(vctx, probe, resolved)
			cancel()
			if err != nil {
				return nil, err
			}
		}
		return &Provider{
			binary:            resolved,
			apiKey:            apiKey,
			versionUnverified: unverified,
			preferServer:      opts.PreferServer,
			clientFactory:     opts.ServerClientFactory,
			configTempDir:     opts.configTempDir,
		}, nil
	}

	// Fall back to HTTP-server mode.
	endpoint := strings.TrimRight(opts.Endpoint, "/")
	if endpoint == "" {
		endpoint = strings.TrimRight(getenv(EnvEndpoint), "/")
	}
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}

	if !opts.SkipProbe {
		client := opts.HTTPClient
		if client == nil {
			client = &http.Client{Timeout: DefaultProbeTimeout}
		}
		if err := probeLive(client, endpoint, apiKey); err != nil {
			return nil, fmt.Errorf(
				"%w: opencode CLI %q not on PATH and HTTP server at %s unreachable "+
					"(install CLI: `npm i -g opencode-ai` or start server with `opencode serve`): %v",
				agent.ErrProviderUnavailable, binary, endpoint, err,
			)
		}
	}

	return &Provider{
		endpoint:      endpoint,
		apiKey:        apiKey,
		preferServer:  opts.PreferServer,
		clientFactory: opts.ServerClientFactory,
		configTempDir: opts.configTempDir,
	}, nil
}

// probeLive issues a GET against the server's root and accepts any
// non-5xx response as a successful liveness check.
func probeLive(client *http.Client, endpoint, apiKey string) error {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4*1024))
	if resp.StatusCode >= 500 {
		return errors.New("HTTP " + resp.Status)
	}
	return nil
}

// Name returns ProviderOpenCode. Stable for the lifetime of the Provider.
func (*Provider) Name() agent.ProviderName { return agent.ProviderOpenCode }

// Capabilities returns the v1.0.0 capability matrix for the OpenCode
// CLI runner.
func (*Provider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		SupportsMessageInjection:            true,  // Lane B: Prompt on a live session (07 §7)
		SupportsSessionResume:               true,  // Lane B: create-with-session / Resume (07 §7, §9)
		SupportsToolPlugins:                 false, // opencode plugins are not donmai tool plugins
		NeedsBaseInstructions:               false,
		NeedsPermissionConfig:               false,
		SupportsCodeIntelligenceEnforcement: false,
		EmitsSubagentEvents:                 false,
		SupportsReasoningEffort:             true, // --variant flag maps to effort levels
		ToolPermissionFormat:                "claude",
		AcceptsAllowedToolsList:             true, // projected into the owned opencode.json permission map (07 §5.2)
		AcceptsMcpServerSpec:                true, // §5.3 owned per-session MCP config is independent of OpenCode plugin support.
		HumanLabel:                          "OpenCode",
	}
}

// launchManifest narrows the static provider manifest to the delivery
// boundary this Provider instance actually owns. An attached external server
// has its own project config; writing a local opencode.json does not activate
// it there. Mark project-config-backed channels unsupported so PrepareHarness
// persists a typed denial before any external session or prompt side effect.
func (p *Provider) launchManifest() agent.HarnessManifest {
	manifest := p.Manifest()
	if p.binary != "" {
		return manifest
	}
	for i := range manifest.ToolLifecycle {
		profile := &manifest.ToolLifecycle[i]
		profile.MCPDelivery = agent.ToolDeliveryUnsupported
		profile.NativeToolPolicyDelivery = agent.ToolDeliveryUnsupported
		profile.PermissionConfigDelivery = agent.ToolDeliveryUnsupported
		profile.MCPToolPolicyDelivery = agent.ToolDeliveryUnsupported
	}
	return manifest
}

// Spawn starts a new OpenCode session, selecting a lane (07 §2):
//
//   - Lane A — one-shot CLI (`opencode run --format json`): the default for a
//     binary-backed fire-and-forget spawn. cwd via --dir plus cmd.Dir, prompt on stdin,
//     NDJSON events mapped by mapOpenCodeLine, and any endpoint/tool/MCP policy
//     delivered through the same owned temporary config boundary as Lane B.
//   - Lane B — serve/HTTP (`opencode serve` + REST/SSE): chosen when the
//     operator explicitly requests it (Options.PreferServer), or when no
//     binary is present and only an endpoint is reachable (attach mode).
//
// Endpoint routing (07 §9) is applied to both lanes first: a resolved
// Spec.Endpoint whose company/host this harness cannot drive fails loudly.
//
// On any pre-spawn failure the provider returns an error wrapping
// agent.ErrSpawnFailed.
func (p *Provider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	if err := p.checkOpen(); err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	if err := p.validateLaunchAuthority(spec); err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	var err error
	spec, err = agent.PrepareHarness(spec, p.launchManifest())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	spec, err = applyEndpoint(spec)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	if p.useServerLane(spec) {
		return p.spawnServer(ctx, spec, "")
	}
	h, err := p.spawnCLI(ctx, spec)
	if err != nil {
		// Return a typed nil to avoid the interface-nil trap: a non-nil
		// agent.Handle wrapping a nil *openCodeHandle would panic on
		// method calls in callers that check handle != nil.
		return nil, err
	}
	return h, nil
}

// validateLaunchAuthority rejects endpoint binding on external attach. The
// endpoint's base URL, credential indirection, model pin, and provider lockout
// are all delivered through opencode.json; an attached server does not read
// the local file. Tool/MCP policy fields are rejected separately by
// launchManifest so their denial receipt keeps the precise lifecycle channel.
func (p *Provider) validateLaunchAuthority(spec agent.Spec) error {
	if p.binary == "" && spec.Endpoint != nil {
		return fmt.Errorf("%w: cannot prove Spec.Endpoint is active on the attached server", errExternalAttachConfigUnproven)
	}
	return nil
}

// useServerLane decides Lane B vs Lane A. The pinned binary's v2 server can
// list custom OpenAI-compatible models but its SessionRunner cannot resolve
// them, while `opencode run` executes the same owned config successfully.
// Project-config-bearing one-shot work therefore stays on Lane A; only an
// explicit server request or attach-only provider selects Lane B.
func (p *Provider) useServerLane(_ agent.Spec) bool {
	if p.binary == "" {
		return true // attach mode: only Lane B can serve
	}
	return p.preferServer
}

// requiresProjectConfig reports whether a spec carries any input that this
// adapter must deliver through its session-scoped opencode.json. Both owned
// binary lanes set OPENCODE_CONFIG when this returns true.
func requiresProjectConfig(spec agent.Spec) bool {
	return spec.Endpoint != nil ||
		len(spec.AllowedTools) > 0 ||
		len(spec.DisallowedTools) > 0 ||
		spec.PermissionConfig != nil ||
		len(spec.MCPServers) > 0 ||
		len(spec.MCPToolNames) > 0
}

// spawnCLI starts `opencode run --format json` as a subprocess and
// returns a Handle backed by the running process.
func (p *Provider) spawnCLI(ctx context.Context, spec agent.Spec) (*openCodeHandle, error) {
	var boundary *openCodeConfigBoundary
	if requiresProjectConfig(spec) {
		var err error
		boundary, err = newOpenCodeConfigBoundary(p.configTempDir, spec)
		if err != nil {
			return nil, fmt.Errorf("%w: create owned opencode config: %v", agent.ErrSpawnFailed, err)
		}
		if err := boundary.validate(); err != nil {
			cleanupErr := boundary.remove()
			return nil, errors.Join(fmt.Errorf("%w: validate owned opencode config: %v", agent.ErrSpawnFailed, err), cleanupErr)
		}
		env := make(map[string]string, len(spec.Env)+1)
		for key, value := range spec.Env {
			env[key] = value
		}
		env[OCConfigEnvVar] = boundary.configPath
		spec.Env = env
	}
	fail := func(primary error) (*openCodeHandle, error) {
		if boundary == nil {
			return nil, primary
		}
		return nil, errors.Join(primary, boundary.remove())
	}
	argv := buildOpenCodeArgs(spec)

	// nolint:gosec // p.binary is resolved at provider construction
	// from exec.LookPath; argv values come from a typed agent.Spec
	// and a closed set of CLI flags.
	cmd := exec.CommandContext(ctx, p.binary, argv...)
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	cmd.Env = composeEnv(os.Environ(), spec.Env)
	configureProcessGroup(cmd)
	cmd.Cancel = func() error {
		signalProcessGroup(cmd, syscall.SIGKILL)
		return nil
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fail(fmt.Errorf("%w: stdin pipe: %v", agent.ErrSpawnFailed, err))
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fail(fmt.Errorf("%w: stdout pipe: %v", agent.ErrSpawnFailed, err))
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return fail(fmt.Errorf("%w: stderr pipe: %v", agent.ErrSpawnFailed, err))
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return fail(fmt.Errorf("%w: cmd start: %v", agent.ErrSpawnFailed, err))
	}

	if spec.OnProcessSpawned != nil && cmd.Process != nil {
		spec.OnProcessSpawned(cmd.Process.Pid)
	}

	stderrBuf := &boundedBuffer{limit: stderrBufferSize, buf: make([]byte, 0, stderrBufferSize)}

	h := &openCodeHandle{
		binary:     p.binary,
		cwd:        spec.Cwd,
		cmd:        cmd,
		events:     make(chan agent.Event, eventBufferSize),
		logger:     slog.With("provider", "opencode", "pid", cmd.Process.Pid),
		stdoutPipe: stdout,
		stderrPipe: stderr,
		stderrBuf:  stderrBuf,
		shutdown:   make(chan struct{}),
		done:       make(chan struct{}),
	}
	if boundary != nil {
		h.releaseOwned = boundary.remove
	}

	if p.versionUnverified {
		// Observability, not gating (DEC-2): the events channel is
		// buffered (eventBufferSize) and nothing has been sent yet, so
		// this send never blocks. Emitted once per session, before any
		// stream events, satisfying the "no direct emit ordering
		// requirement beyond the terminal rule" conformance contract.
		h.events <- agent.SystemEvent{
			Subtype: unverifiedVersionSubtype,
			Message: fmt.Sprintf(
				"opencode binary version could not be confirmed within the verified range (pinned %s, verified through %s) — proceeding unverified",
				PinnedVersion, VerifiedAgainst,
			),
		}
	}

	go writePromptStdin(stdin, spec.Prompt, h.logger)
	go drainStderr(stderr, stderrBuf, h.logger)
	go h.readStdout()
	go h.watchCtx(ctx) //nolint:gosec // G118: watchCtx intentionally uses context.Background for shutdown (see body)

	return h, nil
}

// buildOpenCodeArgs translates an agent.Spec into argv for
// `opencode run --format json`.
//
// opencode 1.x `run` flag mapping (verified against the installed
// binary, opencode 1.17.18 — `opencode run --help`):
//
//	opencode run   → headless run subcommand
//	--format json  → NDJSON output (required for event streaming)
//	--auto         → auto-approve permissions that are not explicitly
//	                 denied (gated on Spec.Autonomous). Explicit deny
//	                 rules stay enforced — strictly safer than the
//	                 removed `--dangerously-skip-permissions` (a flag
//	                 that does not exist on opencode 1.x).
//	--model <id>   → model in provider/model format
//	--variant <v>  → provider-specific reasoning effort (e.g. high, max,
//	                 minimal); maps Spec.Effort.
//
// The working directory is delivered through both `--dir` and cmd.Dir.
// OpenCode consults the inherited PWD when `--dir` is absent, and Go does not
// rewrite PWD when exec.Cmd.Dir is set. The explicit flag therefore prevents a
// stale parent PWD from selecting a different project while cmd.Dir preserves
// the operating-system process boundary.
// The prompt is delivered via stdin.
func buildOpenCodeArgs(spec agent.Spec) []string {
	argv := []string{
		"run",
		"--format", "json",
	}
	if spec.Cwd != "" {
		argv = append(argv, "--dir", spec.Cwd)
	}

	if spec.Autonomous {
		argv = append(argv, "--auto")
	}

	model := spec.Model
	if spec.Endpoint != nil && resolvedModel(spec) != "" {
		model = OCProviderID + "/" + resolvedModel(spec)
	}
	if model != "" {
		argv = append(argv, "--model", model)
	}

	if spec.Effort != "" {
		argv = append(argv, "--variant", string(spec.Effort))
	}

	return argv
}

// spawnServer runs the Lane-B path: render + inject the per-session
// opencode.json, bring up (or attach to) a serve endpoint, create-or-resume the
// session, subscribe to events, admit the prompt, and return a live handle.
// resumeSessionID is empty for a fresh spawn and set for Resume.
func (p *Provider) spawnServer(ctx context.Context, spec agent.Spec, resumeSessionID string) (agent.Handle, error) {
	var configPath string
	var resource *openCodeServerResource
	if p.binary != "" {
		boundary, err := newOpenCodeConfigBoundary(p.configTempDir, spec)
		if err != nil {
			return nil, fmt.Errorf("%w: create owned opencode config: %v", agent.ErrSpawnFailed, err)
		}
		resource = &openCodeServerResource{config: boundary}
		if err := p.registerResource(resource); err != nil {
			cleanupErr := resource.close()
			return nil, errors.Join(fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err), cleanupErr)
		}
		configPath = boundary.configPath
	}
	fail := func(primary error) (agent.Handle, error) {
		if resource == nil {
			return nil, primary
		}
		return nil, errors.Join(primary, p.releaseResource(resource))
	}
	if resource != nil {
		if err := resource.config.validate(); err != nil {
			return fail(fmt.Errorf("%w: validate owned opencode config: %v", agent.ErrSpawnFailed, err))
		}
	}

	child, endpoint, err := p.bringUpServer(ctx, spec, configPath)
	if err != nil {
		return fail(err)
	}
	if resource != nil {
		if err := resource.attachChild(child); err != nil {
			return fail(fmt.Errorf("%w: attach owned opencode child: %v", agent.ErrSpawnFailed, err))
		}
	}

	client := p.newServerClient(endpoint, p.apiKey)

	sessionID := resumeSessionID
	if sessionID == "" {
		sessionID, err = client.CreateSession(ctx, createSessionReq{
			Model:    modelRef{ProviderID: OCProviderID, ID: resolvedModel(spec), Variant: string(spec.Effort)},
			Location: locationRef{Directory: spec.Cwd},
		})
		if err != nil {
			return fail(fmt.Errorf("%w: create opencode session: %v", agent.ErrSpawnFailed, err))
		}
	}

	logger := slog.With("provider", "opencode", "lane", "server", "session", sessionID)
	h := newServerHandle(child, client, sessionID, spec, logger)
	if resource != nil {
		h.releaseOwned = func() error { return p.releaseResource(resource) }
	}

	// Subscribe to the event feed BEFORE admitting the prompt so no post-prompt
	// frame is missed (the mapper synthesizes InitEvent from the first
	// in-session frame regardless of ordering).
	if err := h.start(ctx); err != nil {
		return fail(err)
	}

	if p.versionUnverified {
		h.emit(agent.SystemEvent{
			Subtype: unverifiedVersionSubtype,
			Message: fmt.Sprintf(
				"opencode binary version could not be confirmed within the verified range (pinned %s, verified through %s) — proceeding unverified",
				PinnedVersion, VerifiedAgainst,
			),
		})
	}

	if spec.Prompt != "" {
		req := promptReq{Prompt: promptInput{Text: spec.Prompt}, Delivery: "steer", Resume: resumeSessionID != ""}
		if err := client.Prompt(ctx, sessionID, req); err != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), stopGracePeriod)
			cleanupErr := h.Stop(stopCtx)
			cancel()
			return nil, errors.Join(fmt.Errorf("%w: admit opencode prompt: %v", agent.ErrSpawnFailed, err), cleanupErr)
		}
	}
	return h, nil
}

// bringUpServer either spawns a per-session serve child (CLI mode) or returns
// the attached external endpoint (HTTP mode, the OPENCODE_ENDPOINT escape
// hatch). In attach mode the injected config governs only children the
// provider owns — an external server uses its own config.
func (p *Provider) bringUpServer(ctx context.Context, spec agent.Spec, configPath string) (*serveChild, string, error) {
	if p.binary == "" {
		if p.endpoint == "" {
			return nil, "", fmt.Errorf("%w: opencode has neither a binary nor an endpoint", agent.ErrSpawnFailed)
		}
		return nil, p.endpoint, nil
	}
	child, err := startServe(ctx, serveConfig{
		binary:     p.binary,
		cwd:        spec.Cwd,
		configPath: configPath,
		env:        spec.Env,
		apiKey:     p.apiKey,
		logger:     slog.Default(),
	})
	if err != nil {
		return nil, "", err
	}
	return child, child.endpoint, nil
}

// Resume reattaches to a persisted opencode session (Lane B). In attach mode it
// reuses the running server that owns the session; in CLI mode a fresh serve
// child reopens the on-disk session store. Was ErrUnsupported before Lane B.
func (p *Provider) Resume(ctx context.Context, sessionID string, spec agent.Spec) (agent.Handle, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("provider/opencode: Resume: empty session id: %w", agent.ErrUnsupported)
	}
	if err := p.checkOpen(); err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	if err := p.validateLaunchAuthority(spec); err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	var err error
	spec, err = agent.PrepareHarness(spec, p.launchManifest())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	spec, err = applyEndpoint(spec)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	return p.spawnServer(ctx, spec, sessionID)
}

// Shutdown sweeps every registered Lane-B child and owned config, including
// resources registered by a spawn that is still in flight.
func (p *Provider) Shutdown(_ context.Context) error {
	p.mu.Lock()
	p.shuttingDown = true
	resources := make([]*openCodeServerResource, 0, len(p.resources))
	for resource := range p.resources {
		resources = append(resources, resource)
	}
	p.mu.Unlock()
	var cleanupErr error
	for _, resource := range resources {
		cleanupErr = errors.Join(cleanupErr, p.releaseResource(resource))
	}
	return cleanupErr
}

// openCodeHandle is the agent.Handle implementation backed by an
// `opencode run` subprocess.
type openCodeHandle struct {
	binary       string
	cwd          string
	cmd          *exec.Cmd
	events       chan agent.Event
	logger       *slog.Logger
	stdoutPipe   io.ReadCloser
	stderrPipe   io.ReadCloser
	stderrBuf    *boundedBuffer
	releaseOwned func() error
	cleanupOnce  sync.Once
	cleanupErr   error

	// sessionID captured from the first step_start event.
	sessionID atomic.Pointer[string]
	initSent  atomic.Bool

	stopOnce sync.Once
	stopErr  error

	shutdown chan struct{}

	eventsClosed atomic.Bool
	eventsMu     sync.RWMutex

	done    chan struct{}
	waitErr atomic.Pointer[error]
}

// SessionID returns the provider-native session id captured from the
// first NDJSON event. Empty until the first event fires.
func (h *openCodeHandle) SessionID() string {
	if v := h.sessionID.Load(); v != nil {
		return *v
	}
	return ""
}

// Events returns the read-only event channel. Closed by Stop() after
// the subprocess terminates.
func (h *openCodeHandle) Events() <-chan agent.Event { return h.events }

// Inject always returns agent.ErrUnsupported because
// SupportsMessageInjection is false for the OpenCode provider.
func (h *openCodeHandle) Inject(_ context.Context, _ string) error {
	return fmt.Errorf("provider/opencode: Inject: %w", agent.ErrUnsupported)
}

// Stop aborts the session. Idempotent; safe to call after the events
// channel has closed.
func (h *openCodeHandle) Stop(ctx context.Context) error {
	h.stopOnce.Do(func() {
		h.stopErr = h.doStop(ctx)
	})
	return h.stopErr
}

func (h *openCodeHandle) doStop(ctx context.Context) error {
	close(h.shutdown)
	defer h.closeEvents()

	select {
	case <-h.done:
		return h.cleanupOwned()
	default:
	}

	if h.cmd != nil && h.cmd.Process != nil {
		signalProcessGroup(h.cmd, syscall.SIGTERM)
	}

	timer := time.NewTimer(stopGracePeriod)
	defer timer.Stop()

	select {
	case <-h.done:
	case <-timer.C:
		if h.cmd != nil && h.cmd.Process != nil {
			signalProcessGroup(h.cmd, syscall.SIGKILL)
		}
		<-h.done
	case <-ctx.Done():
		if h.cmd != nil && h.cmd.Process != nil {
			signalProcessGroup(h.cmd, syscall.SIGKILL)
		}
		<-h.done
	}
	return h.cleanupOwned()
}

func (h *openCodeHandle) cleanupOwned() error {
	h.cleanupOnce.Do(func() {
		if h.releaseOwned != nil {
			h.cleanupErr = h.releaseOwned()
		}
	})
	return h.cleanupErr
}

func (h *openCodeHandle) sendEvent(ev agent.Event) {
	h.eventsMu.RLock()
	defer h.eventsMu.RUnlock()
	if h.eventsClosed.Load() {
		return
	}
	select {
	case h.events <- ev:
	case <-h.shutdown:
	}
}

func (h *openCodeHandle) closeEvents() {
	h.eventsMu.Lock()
	defer h.eventsMu.Unlock()
	if h.eventsClosed.Load() {
		return
	}
	h.eventsClosed.Store(true)
	close(h.events)
}

func (h *openCodeHandle) watchCtx(ctx context.Context) {
	select {
	case <-ctx.Done():
		// Intentional context.Background(): parent ctx is already canceled at
		// this branch, so deriving from it would yield an already-canceled
		// context unfit for the grace-period Stop.
		stopCtx, cancel := context.WithTimeout(context.Background(), stopGracePeriod+2*time.Second) //nolint:gosec // G118: see comment above
		defer cancel()
		_ = h.Stop(stopCtx)
	case <-h.shutdown:
	}
}

// readStdout is the goroutine that drains opencode's NDJSON stdout,
// decodes each line via mapOpenCodeLine, and forwards events onto the
// channel via sendEvent.
func (h *openCodeHandle) readStdout() {
	defer close(h.done)
	defer func() {
		err := h.cmd.Wait()
		if err != nil {
			h.waitErr.Store(&err)
		}
	}()

	// Force-close the pipe when shutdown fires to unblock the scanner.
	pipeCloseDone := make(chan struct{})
	go func() {
		defer close(pipeCloseDone)
		select {
		case <-h.done:
		case <-h.shutdown:
			_ = h.stdoutPipe.Close()
		}
	}()
	defer func() { <-pipeCloseDone }()

	scanner := bufio.NewScanner(h.stdoutPipe)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	terminal := false
	for scanner.Scan() {
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		line := append([]byte(nil), raw...)
		for _, ev := range mapOpenCodeLine(line) {
			if ev == nil {
				continue
			}
			// OpenCode emits step_start for every model/tool step. The provider
			// contract permits exactly one InitEvent per run, so capture and
			// forward only the first one.
			if typed, ok := ev.(agent.InitEvent); ok {
				if !h.initSent.CompareAndSwap(false, true) {
					continue
				}
				if typed.SessionID != "" {
					id := typed.SessionID
					h.sessionID.Store(&id)
				}
			}
			// A ResultEvent is the provider-contract terminal event
			// (agent/provider.go): once it is emitted the session is
			// complete, so the scanner-EOF path below must NOT append a
			// spurious spawn_no_result ErrorEvent. Without this flag every
			// successful run violated the "exactly one terminal event"
			// contract (D-1).
			if _, ok := ev.(agent.ResultEvent); ok {
				terminal = true
			}
			h.sendEvent(ev)
		}
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		h.sendEvent(agent.ErrorEvent{
			Message: fmt.Sprintf("provider/opencode: stdout scan: %v", err),
			Code:    "stdout_scan",
		})
		return
	}
	if terminal {
		return
	}
	stderrTail := h.stderrBuf.String()
	msg := "opencode exited without terminal result"
	if stderrTail != "" {
		msg = fmt.Sprintf("%s: stderr=%s", msg, stderrTail)
	}
	h.sendEvent(agent.ErrorEvent{
		Message: msg,
		Code:    "spawn_no_result",
	})
}

// ─── OpenCode NDJSON event mapping ──────────────────────────────────────────
//
// OpenCode --format json emits one JSON object per line. The known types are:
//
//	{"type":"step_start","sessionID":"ses_…","part":{"type":"step-start",…}}
//	{"type":"text","sessionID":"ses_…","part":{"type":"text","text":"…",…}}
//	{"type":"tool_use","sessionID":"ses_…","part":{"type":"tool","tool":"…","callID":"…","state":{…}}}
//	{"type":"step_finish","sessionID":"ses_…","part":{"type":"step-finish","reason":"stop"|"tool-calls",…,"tokens":{…},"cost":…}}
//
// We synthesize a single InitEvent from the first event's sessionID, then
// map text → AssistantTextEvent, tool_use → ToolUseEvent + ToolResultEvent
// (when state.status = "completed"), and step_finish with reason="stop" and
// no pending tool calls → ResultEvent.

// rawOpenCodeEnvelope is the discriminator-only decode for routing.
type rawOpenCodeEnvelope struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionID"`
	Part      json.RawMessage `json:"part"`
}

// rawOpenCodePart decodes the nested "part" object.
type rawOpenCodePart struct {
	Type   string  `json:"type"`
	Text   string  `json:"text,omitempty"`
	Reason string  `json:"reason,omitempty"`
	Tokens *tokens `json:"tokens,omitempty"`
	Cost   float64 `json:"cost"`
	// Tool-use part fields.
	Tool   string                `json:"tool,omitempty"`
	CallID string                `json:"callID,omitempty"`
	State  *rawOpenCodeToolState `json:"state,omitempty"`
}

// rawOpenCodeToolState carries a tool call's final state.
type rawOpenCodeToolState struct {
	Status string          `json:"status"` // "pending" | "running" | "completed" | "error"
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
}

type tokens struct {
	Total  int64 `json:"total"`
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
}

// mapOpenCodeLine decodes one NDJSON line from `opencode run --format json`
// and returns the corresponding agent.Event slice.
//
// Mapping:
//
//	step_start                         → (sessionID capture only; no event emitted here —
//	                                      the caller emits InitEvent on the first line)
//	text                               → AssistantTextEvent
//	tool_use (state.status=completed)  → ToolUseEvent + ToolResultEvent
//	tool_use (state.status=pending/running) → ToolUseEvent (no result yet)
//	step_finish (reason=stop)          → ResultEvent(success=true)
//	step_finish (reason=tool-calls)    → (internal step; no terminal event)
//	unknown / decode error             → ErrorEvent
func mapOpenCodeLine(line []byte) []agent.Event {
	var env rawOpenCodeEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return []agent.Event{agent.ErrorEvent{
			Message: fmt.Sprintf("provider/opencode: decode NDJSON envelope: %v", err),
			Code:    "decode_envelope",
			Raw:     json.RawMessage(line),
		}}
	}

	switch env.Type {
	case "step_start":
		// The sessionID is carried in every event. We synthesize a
		// single InitEvent from the first event; readStdout handles
		// the once-only emission by checking h.sessionID.
		// Return a lightweight SystemEvent carrying the session id so
		// readStdout can pick it up without extra state.
		return []agent.Event{agent.InitEvent{
			SessionID: env.SessionID,
			Raw:       json.RawMessage(line),
		}}

	case "text":
		var part rawOpenCodePart
		if err := json.Unmarshal(env.Part, &part); err != nil {
			return []agent.Event{agent.ErrorEvent{
				Message: fmt.Sprintf("provider/opencode: decode text part: %v", err),
				Code:    "decode_text",
				Raw:     json.RawMessage(line),
			}}
		}
		if part.Text == "" {
			return nil
		}
		return []agent.Event{agent.AssistantTextEvent{
			Text: part.Text,
			Raw:  json.RawMessage(line),
		}}

	case "tool_use":
		var part rawOpenCodePart
		if err := json.Unmarshal(env.Part, &part); err != nil {
			return []agent.Event{agent.ErrorEvent{
				Message: fmt.Sprintf("provider/opencode: decode tool_use part: %v", err),
				Code:    "decode_tool_use",
				Raw:     json.RawMessage(line),
			}}
		}
		return mapOpenCodeToolUse(line, &part)

	case "step_finish":
		var part rawOpenCodePart
		if err := json.Unmarshal(env.Part, &part); err != nil {
			return []agent.Event{agent.ErrorEvent{
				Message: fmt.Sprintf("provider/opencode: decode step_finish part: %v", err),
				Code:    "decode_step_finish",
				Raw:     json.RawMessage(line),
			}}
		}
		return mapOpenCodeStepFinish(line, &part)

	case "":
		return []agent.Event{agent.ErrorEvent{
			Message: "provider/opencode: NDJSON line missing top-level type",
			Code:    "missing_type",
			Raw:     json.RawMessage(line),
		}}
	default:
		return []agent.Event{agent.SystemEvent{
			Subtype: "unknown",
			Message: fmt.Sprintf("Unhandled opencode event type: %s", env.Type),
			Raw:     json.RawMessage(line),
		}}
	}
}

func mapOpenCodeToolUse(line []byte, part *rawOpenCodePart) []agent.Event {
	var input map[string]any
	if len(part.State.Input) > 0 {
		_ = json.Unmarshal(part.State.Input, &input)
	}

	toolEvent := agent.ToolUseEvent{
		ToolName:  part.Tool,
		ToolUseID: part.CallID,
		Input:     input,
		Raw:       json.RawMessage(line),
	}

	if part.State == nil || part.State.Status != "completed" {
		// Tool still pending/running — emit tool_use only.
		return []agent.Event{toolEvent}
	}

	// Tool completed — emit tool_use + tool_result.
	resultEvent := agent.ToolResultEvent{
		ToolName:  part.Tool,
		ToolUseID: part.CallID,
		Content:   part.State.Output,
		IsError:   part.State.Status == "error",
		Raw:       json.RawMessage(line),
	}
	return []agent.Event{toolEvent, resultEvent}
}

func mapOpenCodeStepFinish(line []byte, part *rawOpenCodePart) []agent.Event {
	llm := agent.LlmCallEvent{
		System:       "opencode",
		FinishReason: part.Reason,
		UsageSource:  agent.LlmUsageProvider,
	}
	if part.Tokens != nil {
		llm.InputTokens = part.Tokens.Input
		llm.OutputTokens = part.Tokens.Output
	}
	switch part.Reason {
	case "stop":
		// Terminal step — emit ResultEvent(success=true).
		var cost *agent.CostData
		if part.Tokens != nil {
			cost = &agent.CostData{
				InputTokens:  part.Tokens.Input,
				OutputTokens: part.Tokens.Output,
				TotalCostUsd: part.Cost,
			}
		}
		return []agent.Event{llm, agent.ResultEvent{
			Success: true,
			Cost:    cost,
			Raw:     json.RawMessage(line),
		}}
	case "tool-calls":
		// Intermediate step — agent is about to execute tools; not terminal.
		return []agent.Event{llm}
	case "error":
		return []agent.Event{llm, agent.ResultEvent{
			Success:      false,
			Errors:       []string{"opencode step finished with error"},
			ErrorSubtype: "error",
			Raw:          json.RawMessage(line),
		}}
	default:
		// Unknown reason — emit as a system event so runners observe it.
		return []agent.Event{llm, agent.SystemEvent{
			Subtype: "step_finish_unknown",
			Message: fmt.Sprintf("step_finish reason=%s", part.Reason),
			Raw:     json.RawMessage(line),
		}}
	}
}

// ─── Helpers shared with spawnCLI ────────────────────────────────────────────

func writePromptStdin(stdin io.WriteCloser, prompt string, logger *slog.Logger) {
	defer func() { _ = stdin.Close() }()
	if prompt == "" {
		return
	}
	if _, err := io.WriteString(stdin, prompt); err != nil {
		logger.Debug("provider/opencode: write prompt to stdin", "err", err)
	}
}

func drainStderr(r io.ReadCloser, buf *boundedBuffer, logger *slog.Logger) {
	defer func() { _ = r.Close() }()
	if _, err := io.Copy(buf, r); err != nil && !errors.Is(err, io.EOF) {
		logger.Debug("provider/opencode: drain stderr", "err", err)
	}
}

func composeEnv(parentEnv []string, specEnv map[string]string) []string {
	return runtimeenv.ComposeChildEnv(parentEnv, specEnv)
}

// boundedBuffer accumulates the last N bytes written, dropping the
// oldest data once the limit is reached. Goroutine-safe.
type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= b.limit {
		b.buf = append(b.buf[:0], p[len(p)-b.limit:]...)
		return len(p), nil
	}
	overflow := (len(b.buf) + len(p)) - b.limit
	if overflow > 0 {
		copy(b.buf, b.buf[overflow:])
		b.buf = b.buf[:len(b.buf)-overflow]
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
