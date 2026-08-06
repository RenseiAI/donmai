package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// ─── Per-session opencode.json injection (07-design-opencode-spawn.md §5) ────
//
// Lane B stands up one `opencode serve` child per donmai session on a
// loopback port the provider owns. Before the child starts, config.go
// renders a session-scoped opencode.json and points the child at it via the
// OPENCODE_CONFIG env var (a FILE, not OPENCODE_CONFIG_CONTENT: it survives
// a subprocess re-exec and is auditable post-mortem). The file is written
// 0600 inside the session state dir, regenerated per spawn, never reused.
//
// The config enforces three Rensei routing guarantees:
//
//  1. §5.1 model pin — a single custom OpenAI-compatible "donmai" provider
//     whose baseURL is the resolved EndpointBinding (the vendor's own compat
//     URL for a direct cell; the local gateway URL once 08 ships). The key
//     value never enters the JSON — apiKey is "{env:DONMAI_OC_KEY}", resolved
//     from the spawned process env (keys ride env, never disk). enabled_providers
//     + an experimental.policies provider.use deny-all-but-donmai statement
//     hard-block any silent fallback to a provider outside the resolved cell.
//  2. §5.2 tool policy — Spec.AllowedTools / Spec.DisallowedTools (Claude
//     grammar) and Spec.PermissionConfig project onto opencode's per-tool
//     allow/ask/deny maps. Anything that lands on "ask" at runtime is
//     adjudicated by the provider's own permission pump (permission.go), not
//     a human.
//  3. §5.3 MCP whitelist — the mcp block is rendered ONLY from Spec.MCPServers;
//     no spec means no mcp key, so no unexpected server's tools ever appear.

// OCProviderID is the synthetic provider id the injected config declares.
// A single provider named "donmai" fronts whatever endpoint the resolved
// cell (or, later, the gateway) points at, so opencode only ever sees one
// legal provider.
const OCProviderID = "donmai"

// OCKeyEnvVar is the env-var NAME the injected config's apiKey references
// via opencode's {env:...} substitution. The runner populates its VALUE in
// the spawned process env from Endpoint.Env / Spec.Env after OnPreSpawn —
// the credential-socket posture (keys ride env, never disk).
const OCKeyEnvVar = "DONMAI_OC_KEY" //nolint:gosec // G101: env-var NAME, not a credential

// OCConfigEnvVar is the env var opencode reads a config file path from.
const OCConfigEnvVar = "OPENCODE_CONFIG"

// ocConfig is the subset of the opencode.json schema
// (https://opencode.ai/config.json) this adapter renders. Fields are
// omitempty so an absent policy / MCP whitelist produces no key at all
// (§5.3: "No spec → no mcp key").
type ocConfig struct {
	Schema           string                 `json:"$schema"`
	Provider         map[string]ocProvider  `json:"provider,omitempty"`
	Model            string                 `json:"model,omitempty"`
	EnabledProviders []string               `json:"enabled_providers,omitempty"`
	Permission       map[string]any         `json:"permission,omitempty"`
	MCP              map[string]ocMCPServer `json:"mcp,omitempty"`
	Experimental     *ocExperimental        `json:"experimental,omitempty"`
}

type ocProvider struct {
	NPM     string                  `json:"npm"`
	Options ocProviderOptions       `json:"options"`
	Models  map[string]ocModelEntry `json:"models"`
}

type ocProviderOptions struct {
	BaseURL string `json:"baseURL,omitempty"`
	APIKey  string `json:"apiKey,omitempty"`
}

// ocModelEntry is intentionally empty — the model id is the map key and the
// custom OpenAI-compatible provider needs no per-model overrides.
type ocModelEntry struct{}

