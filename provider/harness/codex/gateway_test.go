package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/gateway"
)

func gatewayTestSpec(baseURL, key string) agent.Spec {
	env := map[string]string{}
	if key != "" {
		env[codexGatewayEnvKey] = key
	}
	return agent.Spec{
		Endpoint: &agent.EndpointBinding{
			BaseURL:  baseURL,
			Protocol: agent.ProtoOpenAIResponses,
		},
		Env: env,
	}
}

// gatewayCellSpec builds a session whose binding cell key differs from the
// session layer key, so tests can prove the cell key wins at every entry
// point.
func gatewayCellSpec(baseURL, cellKey, sessionKey string) agent.Spec {
	spec := agent.Spec{
		Prompt: "gateway probe",
		Endpoint: &agent.EndpointBinding{
			BaseURL:  baseURL,
			Protocol: agent.ProtoOpenAIResponses,
			Env:      map[string]string{},
		},
		Env: map[string]string{},
	}
	if cellKey != "" {
		spec.Endpoint.Env[codexGatewayEnvKey] = cellKey
	}
	if sessionKey != "" {
		spec.Env[codexGatewayEnvKey] = sessionKey
	}
	return spec
}

func TestGatewayBinding_NoBaseURLUsesTodayConfig(t *testing.T) {
	t.Parallel()
	_, _, ok, err := gatewayBinding(agent.Spec{})
	if err != nil || ok {
		t.Fatalf("gatewayBinding(empty) = ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

func TestGatewayBinding_BaseURLNeedsResponsesProtocol(t *testing.T) {
	t.Parallel()
	spec := gatewayTestSpec("https://gateway.example/codex/v1", "k")
	spec.Endpoint.Protocol = agent.ProtoOpenAIChat
	if _, _, _, err := gatewayBinding(spec); err == nil {
		t.Fatal("gatewayBinding(chat protocol) = nil error, want a loud refusal")
	}
}

func TestGatewayBinding_BaseURLNeedsKey(t *testing.T) {
	t.Parallel()
	if _, _, _, err := gatewayBinding(gatewayTestSpec("https://gateway.example/codex/v1", "")); err == nil {
		t.Fatal("gatewayBinding without key = nil error, want a loud refusal")
	}
}

func TestGatewayProviderBlock_RoundTrip(t *testing.T) {
	t.Parallel()
	boundary, err := newCodexConfigBoundary(t.TempDir(), false)
	if err != nil {
		t.Fatalf("new boundary: %v", err)
	}
	t.Cleanup(func() { _ = boundary.remove() })

	baseURL := "https://gateway.example/codex/v1"
	env, err := boundary.appendGatewayProviderBlock(baseURL, "k", map[string]string{codexGatewayEnvKey: "k"})
	if err != nil {
		t.Fatalf("append block: %v", err)
	}
	if env[codexGatewayEnvKey] != "k" {
		t.Fatalf("projected env lost the key: %v", env)
	}
	body, err := os.ReadFile(boundary.configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	text := string(body)
	for _, want := range []string{
		`model_provider = "donmai-gateway"`,
		`[model_providers.donmai-gateway]`,
		`base_url = "https://gateway.example/codex/v1"`,
		`env_key = "AI_GATEWAY_API_KEY"`,
		`wire_api = "responses"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing %q:\n%s", want, text)
		}
	}
	// Idempotent: a second append leaves the single block alone while still
	// projecting the binding-selected key.
	before := text
	env, err = boundary.appendGatewayProviderBlock(baseURL, "k2", map[string]string{})
	if err != nil {
		t.Fatalf("second append: %v", err)
	}
	if env[codexGatewayEnvKey] != "k2" {
		t.Fatalf("second append projected %q, want the binding-selected key", env[codexGatewayEnvKey])
	}
	after, err := os.ReadFile(boundary.configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if string(after) != before {
		t.Fatalf("second append changed the config:\n%s", after)
	}
}

func TestGatewayProviderBlock_RefusesEmptyKey(t *testing.T) {
	t.Parallel()
	boundary, err := newCodexConfigBoundary(t.TempDir(), false)
	if err != nil {
		t.Fatalf("new boundary: %v", err)
	}
	t.Cleanup(func() { _ = boundary.remove() })
	if _, err := boundary.appendGatewayProviderBlock("https://gateway.example/codex/v1", "", map[string]string{}); err == nil {
		t.Fatal("append with empty key = nil error, want a loud refusal")
	}
}

func TestGatewayBinding_EndpointEnvBeatsSpecEnv(t *testing.T) {
	t.Parallel()
	spec := gatewayTestSpec("https://gateway.example/codex/v1", "spec-key")
	spec.Endpoint.Env = map[string]string{codexGatewayEnvKey: "cell-key"}
	baseURL, key, ok, err := gatewayBinding(spec)
	if err != nil || !ok {
		t.Fatalf("gatewayBinding = ok=%v err=%v, want routed", ok, err)
	}
	if baseURL != "https://gateway.example/codex/v1" {
		t.Fatalf("gatewayBinding baseURL = %q, want the binding URL", baseURL)
	}
	if key != "cell-key" {
		t.Fatalf("gatewayBinding key = %q, want the binding cell key", key)
	}
}

// TestGatewayHeadlessSpawn_ProjectsCellKeyAndWritesProviderBlock drives the
// production Spawn entry point with a binding whose cell key differs from
// the session layer key. Disabling the gateway branch (or projecting the
// session key instead of the cell key) turns this red.
func TestGatewayHeadlessSpawn_ProjectsCellKeyAndWritesProviderBlock(t *testing.T) {
	const (
		baseURL    = "https://gateway.example/codex/v1"
		cellKey    = "cell-key"
		sessionKey = "spec-key"
	)
	fs, stdinW, stdoutR := newFakeServer()
	p, err := New(Options{
		skipProcess:      true,
		stdinOverride:    stdinW,
		stdoutOverride:   stdoutR,
		configTempDir:    t.TempDir(),
		HandshakeTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})
	go fs.run(t, "thread-gateway-headless")

	spec := gatewayCellSpec(baseURL, cellKey, sessionKey)
	spec.Cwd = t.TempDir()
	h, err := p.Spawn(t.Context(), spec)
	if err != nil {
		t.Fatalf("gateway Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	if got := p.pinnedSessionEnv[codexGatewayEnvKey]; got != cellKey {
		t.Fatalf("pinned child env key = %q, want the binding cell key %q", got, cellKey)
	}
	if p.config.authPath != "" {
		t.Fatalf("gateway Spawn projected host auth %q, want no operator login", p.config.authPath)
	}
	body, err := os.ReadFile(p.config.configPath)
	if err != nil {
		t.Fatalf("read private config: %v", err)
	}
	for _, want := range []string{
		`model_provider = "donmai-gateway"`,
		`base_url = "` + baseURL + `"`,
		`env_key = "AI_GATEWAY_API_KEY"`,
		`wire_api = "responses"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("private config missing %q:\n%s", want, body)
		}
	}
}

// TestGatewayHeadlessSpawn_RefusesHostSessionAuthCombination proves the
// production Spawn entry point refuses a gateway cell on a provider built
// with host-session auth, before any start side effect.
func TestGatewayHeadlessSpawn_RefusesHostSessionAuthCombination(t *testing.T) {
	hostHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostHome, codexAuthFileName), []byte(`{"auth_mode":"chatgpt"}`), 0o600); err != nil {
		t.Fatalf("write host auth: %v", err)
	}
	t.Setenv("CODEX_HOME", hostHome)

	fs, stdinW, stdoutR := newFakeServer()
	p, err := New(Options{
		HostSessionAuth:  true,
		skipProcess:      true,
		stdinOverride:    stdinW,
		stdoutOverride:   stdoutR,
		configTempDir:    t.TempDir(),
		HandshakeTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})

	spec := gatewayCellSpec("https://gateway.example/codex/v1", "cell-key", "spec-key")
	spec.Cwd = t.TempDir()
	_, err = p.Spawn(t.Context(), spec)
	if !errors.Is(err, agent.ErrSpawnFailed) || !strings.Contains(err.Error(), "cannot combine") {
		t.Fatalf("gateway+host-auth Spawn error = %v, want a cannot-combine refusal", err)
	}
	if p.started || p.client != nil {
		t.Fatal("gateway+host-auth Spawn started the app-server despite the refusal")
	}
}

// TestSpawnInteractive_GatewayRouteNeedsNoOperatorAuth drives the
// production interactive entry point with only a gateway key and no host
// login. The pre-PTY hook observes the private config and child env, then
// aborts before any PTY starts. Resolving operator auth before the gateway
// route (or forcing the non-routed branch) turns this red: the former
// refuses with an auth-projection error before the hook runs, the latter
// spawns the PTY without ever calling the hook.
func TestSpawnInteractive_GatewayRouteNeedsNoOperatorAuth(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	clearInteractiveCodexAuthEnv(t)
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "absent-codex-home"))
	workdir := t.TempDir()
	boundaryRoot := t.TempDir()
	bin := writeFakeCodexScript(t, `exit 0`)

	const (
		baseURL    = "https://gateway.example/codex/v1"
		cellKey    = "cell-key"
		sessionKey = "spec-key"
	)
	spec := gatewayCellSpec(baseURL, cellKey, sessionKey)
	spec.Cwd = workdir
	spec.Interactive = &agent.InteractiveSpec{Cols: 80, Rows: 24}
	// The platform authority shape is what marks the project untrusted in
	// the effective-config preflight the fake inventory runner asserts.
	mcpServers := []agent.MCPServerConfig{{
		Name: "donmai-platform",
		Type: "http",
		URL:  "https://platform.example.com/api/mcp/session",
		Headers: map[string]string{
			"Authorization": "Bearer session-mcp-bearer",
		},
	}}
	spec.MCPServers = mcpServers

	var (
		hookCalled bool
		seenKey    string
		seenBlock  bool
	)
	_, err := SpawnInteractive(context.Background(), Options{
		CodexBin:                      bin,
		configTempDir:                 boundaryRoot,
		interactiveMCPInventoryRunner: inventoryRunnerFor(t, mcpServers),
		interactiveBeforePTYSpawn: func(_ context.Context, _ string, _ agent.Spec, launch interactiveLaunch, home string) error {
			hookCalled = true
			seenKey = launch.env[codexGatewayEnvKey]
			body, rerr := os.ReadFile(filepath.Join(home, codexConfigFileName))
			if rerr != nil {
				return rerr
			}
			seenBlock = strings.Contains(string(body), `model_provider = "donmai-gateway"`) &&
				strings.Contains(string(body), `base_url = "`+baseURL+`"`)
			if _, serr := os.Lstat(filepath.Join(home, codexAuthFileName)); !errors.Is(serr, os.ErrNotExist) {
				return errors.New("gateway boundary carries an operator auth file")
			}
			return errors.New("stop before PTY")
		},
	}, spec)
	if !hookCalled {
		t.Fatal("gateway cell never reached the isolated pre-PTY boundary")
	}
	if !seenBlock {
		t.Fatal("private interactive config lacks the gateway provider block")
	}
	if seenKey != cellKey {
		t.Fatalf("pre-PTY child env key = %q, want the binding cell key %q", seenKey, cellKey)
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("hook-aborted SpawnInteractive error = %v, want ErrSpawnFailed", err)
	}
	if entries, rerr := os.ReadDir(boundaryRoot); rerr != nil || len(entries) != 0 {
		t.Fatalf("hook-aborted spawn leaked its private boundary: err=%v entries=%v", rerr, entries)
	}
}

