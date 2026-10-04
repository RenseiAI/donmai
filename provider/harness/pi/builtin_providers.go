package pi

import (
	"net/url"
	"strings"
)

// builtinProviderCredentialEnv is the explicit allowlist requirement 1 calls
// for: pi's own BUILT-IN providers, mapped to the environment variable pi
// itself resolves that provider's API key from.
//
// Source and provenance: pi's `--model <pattern>` flag documents "supports
// provider/id" (pi --help, verified against the pinned-adjacent
// @earendil-works/pi-coding-agent binary), and pi ships one static catalog
// file per built-in provider — the same slugs this map's keys use — under
// the bundled @earendil-works/pi-ai package's providers/data/*.json (one
// file per provider, e.g. providers/data/zai.json, whose "provider" field
// and directory basename agree with docs/providers.md's "auth.json key"
// column). This map is transcribed from pi's own shipped
// docs/providers.md "API Keys" table (the "Environment Variable" /
// "auth.json key" columns) rather than read live at build or run time: pi
// ships no scriptable "list built-in providers" surface this package could
// call without shelling to node and bundling (or fetching) a copy of
// @earendil-works/pi-ai, and the whole point of requirement 2's catalog
// preflight is to catch this map drifting from a real pi install rather than
// trusting it silently. Deliberately scoped to providers.md's plain
// API-key table only — OAuth/subscription-only providers (openai-codex,
// github-copilot, google-vertex, …) and llama.cpp have no static env var to
// route a BYOK credential through, so a "<name>/<model>" pin using one of
// those prefixes is left unsplit (falls through to the injected "donmai"
// provider, or is treated as an opaque unprefixed model id) rather than
// guessed at. Radius (below) is a genuine exception to that OAuth-only
// pattern: docs/providers.md documents it as BOTH an OAuth subscription
// (`/login radius`) AND a plain API-key credential (`RADIUS_API_KEY`), so it
// keeps its entry in this map.
//
// Keep in sync with docs/providers.md when the pi version pin
// (probe.go PinnedVersion) moves; builtin_providers_test.go documents the
// exact verification command run against the pinned-adjacent binary.
var builtinProviderCredentialEnv = map[string]string{ //nolint:gosec // G101: map values are env-var NAMES, never credential bytes.
	"anthropic":                  "ANTHROPIC_API_KEY",
	"ant-ling":                   "ANT_LING_API_KEY",
	"azure-openai-responses":     "AZURE_OPENAI_API_KEY",
	"openai":                     "OPENAI_API_KEY",
	"deepseek":                   "DEEPSEEK_API_KEY",
	"nvidia":                     "NVIDIA_API_KEY",
	"google":                     "GEMINI_API_KEY",
	"amazon-bedrock":             "AWS_BEARER_TOKEN_BEDROCK",
	"mistral":                    "MISTRAL_API_KEY",
	"groq":                       "GROQ_API_KEY",
	"cerebras":                   "CEREBRAS_API_KEY",
	"cloudflare-ai-gateway":      "CLOUDFLARE_API_KEY",
	"cloudflare-workers-ai":      "CLOUDFLARE_API_KEY",
	"xai":                        "XAI_API_KEY",
	"openrouter":                 "OPENROUTER_API_KEY",
	"vercel-ai-gateway":          "AI_GATEWAY_API_KEY",
	"zai":                        "ZAI_API_KEY",
	"zai-coding-cn":              "ZAI_CODING_CN_API_KEY",
	"opencode":                   "OPENCODE_API_KEY",
	"opencode-go":                "OPENCODE_API_KEY",
	"radius":                     "RADIUS_API_KEY",
	"huggingface":                "HF_TOKEN",
	"fireworks":                  "FIREWORKS_API_KEY",
	"together":                   "TOGETHER_API_KEY",
	"baseten":                    "BASETEN_API_KEY",
	"kimi-coding":                "KIMI_API_KEY",
	"minimax":                    "MINIMAX_API_KEY",
	"minimax-cn":                 "MINIMAX_CN_API_KEY",
	"qwen-token-plan":            "QWEN_TOKEN_PLAN_API_KEY",
	"qwen-token-plan-individual": "QWEN_TOKEN_PLAN_API_KEY",
	"qwen-token-plan-cn":         "QWEN_TOKEN_PLAN_CN_API_KEY",
	"xiaomi":                     "XIAOMI_API_KEY",
	"xiaomi-token-plan-cn":       "XIAOMI_TOKEN_PLAN_CN_API_KEY",
	"xiaomi-token-plan-ams":      "XIAOMI_TOKEN_PLAN_AMS_API_KEY",
	"xiaomi-token-plan-sgp":      "XIAOMI_TOKEN_PLAN_SGP_API_KEY",
}

