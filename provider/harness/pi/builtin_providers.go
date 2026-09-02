package pi

import "strings"

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