// TestGatewayHeadlessResumeAfterSpawnWithEndpointKey drives the production
// Spawn then Resume entry points on the same provider with an endpoint-bound
// gateway key, and the resume succeeds with the child carrying the endpoint
// key. Against the pre-fix comparison (raw Spec.Env instead of the projected
// layer) the Resume refuses with errSessionEnvConflict naming the gateway env
// name — the exact stop-and-resume steering failure this change fixes.
func TestGatewayHeadlessResumeAfterSpawnWithEndpointKey(t *testing.T) {
	const (
		baseURL    = "https://gateway.example/codex/v1"
		cellKey    = "cell-key"
		sessionKey = "spec-key"
	)
	fs, stdinW, stdoutR := newFakeServer()
	p, err := New(Options{
		skipProcess:      true,
		stdinOverride:    stdinW,
		stdoutOverride:   stdoutR,
		configTempDir:    t.TempDir(),
		HandshakeTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})
	go fs.run(t, "thread-gateway-resume")

	spec := gatewayCellSpec(baseURL, cellKey, sessionKey)
	spec.Cwd = t.TempDir()
	spawned, err := p.Spawn(t.Context(), spec)
	if err != nil {
		t.Fatalf("gateway Spawn: %v", err)
	}
	t.Cleanup(func() { _ = spawned.Stop(context.Background()) })
	spawnThreadID := spawned.SessionID()
	if spawnThreadID == "" {
		t.Fatal("Spawn returned an empty session id")
	}

	resumed, err := p.Resume(t.Context(), spawnThreadID, gatewayCellSpec(baseURL, cellKey, sessionKey))
	if err != nil {
		t.Fatalf("gateway Resume on the same provider: %v", err)
	}
	t.Cleanup(func() { _ = resumed.Stop(context.Background()) })
	if resumed.SessionID() != spawnThreadID {
		t.Fatalf("resumed session id = %q, want the spawned thread %q", resumed.SessionID(), spawnThreadID)
	}
	if got := p.pinnedSessionEnv[codexGatewayEnvKey]; got != cellKey {
		t.Fatalf("pinned child env key = %q, want the binding cell key %q", got, cellKey)
	}
}