// splitBuiltinProviderPin splits a "<provider>/<model>" pin into its
// provider and bare-model halves IFF provider names one of pi's built-in
// providers (builtinProviderCredentialEnv's key set). Any other shape — no
// "/", an empty provider or model half, or a prefix that is not a
// recognized pi provider slug — returns ok=false, and the caller must treat
// pin as an opaque, already-bare model id (e.g. a slash-bearing id like a
// Bedrock inference-profile ARN, or a "vendor/model"-shaped id from a
// provider pi does not ship). Only the FIRST "/" is significant: the model
// half may itself contain further slashes (e.g. Cloudflare Workers AI's
// "@cf/…" ids), which pin[i+1:] preserves whole.
func splitBuiltinProviderPin(pin string) (provider, model string, ok bool) {
	i := strings.IndexByte(pin, '/')
	if i <= 0 || i == len(pin)-1 {
		return "", "", false
	}
	provider, model = pin[:i], pin[i+1:]
	if _, known := builtinProviderCredentialEnv[provider]; !known {
		return "", "", false
	}
	return provider, model, true
}

// builtinProviderServingHost maps each built-in provider that pi serves from
// one fixed https endpoint to that endpoint's hostname, transcribed from the
// baseUrl each bundled @earendil-works/pi-ai providers/<name>.js of the
// pinned-adjacent binary registers. Providers whose endpoint is templated per
// account or region (amazon-bedrock, azure-openai-responses, the cloudflare
// pair) or not shipped as a fixed URL (the qwen token plans) are absent.
//
// It decides whether a bound endpoint IS the built-in provider's own
// endpoint (nativeProviderPin). pi's native route talks to the provider's
// own endpoint and ignores the binding's BaseURL, so it is only correct when
// the two agree; any other BaseURL must go through the injected provider,
// which is registered against the BaseURL itself.
var builtinProviderServingHost = map[string]string{ //nolint:gosec // G101: map values are public API hostnames, never credential bytes.
	"anthropic":             "api.anthropic.com",
	"ant-ling":              "api.ant-ling.com",
	"openai":                "api.openai.com",
	"deepseek":              "api.deepseek.com",
	"nvidia":                "integrate.api.nvidia.com",
	"google":                "generativelanguage.googleapis.com",
	"mistral":               "api.mistral.ai",
	"groq":                  "api.groq.com",
	"cerebras":              "api.cerebras.ai",
	"xai":                   "api.x.ai",
	"openrouter":            "openrouter.ai",
	"vercel-ai-gateway":     "ai-gateway.vercel.sh",
	"zai":                   "api.z.ai",
	"zai-coding-cn":         "open.bigmodel.cn",
	"opencode":              "opencode.ai",
	"opencode-go":           "opencode.ai",
	"huggingface":           "router.huggingface.co",
	"fireworks":             "api.fireworks.ai",
	"together":              "api.together.ai",
	"kimi-coding":           "api.kimi.com",
	"minimax":               "api.minimax.io",
	"minimax-cn":            "api.minimaxi.com",
	"xiaomi":                "api.xiaomimimo.com",
	"xiaomi-token-plan-cn":  "token-plan-cn.xiaomimimo.com",
	"xiaomi-token-plan-ams": "token-plan-ams.xiaomimimo.com",
	"xiaomi-token-plan-sgp": "token-plan-sgp.xiaomimimo.com",
}

