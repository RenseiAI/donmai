package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/RenseiAI/donmai/agent"
)

// launchSeedPrefixFor renders the argv prefix every interactive launch now
// carries: the pre-seeded workspace trust and default hooks posture (trust.go)
// followed by the default approval posture (approvals_seed.go). Tests that pin
// a FULL argv use it so the pin stays about the thing they are testing.
func launchSeedPrefixFor(t *testing.T, cwd string) []string {
	t.Helper()
	args, err := interactiveTrustArgs(cwd, codexHooksOff, os.Getwd)
	if err != nil {
		t.Fatalf("interactiveTrustArgs(%q): %v", cwd, err)
	}
	return append(args, interactiveApprovalArgs(codexApprovalsOff)...)
}

// decodeTrustOverride parses the `projects=…` value out of an argv slice and
// returns the trust level keyed by directory. It decodes real TOML rather than
// string-matching so a change that produces syntactically valid but
// semantically different configuration cannot pass.
func decodeTrustOverride(t *testing.T, argv []string) map[string]string {
	t.Helper()
	idx := -1
	for i, arg := range argv {
		if strings.HasPrefix(arg, "projects=") {
			if idx >= 0 {
				t.Fatalf("argv carries more than one projects override: %q", argv)
			}
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("argv carries no projects override: %q", argv)
	}
	if idx == 0 || argv[idx-1] != "--config" {
		t.Fatalf("projects override is not introduced by --config: %q", argv)
	}
	var decoded struct {
		Projects map[string]struct {
			TrustLevel string `toml:"trust_level"`
		} `toml:"projects"`
	}
	if err := toml.Unmarshal([]byte(argv[idx]), &decoded); err != nil {
		t.Fatalf("projects override is not semantic TOML: %v\n%s", err, argv[idx])
	}
	out := make(map[string]string, len(decoded.Projects))
	for dir, entry := range decoded.Projects {
		out[dir] = entry.TrustLevel
	}
	return out
}

func TestCodexHooksPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "unset defaults to off", value: "", want: codexHooksOff},
		{name: "explicit off", value: "off", want: codexHooksOff},
		{name: "case and space insensitive", value: "  Off  ", want: codexHooksOff},
		{name: "inherit keeps codex behaviour", value: "inherit", want: codexHooksInherit},
		{name: "inherit mixed case", value: "INHERIT", want: codexHooksInherit},
		{name: "typo fails closed rather than guessing", value: "of", wantErr: true},
		{name: "boolean-looking value fails closed", value: "true", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := codexHooksPolicy(func(key string) string {
				if key != codexHooksEnv {
					t.Fatalf("policy read %q, want %q", key, codexHooksEnv)
				}
				return tt.value
			})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("codexHooksPolicy(%q) = %q, want an error", tt.value, got)
				}
				if !strings.Contains(err.Error(), codexHooksEnv) {
					t.Fatalf("error does not name the variable: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("codexHooksPolicy(%q): %v", tt.value, err)
			}
			if got != tt.want {
				t.Fatalf("codexHooksPolicy(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestInteractiveTrustArgs(t *testing.T) {
	t.Parallel()
	const workspace = "/session/workspace"
	tests := []struct {
		name          string
		cwd           string
		hooks         string
		wantTrusted   string
		wantHooksArgs bool
	}{
		{
			name:          "workspace is pre-trusted and hooks default to off",
			cwd:           workspace,
			hooks:         codexHooksOff,
			wantTrusted:   workspace,
			wantHooksArgs: true,
		},
		{
			name:        "inherit leaves codex hook handling alone",
			cwd:         workspace,
			hooks:       codexHooksInherit,
			wantTrusted: workspace,
		},
		{
			name:          "relative workspace is trusted by its absolute form",
			cwd:           ".",
			hooks:         codexHooksOff,
			wantHooksArgs: true,
		},
		{
			name:          "trailing separators and space do not mint a second key",
			cwd:           "  " + workspace + "/  ",
			hooks:         codexHooksOff,
			wantTrusted:   workspace,
			wantHooksArgs: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args, err := interactiveTrustArgs(tt.cwd, tt.hooks, os.Getwd)
			if err != nil {
				t.Fatalf("interactiveTrustArgs(%q, %q): %v", tt.cwd, tt.hooks, err)
			}

			trusted := decodeTrustOverride(t, args)
			want := tt.wantTrusted
			if want == "" {
				abs, absErr := filepath.Abs(tt.cwd)
				if absErr != nil {
					t.Fatalf("filepath.Abs(%q): %v", tt.cwd, absErr)
				}
				want = abs
			}
			level, ok := trusted[want]
			if !ok {
				t.Fatalf("workspace %q is not in the trust override %#v", want, trusted)
			}
			if level != codexTrustLevelTrusted {
				t.Fatalf("trust_level for %q = %q, want %q", want, level, codexTrustLevelTrusted)
			}
			for dir, lvl := range trusted {
				if lvl != codexTrustLevelTrusted {
					t.Fatalf("override grants %q to %q; only %q may be seeded", lvl, dir, codexTrustLevelTrusted)
				}
			}

			hooksOff := slices.Contains(args, "features.hooks=false")
			if hooksOff != tt.wantHooksArgs {
				t.Fatalf("hooks override present = %v, want %v (argv %q)", hooksOff, tt.wantHooksArgs, args)
			}
			// Nothing here may pre-approve a hook: trusting repo-supplied hooks
			// grants command execution outside the sandbox, and no hook in an
			// interactive session is platform-provisioned.
			for _, arg := range args {
				if strings.Contains(arg, "hooks.state") || strings.Contains(arg, "trusted_hash") {
					t.Fatalf("argv marks a hook trusted: %q", arg)
				}
			}
		})
	}
}

func TestInteractiveTrustArgs_SymlinkedWorkspaceTrustsBothPaths(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", link, err)
	}
	if resolved == link {
		t.Skip("link and target resolve identically on this filesystem")
	}

	args, err := interactiveTrustArgs(link, codexHooksOff, os.Getwd)
	if err != nil {
		t.Fatalf("interactiveTrustArgs: %v", err)
	}
	trusted := decodeTrustOverride(t, args)
	// Codex matches a project entry by exact path, so a workspace reached
	// through a symlinked prefix needs BOTH forms or the review appears anyway.
	for _, dir := range []string{link, resolved} {
		if trusted[dir] != codexTrustLevelTrusted {
			t.Fatalf("path %q is not trusted in %#v", dir, trusted)
		}
	}
}