// TestGatewayHeadlessResume_RefusesDifferentEndpointKey drives the
// production Spawn then Resume entry points on the same provider, where the
// Resume carries a different endpoint key. The running child was started
// with the first key pinned into its environment layer, so comparing
// against anything but the pinned gateway key would serve the second
// session the first session's credential. It must fail closed: the Resume
// refuses with errSessionEnvConflict naming the gateway env name, quotes
// neither key, and leaves the child's pinned key untouched.
//
// RED proof: compare the Resume against the candidate spec's raw Spec.Env
// (or drop the gateway projection) and this goes red — the Resume is
// refused for carrying exactly what the child was started with, or the
// divergence check misses the pinned-key difference.
func TestGatewayHeadlessResume_RefusesDifferentEndpointKey(t *testing.T) {
	const (
		baseURL    = "https://gateway.example/codex/v1"
		cellKey    = "cell-key"
		sessionKey = "spec-key"
		otherKey   = "other-cell-key"
	)
	fs, stdinW, stdoutR := newFakeServer()
	p, err := New(Options{
		skipProcess:      true,
		stdinOverride:    stdinW,
		stdoutOverride:   stdoutR,
		configTempDir:    t.TempDir(),
		HandshakeTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})
	go fs.run(t, "thread-gateway-key-refusal")

	spec := gatewayCellSpec(baseURL, cellKey, sessionKey)
	spec.Cwd = t.TempDir()
	spawned, err := p.Spawn(t.Context(), spec)
	if err != nil {
		t.Fatalf("gateway Spawn: %v", err)
	}
	t.Cleanup(func() { _ = spawned.Stop(context.Background()) })
	spawnThreadID := spawned.SessionID()
	if spawnThreadID == "" {
		t.Fatal("Spawn returned an empty session id")
	}

	_, err = p.Resume(t.Context(), spawnThreadID, gatewayCellSpec(baseURL, otherKey, sessionKey))
	if err == nil {
		t.Fatal("Resume with a different endpoint key was accepted, want a fail-closed refusal")
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("refusal error %v does not wrap agent.ErrSpawnFailed", err)
	}
	if !errors.Is(err, errSessionEnvConflict) {
		t.Fatalf("refusal error %v does not wrap errSessionEnvConflict", err)
	}
	if message := err.Error(); !strings.Contains(message, codexGatewayEnvKey) {
		t.Fatalf("refusal error %q does not name the gateway env name", message)
	}
	for _, secret := range []string{cellKey, otherKey} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("refusal error quotes a gateway key: %q", err.Error())
		}
	}
	if got := p.pinnedSessionEnv[codexGatewayEnvKey]; got != cellKey {
		t.Fatalf("pinned child env key = %q, want the spawned key %q", got, cellKey)
	}
}

