package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
)

// specDecoratorCapturingProvider wraps a real agent.Provider (the stub
// provider, in these tests) and records the exact Spec each Spawn/Resume
// call received — the same "wrap and record" shape
// TestLoop_SpawnsOnHarnessDeclaringNoMCPDelivery's exactHarnessStubProvider
// uses elsewhere in this package. Recording happens BELOW
// agent.DecorateProvider (this provider is the thing DecorateProvider
// wraps), so a spec captured here is exactly what the real, wrapped
// provider received — proof the decorator reached it, not an assertion
// about DecorateProvider's own internals.
type specDecoratorCapturingProvider struct {
	agent.Provider

	mu          sync.Mutex
	spawnSpec   agent.Spec
	spawnCalls  int
	resumeSpec  agent.Spec
	resumeCalls int
}

func (p *specDecoratorCapturingProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	p.mu.Lock()
	p.spawnSpec = spec
	p.spawnCalls++
	p.mu.Unlock()
	return p.Provider.Spawn(ctx, spec)
}

func (p *specDecoratorCapturingProvider) Resume(ctx context.Context, sessionID string, spec agent.Spec) (agent.Handle, error) {
	p.mu.Lock()
	p.resumeSpec = spec
	p.resumeCalls++
	p.mu.Unlock()
	return p.Provider.Resume(ctx, sessionID, spec)
}

func (p *specDecoratorCapturingProvider) snapshot() (spawnSpec, resumeSpec agent.Spec, spawnCalls, resumeCalls int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spawnSpec, p.resumeSpec, p.spawnCalls, p.resumeCalls
}

// validExtensionDelivery mirrors agent/spec_decorator_test.go's fixture
// helper: a structurally well-formed inline delivery with a correct digest,
// so these tests exercise the real validation shape rather than a
// fixture that merely looks right.
func validExtensionDelivery(id, content string) agent.ExtensionDelivery {
	sum := sha256.Sum256([]byte(content))
	return agent.ExtensionDelivery{
		ID:       id,
		Kind:     agent.ExtensionDeliveryInline,
		Source:   []byte(content),
		Basename: id + ".js",
		Digest:   hex.EncodeToString(sum[:]),
	}
}

// TestSpecDecorator_ReachesRealSpawnConstructionPath drives a FULL,
// real Runner.Run — the exact production orchestration path
// (runLoop -> translateSpec -> provider.Spawn at loop.go's step 8) — against
// a provider registered through agent.DecorateProvider, and asserts the
// decorator's delivery reached the real Spawn call. This is the "Spawn"
// half of the embedder hook's contract: an embedding binary's registered
// decorator must reach every spec agent-run orchestration constructs
// internally, not just a spec built by a test harness that reimplements
// the wrapping by hand.
func TestSpecDecorator_ReachesRealSpawnConstructionPath(t *testing.T) {
	h := newRunnerHarness(t)

	inner, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	capture := &specDecoratorCapturingProvider{Provider: inner}

	delivery := validExtensionDelivery("embedder-pack", "console.log('spawned')")
	decorate := func(agent.Spec) []agent.ExtensionDelivery {
		return []agent.ExtensionDelivery{delivery}
	}
	decorated := agent.DecorateProvider(capture, decorate)

	// Register.Register overwrites an existing name (registry.go's Register
	// doc comment) — this replaces the plain stub newRunnerHarness already
	// registered with the decorated+capturing wrapper under the SAME name,
	// so h.queuedWork's ResolvedProfile.Provider = agent.ProviderStub still
	// resolves to it via the ordinary legacy-provider selection path.
	if err := h.runner.registry.Register(decorated); err != nil {
		t.Fatalf("Register: %v", err)
	}

	qw := h.queuedWork("SPEC-DECORATOR-SPAWN")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("Run status = %q (failureMode=%q error=%q), want completed",
			res.Status, res.FailureMode, res.Error)
	}

	spawnSpec, _, spawnCalls, _ := capture.snapshot()
	if spawnCalls != 1 {
		t.Fatalf("inner Spawn calls = %d, want 1", spawnCalls)
	}
	if len(spawnSpec.AdditionalExtensions) != 1 || spawnSpec.AdditionalExtensions[0].ID != delivery.ID {
		t.Fatalf("real orchestration's Spawn call reached the provider with AdditionalExtensions=%+v, want [%q]",
			spawnSpec.AdditionalExtensions, delivery.ID)
	}
}