// ocMCPServer is opencode's per-project local or remote MCP entry shape.
type ocMCPServer struct {
	Type        string            `json:"type"`
	Command     []string          `json:"command,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	OAuth       *bool             `json:"oauth,omitempty"`
	Enabled     bool              `json:"enabled"`
}

type ocExperimental struct {
	Policies []ocPolicy `json:"policies"`
}

// ocPolicy is one experimental.policies statement. opencode uses this layer
// to gate exactly one action today — provider.use — which is what the
// deny-all-but-donmai lockout rides (§5.1).
type ocPolicy struct {
	Effect   string `json:"effect"`
	Action   string `json:"action"`
	Resource string `json:"resource"`
}

// buildConfig renders the session-scoped opencode.json from a resolved Spec.
// It never reads or embeds credential values — apiKey is always the
// {env:...} indirection.
func buildConfig(spec agent.Spec) ocConfig {
	cfg := ocConfig{
		Schema:           "https://opencode.ai/config.json",
		EnabledProviders: []string{OCProviderID},
		Permission:       projectPermissions(spec),
		Experimental: &ocExperimental{
			// Double-lock with enabled_providers: an explicit deny-all,
			// allow-donmai provider.use policy so a worker can never fall
			// back to a provider outside the resolved cell (the cost-honesty
			// property D4 requires — 07 §5.1).
			Policies: []ocPolicy{
				{Effect: "allow", Action: "provider.use", Resource: OCProviderID},
				{Effect: "deny", Action: "provider.use", Resource: "*"},
			},
		},
	}

	model := resolvedModel(spec)
	if model != "" {
		cfg.Provider = map[string]ocProvider{
			OCProviderID: {
				NPM: "@ai-sdk/openai-compatible",
				Options: ocProviderOptions{
					BaseURL: resolvedBaseURL(spec),
					APIKey:  "{env:" + OCKeyEnvVar + "}",
				},
				Models: map[string]ocModelEntry{model: {}},
			},
		}
		cfg.Model = OCProviderID + "/" + model
	}

	if mcp := projectMCP(spec); len(mcp) > 0 {
		cfg.MCP = mcp
	}
	return cfg
}

// resolvedModel returns Endpoint.Model when the binding carries one, else
// Spec.Model — the same "binding wins over spec" rule the claude one-shot
// lane uses (§9).
func resolvedModel(spec agent.Spec) string {
	if spec.Endpoint != nil && spec.Endpoint.Model != "" {
		return spec.Endpoint.Model
	}
	return spec.Model
}

// resolvedBaseURL returns the resolved cell's compat base URL, or empty when
// no binding is present (opencode then falls back to the provider default —
// only meaningful for a local unauthenticated server in tests).
func resolvedBaseURL(spec agent.Spec) string {
	if spec.Endpoint != nil {
		return spec.Endpoint.BaseURL
	}
	return ""
}

// writeConfig renders buildConfig(spec) and writes it 0600 into dir, returning
// the file path. The caller points OPENCODE_CONFIG at it.
func writeConfig(dir string, spec agent.Spec) (string, error) {
	cfg := buildConfig(spec)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("provider/opencode: marshal config: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("provider/opencode: config dir: %w", err)
	}
	path := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("provider/opencode: write config: %w", err)
	}
	return path, nil
}

// ─── §5.2 permission projection ──────────────────────────────────────────────
//
// Claude-grammar tool patterns ("Bash(git push:*)", "Edit", "WebFetch") project
// onto opencode's per-tool allow/ask/deny maps. opencode permission keys:
// read, edit (write/edit/patch/apply_patch), glob, grep, bash, task, skill,
// lsp, question, webfetch, websearch, plus the two safety guards
// external_directory and doom_loop (both kept at "ask" so they route to the
// pump).

// claudeToolToOC maps a Claude tool name onto its opencode permission key.
// Unknown tools return "" (skipped — opencode's own default applies).
func claudeToolToOC(tool string) string {
	if key := mcpToolToOC(tool); key != "" {
		return key
	}
	switch tool {
	case "Bash":
		return "bash"
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return "edit"
	case "Read", "NotebookRead":
		return "read"
	case "Glob":
		return "glob"
	case "Grep":
		return "grep"
	case "WebFetch":
		return "webfetch"
	case "WebSearch":
		return "websearch"
	case "Task":
		return "task"
	default:
		return ""
	}
}

// mcpToolToOC maps Donmai's canonical mcp__<server>__<tool> name onto
// OpenCode's <server>_<tool> permission key.
func mcpToolToOC(tool string) string {
	if !strings.HasPrefix(tool, "mcp__") {
		return ""
	}
	rest := strings.TrimPrefix(tool, "mcp__")
	server, name, ok := strings.Cut(rest, "__")
	if !ok || server == "" || name == "" {
		return ""
	}
	normalize := func(value string) string {
		return strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
				return r
			default:
				return '_'
			}
		}, value)
	}
	return normalize(server) + "_" + normalize(name)
}

// parseClaudePattern splits "Tool(inner)" into (tool, inner). A bare "Tool"
// yields (tool, "").
func parseClaudePattern(p string) (tool, inner string) {
	p = strings.TrimSpace(p)
	open := strings.IndexByte(p, '(')
	if open < 0 || !strings.HasSuffix(p, ")") {
		return p, ""
	}
	return p[:open], p[open+1 : len(p)-1]
}

// claudeInnerToGlob turns the Claude inner arg grammar ("git push:*", "npm:*",
// "*") into an opencode bash command glob ("git push*", "npm*", "*").
func claudeInnerToGlob(inner string) string {
	inner = strings.TrimSpace(inner)
	if inner == "" || inner == "*" {
		return "*"
	}
	// "prefix:glob" — opencode matches on the whole command string.
	if i := strings.LastIndexByte(inner, ':'); i >= 0 {
		prefix := strings.TrimSpace(inner[:i])
		rest := strings.TrimSpace(inner[i+1:])
		if rest == "*" || rest == "" {
			return prefix + "*"
		}
		return prefix + " " + rest
	}
	return inner
}

// projectPermissions builds the opencode `permission` map from the Spec's
// AllowedTools / DisallowedTools and PermissionConfig. The two safety guards
// are always pinned to "ask" so they route to the provider's permission pump.
func projectPermissions(spec agent.Spec) map[string]any {
	perm := map[string]any{
		"external_directory": "ask",
		"doom_loop":          "ask",
	}

	// Default decision → per-tool catch-all. Empty defaults to "ask" so
	// unmatched calls reach the pump rather than silently allowing.
	def := "ask"
	if spec.PermissionConfig == nil && (len(spec.AllowedTools) > 0 || len(spec.MCPToolNames) > 0) {
		def = "deny"
	}
	perm["*"] = def

	// bash accumulates a command-glob map; other tools accumulate a single
	// whole-tool decision. denies are collected separately so they always
	// take precedence over allows for the same key.
	bashGlobs := map[string]string{}
	toolAllow := map[string]bool{}
	toolDeny := map[string]bool{}

	apply := func(pattern, decision string) {
		tool, inner := parseClaudePattern(pattern)
		key := claudeToolToOC(tool)
		if key == "" {
			return
		}
		if key == "bash" {
			bashGlobs[claudeInnerToGlob(inner)] = decision
			return
		}
		if decision == "deny" {
			toolDeny[key] = true
		} else if decision == "allow" {
			toolAllow[key] = true
		}
	}

	// Order: allows first, then denies — denies overwrite the same bash glob
	// / mark the tool denied, honoring "denies win" (§5.2).
	for _, p := range spec.AllowedTools {
		apply(p, "allow")
	}
	for _, p := range spec.MCPToolNames {
		apply(p, "allow")
	}
	if len(spec.MCPToolNames) == 0 {
		for _, server := range spec.MCPServers {
			if key := mcpToolToOC("mcp__" + server.Name + "__*"); key != "" {
				toolAllow[key] = true
			}
		}
	}
	for _, p := range spec.DisallowedTools {
		apply(p, "deny")
	}

	for k := range toolDeny {
		perm[k] = "deny"
	}
	for k := range toolAllow {
		if _, denied := toolDeny[k]; !denied {
			perm[k] = "allow"
		}
	}

	// bash: emit a glob map with the catch-all default last. When no bash
	// pattern was supplied, bash still gets the catch-all so unmatched shell
	// calls route to the pump / default.
	if _, ok := bashGlobs["*"]; !ok {
		bashGlobs["*"] = def
	}
	perm["bash"] = bashGlobs

	return perm
}

// ─── §5.3 MCP whitelist ──────────────────────────────────────────────────────

// projectMCP renders the session-scoped mcp block from Spec.MCPServers. Local
// stdio services carry command/environment; platform A2A and other Streamable
// HTTP services carry a remote URL and headers with OpenCode OAuth disabled.
func projectMCP(spec agent.Spec) map[string]ocMCPServer {
	if len(spec.MCPServers) == 0 {
		return nil
	}
	out := make(map[string]ocMCPServer, len(spec.MCPServers))
	// Deterministic order is not required (JSON object), but iterate a sorted
	// copy so the rendered file is byte-stable for the same spec (auditability).
	servers := append([]agent.MCPServerConfig(nil), spec.MCPServers...)
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	for _, s := range servers {
		if s.Name == "" {
			continue
		}
		if strings.EqualFold(s.Type, "http") {
			disabled := false
			out[s.Name] = ocMCPServer{Type: "remote", URL: s.URL, Headers: s.Headers, OAuth: &disabled, Enabled: true}
			continue
		}
		if s.Command == "" {
			continue
		}
		cmd := append([]string{s.Command}, s.Args...)
		out[s.Name] = ocMCPServer{
			Type:        "local",
			Command:     cmd,
			Environment: s.Env,
			Enabled:     true,
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