// TestGatewayHeadlessResume_RefusesDifferentBaseURL drives the production
// Spawn then Resume entry points on the same provider, where the Resume
// carries the same key but a different gateway base URL. The private
// config's provider block is append-once, so the running child stays on the
// first gateway no matter what the Resume names; accepting the Resume would
// hand the caller a session that claims one route while serving another.
// It must fail closed with errGatewayRouteConflict, quote neither URL, and
// leave the private config and pinned route on the first gateway.
//
// RED proof: drop the checkGatewayRouteLocked call from ensureHeadlessReady
// and the Resume succeeds while the config still carries the first base URL
// — exactly the silent misroute this refuses.
func TestGatewayHeadlessResume_RefusesDifferentBaseURL(t *testing.T) {
	const (
		firstBaseURL  = "https://gateway.example/codex/v1"
		secondBaseURL = "https://gateway.example/codex/v2"
		cellKey       = "cell-key"
		sessionKey    = "spec-key"
	)
	fs, stdinW, stdoutR := newFakeServer()
	p, err := New(Options{
		skipProcess:      true,
		stdinOverride:    stdinW,
		stdoutOverride:   stdoutR,
		configTempDir:    t.TempDir(),
		HandshakeTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})
	go fs.run(t, "thread-gateway-url-refusal")

	spec := gatewayCellSpec(firstBaseURL, cellKey, sessionKey)
	spec.Cwd = t.TempDir()
	spawned, err := p.Spawn(t.Context(), spec)
	if err != nil {
		t.Fatalf("gateway Spawn: %v", err)
	}
	t.Cleanup(func() { _ = spawned.Stop(context.Background()) })
	spawnThreadID := spawned.SessionID()
	if spawnThreadID == "" {
		t.Fatal("Spawn returned an empty session id")
	}

	_, err = p.Resume(t.Context(), spawnThreadID, gatewayCellSpec(secondBaseURL, cellKey, sessionKey))
	if err == nil {
		t.Fatal("Resume with a different base URL was accepted, want a fail-closed refusal")
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("refusal error %v does not wrap agent.ErrSpawnFailed", err)
	}
	if !errors.Is(err, errGatewayRouteConflict) {
		t.Fatalf("refusal error %v does not wrap errGatewayRouteConflict", err)
	}
	for _, url := range []string{firstBaseURL, secondBaseURL} {
		if strings.Contains(err.Error(), url) {
			t.Fatalf("refusal error quotes a gateway URL: %q", err.Error())
		}
	}
	body, rerr := os.ReadFile(p.config.configPath)
	if rerr != nil {
		t.Fatalf("read private config: %v", rerr)
	}
	if !strings.Contains(string(body), `base_url = "`+firstBaseURL+`"`) {
		t.Fatalf("private config lost the first gateway route:\n%s", body)
	}
	if strings.Contains(string(body), secondBaseURL) {
		t.Fatalf("refused Resume re-pointed the private config:\n%s", body)
	}
	if !p.gatewayRoutePinned || p.pinnedGatewayBaseURL != firstBaseURL || !p.pinnedGatewayRouted {
		t.Fatalf("pinned route = %q routed=%v pinned=%v, want the spawned route",
			p.pinnedGatewayBaseURL, p.pinnedGatewayRouted, p.gatewayRoutePinned)
	}
}

