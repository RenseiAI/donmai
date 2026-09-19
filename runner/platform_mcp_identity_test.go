package runner

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/statehome"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

func TestPlatformMCPServerIdentityConstructorsCaptureExplicitAndDefault(t *testing.T) {
	statehome.ResetForTest()
	t.Cleanup(statehome.ResetForTest)
	base := t.TempDir()
	statehome.SetBaseHome(base)
	statehome.SetBrand("example-local-a")

	newRunner := func(name string) *Runner {
		t.Helper()
		r, err := New(Options{
			Registry: runnerRegistryForPlatformMCPIdentityTest(t), WorktreeManager: &worktree.Manager{}, Poster: &result.Poster{},
			PlatformMCPServerName: name,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return r
	}

	explicitRunner := newRunner("example-platform")
	explicitView, err := NewProviderViewWithOptions(runnerRegistryForPlatformMCPIdentityTest(t), ProviderViewOptions{PlatformMCPServerName: "example-platform"})
	if err != nil {
		t.Fatalf("NewProviderViewWithOptions: %v", err)
	}
	defaultRunner := newRunner("")
	defaultView, err := NewProviderViewWithOptions(runnerRegistryForPlatformMCPIdentityTest(t), ProviderViewOptions{})
	if err != nil {
		t.Fatalf("NewProviderViewWithOptions default: %v", err)
	}
	pathA := statehome.StateDir("sessions")

	statehome.SetBrand("example-local-b")
	pathB := statehome.StateDir("sessions")
	if pathA == pathB || pathA != filepath.Join(base, ".example-local-a", "sessions") || pathB != filepath.Join(base, ".example-local-b", "sessions") {
		t.Fatalf("state namespaces = %q and %q", pathA, pathB)
	}
	if explicitRunner.platformMCPServerName != "example-platform" || explicitView.platformMCPServerName != "example-platform" {
		t.Fatalf("explicit identities changed: runner=%q view=%q", explicitRunner.platformMCPServerName, explicitView.platformMCPServerName)
	}
	if defaultRunner.platformMCPServerName != "example-local-a-platform" || defaultView.platformMCPServerName != "example-local-a-platform" {
		t.Fatalf("captured defaults changed: runner=%q view=%q", defaultRunner.platformMCPServerName, defaultView.platformMCPServerName)
	}
}

func TestPlatformMCPServerIdentityRejectsMalformedExplicitNames(t *testing.T) {
	for _, name := range []string{" example-platform", "example-platform ", "example platform", "example#platform"} {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			if _, err := New(Options{Registry: NewRegistry(), WorktreeManager: &worktree.Manager{}, Poster: &result.Poster{}, PlatformMCPServerName: name}); err == nil {
				t.Fatalf("Runner accepted malformed explicit name %q", name)
			}
			if _, err := NewProviderViewWithOptions(NewRegistry(), ProviderViewOptions{PlatformMCPServerName: name}); err == nil {
				t.Fatalf("ProviderView accepted malformed explicit name %q", name)
			}
		})
	}
}

func TestPreparedSourceUsesConstructedPlatformMCPServerIdentity(t *testing.T) {
	const serverName = "example-platform"
	provider := mcpDeliveringHarness()
	selection := harnessSelection{Provider: provider}
	qw := platformGatewayWork("example-session")
	qw.Body = "exercise the prepared source"

	r, err := New(Options{
		Registry: runnerRegistryForPlatformMCPIdentityTest(t), WorktreeManager: &worktree.Manager{}, Poster: &result.Poster{},
		PlatformMCPServerName: serverName,
	})
	if err != nil {
		t.Fatal(err)
	}
	spec, runtimeNames, err := buildPreparedSourceSpecWithPlatformMCPServerName(qw, selection, nil, r.platformMCPServerName)
	if err != nil {
		t.Fatalf("buildPreparedSourceSpecWithPlatformMCPServerName: %v", err)
	}
	if !slices.Equal(runtimeNames, []string{serverName}) {
		t.Fatalf("runtime names = %v, want [%s]", runtimeNames, serverName)
	}
	if len(spec.MCPServers) != 1 || spec.MCPServers[0].Name != serverName {
		t.Fatalf("prepared MCP servers = %+v, want one %q", spec.MCPServers, serverName)
	}
	entryID, err := agent.MCPServerCapabilityEntryID(spec.MCPServers[0].Name)
	if err != nil || entryID != agent.MCPServerCapabilityEntryPrefix+serverName {
		t.Fatalf("prepared server name is not canonical: entry=%q err=%v", entryID, err)
	}
}

func runnerRegistryForPlatformMCPIdentityTest(t *testing.T) *Registry {
	t.Helper()
	registry := NewRegistry()
	if err := registry.Register(mcpDeliveringHarness()); err != nil {
		t.Fatal(err)
	}
	return registry
}