// builtinProviderBaseURL maps each built-in provider with a fixed provider-level
// endpoint to pi's own catalog base URL for it, transcribed from the baseUrl
// each bundled @earendil-works/pi-ai providers/<name>.js registers. It sits
// next to builtinProviderServingHost (which carries the host half of the
// same endpoints) so nativeProviderPin can compare the bound URL's path
// against pi's own catalog, not just the host: one vendor host can serve
// separately billed APIs on different paths, and a binding to one path must
// not be silently served from another path's quota. Providers whose endpoint
// is templated per account or region, set per model rather than per provider,
// or otherwise not shipped as a single fixed URL are absent; for those the
// native check falls back to the host comparison. Aggregator entries are
// data only: aggregator routing stays host-based (builtinAggregatorForBaseURL)
// and is never path-checked.
var builtinProviderBaseURL = map[string]string{ //nolint:gosec // G101: map values are public API base URLs, never credential bytes.
	"anthropic":             "https://api.anthropic.com",
	"ant-ling":              "https://api.ant-ling.com/v1",
	"openai":                "https://api.openai.com/v1",
	"deepseek":              "https://api.deepseek.com",
	"nvidia":                "https://integrate.api.nvidia.com/v1",
	"google":                "https://generativelanguage.googleapis.com/v1beta",
	"mistral":               "https://api.mistral.ai",
	"groq":                  "https://api.groq.com/openai/v1",
	"cerebras":              "https://api.cerebras.ai/v1",
	"xai":                   "https://api.x.ai/v1",
	"openrouter":            "https://openrouter.ai/api/v1",
	"vercel-ai-gateway":     "https://ai-gateway.vercel.sh",
	"zai":                   "https://api.z.ai/api/coding/paas/v4",
	"zai-coding-cn":         "https://open.bigmodel.cn/api/coding/paas/v4",
	"huggingface":           "https://router.huggingface.co/v1",
	"fireworks":             "https://api.fireworks.ai/inference",
	"together":              "https://api.together.ai/v1",
	"kimi-coding":           "https://api.kimi.com/coding",
	"minimax":               "https://api.minimax.io/anthropic",
	"minimax-cn":            "https://api.minimaxi.com/anthropic",
	"xiaomi":                "https://api.xiaomimimo.com/v1",
	"xiaomi-token-plan-cn":  "https://token-plan-cn.xiaomimimo.com/v1",
	"xiaomi-token-plan-ams": "https://token-plan-ams.xiaomimimo.com/v1",
	"xiaomi-token-plan-sgp": "https://token-plan-sgp.xiaomimimo.com/v1",
}

// builtinAggregatorProviders is the subset of built-in providers that are
// AGGREGATORS: their own catalog ids are themselves "<author>/<model>" slugs
// (pi's vercel-ai-gateway catalog lists "anthropic/claude-sonnet-4.6", served
// over the anthropic-messages API; openrouter lists "anthropic/…" ids too).
//
// A control plane that binds an aggregator endpoint sends the aggregator's
// own slug verbatim (Endpoint.Model "anthropic/claude-sonnet-4.6",
// Endpoint.BaseURL "https://ai-gateway.vercel.sh/v1"). Read as a pi pin, that
// slug's first segment names pi's DIRECT "anthropic" provider, whose catalog
// spells the model "claude-sonnet-4-6" and whose endpoint is
// api.anthropic.com. The slug must therefore stay whole on an aggregator
// endpoint, and the aggregator itself is only selected natively once pi's
// catalog confirms it carries the exact slug (Provider.promoteAggregatorPin).
var builtinAggregatorProviders = map[string]bool{
	"vercel-ai-gateway": true,
	"openrouter":        true,
}