// TestGatewayHeadlessSpawn_SpecKeyFallback drives the production Spawn entry
// point with the gateway key carried only on the session layer (the shape
// production uses today). Dropping the session-layer fallback from
// gatewayBinding turns this red with a missing-key refusal.
func TestGatewayHeadlessSpawn_SpecKeyFallback(t *testing.T) {
	const (
		baseURL = "https://gateway.example/codex/v1"
		key     = "spec-only-key"
	)
	fs, stdinW, stdoutR := newFakeServer()
	p, err := New(Options{
		skipProcess:      true,
		stdinOverride:    stdinW,
		stdoutOverride:   stdoutR,
		configTempDir:    t.TempDir(),
		HandshakeTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})
	go fs.run(t, "thread-gateway-speckey")

	spec := gatewayTestSpec(baseURL, key)
	spec.Prompt = "gateway probe"
	spec.Cwd = t.TempDir()
	h, err := p.Spawn(t.Context(), spec)
	if err != nil {
		t.Fatalf("gateway Spawn with session-layer key: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()
	if got := p.pinnedSessionEnv[codexGatewayEnvKey]; got != key {
		t.Fatalf("pinned child env key = %q, want the session-layer key %q", got, key)
	}
}

// TestSpawnInteractive_GatewayRouteWithoutPlatformMCP drives the production
// interactive entry point with a gateway binding and no platform MCP
// authority. The gateway route must still land in the isolated boundary:
// skipping routing whenever the platform entry is absent (the non-routed
// ambient branch) never calls the pre-PTY hook, so this goes red.
func TestSpawnInteractive_GatewayRouteWithoutPlatformMCP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	clearInteractiveCodexAuthEnv(t)
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "absent-codex-home"))
	workdir := t.TempDir()
	boundaryRoot := t.TempDir()
	bin := writeFakeCodexScript(t, `exit 0`)

	const (
		baseURL = "https://gateway.example/codex/v1"
		cellKey = "cell-key"
	)
	spec := gatewayCellSpec(baseURL, cellKey, "")
	spec.Cwd = workdir
	spec.Interactive = &agent.InteractiveSpec{Cols: 80, Rows: 24}

	var (
		hookCalled bool
		seenKey    string
		seenBlock  bool
	)
	_, err := SpawnInteractive(context.Background(), Options{
		CodexBin:      bin,
		configTempDir: boundaryRoot,
		interactiveMCPInventoryRunner: func(_ context.Context, _ string, _ string, _ []string, _ []string, queryArgs []string) ([]byte, error) {
			if slices.Contains(queryArgs, "list") {
				return json.Marshal([]codexMCPInventoryEntry{})
			}
			return nil, errors.New("unexpected fake inventory query")
		},
		interactiveBeforePTYSpawn: func(_ context.Context, _ string, _ agent.Spec, launch interactiveLaunch, home string) error {
			hookCalled = true
			seenKey = launch.env[codexGatewayEnvKey]
			body, rerr := os.ReadFile(filepath.Join(home, codexConfigFileName))
			if rerr != nil {
				return rerr
			}
			seenBlock = strings.Contains(string(body), `model_provider = "donmai-gateway"`) &&
				strings.Contains(string(body), `base_url = "`+baseURL+`"`)
			if _, serr := os.Lstat(filepath.Join(home, codexAuthFileName)); !errors.Is(serr, os.ErrNotExist) {
				return errors.New("gateway boundary carries an operator auth file")
			}
			return errors.New("stop before PTY")
		},
	}, spec)
	if !hookCalled {
		t.Fatal("gateway cell without platform MCP never reached the isolated pre-PTY boundary")
	}
	if !seenBlock {
		t.Fatal("private interactive config lacks the gateway provider block")
	}
	if seenKey != cellKey {
		t.Fatalf("pre-PTY child env key = %q, want the binding cell key %q", seenKey, cellKey)
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("hook-aborted SpawnInteractive error = %v, want ErrSpawnFailed", err)
	}
	if entries, rerr := os.ReadDir(boundaryRoot); rerr != nil || len(entries) != 0 {
		t.Fatalf("hook-aborted spawn leaked its private boundary: err=%v entries=%v", rerr, entries)
	}
}