func TestInteractiveTrustArgs_UnresolvableWorkspaceFailsLoudRatherThanHanging(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("no working directory")
	_, err := interactiveTrustArgs("", codexHooksOff, func() (string, error) { return "", sentinel })
	if err == nil {
		t.Fatal("interactiveTrustArgs succeeded with no resolvable workspace; a codex TUI would park on its directory review")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error does not wrap the cause: %v", err)
	}
	if !strings.Contains(err.Error(), "pre-trust") {
		t.Fatalf("error does not name the missing trust: %v", err)
	}
}

func TestBuildInteractiveLaunch_SeedsTrustAheadOfPromptAndOverrides(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	spec := agent.Spec{
		Cwd:                workspace,
		Prompt:             "fix the failing tests",
		SystemPromptAppend: "repo rules",
		MCPServers: []agent.MCPServerConfig{
			{Name: "tools", Type: "stdio", Command: "/tmp/tool"},
		},
	}

	launch, err := buildInteractiveLaunchEnv(spec, func(string) string { return "" })
	if err != nil {
		t.Fatalf("buildInteractiveLaunchEnv: %v", err)
	}

	trusted := decodeTrustOverride(t, launch.argv)
	if trusted[workspace] != codexTrustLevelTrusted {
		t.Fatalf("workspace %q not pre-trusted: %#v", workspace, trusted)
	}
	if !slices.Contains(launch.argv, "features.hooks=false") {
		t.Fatalf("hooks posture missing from argv: %q", launch.argv)
	}
	// The seeded prompt must stay the trailing positional, and every override
	// must precede it — codex reads the first positional as the prompt.
	if got := launch.argv[len(launch.argv)-1]; got != spec.Prompt {
		t.Fatalf("last argv = %q, want the prompt %q", got, spec.Prompt)
	}
	for i, arg := range launch.argv[:len(launch.argv)-1] {
		if arg == spec.Prompt {
			t.Fatalf("prompt also appears at argv[%d]: %q", i, launch.argv)
		}
	}
	// The pre-existing overrides still ride along untouched.
	if !slices.Contains(launch.argv, "--strict-config") {
		t.Fatalf("MCP override lost --strict-config: %q", launch.argv)
	}
	if !slices.ContainsFunc(launch.argv, func(a string) bool { return strings.HasPrefix(a, "developer_instructions=") }) {
		t.Fatalf("developer instructions override lost: %q", launch.argv)
	}

	inherit, err := buildInteractiveLaunchEnv(spec, func(key string) string {
		if key == codexHooksEnv {
			return codexHooksInherit
		}
		return ""
	})
	if err != nil {
		t.Fatalf("buildInteractiveLaunchEnv(inherit): %v", err)
	}
	if slices.Contains(inherit.argv, "features.hooks=false") {
		t.Fatalf("inherit still turned hooks off: %q", inherit.argv)
	}
	if got := decodeTrustOverride(t, inherit.argv); got[workspace] != codexTrustLevelTrusted {
		t.Fatalf("inherit dropped the workspace trust: %#v", got)
	}
}

