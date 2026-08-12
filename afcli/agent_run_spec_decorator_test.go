package afcli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/runner"
)

// specDecoratorTestProvider is a minimal agent.Provider double recording the
// Spec each Spawn/Resume call receives — used to assert on exactly what
// decorateRegistryProviders' wrapping delivers, the same "record what the
// wrapped provider actually saw" shape agent/spec_decorator_test.go and
// runner/spec_decorator_test.go use in their own packages.
type specDecoratorTestProvider struct {
	name       agent.ProviderName
	spawnSpec  agent.Spec
	resumeSpec agent.Spec
}

func (p *specDecoratorTestProvider) Name() agent.ProviderName { return p.name }
func (p *specDecoratorTestProvider) Capabilities() agent.Capabilities {
	return agent.Capabilities{SupportsSessionResume: true}
}

func (p *specDecoratorTestProvider) Spawn(_ context.Context, spec agent.Spec) (agent.Handle, error) {
	p.spawnSpec = spec
	return &specDecoratorTestHandle{}, nil
}

func (p *specDecoratorTestProvider) Resume(_ context.Context, _ string, spec agent.Spec) (agent.Handle, error) {
	p.resumeSpec = spec
	return &specDecoratorTestHandle{}, nil
}

func (p *specDecoratorTestProvider) Shutdown(context.Context) error { return nil }

// specDecoratorTestHandle is a closed-events no-op Handle: these tests only
// care about the Spec each Spawn/Resume call captured, never about draining
// a live session.
type specDecoratorTestHandle struct{}

func (h *specDecoratorTestHandle) SessionID() string { return "spec-decorator-test-session" }
func (h *specDecoratorTestHandle) Events() <-chan agent.Event {
	ch := make(chan agent.Event)
	close(ch)
	return ch
}
func (h *specDecoratorTestHandle) Inject(context.Context, string) error { return nil }
func (h *specDecoratorTestHandle) Stop(context.Context) error           { return nil }

// validSpecDecoratorTestDelivery returns a structurally well-formed inline
// ExtensionDelivery (correct digest included) so these tests exercise the
// real delivery shape rather than a fixture that only looks right.
func validSpecDecoratorTestDelivery(id, content string) agent.ExtensionDelivery {
	sum := sha256.Sum256([]byte(content))
	return agent.ExtensionDelivery{
		ID:       id,
		Kind:     agent.ExtensionDeliveryInline,
		Source:   []byte(content),
		Basename: id + ".js",
		Digest:   hex.EncodeToString(sum[:]),
	}
}