// gatewayLoopbackCellSpec builds a loopback session whose binding carries the
// per-session bearer under the gateway bearer name. Loopback bindings never
// carry the remote provider-key shape.
func gatewayLoopbackCellSpec(baseURL, cellBearer, sessionBearer string) agent.Spec {
	spec := agent.Spec{
		Prompt: "gateway loopback probe",
		Endpoint: &agent.EndpointBinding{
			BaseURL:  baseURL,
			Protocol: agent.ProtoOpenAIResponses,
			Env:      map[string]string{},
		},
		Env: map[string]string{},
	}
	if cellBearer != "" {
		spec.Endpoint.Env[gateway.TokenEnvVar] = cellBearer
	}
	if sessionBearer != "" {
		spec.Env[gateway.TokenEnvVar] = sessionBearer
	}
	return spec
}

func TestIsLoopbackGatewayBaseURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		url  string
		want bool
	}{
		{"http://127.0.0.1:1/v1", true},
		{"http://127.0.0.1:8080/v1", true},
		{"http://localhost:8080/v1", true},
		{"http://[::1]:8080/v1", true},
		{"https://127.0.0.1:8080/v1", true},
		{"https://gateway.example/codex/v1", false},
		{"https://api.openai.com/v1", false},
		{"http://192.0.2.1/v1", false},
		{"", false},
		{"not-a-url", false},
	}
	for _, tc := range cases {
		if got := isLoopbackGatewayBaseURL(tc.url); got != tc.want {
			t.Errorf("isLoopbackGatewayBaseURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func TestGatewayBinding_LoopbackCarriesBearer(t *testing.T) {
	t.Parallel()
	const baseURL = "http://127.0.0.1:9/v1"
	// Loopback with the bearer on the binding resolves.
	spec := gatewayLoopbackCellSpec(baseURL, "loop-bearer", "")
	gotURL, gotKey, ok, err := gatewayBinding(spec)
	if err != nil || !ok {
		t.Fatalf("loopback gatewayBinding = ok=%v err=%v, want routed", ok, err)
	}
	if gotURL != baseURL || gotKey != "loop-bearer" {
		t.Fatalf("loopback gatewayBinding = %q/%q, want %q/loop-bearer", gotURL, gotKey, baseURL)
	}
	// Loopback without any bearer fails naming the bearer env name.
	if _, _, _, err := gatewayBinding(gatewayLoopbackCellSpec(baseURL, "", "")); err == nil {
		t.Fatal("loopback without bearer = nil error, want a loud refusal")
	} else if !strings.Contains(err.Error(), gateway.TokenEnvVar) {
		t.Fatalf("loopback refusal %q does not name %s", err.Error(), gateway.TokenEnvVar)
	}
	// Loopback with only the remote provider key does not satisfy the bearer.
	remoteOnly := agent.Spec{
		Endpoint: &agent.EndpointBinding{
			BaseURL:  baseURL,
			Protocol: agent.ProtoOpenAIResponses,
			Env:      map[string]string{codexGatewayEnvKey: "remote-key"},
		},
		Env: map[string]string{},
	}
	if _, _, _, err := gatewayBinding(remoteOnly); err == nil {
		t.Fatal("loopback with only the remote key = nil error, want a bearer refusal")
	} else if !strings.Contains(err.Error(), gateway.TokenEnvVar) {
		t.Fatalf("loopback remote-key refusal %q does not name %s", err.Error(), gateway.TokenEnvVar)
	}
}

func TestGatewayBinding_RemoteKeepsProviderKeyShape(t *testing.T) {
	t.Parallel()
	const baseURL = "https://gateway.example/codex/v1"
	// Remote with the provider key on the binding resolves.
	spec := gatewayCellSpec(baseURL, "cell-key", "")
	gotURL, gotKey, ok, err := gatewayBinding(spec)
	if err != nil || !ok {
		t.Fatalf("remote gatewayBinding = ok=%v err=%v, want routed", ok, err)
	}
	if gotURL != baseURL || gotKey != "cell-key" {
		t.Fatalf("remote gatewayBinding = %q/%q, want %q/cell-key", gotURL, gotKey, baseURL)
	}
	// Remote with only the loopback bearer does not satisfy the provider key.
	bearerOnly := gatewayLoopbackCellSpec(baseURL, "loop-bearer", "")
	// gatewayLoopbackCellSpec uses a loopback bearer name on a remote URL:
	// the remote shape must not accept it.
	if _, _, _, err := gatewayBinding(bearerOnly); err == nil {
		t.Fatal("remote with only the loopback bearer = nil error, want a provider-key refusal")
	} else if !strings.Contains(err.Error(), codexGatewayEnvKey) {
		t.Fatalf("remote bearer refusal %q does not name %s", err.Error(), codexGatewayEnvKey)
	}
}

func TestGatewayBinding_LoopbackCellBeatsSession(t *testing.T) {
	t.Parallel()
	const baseURL = "http://127.0.0.1:9/v1"
	spec := gatewayLoopbackCellSpec(baseURL, "cell-bearer", "session-bearer")
	_, gotKey, ok, err := gatewayBinding(spec)
	if err != nil || !ok {
		t.Fatalf("loopback gatewayBinding = ok=%v err=%v, want routed", ok, err)
	}
	if gotKey != "cell-bearer" {
		t.Fatalf("loopback gatewayBinding key = %q, want the binding cell bearer", gotKey)
	}
}

// TestSpawnInteractive_LoopbackNamedAndUnnamedShareSelection drives the
// production interactive entry point twice with the same loopback binding:
// once unnamed and once named. Both must reach the same isolated pre-PTY
// boundary with the same provider block and the same projected child key,
// and both must clean the private boundary when the hook aborts. A named
// path that fell back to ambient (or a loopback that skipped the isolated
// boundary) never calls the hook, so either goes red.
func TestSpawnInteractive_LoopbackNamedAndUnnamedShareSelection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	const (
		baseURL    = "http://127.0.0.1:9/v1"
		cellBearer = "loop-cell-bearer"
	)
	for _, tc := range []struct {
		name        string
		sessionName string
	}{
		{name: "unnamed", sessionName: ""},
		{name: "named", sessionName: "loopback-named-session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearInteractiveCodexAuthEnv(t)
			t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "absent-codex-home"))
			workdir := t.TempDir()
			boundaryRoot := t.TempDir()
			bin := writeFakeCodexScript(t, `exit 0`)

			spec := gatewayLoopbackCellSpec(baseURL, cellBearer, "")
			spec.Cwd = workdir
			spec.SessionName = tc.sessionName
			spec.Interactive = &agent.InteractiveSpec{Cols: 80, Rows: 24}

			var (
				hookCalled bool
				seenKey    string
				seenBlock  bool
				seenArg    bool
			)
			_, err := SpawnInteractive(context.Background(), Options{
				CodexBin:      bin,
				configTempDir: boundaryRoot,
				interactiveMCPInventoryRunner: func(_ context.Context, _ string, _ string, _ []string, _ []string, queryArgs []string) ([]byte, error) {
					if slices.Contains(queryArgs, "list") {
						return json.Marshal([]codexMCPInventoryEntry{})
					}
					return nil, errors.New("unexpected fake inventory query")
				},
				interactiveBeforePTYSpawn: func(_ context.Context, _ string, spec agent.Spec, launch interactiveLaunch, home string) error {
					hookCalled = true
					seenKey = launch.env[codexGatewayEnvKey]
					if _, hasBearer := launch.env[gateway.TokenEnvVar]; hasBearer {
						return errors.New("private child env carries the loopback bearer name")
					}
					body, rerr := os.ReadFile(filepath.Join(home, codexConfigFileName))
					if rerr != nil {
						return rerr
					}
					seenBlock = strings.Contains(string(body), `model_provider = "donmai-gateway"`) &&
						strings.Contains(string(body), `base_url = "`+baseURL+`"`) &&
						strings.Contains(string(body), `env_key = "`+codexGatewayEnvKey+`"`)
					seenArg = slices.ContainsFunc(launch.argv, func(arg string) bool {
						return strings.Contains(arg, baseURL)
					})
					if _, serr := os.Lstat(filepath.Join(home, codexAuthFileName)); !errors.Is(serr, os.ErrNotExist) {
						return errors.New("gateway boundary carries an operator auth file")
					}
					return errors.New("stop before PTY")
				},
			}, spec)
			if !hookCalled {
				t.Fatal("loopback cell never reached the isolated pre-PTY boundary")
			}
			if !seenBlock {
				t.Fatal("private interactive config lacks the loopback provider block")
			}
			if !seenArg {
				t.Fatal("interactive argv lacks the loopback provider override")
			}
			if seenKey != cellBearer {
				t.Fatalf("pre-PTY child env key = %q, want the binding cell bearer %q", seenKey, cellBearer)
			}
			if !errors.Is(err, agent.ErrSpawnFailed) {
				t.Fatalf("hook-aborted SpawnInteractive error = %v, want ErrSpawnFailed", err)
			}
			if entries, rerr := os.ReadDir(boundaryRoot); rerr != nil || len(entries) != 0 {
				t.Fatalf("hook-aborted spawn leaked its private boundary: err=%v entries=%v", rerr, entries)
			}
		})
	}
}