func TestBuildInteractiveLaunch_PlatformSessionMarksProjectUntrusted(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	launch, err := buildInteractiveLaunchEnv(agent.Spec{
		Cwd: workspace,
		MCPServers: []agent.MCPServerConfig{{
			Name: "donmai-platform", Type: "http", URL: "https://platform.example/api/mcp/session",
		}},
	}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	trust := decodeTrustOverride(t, launch.argv)
	if trust[workspace] != codexTrustLevelUntrusted {
		t.Fatalf("platform workspace trust = %q, want %q", trust[workspace], codexTrustLevelUntrusted)
	}
}

func TestBuildInteractiveLaunch_UnknownHooksPolicyFailsClosed(t *testing.T) {
	t.Parallel()
	_, err := buildInteractiveLaunchEnv(agent.Spec{Cwd: t.TempDir(), Prompt: "hi"}, func(string) string {
		return "yes-please"
	})
	if err == nil {
		t.Fatal("unknown hooks policy was accepted; the session would silently take one of two opposite postures")
	}
	if !strings.Contains(err.Error(), codexHooksEnv) {
		t.Fatalf("error does not name the variable: %v", err)
	}
}

func TestTrustedProjectsOverride_QuotesExoticPaths(t *testing.T) {
	t.Parallel()
	dirs := []string{`/w/quote"dir`, `/w/back\slash`, "/w/雪"}
	override := trustedProjectsOverride(dirs)
	var decoded struct {
		Projects map[string]struct {
			TrustLevel string `toml:"trust_level"`
		} `toml:"projects"`
	}
	if err := toml.Unmarshal([]byte(override), &decoded); err != nil {
		t.Fatalf("override is not semantic TOML: %v\n%s", err, override)
	}
	got := make([]string, 0, len(decoded.Projects))
	for dir := range decoded.Projects {
		got = append(got, dir)
	}
	slices.Sort(got)
	want := slices.Clone(dirs)
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded keys = %#v, want %#v", got, want)
	}
}