// httpsHostname returns the lower-cased hostname of an absolute https URL, or
// ok=false for anything else. Plain http never matches a built-in provider:
// every fixed endpoint above is served over TLS.
func httpsHostname(baseURL string) (string, bool) {
	if baseURL == "" {
		return "", false
	}
	u, err := url.Parse(baseURL)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" {
		return "", false
	}
	return strings.ToLower(u.Hostname()), true
}

// baseURLMatchesProviderEndpoint reports whether baseURL is an https URL on
// provider's own serving host (exact hostname match; no suffix or subdomain
// matching) whose path agrees with pi's own catalog base URL for a DIRECT
// (non-aggregator) built-in provider. The comparison is a normalized,
// segment-bounded path-prefix match: both paths are trimmed of a single
// trailing slash (so "…/v1" and "…/v1/" agree), and either they are equal
// or the bound path extends the catalog path at a "/" boundary (so a
// binding "…/v1/projects/…" still selects the catalog's "…/v1", but
// "…/v10" does not). The reverse — a bound path SHORTER than the catalog
// path — never matches: the bound URL must carry at least the catalog's own
// path before it can be that provider's endpoint.
//
// Aggregator providers never reach the path check: their routing stays
// host-based (builtinAggregatorForBaseURL + promoteAggregatorPin), and a
// provider with no catalog base URL entry falls back to the host
// comparison, preserving today's behavior for templated or per-model
// endpoints.
func baseURLMatchesProviderEndpoint(baseURL, provider string) bool {
	wantHost, known := builtinProviderServingHost[provider]
	if !known {
		return false
	}
	u, err := url.Parse(baseURL)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" {
		return false
	}
	if !strings.EqualFold(u.Hostname(), wantHost) {
		return false
	}
	if builtinAggregatorProviders[provider] {
		return true
	}
	wantBase, known := builtinProviderBaseURL[provider]
	if !known {
		return true
	}
	wantURL, err := url.Parse(wantBase)
	if err != nil || !strings.EqualFold(wantURL.Scheme, "https") || wantURL.Hostname() == "" {
		return true
	}
	if !strings.EqualFold(u.Hostname(), wantURL.Hostname()) {
		return false
	}
	return boundPathExtendsCatalogPath(strings.TrimSuffix(u.EscapedPath(), "/"), strings.TrimSuffix(wantURL.EscapedPath(), "/"))
}

// boundPathExtendsCatalogPath reports whether bound (a URL path with any
// trailing slash already trimmed) names the catalog path itself or a route
// under it: equal, or bound extends catalog at a "/" segment boundary.
// An empty catalog path (a host-root base URL) matches every path on that
// host; a non-empty one never matches a shorter or sibling path.
func boundPathExtendsCatalogPath(bound, catalog string) bool {
	if catalog == "" {
		return true
	}
	if bound == catalog {
		return true
	}
	return strings.HasPrefix(bound, catalog+"/")
}

// baseURLIsProviderHost reports whether baseURL is an https URL on provider's
// own serving host (exact hostname match; no suffix or subdomain matching).
func baseURLIsProviderHost(baseURL, provider string) bool {
	want, known := builtinProviderServingHost[provider]
	if !known {
		return false
	}
	host, ok := httpsHostname(baseURL)
	return ok && host == want
}

// builtinAggregatorForBaseURL reports the built-in aggregator provider whose
// own serving host baseURL names, or ok=false for an empty, unparseable,
// non-https, or non-aggregator URL.
func builtinAggregatorForBaseURL(baseURL string) (provider string, ok bool) {
	for agg := range builtinAggregatorProviders {
		if baseURLIsProviderHost(baseURL, agg) {
			return agg, true
		}
	}
	return "", false
}
