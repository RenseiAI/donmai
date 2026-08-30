package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/ptycli"
	"github.com/RenseiAI/donmai/runtime/mcp"
)

// SpawnInteractive opens the codex CLI's own interactive TUI under a PTY via
// ptycli, seeded with spec.Prompt when set. An unnamed session uses bare
// `codex`. A named session uses a bounded per-session app-server:
//
//   - fresh (spec.Interactive.ResumeExisting false — the default; every
//     current producer, custom name or platform-canonical id-shaped name
//     alike): the PTY attaches to the bootstrap server with bare
//     `--remote <socket>` (no resume subcommand) and creates its own
//     thread there; the bootstrap connection observes that thread's
//     thread/started notification and names it post-hoc via
//     thread/name/set, reading back the result before the Handle is
//     returned.
//   - attach-to-existing (spec.Interactive.ResumeExisting true, only ever
//     set by an explicit platform signal): the bootstrap connection first
//     resumes the target by name via thread/resume — proving it exists —
//     then the PTY attaches with `codex resume --remote <socket> <name>`.
//
// The pinned CLI supports the --remote local transport only through Unix
// sockets; named Windows requests fail before any spawn side effect while
// unnamed Windows requests retain the bare-TUI path. See
// interactive_name.go's startNamedInteractiveAppServer for why these are
// two genuinely different native RPC sequences, not one gated by name
// presence alone (that conflation was the root cause of a production
// defect: a thread created before the PTY starts, but never given a turn,
// cannot be reattached by any resume/--remote invocation this CLI
// supports).
//
// It remains independent of the Provider's shared headless app-server state:
// it never touches Provider.client/cmd and resolves the binary itself. The
// optional named-session server is owned and cleaned up with this one PTY
// session, so no headless thread or configuration is shared by accident.
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
// is up (SessionID stays empty on this coarse PTY surface) and a single
// terminal ResultEvent when the CLI process exits.
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
	return spawnInteractivePreparedForGOOS(ctx, opts, spec, runtime.GOOS)
}

func spawnInteractivePreparedForGOOS(ctx context.Context, opts Options, spec agent.Spec, goos string) (agent.Handle, error) {
	if err := validateNamedInteractiveTransport(spec, goos); err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
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
	if !hasPlatformSessionMCPAuthority(spec.MCPServers) {
		if spec.SessionName != "" {
			home, homeErr := ambientCodexHome()
			if homeErr != nil {
				return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, homeErr)
			}
			server, nameErr := startNamedInteractiveAppServer(ctx, bin, opts, spec, launch, launch.env, home)
			if nameErr != nil {
				return nil, fmt.Errorf("%w: name codex interactive session: %w", agent.ErrSpawnFailed, nameErr)
			}
			spec.Env = launch.env
			return spawnNamedInteractivePTY(ctx, bin, opts, spec, launch, server, server.close)
		}
		spec.Env = launch.env
		return ptycli.Spawn(ctx, bin, launch.argv, spec, (&Provider{}).Manifest())
	}
	config, auth, err := newInteractiveCodexConfigBoundary(opts.configTempDir, launch.env)
	if err != nil {
		return nil, fmt.Errorf("%w: isolate codex interactive config: %w", agent.ErrSpawnFailed, err)
	}
	if launch.env == nil {
		launch.env = make(map[string]string)
	}
	// CODEX_HOME is runner-owned for this process. Overwrite both an ambient
	// value and a value supplied through Spec.Env: either one may contain a
	// caller-global MCP server carrying an external requester registration.
	// The selected session's MCP server and bearer remain in the process-local
	// --config override built above, while the private config.toml starts from
	// an explicit empty mcp_servers table.
	launch.env["CODEX_HOME"] = config.home
	spec.Env = launch.env
	authSeeder := opts.interactiveAuthSeeder
	if authSeeder == nil {
		authSeeder = seedInteractiveCodexEnvironmentAuth
	}
	if err := authSeeder(ctx, bin, config.home, auth); err != nil {
		return nil, errors.Join(
			fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err),
			config.remove(),
		)
	}
	// The verified private store is now the sole child authority. Empty values
	// deliberately override inherited parent credentials in both the effective
	// config preflight and the PTY process environment.
	launch.env = clearInteractiveCodexAuthEnvironment(launch.env)
	spec.Env = launch.env
	if err := verifyExclusiveInteractiveMCP(
		ctx,
		opts.interactiveMCPInventoryRunner,
		bin,
		spec,
		launch,
		config.home,
	); err != nil {
		return nil, errors.Join(
			fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err),
			config.remove(),
		)
	}
	if spec.SessionName != "" {
		server, err := startNamedInteractiveAppServer(ctx, bin, opts, spec, launch, launch.env, config.home)
		if err != nil {
			return nil, errors.Join(
				fmt.Errorf("%w: name codex interactive session: %w", agent.ErrSpawnFailed, err),
				config.remove(),
			)
		}
		cleanup := func() error { return errors.Join(server.close(), config.remove()) }
		return spawnNamedInteractivePTY(ctx, bin, opts, spec, launch, server, cleanup)
	}
	return ptycli.SpawnWithCleanup(
		ctx,
		bin,
		launch.argv,
		spec,
		(&Provider{}).Manifest(),
		config.remove,
	)
}

