package runner

import (
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestResolvedProvider_HarnessNative is the harness-native selection table.
// It covers the platform's catalog re-vocab (model identity = Provider with
// Harness as an attribute) plus the legacy Provider="agy-cli" alias the
// platform still emits today, and the normal Provider-only fallback path.
func TestResolvedProvider_HarnessNative(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		profile ResolvedProfile
		want    agent.ProviderName
	}{
		{
			// (a) Harness-native: catalog models the model as gemini with
			// harness=agy; Harness is authoritative over Provider.
			name:    "harness agy with provider gemini resolves agy-cli",
			profile: ResolvedProfile{Provider: agent.ProviderGemini, Harness: "agy"},
			want:    agent.ProviderAGYCLI,
		},
		{
			// (b) Legacy alias: in-flight/stale platform payload with the
			// transitional provider=agy-cli token and no harness still works.
			name:    "legacy provider agy-cli without harness resolves agy-cli",
			profile: ResolvedProfile{Provider: agent.ProviderAGYCLI},
			want:    agent.ProviderAGYCLI,
		},
		{
			// (c) Fallback path intact: a normal claude dispatch with no
			// harness resolves claude via the Provider field.
			name:    "provider claude without harness resolves claude",
			profile: ResolvedProfile{Provider: agent.ProviderClaude},
			want:    agent.ProviderClaude,
		},
		{
			// Harness takes precedence even when both are set to the same
			// family — the catalog attribute wins.
			name:    "harness agy with provider agy-cli resolves agy-cli",
			profile: ResolvedProfile{Provider: agent.ProviderAGYCLI, Harness: "agy"},
			want:    agent.ProviderAGYCLI,
		},
		{
			// An unrecognized harness token is a forward-compatible no-op:
			// the runner falls through to the Provider field rather than
			// failing, so a new token a stale runner doesn't know still
			// resolves via Provider.
			name:    "unknown harness falls back to provider",
			profile: ResolvedProfile{Provider: agent.ProviderCodex, Harness: "future-harness"},
			want:    agent.ProviderCodex,
		},
		{
			// Legacy Runner field still resolves when Provider + Harness are
			// both empty.
			name:    "empty provider and harness falls back to runner field",
			profile: ResolvedProfile{Runner: string(agent.ProviderCodex)},
			want:    agent.ProviderCodex,
		},
		{
			// Empty everything defaults to claude (unchanged behaviour).
			name:    "empty profile defaults to claude",
			profile: ResolvedProfile{},
			want:    agent.ProviderClaude,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			qw := QueuedWork{ResolvedProfile: tt.profile}
			if got := qw.resolvedProvider(); got != tt.want {
				t.Errorf("resolvedProvider() = %q; want %q", got, tt.want)
			}
		})
	}
}

// TestHarnessToProvider exercises the mapping helper directly so a new
// token addition is covered without going through the full QueuedWork.
func TestHarnessToProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		harness string
		want    agent.ProviderName
		wantOK  bool
	}{
		{harness: "agy", want: agent.ProviderAGYCLI, wantOK: true},
		{harness: "", want: "", wantOK: false},
		{harness: "antigravity", want: "", wantOK: false}, // internal HarnessName is NOT the wire token
		{harness: "claude", want: "", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.harness, func(t *testing.T) {
			t.Parallel()
			got, ok := harnessToProvider(tt.harness)
			if ok != tt.wantOK {
				t.Fatalf("harnessToProvider(%q) ok = %v; want %v", tt.harness, ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("harnessToProvider(%q) = %q; want %q", tt.harness, got, tt.want)
			}
		})
	}
}