// TestSpawnInteractive_LoopbackExclusiveMCPNotLoosened proves the loopback
// route keeps the exclusive-MCP readback: an inventory that reports an extra
// server fails closed before any PTY starts and still cleans the boundary.
func TestSpawnInteractive_LoopbackExclusiveMCPNotLoosened(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	clearInteractiveCodexAuthEnv(t)
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "absent-codex-home"))
	workdir := t.TempDir()
	boundaryRoot := t.TempDir()
	bin := writeFakeCodexScript(t, `exit 0`)

	const (
		baseURL    = "http://127.0.0.1:9/v1"
		cellBearer = "loop-cell-bearer"
	)
	mcpServers := []agent.MCPServerConfig{{
		Name: "donmai-platform",
		Type: "http",
		URL:  "https://platform.example.com/api/mcp/session",
		Headers: map[string]string{
			"Authorization": "Bearer session-mcp-bearer",
		},
	}}
	spec := gatewayLoopbackCellSpec(baseURL, cellBearer, "")
	spec.Cwd = workdir
	spec.Interactive = &agent.InteractiveSpec{Cols: 80, Rows: 24}
	spec.MCPServers = mcpServers

	_, err := SpawnInteractive(context.Background(), Options{
		CodexBin:      bin,
		configTempDir: boundaryRoot,
		interactiveMCPInventoryRunner: func(_ context.Context, _ string, _ string, _ []string, _ []string, queryArgs []string) ([]byte, error) {
			if slices.Contains(queryArgs, "list") {
				entries := []codexMCPInventoryEntry{
					{Name: "donmai-platform", Enabled: true, Transport: codexMCPInventoryTransport{Type: "streamable_http", URL: "https://platform.example.com/api/mcp/session", EnvHTTPHeaders: map[string]string{"Authorization": codexHTTPHeaderEnvName("donmai-platform", "Authorization")}}},
					{Name: "extra-server", Enabled: true, Transport: codexMCPInventoryTransport{Type: "stdio", Command: "/usr/bin/false"}},
				}
				return json.Marshal(entries)
			}
			return nil, errors.New("unexpected fake inventory query")
		},
		interactiveBeforePTYSpawn: func(_ context.Context, _ string, _ agent.Spec, _ interactiveLaunch, _ string) error {
			return errors.New("exclusive check must fail before the pre-PTY hook")
		},
	}, spec)
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("extra-server SpawnInteractive error = %v, want ErrSpawnFailed", err)
	}
	if !errors.Is(err, ErrInteractiveCodexMCPIsolation) {
		t.Fatalf("extra-server SpawnInteractive error = %v, want the exclusive-MCP isolation sentinel", err)
	}
	if entries, rerr := os.ReadDir(boundaryRoot); rerr != nil || len(entries) != 0 {
		t.Fatalf("refused spawn leaked its private boundary: err=%v entries=%v", rerr, entries)
	}
}