// spawnNamedInteractivePTY points launch's argv at server (remoteInteractiveArgs
// already knows, from whether buildInteractiveLaunchEnv prepended "resume",
// which of the two named shapes this is), spawns the PTY, and — for the
// fresh (non-attach) path only — waits for and names the thread the PTY
// itself creates. cleanup is whatever the caller owns for server (plus any
// isolated-config-boundary removal); it always runs on PTY teardown, exactly
// as before this refactor.
//
// For the attach-to-existing path, startNamedInteractiveAppServer has
// already proven the target exists (via thread/resume) before this function
// is ever reached, so no further post-spawn step is needed here: the argv
// already carries `resume --remote <socket> <name>`, the #480 construction,
// unchanged.
func spawnNamedInteractivePTY(
	ctx context.Context,
	bin string,
	opts Options,
	spec agent.Spec,
	launch interactiveLaunch,
	server *namedInteractiveAppServer,
	cleanup func() error,
) (agent.Handle, error) {
	launch.argv = remoteInteractiveArgs(launch.argv, server.remoteURL)
	handle, err := ptycli.SpawnWithCleanup(ctx, bin, launch.argv, spec, (&Provider{}).Manifest(), cleanup)
	if err != nil {
		return nil, err
	}
	if attachToExistingNamedSession(spec) {
		return handle, nil
	}
	nameTimeout := opts.HandshakeTimeout
	if nameTimeout <= 0 {
		nameTimeout = 30 * time.Second
	}
	if err := finishNamingLiveInteractiveThread(ctx, spec, server, nameTimeout); err != nil {
		_ = handle.Stop(ctx)
		return nil, fmt.Errorf("%w: name codex interactive session: %w", agent.ErrSpawnFailed, err)
	}
	return handle, nil
}

// validateNamedInteractiveTransport keeps the optional naming layer honest on
// platforms where the pinned Codex CLI has no supported local transport for
// sharing one app-server between the name-set RPC and the interactive TUI.
// Unnamed sessions never enter that layer and retain the historical bare-TUI
// path unchanged.
func validateNamedInteractiveTransport(spec agent.Spec, goos string) error {
	if spec.SessionName == "" || goos != "windows" {
		return nil
	}
	return fmt.Errorf(
		"named Codex interactive sessions require Unix-socket app-server attach; Windows supports unnamed interactive sessions only: %w",
		agent.ErrUnsupported,
	)
}

// hasPlatformSessionMCPAuthority identifies the reserved gateway entry the
// runner prepends for a platform-launched session. Standalone interactive use
// retains its historical ambient Codex configuration; only the session shape
// carrying the runner-owned authority needs the exclusive boundary.
func hasPlatformSessionMCPAuthority(servers []agent.MCPServerConfig) bool {
	if len(servers) == 0 {
		return false
	}
	gateway := servers[0]
	return strings.EqualFold(strings.TrimSpace(gateway.Type), "http") &&
		strings.HasSuffix(strings.TrimSpace(gateway.Name), "-platform") &&
		strings.Contains(gateway.URL, "/api/mcp/")
}

