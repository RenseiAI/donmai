package pi

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// child_env_real_binary_test.go — real `pi` evidence for the allowlisted
// exec environment (child_env.go). Gated on pi and node being on PATH
// (realBinaryAvailable), like every real-binary test in this package.
//
// The threat is concrete: pi rewrites its process title at startup, and a
// same-user process listing then prints the leading strings of its
// exec-time environment block as if they were arguments. These tests read
// that block straight from the kernel for the LIVE pi process
// (processListingStrings: KERN_PROCARGS2 on macOS, /proc on Linux) — the
// bytes any listing renders — and, on macOS, the literal `pgrep -fl` output
// an operator would see. No sentinel may appear there, while every
// credential family still authenticates and the agent's tools still receive
// the session's other bindings.

// listingProbe samples the live session's process listings once, from
// inside the model stub (the pi process is alive and between tool calls
// while it waits on a model response).
type listingProbe struct {
	stateRoot string

	mu       sync.Mutex
	sampled  bool
	pids     []int
	leaks    []string
	scanErr  error
	pgrepOut string
}

func (p *listingProbe) sample() {
	listings, err := sessionProcessListings(p.stateRoot)
	pgrep := ""
	if runtime.GOOS == "darwin" {
		// The literal command an operator runs while debugging.
		out, _ := exec.Command("pgrep", "-fl", ".").Output() //nolint:gosec // fixed test-only command
		pgrep = string(out)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sampled = true
	p.scanErr = err
	p.pgrepOut = pgrep
	for pid, strs := range listings {
		p.pids = append(p.pids, pid)
		for _, s := range strs {
			if strings.Contains(s, "sentinel-") {
				p.leaks = append(p.leaks, fmt.Sprintf("pid %d exec-time block carries %q", pid, s))
			}
		}
	}
}

// assertNoLeak fails unless the probe sampled at least one live session
// process and found no sentinel in its exec-time block or its pgrep line.
func (p *listingProbe) assertNoLeak(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.sampled {
		t.Fatal("the listing was never sampled while the session was live")
	}
	if p.scanErr != nil {
		t.Skipf("process listing unavailable on this platform: %v", p.scanErr)
	}
	if len(p.pids) == 0 {
		t.Fatal("no live process carried this session's state root; the listing check would be vacuous")
	}
	for _, leak := range p.leaks {
		t.Error(leak)
	}
	if runtime.GOOS != "darwin" {
		return
	}
	seen := 0
	for _, line := range strings.Split(p.pgrepOut, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		for _, sessionPID := range p.pids {
			if pid != sessionPID {
				continue
			}
			seen++
			if strings.Contains(line, "sentinel-") {
				t.Errorf("pgrep -fl renders a sentinel for the live pi process: %q", line)
			}
		}
	}
	if seen == 0 {
		t.Error("pgrep -fl listed no live session process; the listing check would be vacuous")
	}
}

// realSessionSentinelSpec is the sentinel table on Spec.Env for a real
// session. The injected provider's key is left to the binding (applyEndpoint
// mirrors it), so the stub can assert the session authenticated with the
// binding's own credential.
func realSessionSentinelSpec(spec agent.Spec) agent.Spec {
	env := sentinelSpecEnv()
	delete(env, PiKeyEnvVar)
	spec.Env = env
	return spec
}

// assertToolEnvironment checks the environment a real bash tool call
// recorded: every tool binding arrives with the session's own value, and
// no model credential or refused name arrives at all.
func assertToolEnvironment(t *testing.T, workdir string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(workdir, "tool-env.txt")) //nolint:gosec // test-owned capture
	if err != nil {
		t.Fatalf("the bash tool recorded no environment: %v", err)
	}
	toolEnv := envSliceMap(strings.Split(strings.TrimSpace(string(raw)), "\n"))
	for _, s := range childEnvSentinels {
		got, present := toolEnv[s.name]
		switch s.route {
		case routeDeferred:
			if want := sentinelValue("spec", s.name); got != want {
				t.Errorf("tool saw %s = %q, want the session's own %q restored from the credential file", s.name, got, want)
			}
		default:
			if present {
				t.Errorf("tool saw %s (%q); a model credential or refused name must not reach tools", s.name, got)
			}
		}
	}
}

