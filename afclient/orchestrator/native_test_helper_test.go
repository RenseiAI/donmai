package orchestrator

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	providerclaude "github.com/RenseiAI/donmai/provider/harness/claude"
)

// claudeBinaryFactoryForTest builds a ProviderFactory that constructs a
// real claude harness provider pointed at a fake CLI binary. This drives
// the REAL adapter path (claude Spawn → clijsonl JSONL mapping → terminal
// ResultEvent) against a native-protocol fixture — not a fake-provider
// shortcut. LookPath is stubbed so construction never touches host PATH.
func claudeBinaryFactoryForTest(t *testing.T, binary string) ProviderFactory {
	t.Helper()
	return ProviderFactoryFunc(func(_ context.Context, harness string) (agent.Provider, error) {
		if harness != HarnessAuto && harness != HarnessClaude {
			t.Errorf("unexpected harness %q for claude-binary factory", harness)
		}
		return providerclaude.New(providerclaude.Options{
			Binary:   binary,
			LookPath: func(name string) (string, error) { return name, nil },
		})
	})
}

// ProviderFactoryFunc adapts a function to the ProviderFactory interface.
type ProviderFactoryFunc func(ctx context.Context, harness string) (agent.Provider, error)

// NewProvider implements ProviderFactory.
func (f ProviderFactoryFunc) NewProvider(ctx context.Context, harness string) (agent.Provider, error) {
	return f(ctx, harness)
}

// shutdownTrackingFactory wraps a factory and counts Shutdown calls on
// the providers it hands out, proving cancellation releases owned
// provider resources.
type shutdownTrackingFactory struct {
	inner     ProviderFactory
	shutdowns atomic.Int64
}

func (f *shutdownTrackingFactory) NewProvider(ctx context.Context, harness string) (agent.Provider, error) {
	p, err := f.inner.NewProvider(ctx, harness)
	if err != nil {
		return nil, err
	}
	return &shutdownTrackingProvider{Provider: p, counter: &f.shutdowns}, nil
}

type shutdownTrackingProvider struct {
	agent.Provider
	counter *atomic.Int64
}

func (p *shutdownTrackingProvider) Shutdown(ctx context.Context) error {
	p.counter.Add(1)
	return p.Provider.Shutdown(ctx)
}