// TestGatewayHeadlessSpawn_LoopbackProjectsBearer proves the headless lane
// also accepts a loopback binding carrying the per-session bearer: the child
// env carries the projected provider key, the private config carries the
// loopback route, and no operator login is projected. Remote coverage above
// pins the unchanged remote shape; this pins the additive loopback shape.
func TestGatewayHeadlessSpawn_LoopbackProjectsBearer(t *testing.T) {
	const (
		baseURL    = "http://127.0.0.1:9/v1"
		cellBearer = "loop-cell-bearer"
	)
	fs, stdinW, stdoutR := newFakeServer()
	p, err := New(Options{
		skipProcess:      true,
		stdinOverride:    stdinW,
		stdoutOverride:   stdoutR,
		configTempDir:    t.TempDir(),
		HandshakeTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		fs.close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		fs.close()
	})
	go fs.run(t, "thread-gateway-loopback-headless")

	spec := gatewayLoopbackCellSpec(baseURL, cellBearer, "")
	spec.Cwd = t.TempDir()
	h, err := p.Spawn(t.Context(), spec)
	if err != nil {
		t.Fatalf("loopback gateway Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	if got := p.pinnedSessionEnv[codexGatewayEnvKey]; got != cellBearer {
		t.Fatalf("pinned child env key = %q, want the loopback cell bearer %q", got, cellBearer)
	}
	if p.config.authPath != "" {
		t.Fatalf("loopback gateway Spawn projected host auth %q, want no operator login", p.config.authPath)
	}
	body, err := os.ReadFile(p.config.configPath)
	if err != nil {
		t.Fatalf("read private config: %v", err)
	}
	for _, want := range []string{
		`model_provider = "donmai-gateway"`,
		`base_url = "` + baseURL + `"`,
		`env_key = "AI_GATEWAY_API_KEY"`,
		`wire_api = "responses"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("private config missing %q:\n%s", want, body)
		}
	}
}

// TestGatewayBinding_LoopbackSessionFallback proves a loopback binding with
// the bearer only on the session layer still resolves, the shape production
// uses when the runner forwards the bearer without endpoint env.
func TestGatewayBinding_LoopbackSessionFallback(t *testing.T) {
	t.Parallel()
	const baseURL = "http://127.0.0.1:9/v1"
	spec := gatewayLoopbackCellSpec(baseURL, "", "session-bearer")
	_, gotKey, ok, err := gatewayBinding(spec)
	if err != nil || !ok {
		t.Fatalf("loopback session-fallback gatewayBinding = ok=%v err=%v, want routed", ok, err)
	}
	if gotKey != "session-bearer" {
		t.Fatalf("loopback session-fallback key = %q, want session-bearer", gotKey)
	}
}