// TestRealBinary_SessionEnvironment_HeadlessNeverInListing drives a REAL
// headless pi session carrying the full sentinel table on Spec.Env and in
// the parent environment. The model stub makes pi run one bash tool call
// that records its environment. Proves, against the real binary:
//   - the live pi process's exec-time block (and its pgrep line) carries no
//     sentinel, sampled while the session runs;
//   - the tool received every session binding, restored from the credential
//     file, and no model credential or refused name;
//   - the session authenticated with the binding's key, read from the file.
//
// RED proofs: admit every name in childEnvAllowed and the listing check
// fails with the sentinels quoted; make restoreSessionEnvironment
// (extensions/donmai-policy.ts) a no-op and the tool check fails.
func TestRealBinary_SessionEnvironment_HeadlessNeverInListing(t *testing.T) {
	realBinaryAvailable(t)
	// Not parallel: writes the sentinel table into the process environment.
	setParentSentinels(t)
	workdir := t.TempDir()
	probe := &listingProbe{stateRoot: newSessionLayout(workdir).root}
	stub := newRealBinaryStub(t, realBinaryModel)
	var authMu sync.Mutex
	var auths []string
	stub.responses = []stubResponse{
		{ToolCall: &stubToolCall{ID: "session-env-call", Name: "bash", Arguments: `{"command":"env > tool-env.txt"}`}},
		{Text: "session-env-done"},
	}
	stub.beforeResponse = func(r *http.Request, n int) {
		authMu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		authMu.Unlock()
		if n == 2 {
			probe.sample()
		}
	}

	p, err := New(Options{HandshakeTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("New(real pi): %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, realSessionSentinelSpec(realBinarySpec(workdir, "run the requested command", stub.baseURL())))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	_ = drainToResult(t, h, 60*time.Second)

	probe.assertNoLeak(t)
	assertToolEnvironment(t, workdir)
	authMu.Lock()
	defer authMu.Unlock()
	if len(auths) < 2 {
		t.Fatalf("model stub saw %d requests, want the tool turn and its follow-up", len(auths))
	}
	for _, auth := range auths {
		if auth != "Bearer real-binary-stub-key" {
			t.Errorf("model request authorization = %q, want the binding's key from the credential file", auth)
		}
	}
}

// TestRealBinary_SessionEnvironment_InteractiveNeverInListing is the PTY
// companion: a REAL interactive pi session, the same sentinel table on
// Spec.Env and in the parent environment, the same bash tool call. The PTY
// host spawns pi with the lane's complete allowlisted environment and
// inherits nothing from the parent.
//
// RED proof: spawn without ExactEnv (spawnInteractive) and the parent's
// sentinels ride back into the live process's exec-time block.
func TestRealBinary_SessionEnvironment_InteractiveNeverInListing(t *testing.T) {
	realBinaryAvailable(t)
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	// Not parallel: writes the sentinel table into the process environment.
	setParentSentinels(t)
	workdir := t.TempDir()
	probe := &listingProbe{stateRoot: newSessionLayout(workdir).root}
	stub := newRealBinaryStub(t, realBinaryModel)
	var authMu sync.Mutex
	var auths []string
	sampled := make(chan struct{})
	var sampleOnce sync.Once
	stub.responses = []stubResponse{
		{ToolCall: &stubToolCall{ID: "session-env-call", Name: "bash", Arguments: `{"command":"env > tool-env.txt"}`}},
		{Text: "session-env-done"},
	}
	stub.beforeResponse = func(r *http.Request, n int) {
		authMu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		authMu.Unlock()
		if n == 2 {
			probe.sample()
			sampleOnce.Do(func() { close(sampled) })
		}
	}

	spec := realSessionSentinelSpec(realBinarySpec(workdir, "run the requested command", stub.baseURL()))
	spec.Interactive = &agent.InteractiveSpec{Cols: 100, Rows: 30}
	p, err := New(Options{})
	if err != nil {
		t.Fatalf("New(real pi): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	select {
	case <-sampled:
	case <-time.After(60 * time.Second):
		t.Fatalf("real interactive pi never reached the follow-up model turn; requests=%d", len(stub.recordedTurns()))
	}

	probe.assertNoLeak(t)
	assertToolEnvironment(t, workdir)
	authMu.Lock()
	defer authMu.Unlock()
	for _, auth := range auths {
		if auth != "Bearer real-binary-stub-key" {
			t.Errorf("model request authorization = %q, want the binding's key from the credential file", auth)
		}
	}
}

// TestRealBinary_CredentialFamiliesAuthenticateThroughFile drives one REAL
// headless session per credential family the injected provider serves. In
// every family the credential reaches pi only through the session
// credential file, and the stub must see it as the request's bearer — while
// the live pi process's exec-time block carries none of it.
func TestRealBinary_CredentialFamiliesAuthenticateThroughFile(t *testing.T) {
	realBinaryAvailable(t)
	stub := newRealBinaryStub(t, realBinaryModel)
	for _, family := range []struct {
		name       string
		endpoint   func(baseURL string) *agent.EndpointBinding
		wantBearer string
	}{
		{
			name: "BYOK key on a direct cell",
			endpoint: func(baseURL string) *agent.EndpointBinding {
				return &agent.EndpointBinding{
					Company:  agent.CompanyStub,
					Host:     agent.HostDirect,
					Protocol: agent.ProtoOpenAIChat,
					Model:    realBinaryModel,
					BaseURL:  baseURL,
					Env:      map[string]string{"OPENAI_API_KEY": "sentinel-family-byok-key"},
				}
			},
			wantBearer: "sentinel-family-byok-key",
		},
		{
			name: "per-session gateway bearer",
			endpoint: func(baseURL string) *agent.EndpointBinding {
				return &agent.EndpointBinding{
					Company:  agent.CompanyOpenAI,
					Host:     agent.HostGateway,
					Protocol: agent.ProtoOpenAIChat,
					Model:    realBinaryModel,
					BaseURL:  baseURL,
					Env:      map[string]string{gatewayBearerEnvVar: "sentinel-family-gateway-bearer"},
				}
			},
			wantBearer: "sentinel-family-gateway-bearer",
		},
		{
			name: "gateway key on an aggregator cell",
			endpoint: func(baseURL string) *agent.EndpointBinding {
				return &agent.EndpointBinding{
					Company:  agent.CompanyOpenAI,
					Host:     agent.HostDirect,
					Protocol: agent.ProtoOpenAIChat,
					Model:    realBinaryModel,
					BaseURL:  baseURL,
					Env:      map[string]string{"AI_GATEWAY_API_KEY": "sentinel-family-gateway-key"},
				}
			},
			wantBearer: "sentinel-family-gateway-key",
		},
		{
			name: "bedrock-style cell credentials",
			endpoint: func(baseURL string) *agent.EndpointBinding {
				return &agent.EndpointBinding{
					Company:  agent.CompanyAnthropic,
					Host:     agent.HostBedrock,
					Protocol: agent.ProtoOpenAIChat,
					Model:    realBinaryModel,
					BaseURL:  baseURL,
					Env:      map[string]string{"AWS_ACCESS_KEY_ID": "sentinel-family-aws-access-key", "AWS_SECRET_ACCESS_KEY": "sentinel-family-aws-secret", "AWS_REGION": "sentinel-family-aws-region"},
				}
			},
			wantBearer: "sentinel-family-aws-access-key",
		},
		{
			name: "vertex-style cell credentials",
			endpoint: func(baseURL string) *agent.EndpointBinding {
				return &agent.EndpointBinding{
					Company:  agent.CompanyGoogle,
					Host:     agent.HostVertex,
					Protocol: agent.ProtoOpenAIChat,
					Model:    realBinaryModel,
					BaseURL:  baseURL,
					Env:      map[string]string{"GOOGLE_API_KEY": "sentinel-family-vertex-key", "GOOGLE_VERTEX_PROJECT_ID": "sentinel-family-vertex-project"},
				}
			},
			wantBearer: "sentinel-family-vertex-key",
		},
	} {
		t.Run(family.name, func(t *testing.T) {
			workdir := t.TempDir()
			probe := &listingProbe{stateRoot: newSessionLayout(workdir).root}
			var authMu sync.Mutex
			var auths []string
			stub.mu.Lock()
			stub.turns = nil
			stub.responses = nil
			stub.beforeResponse = func(r *http.Request, n int) {
				authMu.Lock()
				auths = append(auths, r.Header.Get("Authorization"))
				authMu.Unlock()
				if n == 1 {
					probe.sample()
				}
			}
			stub.mu.Unlock()

			spec := agent.Spec{Prompt: "authenticate", Cwd: workdir, Endpoint: family.endpoint(stub.baseURL())}
			p, err := New(Options{HandshakeTimeout: 30 * time.Second})
			if err != nil {
				t.Fatalf("New(real pi): %v", err)
			}
			t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h, err := p.Spawn(ctx, spec)
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			t.Cleanup(func() { _ = h.Stop(context.Background()) })
			_ = drainToResult(t, h, 45*time.Second)

			probe.assertNoLeak(t)
			authMu.Lock()
			defer authMu.Unlock()
			if len(auths) == 0 {
				t.Fatal("the session never reached the model endpoint")
			}
			for _, auth := range auths {
				if auth != "Bearer "+family.wantBearer {
					t.Errorf("model request authorization = %q, want the %s from the credential file", auth, family.name)
				}
			}
		})
	}
}

// TestRealBinary_NativeRouteResolvesKeyFromSessionStoreWithEnvAbsent proves
// the BYOK native route against the REAL binary: with the provider's own
// key variable ABSENT from the exec environment the spawn composes, pi
// still resolves the session's key — from the session auth.json the
// harness writes — and selects the pinned provider. The negative control
// removes the session store: pi then resolves no key at all, so neither the
// spawn environment nor the parent's own copy of the variable is doing it.
func TestRealBinary_NativeRouteResolvesKeyFromSessionStoreWithEnvAbsent(t *testing.T) {
	realBinaryAvailable(t)
	// Not parallel: sets a parent copy of the provider key.
	t.Setenv("ANTHROPIC_API_KEY", "sentinel-parent-native-key")
	workdir := t.TempDir()
	layout := newSessionLayout(workdir)
	if err := os.MkdirAll(layout.agentHome, 0o700); err != nil {
		t.Fatal(err)
	}
	projected, err := applyEndpoint(agent.Spec{
		Cwd: workdir,
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyAnthropic, Host: agent.HostDirect, Protocol: agent.ProtoAnthropicMessages,
			Model: "anthropic/claude-sonnet-4-5",
			Env:   map[string]string{"ANTHROPIC_API_KEY": "sentinel-session-native-key"},
		},
	})
	if err != nil {
		t.Fatalf("applyEndpoint: %v", err)
	}
	if pin := modelPinArgs(projected); len(pin) < 2 || pin[0] != "--provider" || pin[1] != "anthropic" {
		t.Fatalf("native cell pin = %q, want the anthropic provider selected by argv", pin)
	}
	env := composeChildEnv(projected, layout, "")
	if hasEnvKey(env, "ANTHROPIC_API_KEY") {
		t.Fatal("the spawn environment carries the provider key variable")
	}
	if _, err := writeNativeProviderAuthFile(layout, projected); err != nil {
		t.Fatalf("writeNativeProviderAuthFile: %v", err)
	}

	listAnthropic := func() string {
		cmd := exec.Command("pi", "--list-models", "anthropic", "--offline") //nolint:gosec // fixed test-only command
		cmd.Dir = workdir
		cmd.Env = env
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	hasAnthropicRow := func(out string) bool {
		for _, line := range strings.Split(out, "\n") {
			if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "anthropic" {
				return true
			}
		}
		return false
	}
	if out := listAnthropic(); !hasAnthropicRow(out) {
		t.Fatalf("pi resolved no anthropic key from the session store with the env var absent:\n%s", out)
	}
	removeNativeProviderAuthFile(layout)
	if out := listAnthropic(); hasAnthropicRow(out) {
		t.Fatalf("pi resolved an anthropic key with no session store; an ambient copy reached it:\n%s", out)
	}
}
