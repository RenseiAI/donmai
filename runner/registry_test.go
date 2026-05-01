package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/provider/stub"
)

// TestRegistry_RegisterResolve covers the happy path and the
// unknown-provider path against a stub provider instance.
func TestRegistry_RegisterResolve(t *testing.T) {
	r := NewRegistry()
	p, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	if err := r.Register(p); err != nil {
		t.Fatalf("Register stub: %v", err)
	}

	got, err := r.Resolve(agent.ProviderStub)
	if err != nil {
		t.Fatalf("Resolve stub: %v", err)
	}
	if got.Name() != agent.ProviderStub {
		t.Fatalf("resolved provider name = %q; want %q", got.Name(), agent.ProviderStub)
	}

	if _, err := r.Resolve(agent.ProviderClaude); !errors.Is(err, agent.ErrNoProvider) {
		t.Fatalf("Resolve claude on empty registry: err = %v; want ErrNoProvider", err)
	}
}

// TestRegistry_RegisterRejectsNil ensures a programmer error fails fast.
func TestRegistry_RegisterRejectsNil(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Fatal("Register(nil) returned nil err; want error")
	}
}

// TestRegistry_Names returns providers sorted lexicographically.
func TestRegistry_Names(t *testing.T) {
	r := NewRegistry()
	pStub, _ := stub.New()
	if err := r.Register(pStub); err != nil {
		t.Fatalf("register stub: %v", err)
	}
	got := r.Names()
	if len(got) != 1 || got[0] != agent.ProviderStub {
		t.Fatalf("Names() = %v; want [stub]", got)
	}
}

// TestRegistry_Shutdown calls Shutdown on every registered provider
// and joins errors. The stub provider's Shutdown is a no-op so the
// happy path returns nil.
func TestRegistry_Shutdown(t *testing.T) {
	r := NewRegistry()
	p, _ := stub.New()
	_ = r.Register(p)
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