// TestGatewayProviderConfigOverride_RestatesFileBlockOnTheCLILayer proves
// the gateway defense-in-depth restatement: a routed cell renders the same
// provider route the private config file carries (provider id, base URL,
// env key, wire API) as process-local --config overrides — and a
// non-routed session renders nothing, keeping its argv byte-identical.
// RED proof: drop the restatement from buildInteractiveLaunchEnv and the
// routed argv carries no provider block.
func TestGatewayProviderConfigOverride_RestatesFileBlockOnTheCLILayer(t *testing.T) {
	t.Parallel()
	routed := gatewayCellSpec("https://gateway.example/codex/v1", "cell-key", "")
	launch, err := buildInteractiveLaunch(routed)
	if err != nil {
		t.Fatalf("buildInteractiveLaunch(routed): %v", err)
	}
	joined := strings.Join(launch.argv, "\n")
	for _, want := range []string{
		`model_provider="donmai-gateway"`,
		`model_providers.donmai-gateway.base_url="https://gateway.example/codex/v1"`,
		`model_providers.donmai-gateway.env_key="AI_GATEWAY_API_KEY"`,
		`model_providers.donmai-gateway.wire_api="responses"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("routed launch argv missing %q; argv: %q", want, launch.argv)
		}
	}
	for _, arg := range launch.argv {
		if strings.Contains(arg, "cell-key") {
			t.Errorf("session key rides argv (visible to every same-user process): %q", arg)
		}
	}

	plain, err := buildInteractiveLaunch(agent.Spec{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("buildInteractiveLaunch(plain): %v", err)
	}
	for _, arg := range plain.argv {
		if strings.Contains(arg, "donmai-gateway") {
			t.Errorf("non-routed launch restates a provider block it has no route for: %q", arg)
		}
	}

	if got := gatewayProviderConfigOverride(""); len(got) != 0 {
		t.Errorf("gatewayProviderConfigOverride(empty) = %q, want nothing", got)
	}
	if got := gatewayProviderConfigOverride("https://gateway.example/codex/v1"); len(got)%2 != 0 {
		t.Errorf("gatewayProviderConfigOverride renders %d args, want --config pairs", len(got))
	} else {
		for i := 0; i < len(got); i += 2 {
			if got[i] != "--config" {
				t.Errorf("gatewayProviderConfigOverride arg %d = %q, want --config", i, got[i])
			}
		}
	}
}

// headlessProjectTrust decodes the private config.toml and returns the
// trust level recorded per project directory. Decoding real TOML also
// proves the file stays well-formed (a repeated table fails to decode).
func headlessProjectTrust(t *testing.T, configPath string) map[string]string {
	t.Helper()
	var config struct {
		Projects map[string]struct {
			TrustLevel string `toml:"trust_level"`
		} `toml:"projects"`
	}
	if _, err := toml.DecodeFile(configPath, &config); err != nil {
		t.Fatalf("decode private config.toml: %v", err)
	}
	out := make(map[string]string, len(config.Projects))
	for dir, project := range config.Projects {
		out[dir] = project.TrustLevel
	}
	return out
}

// TestHeadlessSpawnRecordsWorkspaceUntrusted pins the headless lane's
// explicit untrusted entry for the session working directory, under both
// the given and the symlink-resolved path. With no entry, codex applied a
// repository's .codex/config.toml developer_instructions to the thread.
// A Resume on the same cwd adds nothing twice.
func TestHeadlessSpawnRecordsWorkspaceUntrusted(t *testing.T) {
	t.Parallel()
	p, _ := newSkillProvider(t, nil)
	cwd := t.TempDir()
	h, err := p.Spawn(t.Context(), agent.Spec{Prompt: "go", Cwd: cwd})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	_ = drainEvents(t, h.Events(), 5*time.Second)
	r, err := p.Resume(t.Context(), "thread-S", agent.Spec{Prompt: "again", Cwd: cwd})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	_ = drainEvents(t, r.Events(), 5*time.Second)

	want := map[string]string{cwd: codexTrustLevelUntrusted}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		want[resolved] = codexTrustLevelUntrusted
	}
	if got := headlessProjectTrust(t, p.config.configPath); !reflect.DeepEqual(got, want) {
		t.Fatalf("projects = %v, want %v", got, want)
	}
}

// TestHeadlessSpawnKeepsOperatorProjectTrust leaves project trust alone
// when the operator's app-server argv sets it: that session-flag layer
// outranks the private config.toml.
func TestHeadlessSpawnKeepsOperatorProjectTrust(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"app-server", "-c", `projects={"/srv/work"={trust_level="trusted"}}`},
		{"app-server", `--config=projects."/srv/work".trust_level="trusted"`},
	} {
		fs, stdinW, stdoutR := newFakeServer()
		p, err := New(Options{Args: args, skipProcess: true, stdinOverride: stdinW, stdoutOverride: stdoutR, configTempDir: t.TempDir()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		go fs.run(t, "thread-S")
		h, err := p.Spawn(t.Context(), agent.Spec{Prompt: "go", Cwd: t.TempDir()})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		_ = drainEvents(t, h.Events(), 5*time.Second)
		if got := headlessProjectTrust(t, p.config.configPath); len(got) != 0 {
			t.Fatalf("args %q: projects = %v, want none of the runner's", args, got)
		}
		_ = p.Shutdown(context.Background())
		fs.close()
	}
}

// TestUntrustedProjectsFollowTheGatewayBlock keeps the file well-formed in
// the order a routed spawn writes it: the gateway block's top-level key must
// stay top-level, ahead of the projects tables.
func TestUntrustedProjectsFollowTheGatewayBlock(t *testing.T) {
	t.Parallel()
	b, err := newCodexConfigBoundary(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.remove() })
	if _, err := b.appendGatewayProviderBlock("https://gateway.example/v1", "key", nil); err != nil {
		t.Fatal(err)
	}
	if err := b.appendUntrustedProjects([]string{"/work/a", `/work/"quoted"`}); err != nil {
		t.Fatal(err)
	}
	var config struct {
		ModelProvider string                       `toml:"model_provider"`
		Projects      map[string]map[string]string `toml:"projects"`
	}
	if _, err := toml.DecodeFile(b.configPath, &config); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if config.ModelProvider != codexGatewayProviderID {
		t.Fatalf("model_provider = %q, want the top-level gateway selection", config.ModelProvider)
	}
	want := map[string]map[string]string{
		"/work/a":        {"trust_level": codexTrustLevelUntrusted},
		`/work/"quoted"`: {"trust_level": codexTrustLevelUntrusted},
	}
	if !reflect.DeepEqual(config.Projects, want) {
		t.Fatalf("projects = %v, want %v", config.Projects, want)
	}
}
