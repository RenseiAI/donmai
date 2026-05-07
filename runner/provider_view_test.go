package runner_test

import (
	"context"
	"sort"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/runner"
)

// fakeProvider is a minimal agent.Provider implementation for testing
// runner.ProviderView. Mirrors the stub provider's surface but without
// pulling the real provider/stub package into the runner test deps.
type fakeProvider struct {
	name agent.ProviderName
	caps agent.Capabilities
}

func (f *fakeProvider) Name() agent.ProviderName         { return f.name }
func (f *fakeProvider) Capabilities() agent.Capabilities { return f.caps }
func (f *fakeProvider) Spawn(_ context.Context, _ agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}

func (f *fakeProvider) Resume(_ context.Context, _ string, _ agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}
func (f *fakeProvider) Shutdown(_ context.Context) error { return nil }

func TestProviderView_Names_EmptyRegistry(t *testing.T) {
	view := runner.NewProviderView(runner.NewRegistry())
	if got := view.Names(); len(got) != 0 {
		t.Errorf("empty registry Names() = %v, want []", got)
	}
}

func TestProviderView_Names_SortedAndStringTyped(t *testing.T) {
	reg := runner.NewRegistry()
	for _, name := range []agent.ProviderName{"codex", "claude", "stub"} {
		if err := reg.Register(&fakeProvider{name: name, caps: agent.Capabilities{}}); err != nil {
			t.Fatalf("Register %q: %v", name, err)
		}
	}
	view := runner.NewProviderView(reg)
	got := view.Names()
	want := []string{"claude", "codex", "stub"}
	if !sort.StringsAreSorted(got) {
		t.Errorf("Names() = %v, expected sorted output", got)
	}
	if len(got) != len(want) {
		t.Fatalf("Names() len = %d, want %d", len(got), len(want))
	}
	for i, n := range got {
		if n != want[i] {
			t.Errorf("Names()[%d] = %q, want %q", i, n, want[i])
		}
	}
}

func TestProviderView_Capabilities_KnownProvider(t *testing.T) {
	reg := runner.NewRegistry()
	caps := agent.Capabilities{
		SupportsMessageInjection: false,
		SupportsSessionResume:    true,
		SupportsToolPlugins:      true,
		HumanLabel:               "Claude",
	}
	if err := reg.Register(&fakeProvider{name: agent.ProviderClaude, caps: caps}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	view := runner.NewProviderView(reg)
	got, ok := view.Capabilities("claude")
	if !ok {
		t.Fatal("Capabilities(claude) ok=false, want true")
	}
	// Map keys must match the JSON tags from agent.Capabilities.
	if got["supportsMessageInjection"] != false {
		t.Errorf("supportsMessageInjection = %v, want false", got["supportsMessageInjection"])
	}
	if got["supportsSessionResume"] != true {
		t.Errorf("supportsSessionResume = %v, want true", got["supportsSessionResume"])
	}
	if got["supportsToolPlugins"] != true {
		t.Errorf("supportsToolPlugins = %v, want true", got["supportsToolPlugins"])
	}
	if got["humanLabel"] != "Claude" {
		t.Errorf("humanLabel = %v, want Claude", got["humanLabel"])
	}
}

func TestProviderView_Capabilities_UnknownProvider(t *testing.T) {
	view := runner.NewProviderView(runner.NewRegistry())
	got, ok := view.Capabilities("nope")
	if ok {
		t.Errorf("Capabilities(nope) ok=true, want false")
	}
	if got != nil {
		t.Errorf("Capabilities(nope) caps = %v, want nil", got)
	}
}

func TestProviderView_NilSafe(t *testing.T) {
	var view *runner.ProviderView
	if got := view.Names(); got != nil {
		t.Errorf("nil view Names() = %v, want nil", got)
	}
	if _, ok := view.Capabilities("anything"); ok {
		t.Errorf("nil view Capabilities() ok=true, want false")
	}
}