// TestDecorateRegistryProviders_WrapsEveryRegisteredProvider unit-tests
// decorateRegistryProviders directly — the exact helper runAgentRun calls,
// gated on opts.specDecorator != nil, immediately after
// buildRegistryFromCtors. Proves both halves of the contract at once: the
// registry resolves the WRAPPED instance afterwards (Register's documented
// overwrite-by-name behavior), and that wrapped instance's Spawn AND Resume
// both carry the decorator's appended delivery through to the real
// provider.
func TestDecorateRegistryProviders_WrapsEveryRegisteredProvider(t *testing.T) {
	reg := runner.NewRegistry()
	original := &specDecoratorTestProvider{name: agent.ProviderStub}
	if err := reg.Register(original); err != nil {
		t.Fatalf("Register: %v", err)
	}

	delivery := validSpecDecoratorTestDelivery("embedder-pack", "console.log('afcli')")
	decorateRegistryProviders(reg, func(agent.Spec) []agent.ExtensionDelivery {
		return []agent.ExtensionDelivery{delivery}
	})

	resolved, err := reg.Resolve(agent.ProviderStub)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved == agent.Provider(original) {
		t.Fatal("decorateRegistryProviders left the registry resolving the undecorated provider instance")
	}

	if _, err := resolved.Spawn(context.Background(), agent.Spec{Prompt: "spawn"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if _, err := resolved.Resume(context.Background(), "sid", agent.Spec{Prompt: "resume"}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if len(original.spawnSpec.AdditionalExtensions) != 1 || original.spawnSpec.AdditionalExtensions[0].ID != delivery.ID {
		t.Errorf("Spawn: wrapped provider's inner saw AdditionalExtensions=%+v, want [%q]",
			original.spawnSpec.AdditionalExtensions, delivery.ID)
	}
	if len(original.resumeSpec.AdditionalExtensions) != 1 || original.resumeSpec.AdditionalExtensions[0].ID != delivery.ID {
		t.Errorf("Resume: wrapped provider's inner saw AdditionalExtensions=%+v, want [%q]",
			original.resumeSpec.AdditionalExtensions, delivery.ID)
	}
}

// TestDecorateRegistryProviders_EmptyRegistryIsNoOp proves the helper never
// panics or fabricates entries against an empty registry — the shape a
// worst-case host (every provider probe failed) presents.
func TestDecorateRegistryProviders_EmptyRegistryIsNoOp(t *testing.T) {
	reg := runner.NewRegistry()
	decorateRegistryProviders(reg, func(agent.Spec) []agent.ExtensionDelivery {
		t.Fatal("decorate must never be called: no provider was registered to Spawn/Resume against")
		return nil
	})
	if len(reg.Names()) != 0 {
		t.Fatalf("Names() = %v, want empty", reg.Names())
	}
}

// makeSpecDecoratorBareRepo creates a tiny bare git repo on disk so a real
// `agent run` invocation can clone it and reach the provider's real Spawn —
// unlike this package's other agent_run tests (which use the placeholder
// "github.com/foo/bar" and tolerate worktree provisioning failing before
// Spawn is ever reached), this test's whole point is proving the decorator
// reaches a REAL Spawn call end-to-end through Config ->
// newAgentRunCmd -> runAgentRun, so it needs a repository that actually
// clones.
func makeSpecDecoratorBareRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	work := t.TempDir()
	run := func(args ...string) {
		//nolint:gosec // G204: test fixture, args are hard-coded literals.
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# test repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "initial")

	bareParent := t.TempDir()
	bare := filepath.Join(bareParent, "repo.git")
	//nolint:gosec // G204: test fixture, args are hard-coded literals.
	cmd := exec.Command("git", "clone", "-q", "--bare", work, bare)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare: %v\n%s", err, out)
	}
	return bare
}

// runSpecDecoratorAgentRunCmd wires the same fixture shape as
// TestRunAgentRun_HappyPath_StubProvider (a fake daemon resolving to the
// stub provider) but backed by a REAL bare git repo, so worktree
// provisioning succeeds and the session's Spawn call is genuinely reached —
// the property both tests below need, in opposite directions (one asserts
// the decorator ran, the other asserts nothing broke when it's absent).
// Returns the executed command's combined stdout/stderr and Execute's
// error.
func runSpecDecoratorAgentRunCmd(t *testing.T, cfg Config) (stdout string, err error) {
	t.Helper()
	if codexOnPath() && raceEnabled() {
		t.Skip("skipping under -race because codex is on PATH and codex.New/Shutdown have a known race (ENG-1460); rerun without -race or after ENG-1460 lands")
	}
	repo := makeSpecDecoratorBareRepo(t)

	// Every platform endpoint (status, completion, and — the one that
	// matters for a session that runs long enough to tick — lock-refresh)
	// answers 200 with refreshed=true, matching runner package's own
	// mockPlatformServer fixture. Unlike this package's other agent_run
	// tests, THIS session's worktree provisioning genuinely succeeds (a
	// real bare repo, not the placeholder "github.com/foo/bar"), so it
	// lives long enough to reach a real heartbeat tick — a bare `{}` body
	// would read as lock-refresh being refused and fail the session with
	// lost-ownership before it ever reaches a terminal state.
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refreshed":true,"ok":true}`))
	}))
	defer platform.Close()

	detail := &daemon.SessionDetail{
		SessionID:       "sess-decorator-1",
		IssueIdentifier: "ENG-9001",
		Repository:      repo,
		WorkType:        "development",
		Body:            "Spec-decorator end-to-end test issue body.",
		WorkerID:        "wkr_test",
		AuthToken:       "tok_test",
		PlatformURL:     platform.URL,
		ResolvedProfile: &daemon.SessionResolvedProfile{
			Provider: string(agent.ProviderStub),
		},
	}
	daemonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(detail) // nolint:gosec // G117: test fixture
	}))
	defer daemonSrv.Close()

	wtDir := filepath.Join(t.TempDir(), "wt")
	if mkErr := os.MkdirAll(wtDir, 0o750); mkErr != nil {
		t.Fatal(mkErr)
	}

	cmd := newAgentRunCmd(cfg)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"--session-id=" + detail.SessionID,
		"--daemon-url=" + daemonSrv.URL,
		"--worktree-dir=" + wtDir,
		"--json=true",
	})

	err = cmd.Execute()
	return out.String(), err
}