// newInteractiveCodexConfigBoundary creates the same exclusive user-config
// boundary used by the headless app-server lane. If the host login is
// file-backed, its auth.json inode is projected into the private home; no
// config.toml, project table, MCP server, or header mapping is copied. Hosts
// authenticated through an environment variable are seeded through Codex's
// own login command into an ephemeral private auth.json. An OS-keyring
// credential cannot be safely projected to a different CODEX_HOME and is
// refused before the PTY starts.
func newInteractiveCodexConfigBoundary(tempDir string, specEnv map[string]string) (*codexConfigBoundary, interactiveCodexAuthProjection, error) {
	auth, err := resolveInteractiveCodexAuth(specEnv)
	if err != nil {
		return nil, interactiveCodexAuthProjection{}, err
	}

	boundaryParent := tempDir
	if auth.kind == interactiveCodexAuthFile && boundaryParent == "" {
		// A hard link cannot cross filesystems. Keeping the private directory
		// beside the host credential makes the projection portable to hosts
		// whose system temp directory is a separate mount.
		boundaryParent = filepath.Dir(auth.hostAuthFile)
	}
	boundary, err := newCodexConfigBoundaryWithAuthMode(boundaryParent, auth.storeMode)
	if err != nil {
		return nil, interactiveCodexAuthProjection{}, err
	}
	if auth.kind == interactiveCodexAuthFile {
		if err := boundary.linkHostSessionAuth(auth.hostAuthFile); err != nil {
			return nil, interactiveCodexAuthProjection{}, errors.Join(err, boundary.remove())
		}
	}
	return boundary, auth, nil
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
// Codex CLI override. For a platform-launched session,
// spawnInteractivePrepared points the child at a private CODEX_HOME whose
// config starts with `mcp_servers = {}`, so the effective MCP set is
// exclusively the servers in this override. The host's persistent user and
// project configuration never participates in that session. Standalone
// interactive use retains its historical ambient configuration.
//
// It also seeds the startup trust codex would otherwise raise a modal review
// for — see trust.go, which owns that decision and the rule for what may be
// pre-answered at all. Without it the TUI parks on "Do you trust the contents
// of this directory?" before reading the seeded prompt, and an unattended
// session never gets past it.
//
// It also projects Spec.Model, when the platform resolved one
// (QueuedWork.ResolvedProfile.Model → Spec.Model via translateSpec), onto the
// same `--config key=value` mechanism this launch already uses for every
// other session-scoped knob (approval_policy, sandbox_mode,
// developer_instructions, mcp_servers, projects.*, features.hooks) —
// `model` is codex's own top-level config.toml key, mirrored here as a
// process-local override rather than a write to the operator's persistent
// config, exactly like every other seed in this file. This closes the same
// class of defect trust.go and approvals_seed.go closed for their own knobs:
// before this mapping existed the interactive TUI silently ran under
// whatever model codex's own config.toml/CLI default resolved to, even when
// the platform had already resolved a specific one for headless dispatch of
// the identical Spec (spec_translation.go's threadStartParams/
// turnStartParams, which always set "model" via resolveModel).
//
// Unlike AllowedTools/PermissionConfig (which get a typed pre-spawn denial
// receipt when a harness structurally cannot deliver them — see
// persistInteractiveMCPApplicationDenial below), Model IS mechanically
// deliverable here: codex has no upfront way to validate a model id, so
// there is nothing to deny in advance. The honest contract is pass-through:
// an id codex rejects surfaces as codex's own nonzero exit, which
// ptycli.buildResult turns into a failed agent.ResultEvent (see
// runner.TestInteractive_ExitDetailIsNotASummary) — never a silent retry
// under a different model and never swallowed as a false "completed".
// Empty Spec.Model emits no override at all, leaving codex on its own
// default — this deliberately does NOT call resolveModel (spec_translation.go),
// which defaults to DefaultCodexModel / CODEX_MODEL(_TIER) env fallbacks for
// the headless JSON-RPC lane; those fallbacks are a separate legacy
// compatibility shim, not part of the platform-selection contract this
// mapping restores.
func buildInteractiveLaunch(spec agent.Spec) (interactiveLaunch, error) {
	return buildInteractiveLaunchEnv(spec, os.Getenv)
}

// buildInteractiveLaunchEnv is buildInteractiveLaunch with the environment
// lookup injected, so the hooks policy can be table-tested without mutating
// process state (t.Setenv forbids the t.Parallel this package uses throughout).
func buildInteractiveLaunchEnv(spec agent.Spec, getenv func(string) string) (interactiveLaunch, error) {
	env := cloneInteractiveEnv(spec.Env)
	var args []string
	hooks, err := codexHooksPolicy(getenv)
	if err != nil {
		return interactiveLaunch{}, err
	}
	if spec.RepositoryAuthority != nil && hooks != codexHooksOff {
		return interactiveLaunch{}, codexMCPApplicationError("repository authority enforcement requires workspace hooks disabled because hooks execute outside the sandbox")
	}
	approvals, err := codexApprovalsPolicy(getenv)
	if err != nil {
		return interactiveLaunch{}, err
	}
	trustLevel := codexTrustLevelTrusted
	if hasPlatformSessionMCPAuthority(spec.MCPServers) {
		trustLevel = codexTrustLevelUntrusted
		// An unattended platform session cannot answer Codex's self-update
		// modal. The fleet upgrades the binary out of band.
		args = append(args, "--config", "check_for_update_on_startup=false")
	}
	trustArgs, err := interactiveTrustArgsWithLevel(spec.Cwd, hooks, trustLevel, os.Getwd)
	if err != nil {
		return interactiveLaunch{}, err
	}
	args = append(args, trustArgs...)
	args = append(args, interactiveApprovalArgs(approvals)...)
	if spec.RepositoryAuthority != nil {
		// Last-writer-wins over an operator's interactive approval seed: the
		// selected repository remains the sole writable root even when the
		// attended posture would otherwise choose danger-full-access.
		args = append(args, "--config", "sandbox_mode="+tomlBasicString("workspace-write"))
		for _, mutablePath := range spec.RepositoryAuthority.MutablePaths {
			if mutablePath != "" && mutablePath != spec.Cwd {
				args = append(args, "--add-dir", mutablePath)
			}
		}
	}
	if model := strings.TrimSpace(spec.Model); model != "" {
		args = append(args, "--config", "model="+tomlBasicString(model))
	}
	if spec.SystemPromptAppend != "" {
		args = append(args, "--config", "developer_instructions="+strconv.Quote(spec.SystemPromptAppend))
	}
	if len(spec.MCPServers) > 0 {
		override, nextEnv, err := codexCLIMCPOverride(spec.MCPServers, env, mcpToolsApprovalMode(approvals))
		if err != nil {
			return interactiveLaunch{}, err
		}
		env = nextEnv
		args = append(args, "--config", override, "--strict-config")
	}
	// Mere SessionName presence is NOT an attach signal: every current
	// producer (custom name or the platform's canonical id-shaped name
	// alike) sets a fresh session's name this way, and #480 unconditionally
	// running `resume <name>` here for a session that had never taken a
	// turn was the production defect this gate closes. resume is invoked
	// only when the caller explicitly signals attach-to-existing (see
	// agent.InteractiveSpec.ResumeExisting and interactive_name.go's
	// startNamedInteractiveAppServer doc comment for why the two shapes are
	// not interchangeable). A fresh named session's thread is instead
	// created by the PTY itself and named post-hoc — see
	// spawnNamedInteractivePTY.
	if spec.SessionName != "" && attachToExistingNamedSession(spec) {
		args = append([]string{"resume"}, args...)
		args = append(args, spec.SessionName)
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

// codexCLIMCPOverride renders the requested MCP servers as one process-local
// `mcp_servers` override. toolsApprovalMode, when non-empty, is written into
// every requested server's table as `default_tools_approval_mode` so the
// session does not stop on codex's one-review-per-tool-name approval; see
// approvals_seed.go. It is scoped to the servers named here, so a server the
// operator's ambient configuration defines keeps whatever posture it had.
func codexCLIMCPOverride(servers []agent.MCPServerConfig, env map[string]string, toolsApprovalMode string) (string, map[string]string, error) {
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
		if toolsApprovalMode != "" {
			body.WriteString("\"default_tools_approval_mode\"=")
			body.WriteString(tomlBasicString(toolsApprovalMode))
			body.WriteByte(',')
		}
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