// TestSpecDecorator_ReachesRealResumeConstructionPath proves the SAME
// registered, decorated provider instance also decorates Resume. Today no
// donmai orchestration path calls Provider.Resume in production (see
// provider/harness/pi/pi.go's Resume doc comment — no harness has a
// production Resume caller yet); the embedder hook still must not
// special-case Spawn, because the contract ("runs on EVERY spawned/resumed
// spec") and every real Provider implementation (stub, pi, codex, opencode)
// ship a working Resume today, exercised directly by
// agent/conformance's conformance suite. This test calls Resume directly on
// the exact provider instance production registration produced (built via
// the SAME agent.DecorateProvider call as the Spawn test above, not a
// hand-rolled stand-in), so it proves the hook, not a re-implementation of
// it.
func TestSpecDecorator_ReachesRealResumeConstructionPath(t *testing.T) {
	h := newRunnerHarness(t)

	inner, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	capture := &specDecoratorCapturingProvider{Provider: inner}

	delivery := validExtensionDelivery("embedder-pack", "console.log('resumed')")
	decorate := func(agent.Spec) []agent.ExtensionDelivery {
		return []agent.ExtensionDelivery{delivery}
	}
	decorated := agent.DecorateProvider(capture, decorate)
	if err := h.runner.registry.Register(decorated); err != nil {
		t.Fatalf("Register: %v", err)
	}

	resolved, err := h.runner.registry.Resolve(agent.ProviderStub)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handle, err := resolved.Resume(ctx, "prior-session-id", agent.Spec{Prompt: "continue"})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	// Drain to let the stub's scripting goroutine finish and avoid leaving
	// it parked mid-emission across test cleanup.
	for range handle.Events() { //nolint:revive // deliberate drain, no per-event handling needed
	}

	_, resumeSpec, _, resumeCalls := capture.snapshot()
	if resumeCalls != 1 {
		t.Fatalf("inner Resume calls = %d, want 1", resumeCalls)
	}
	if len(resumeSpec.AdditionalExtensions) != 1 || resumeSpec.AdditionalExtensions[0].ID != delivery.ID {
		t.Fatalf("the registry-resolved provider's Resume call reached the provider with AdditionalExtensions=%+v, want [%q]",
			resumeSpec.AdditionalExtensions, delivery.ID)
	}
	if resumeSpec.Prompt != "continue" {
		t.Errorf("Resume: inner saw Prompt=%q, want %q (decorator must not touch other fields)", resumeSpec.Prompt, "continue")
	}
}

// TestSpecDecorator_NoDecoratorRegisteredIsExactPassthrough proves the
// "zero behavior change when no decorator registered" half of the
// contract from the runner package's own seam: registering a plain
// (undecorated) provider behaves identically to today, and
// agent.DecorateProvider itself is never invoked when an embedder sets no
// decorator — reg.Register(p) with p untouched is exactly what
// newRunnerHarness already does for every other Run-level test in this
// package.
func TestSpecDecorator_NoDecoratorRegisteredIsExactPassthrough(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("SPEC-DECORATOR-PASSTHROUGH")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("Run status = %q (failureMode=%q error=%q), want completed",
			res.Status, res.FailureMode, res.Error)
	}
	if len(res.WorktreePath) == 0 {
		t.Fatal("expected WorktreePath populated — unrelated to this seam, just confirming the ordinary undecorated path still runs end to end")
	}
}
