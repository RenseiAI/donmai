package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/ptycli"
	"github.com/RenseiAI/donmai/runtime/mcp"
)

// SpawnInteractive opens the codex CLI's OWN interactive TUI — bare
// `codex`, NOT `codex exec` (the non-interactive/headless subcommand this
// package's default Spawn drives via the app-server) — under a PTY via
// ptycli, seeded with spec.Prompt when set.
//
// It is completely independent of the app-server JSON-RPC subprocess this
// package otherwise drives: it never touches Provider.client/cmd, resolving
// the codex binary itself via resolveCodexBinary. That is why it is a
// package-level function taking Options rather than a (*Provider) method
// bound to a live app-server — the interactive spawn mode needs no live
// Provider at all, only the same binary-resolution rule New uses. Provider's
// Spawn (codex.go) is the production call site; it is exported so a caller
// (or a test) that only wants the interactive path can reach it directly
// without paying for an app-server handshake.
//
// This is a distinct SPAWN MODE from the default headless loop, not a
// different Transport: Manifest().Caps.Transport stays
// agent.TransportSubprocessRPC (the app-server's transport); TransportPTY is
// not codex's declared Transport, only its interactive spawn mode. See
// manifest.go and agent/harness.go's HarnessCaps.SupportsInteractivePTY doc
// comment for why the two are orthogonal, and provider/harness/shell for the
// contrasting harness whose ONLY transport is PTY.
//
// Event semantics are the coarse ptycli contract (program decision D4 — the
// byte-accurate PTY stream is the product): an InitEvent once the PTY child
// is up (SessionID stays empty — codex's thread id is only observable
// through the app-server's JSON-RPC notifications, which the interactive TUI
// never emits) and a single terminal ResultEvent when the CLI process exits.
func SpawnInteractive(ctx context.Context, opts Options, spec agent.Spec) (agent.Handle, error) {
	var err error
	spec, err = agent.PrepareHarness(spec, (&Provider{}).Manifest())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	return spawnInteractivePrepared(ctx, opts, spec)
}

// spawnInteractivePrepared receives the one Spec already admitted by
// PrepareHarness. Keeping all PTY/config/process work below this boundary
// prevents interactive mode from minting a second prompt or tool authority.
func spawnInteractivePrepared(ctx context.Context, opts Options, spec agent.Spec) (agent.Handle, error) {
	if err := validateCodexCLIMCPServers(spec.MCPServers); err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, persistInteractiveMCPApplicationDenial(spec, err))
	}
	launch, err := buildInteractiveLaunch(spec)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, persistInteractiveMCPApplicationDenial(spec, err))
	}
	bin, err := resolveCodexBinary(opts.CodexBin)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	spec.Env = launch.env
	return ptycli.Spawn(ctx, bin, launch.argv, spec, (&Provider{}).Manifest())
}

type interactiveLaunch struct {
	argv []string
	env  map[string]string
}

// interactiveArgs builds the argv for codex's own interactive TUI. The codex
// CLI accepts a positional prompt to seed the first message of an
// interactive session — `codex "fix the failing tests"` launches the TUI
// with that initial prompt already queued, distinct from
// `codex exec "..."` (headless, one-shot, prints the final message to
// stdout and exits) which this package never uses for the interactive
// spawn mode. An empty prompt starts the TUI bare, with no seeded message.
func interactiveArgs(spec agent.Spec) []string {
	launch, _ := buildInteractiveLaunch(spec)
	return launch.argv
}

// buildInteractiveLaunch projects requested MCP servers into one process-local
// Codex CLI override. Codex recursively merges ambient user MCP configuration
// and bare interactive mode has no ignore-user-config switch, so this proves
// requested per-process delivery but deliberately does not claim exclusive MCP
// isolation.
func buildInteractiveLaunch(spec agent.Spec) (interactiveLaunch, error) {
	env := cloneInteractiveEnv(spec.Env)
	var args []string
	if spec.SystemPromptAppend != "" {
		args = append(args, "--config", "developer_instructions="+strconv.Quote(spec.SystemPromptAppend))
	}
	if len(spec.MCPServers) > 0 {
		override, nextEnv, err := codexCLIMCPOverride(spec.MCPServers, env)
		if err != nil {
			return interactiveLaunch{}, err
		}
		env = nextEnv
		args = append(args, "--config", override, "--strict-config")
	}
	if spec.Prompt != "" {
		args = append(args, spec.Prompt)
	}
	return interactiveLaunch{argv: args, env: env}, nil
}

