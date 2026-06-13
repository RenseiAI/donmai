package afcli

import (
	"reflect"
	"sort"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// canonicalAgentRunProviders is the eight-provider set the agent-run registry
// is the single source for. Kept here as the test's independent oracle so a
// silent drop/rename of any ctor in the production list (agentRunProviderCtors)
// fails loudly. Mirrors matrix's realProviderNames() set.
var canonicalAgentRunProviders = []agent.ProviderName{
	agent.ProviderStub,
	agent.ProviderClaude,
	agent.ProviderCodex,
	agent.ProviderOllama,
	agent.ProviderAmp,
	agent.ProviderGemini,
	agent.ProviderAGYCLI,
	agent.ProviderOpenCode,
}

// TestBuildAgentRunRegistry_DeclaresAllEightProviders is the host-independent
// no-behavior-change proof: the single-source ctor list (agentRunProviderCtors)
// enumerates EXACTLY the eight canonical ProviderNames — no more, no fewer —
// regardless of what is probe-available on this host. This is what lets
// rensei-tui delete its byte-for-byte fork and call BuildAgentRunRegistry:
// every embedder gets the same eight providers.
func TestBuildAgentRunRegistry_DeclaresAllEightProviders(t *testing.T) {
	t.Parallel()

	got := make([]agent.ProviderName, 0, len(agentRunProviderCtors()))
	seen := map[agent.ProviderName]bool{}
	for _, c := range agentRunProviderCtors() {
		pn := agent.ProviderName(c.name)
		if seen[pn] {
			t.Errorf("ctor name %q declared more than once", c.name)
		}
		seen[pn] = true
		got = append(got, pn)
	}

	want := append([]agent.ProviderName(nil), canonicalAgentRunProviders...)
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })

	if !reflect.DeepEqual(got, want) {
		t.Errorf("agent-run ctor providers = %v; want exactly %v", got, want)
	}
}

// TestBuildAgentRunRegistry_EachCtorNamesAProvider asserts each ctor's declared
// name is a real ProviderName const. Pairs with the count check above to prove
// no entry is a typo'd / orphaned name that would silently fail to resolve.
func TestBuildAgentRunRegistry_EachCtorNamesAProvider(t *testing.T) {
	t.Parallel()

	known := map[agent.ProviderName]bool{}
	for _, p := range canonicalAgentRunProviders {
		known[p] = true
	}
	for _, c := range agentRunProviderCtors() {
		if !known[agent.ProviderName(c.name)] {
			t.Errorf("ctor name %q is not one of the eight canonical ProviderNames", c.name)
		}
	}
}

// TestBuildAgentRunRegistry_PublicEqualsInternal proves the exported builder
// and the retained unexported alias produce an identical registry — i.e.
// exporting the builder introduced no behavior change. The registries are
// compared by their resolved provider-name sets (whatever is probe-available
// on this host); both must agree exactly.
func TestBuildAgentRunRegistry_PublicEqualsInternal(t *testing.T) {
	t.Parallel()

	pub := BuildAgentRunRegistry(quietLogger()).Names()
	internal := buildAgentRunRegistry(quietLogger()).Names()

	// Names() returns sorted slices, so a direct compare is stable.
	if !reflect.DeepEqual(pub, internal) {
		t.Errorf("BuildAgentRunRegistry resolved %v but buildAgentRunRegistry resolved %v; they must be identical",
			pub, internal)
	}

	// Every resolved provider must be one of the canonical eight — the
	// registry never resolves a name outside the single-source set.
	canon := map[agent.ProviderName]bool{}
	for _, p := range canonicalAgentRunProviders {
		canon[p] = true
	}
	for _, n := range pub {
		if !canon[n] {
			t.Errorf("registry resolved unexpected provider %q (not in the canonical eight)", n)
		}
	}
}

// TestBuildAgentRunRegistry_ForcedSuccessResolvesAllEight proves that when
// every ctor succeeds (the fully-provisioned-host case), the registry resolves
// ALL eight ProviderNames — and that each ctor maps to the right name. It runs
// the SAME buildRegistryFromCtors core the production builder uses, but swaps
// each real New() for a fakeProvider carrying that ctor's declared name, so the
// assertion is host-independent yet exercises the real registration path
// (Register → Resolve) over the real single-source ctor list.
func TestBuildAgentRunRegistry_ForcedSuccessResolvesAllEight(t *testing.T) {
	t.Parallel()

	forced := make([]providerCtor, 0, len(agentRunProviderCtors()))
	for _, c := range agentRunProviderCtors() {
		name := c.name // capture
		forced = append(forced, providerCtor{
			name: name,
			new: func() (agent.Provider, error) {
				return &fakeProvider{name: agent.ProviderName(name)}, nil
			},
		})
	}

	reg := buildRegistryFromCtors(quietLogger(), forced, "donmai")

	for _, want := range canonicalAgentRunProviders {
		if _, err := reg.Resolve(want); err != nil {
			t.Errorf("registry cannot resolve %q: %v", want, err)
		}
	}
	if got := len(reg.Names()); got != len(canonicalAgentRunProviders) {
		t.Errorf("registry size = %d; want %d", got, len(canonicalAgentRunProviders))
	}
}