// TestRunAgentRun_ConfigSpecDecoratorReachesRealSpawn drives the FULL
// production entry point an embedding binary actually uses —
// Config.AgentSpecExtensionDecorator -> newAgentRunCmd -> runAgentRun ->
// buildRegistryFromCtors -> decorateRegistryProviders -> a real
// provider.Spawn call inside runner.RunAdmitted — with a real bare git repo
// so worktree provisioning succeeds and Spawn is genuinely reached (not
// merely constructed). This is the end-to-end proof that the gap the
// embedder hook closes ("an embedding binary registering via
// afcli.RegisterCommands has no sanctioned hook to decorate the specs
// agent-run orchestration constructs internally") is actually closed at the
// surface an embedder uses: setting a field on afcli.Config, nothing more.
func TestRunAgentRun_ConfigSpecDecoratorReachesRealSpawn(t *testing.T) {
	var decoratorCalls atomic.Int32
	delivery := validSpecDecoratorTestDelivery("embedder-pack", "console.log('config-wired')")
	cfg := Config{
		AgentSpecExtensionDecorator: func(agent.Spec) []agent.ExtensionDelivery {
			decoratorCalls.Add(1)
			return []agent.ExtensionDelivery{delivery}
		},
	}

	// Execute's error reflects the SESSION's terminal status (e.g. a real
	// `gh pr create` failing in backstop because this test's throwaway repo
	// has no GitHub remote) — orthogonal to what this test asserts. Spawn
	// (and therefore the decorator) runs at step 8, long before backstop's
	// PR step, so decoratorCalls is the right signal, matching the existing
	// TestRunAgentRun_HappyPath_StubProvider's tolerance of either outcome.
	stdout, err := runSpecDecoratorAgentRunCmd(t, cfg)
	_ = err

	if got := decoratorCalls.Load(); got == 0 {
		t.Fatalf("Config.AgentSpecExtensionDecorator was never invoked; the real Spawn call never reached the wrapped provider (stdout=%s)", stdout)
	}
	if !strings.Contains(stdout, "sess-decorator-1") {
		t.Errorf("expected Result JSON to include the session id; got %q", stdout)
	}
}

// TestRunAgentRun_NoConfigSpecDecoratorMatchesHistoricalBehavior proves the
// "zero behavior change when no decorator registered" half of the contract
// at the exact Config surface embedders use: the identical fixture as the
// test above, minus AgentSpecExtensionDecorator, still runs the session to
// completion — decorateRegistryProviders is never reached (runAgentRun's
// `if opts.specDecorator != nil` guard), so registration and dispatch are
// byte-for-byte what they were before this hook existed.
func TestRunAgentRun_NoConfigSpecDecoratorMatchesHistoricalBehavior(t *testing.T) {
	// See the sibling test above for why Execute's error is not asserted on:
	// it reflects backstop's real `gh pr create` against a remote-less test
	// repo, orthogonal to registry/dispatch behavior.
	stdout, err := runSpecDecoratorAgentRunCmd(t, Config{})
	_ = err
	if !strings.Contains(stdout, "sess-decorator-1") {
		t.Errorf("expected Result JSON to include the session id; got %q", stdout)
	}
}