func cloneInteractiveEnv(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func codexCLIMCPOverride(servers []agent.MCPServerConfig, env map[string]string) (string, map[string]string, error) {
	if err := validateCodexCLIMCPServers(servers); err != nil {
		return "", nil, err
	}
	ordered := append([]agent.MCPServerConfig(nil), servers...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	var body strings.Builder
	body.WriteString("mcp_servers={")
	for i, server := range ordered {
		if i > 0 {
			body.WriteByte(',')
		}
		body.WriteString(tomlBasicString(server.Name))
		body.WriteString("={")
		switch strings.ToLower(strings.TrimSpace(server.Type)) {
		case "", "stdio":
			body.WriteString("\"command\"=")
			body.WriteString(tomlBasicString(server.Command))
			body.WriteString(",\"args\"=")
			body.WriteString(tomlStringArray(server.Args))
			if len(server.Env) > 0 {
				keys := sortedStringKeys(server.Env)
				for _, key := range keys {
					if !validProcessEnvKey(key) {
						return "", nil, codexMCPApplicationError("stdio MCP environment contains an invalid variable name")
					}
					var err error
					env, err = setInteractiveEnv(env, key, server.Env[key])
					if err != nil {
						return "", nil, err
					}
				}
				body.WriteString(",\"env_vars\"=")
				body.WriteString(tomlStringArray(keys))
			}
		case "http":
			body.WriteString("\"url\"=")
			body.WriteString(tomlBasicString(server.URL))
			if len(server.Headers) > 0 {
				headers := sortedStringKeys(server.Headers)
				body.WriteString(",\"env_http_headers\"={")
				for j, header := range headers {
					if j > 0 {
						body.WriteByte(',')
					}
					envName := codexHTTPHeaderEnvName(server.Name, header)
					var err error
					env, err = setInteractiveEnv(env, envName, server.Headers[header])
					if err != nil {
						return "", nil, err
					}
					body.WriteString(tomlBasicString(header))
					body.WriteByte('=')
					body.WriteString(tomlBasicString(envName))
				}
				body.WriteByte('}')
			}
		default:
			return "", nil, codexMCPApplicationError("MCP server type must be stdio or http")
		}
		body.WriteByte('}')
	}
	body.WriteByte('}')
	return body.String(), env, nil
}

func setInteractiveEnv(env map[string]string, key, value string) (map[string]string, error) {
	if existing, ok := env[key]; ok {
		if existing != value {
			return nil, codexMCPApplicationError("MCP environment conflicts with the inherited process variable " + key)
		}
		return env, nil
	}
	if env == nil {
		env = make(map[string]string)
	}
	env[key] = value
	return env, nil
}

func validProcessEnvKey(key string) bool {
	return key != "" && !strings.ContainsAny(key, "=\x00")
}

func codexHTTPHeaderEnvName(server, header string) string {
	sum := sha256.Sum256([]byte(server + "\x00" + header))
	return "DONMAI_MCP_HEADER_" + strings.ToUpper(hex.EncodeToString(sum[:]))
}

func validateCodexCLIMCPServers(servers []agent.MCPServerConfig) error {
	seen := make(map[string]struct{}, len(servers))
	for _, server := range servers {
		name := strings.TrimSpace(server.Name)
		if _, exists := seen[name]; exists {
			return codexMCPApplicationError("requested MCP server names must be unique")
		}
		seen[name] = struct{}{}
	}
	if _, err := mcp.BuildConfigFile(servers); err != nil {
		return codexMCPApplicationError("requested MCP server structure is invalid")
	}
	return nil
}

func sortedStringKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func tomlBasicString(value string) string {
	body, _ := json.Marshal(value)
	return string(body)
}

func tomlStringArray(values []string) string {
	var body strings.Builder
	body.WriteByte('[')
	for i, value := range values {
		if i > 0 {
			body.WriteByte(',')
		}
		body.WriteString(tomlBasicString(value))
	}
	body.WriteByte(']')
	return body.String()
}

func persistInteractiveMCPApplicationDenial(spec agent.Spec, applicationErr error) error {
	receipt := agent.ToolLifecycleReceipt{
		ContractVersion: agent.ToolLifecycleContractVersion,
		ProfileID:       "codex/interactive/tool-lifecycle-v1",
		Decision:        "denied",
		EvidenceTier:    "unit_verified",
		Entries: []agent.ToolLifecycleEntry{{
			ID:         "mcp-servers",
			Channel:    agent.ToolChannelMCPServer,
			Required:   true,
			Outcome:    agent.ToolOutcomeDenied,
			DenialCode: agent.ToolDenialApplicationFailed,
		}},
	}
	if spec.OnToolLifecycleAdapted != nil {
		if err := spec.OnToolLifecycleAdapted(receipt); err != nil {
			return codexMCPApplicationError("persist denied Codex CLI MCP receipt: " + err.Error())
		}
	}
	return applicationErr
}
