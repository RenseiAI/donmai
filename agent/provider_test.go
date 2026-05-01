package agent

import (
	"context"
	"errors"
	"testing"
)

// noopProvider is a compile-time check that the Provider interface is
// implementable from outside the package (mirrors how provider/stub,
// provider/claude, provider/codex will satisfy it).
type noopProvider struct {
	name ProviderName
	caps Capabilities
}

func (p *noopProvider) Name() ProviderName         { return p.name }
func (p *noopProvider) Capabilities() Capabilities { return p.caps }
func (p *noopProvider) Spawn(_ context.Context, _ Spec) (Handle, error) {
	return nil, ErrSpawnFailed
}

func (p *noopProvider) Resume(_ context.Context, _ string, _ Spec) (Handle, error) {
	return nil, ErrUnsupported
}
func (p *noopProvider) Shutdown(_ context.Context) error { return nil }

// noopHandle is a compile-time check that the Handle interface is
// implementable.
type noopHandle struct {
	id string
	ch chan Event
}

func (h *noopHandle) SessionID() string                        { return h.id }
func (h *noopHandle) Events() <-chan Event                     { return h.ch }
func (h *noopHandle) Inject(_ context.Context, _ string) error { return ErrUnsupported }
func (h *noopHandle) Stop(_ context.Context) error             { return nil }

func TestProvider_InterfaceImplementable(t *testing.T) {
	t.Parallel()
	var _ Provider = (*noopProvider)(nil)
	var _ Handle = (*noopHandle)(nil)

	p := &noopProvider{name: ProviderStub, caps: Capabilities{SupportsSessionResume: true}}
	if p.Name() != ProviderStub {
		t.Fatalf("Name() = %q, want %q", p.Name(), ProviderStub)
	}
	if !p.Capabilities().SupportsSessionResume {
		t.Fatal("Capabilities().SupportsSessionResume = false")
	}

	if _, err := p.Spawn(context.Background(), Spec{}); !errors.Is(err, ErrSpawnFailed) {
		t.Fatalf("Spawn err = %v, want ErrSpawnFailed", err)
	}
	if _, err := p.Resume(context.Background(), "x", Spec{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Resume err = %v, want ErrUnsupported", err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown err = %v", err)
	}
}

// TestSentinelErrors_AreDistinct guards against accidental aliasing.
func TestSentinelErrors_AreDistinct(t *testing.T) {
	t.Parallel()
	all := []error{
		ErrUnsupported,
		ErrNoProvider,
		ErrSessionNotFound,
		ErrSpawnFailed,
		ErrProviderUnavailable,
	}
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("ErrIs collision: %v Is %v", a, b)
			}
		}
	}
}
