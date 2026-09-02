package pi

import "testing"

// TestSplitBuiltinProviderPin is the table-driven proof requirement 3 calls
// for: a "<provider>/<model>" pin splits IFF provider is one of pi's
// built-in providers (builtinProviderCredentialEnv); every other shape —
// no "/", an unrecognized prefix, or a malformed provider/model half — is
// left completely unsplit, including the "unchanged un-prefixed pins" case
// (a bare model id with no "/" at all).
func TestSplitBuiltinProviderPin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		pin          string
		wantProvider string
		wantModel    string
		wantOK       bool
	}{
		{
			name: "zai pin splits", pin: "zai/glm-5.3",
			wantProvider: "zai", wantModel: "glm-5.3", wantOK: true,
		},
		{name: "xai sibling seed splits", pin: "xai/grok-4.6", wantProvider: "xai", wantModel: "grok-4.6", wantOK: true},
		{
			name: "anthropic sibling seed splits", pin: "anthropic/claude-opus-4-8",
			wantProvider: "anthropic", wantModel: "claude-opus-4-8", wantOK: true,
		},
		{name: "openai sibling seed splits", pin: "openai/gpt-5.4", wantProvider: "openai", wantModel: "gpt-5.4", wantOK: true},
		{
			name: "model half may itself carry slashes", pin: "cloudflare-workers-ai/@cf/moonshotai/kimi-k2.6",
			wantProvider: "cloudflare-workers-ai", wantModel: "@cf/moonshotai/kimi-k2.6", wantOK: true,
		},
		// Unchanged un-prefixed pins (requirement 3): no "/" at all, never split.
		{name: "unprefixed claude pin stays whole", pin: "claude-opus-4-8", wantOK: false},
		{name: "unprefixed gpt pin stays whole", pin: "gpt-5.4", wantOK: false},
		// An unrecognized prefix (not one of pi's shipped providers) is left
		// alone too — an aggregator's own "vendor/model"-shaped catalog slug
		// must not be mistaken for a pi provider selector.
		{name: "unrecognized prefix stays whole", pin: "agg-vendor/claude-3-haiku", wantOK: false},
		{name: "empty pin", pin: "", wantOK: false},
		{name: "leading slash (empty provider)", pin: "/glm-5.3", wantOK: false},
		{name: "trailing slash (empty model)", pin: "zai/", wantOK: false},
		{name: "provider-looking prefix but no model text at all", pin: "zai", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			provider, model, ok := splitBuiltinProviderPin(tc.pin)
			if ok != tc.wantOK {
				t.Fatalf("splitBuiltinProviderPin(%q) ok = %v, want %v", tc.pin, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if provider != tc.wantProvider || model != tc.wantModel {
				t.Errorf("splitBuiltinProviderPin(%q) = (%q, %q), want (%q, %q)", tc.pin, provider, model, tc.wantProvider, tc.wantModel)
			}
		})
	}
}

// TestBuiltinProviderCredentialEnv_KnownEntries spot-checks a handful of
// entries against docs/providers.md's "API Keys" table (this map's
// documented source — builtin_providers.go's doc comment) so a transcription
// slip on the exact env var name fails a test instead of silently
// mis-routing a credential.
func TestBuiltinProviderCredentialEnv_KnownEntries(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"zai":            "ZAI_API_KEY",
		"xai":            "XAI_API_KEY",
		"anthropic":      "ANTHROPIC_API_KEY",
		"openai":         "OPENAI_API_KEY",
		"google":         "GEMINI_API_KEY",
		"amazon-bedrock": "AWS_BEARER_TOKEN_BEDROCK",
	}
	for provider, envVar := range want {
		got, ok := builtinProviderCredentialEnv[provider]
		if !ok {
			t.Errorf("builtinProviderCredentialEnv missing provider %q", provider)
			continue
		}
		if got != envVar {
			t.Errorf("builtinProviderCredentialEnv[%q] = %q, want %q", provider, got, envVar)
		}
	}
	// Deliberately excluded: OAuth/subscription-only or ADC-based providers
	// have no static API-key env var to route a BYOK credential through (see
	// builtin_providers.go's doc comment), so a "<name>/<model>" pin using
	// one of these prefixes must fall through unsplit rather than guessed.
	for _, excluded := range []string{"openai-codex", "github-copilot", "google-vertex"} {
		if _, ok := builtinProviderCredentialEnv[excluded]; ok {
			t.Errorf("builtinProviderCredentialEnv unexpectedly carries OAuth-only provider %q", excluded)
		}
	}
}
