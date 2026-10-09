package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestTrustBoundaryDirs_OnlyTheAuthorizedBoundaryIsSeeded proves the trust
// seed covers exactly the platform-authorized boundary: the session cwd plus
// its declared mutable repository paths, each with its resolved twin — and
// nothing else. An unrelated path, a parent, or a sibling must never gain a
// trust record from this session.
func TestTrustBoundaryDirs_OnlyTheAuthorizedBoundaryIsSeeded(t *testing.T) {
	t.Parallel()
	workdir := t.TempDir()
	sibling := t.TempDir()
	mutable := t.TempDir()

	dirs := trustBoundaryDirs(agent.Spec{
		Cwd: workdir,
		RepositoryAuthority: &agent.RepositoryAuthorityPolicy{
			Protocol:     "session-root-v1",
			MutablePaths: []string{mutable, "", workdir},
		},
	})
	if len(dirs) == 0 {
		t.Fatal("trustBoundaryDirs returned nothing for a spec with a cwd")
	}
	for _, d := range dirs {
		if !filepath.IsAbs(d) {
			t.Errorf("trust dir %q is not absolute", d)
		}
		if filepath.Clean(d) != d {
			t.Errorf("trust dir %q is not clean", d)
		}
	}
	// The sibling — authorized by nobody — must not appear, in any spelling.
	resolvedSibling, _ := filepath.EvalSymlinks(sibling)
	for _, d := range dirs {
		if d == sibling || (resolvedSibling != "" && d == resolvedSibling) {
			t.Errorf("unauthorized sibling %q is in the trust boundary: %q", sibling, dirs)
		}
	}
	// The cwd and the mutable path must appear (unresolved spelling at
	// minimum; the resolved twin appears when resolution succeeds).
	cleanWorkdir := filepath.Clean(workdir)
	if !slices.Contains(dirs, cleanWorkdir) {
		t.Errorf("session cwd %q missing from the trust boundary: %q", cleanWorkdir, dirs)
	}
	cleanMutable := filepath.Clean(mutable)
	if !slices.Contains(dirs, cleanMutable) {
		t.Errorf("declared mutable path %q missing from the trust boundary: %q", cleanMutable, dirs)
	}
	// No duplicates.
	seen := map[string]int{}
	for _, d := range dirs {
		seen[d]++
		if seen[d] > 1 {
			t.Errorf("duplicate trust dir %q: %q", d, dirs)
		}
	}
}

// TestTrustBoundaryDirs_EmptySpecSeedsNothing proves a spec with no cwd and
// no authority seeds nothing: there is no boundary to authorize.
func TestTrustBoundaryDirs_EmptySpecSeedsNothing(t *testing.T) {
	t.Parallel()
	if dirs := trustBoundaryDirs(agent.Spec{}); len(dirs) != 0 {
		t.Errorf("trustBoundaryDirs(empty) = %q, want nothing", dirs)
	}
	if dirs := trustBoundaryDirs(agent.Spec{
		RepositoryAuthority: &agent.RepositoryAuthorityPolicy{MutablePaths: []string{"", "  "}},
	}); len(dirs) != 0 {
		t.Errorf("trustBoundaryDirs(blank paths) = %q, want nothing", dirs)
	}
}

// TestSeedTrustFile_AddOnlyPreservesUnknownFields proves the seed is
// add-only: existing trust entries survive, unknown record fields survive,
// and the new directory is recorded — all without touching anything else.
func TestSeedTrustFile_AddOnlyPreservesUnknownFields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	existing := "/already/trusted"
	before := map[string]any{
		"operatorField": "untouched",
		"projects": map[string]any{
			existing: map[string]any{"hasTrustDialogAccepted": true, "extra": "kept"},
		},
	}
	raw, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	seedTrustFile(path, []string{"/session/worktree"})
	afterRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]any
	if err := json.Unmarshal(afterRaw, &after); err != nil {
		t.Fatalf("seed wrote unparseable JSON: %v", err)
	}
	if after["operatorField"] != "untouched" {
		t.Errorf("unknown record field was disturbed: %v", after)
	}
	projects, _ := after["projects"].(map[string]any)
	if projects == nil {
		t.Fatalf("projects missing after seed: %v", after)
	}
	kept, _ := projects[existing].(map[string]any)
	if kept["hasTrustDialogAccepted"] != true || kept["extra"] != "kept" {
		t.Errorf("existing trust entry was disturbed: %v", kept)
	}
	added, _ := projects["/session/worktree"].(map[string]any)
	if added["hasTrustDialogAccepted"] != true {
		t.Errorf("new trust entry missing: %v", projects)
	}
}

// TestSeedTrustFile_ToleratesMalformedContent proves a malformed record
// starts fresh instead of failing: the session still gets its seed.
func TestSeedTrustFile_ToleratesMalformedContent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedTrustFile(path, []string{"/session/worktree"})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]any
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatalf("seed did not repair the malformed record: %v", err)
	}
	projects, _ := after["projects"].(map[string]any)
	added, _ := projects["/session/worktree"].(map[string]any)
	if added["hasTrustDialogAccepted"] != true {
		t.Errorf("seed missing after malformed repair: %v", after)
	}
}

// TestSeedTrustFile_ConcurrentSeedsKeepEveryEntry drives the PRODUCTION
// merge under fleet-daemon concurrency: N sessions sharing one config home
// each seed one distinct directory at once, and all N entries — plus a
// pre-existing one — must survive. A read-modify-write merge with no lock
// parks the loser on the workspace-trust modal, the exact defect the seed
// exists to close.
func TestSeedTrustFile_ConcurrentSeedsKeepEveryEntry(t *testing.T) {
	const sessions = 16
	path := filepath.Join(t.TempDir(), ".claude.json")
	if raw, err := json.Marshal(map[string]any{
		"projects": map[string]any{"/pre/existing": map[string]any{claudeTrustAcceptedKey: true}},
	}); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	dirs := make([]string, sessions)
	for i := range dirs {
		dirs[i] = filepath.Join(string(filepath.Separator), "session", "worktree-"+strings.Repeat("x", i+1))
	}
	var wg sync.WaitGroup
	for _, dir := range dirs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seedTrustFile(path, []string{dir})
		}()
	}
	wg.Wait()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]any
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatalf("concurrent seed left an unparseable record: %v", err)
	}
	projects, _ := after["projects"].(map[string]any)
	if projects == nil {
		t.Fatalf("concurrent seed dropped the projects record: %v", after)
	}
	var missing []string
	if _, ok := projects["/pre/existing"]; !ok {
		missing = append(missing, "/pre/existing")
	}
	for _, dir := range dirs {
		entry, _ := projects[dir].(map[string]any)
		if accepted, _ := entry[claudeTrustAcceptedKey].(bool); !accepted {
			missing = append(missing, dir)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("concurrent seed lost %d/%d entries: %q", len(missing), sessions+1, missing)
	}
}

// TestClaudeConfigHome_RejectsARelativeOverride proves a relative
// config-home redirect is not resolved against the worker process cwd: the
// CLI resolves its own config home by its own rules, so seeding the
// worker-relative path would silently seed the wrong home and leave the
// session on the modal.
func TestClaudeConfigHome_RejectsARelativeOverride(t *testing.T) {
	t.Setenv(claudeConfigDirEnv, filepath.Join("relative", "config", "home"))
	if home, err := claudeConfigHome(); err == nil {
		t.Errorf("claudeConfigHome() = %q, want an error for a relative override — seeding it would miss the CLI's home", home)
	}
}

// TestSpawn_Interactive_SeedsWorkspaceTrustFromTheAuthorizedBoundary drives
// the PRODUCTION entry point: an interactive Spawn seeds the trust record
// under an isolated config home for exactly the session's cwd — and for no
// unrelated path. RED proof: remove the seedWorkspaceTrust call from
// spawnInteractive and the record stays absent.
func TestSpawn_Interactive_SeedsWorkspaceTrustFromTheAuthorizedBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	workdir := t.TempDir()
	cfgHome := t.TempDir()
	t.Setenv(claudeConfigDirEnv, cfgHome)

	p := newFakeInteractiveProvider(t, captureEnvArgvScript)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	awaitPTYExit(t, h)

	raw, err := os.ReadFile(filepath.Join(cfgHome, claudeTrustFileName))
	if err != nil {
		t.Fatalf("trust record was not seeded under the isolated config home: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("seeded trust record is unparseable: %v", err)
	}
	projects, _ := record["projects"].(map[string]any)
	if projects == nil {
		t.Fatalf("seeded record carries no projects: %v", record)
	}
	trusted := func(dir string) bool {
		entry, _ := projects[dir].(map[string]any)
		accepted, _ := entry["hasTrustDialogAccepted"].(bool)
		return accepted
	}
	cleanWorkdir := filepath.Clean(workdir)
	if !trusted(cleanWorkdir) {
		t.Errorf("session cwd %q not trusted after interactive Spawn; record: %v", cleanWorkdir, projects)
	}
	if resolved, err := filepath.EvalSymlinks(cleanWorkdir); err == nil && resolved != cleanWorkdir && !trusted(resolved) {
		t.Errorf("resolved cwd %q not trusted after interactive Spawn; record: %v", resolved, projects)
	}
	// The seed must not have trusted anything else: every recorded entry
	// must be the cwd in one of its two spellings.
	for dir := range projects {
		if dir != cleanWorkdir {
			if resolved, err := filepath.EvalSymlinks(cleanWorkdir); err != nil || dir != resolved {
				t.Errorf("seed trusted an unrelated path %q; record: %v", dir, projects)
			}
		}
	}
}

// TestSpawn_Interactive_GatewayBearerRidesTheUnattendedTokenPair drives the
// PRODUCTION entry point for the provider-auth half: an interactive Spawn
// with a translating-gateway binding delivers the session bearer on the
// CLI's unattended token pair (base URL + auth token) while the
// modal-generating key name stays out of the child env entirely — by name
// AND by value. RED proof: revert projectGatewayCredential to merge the
// binding env verbatim and the key name rides the child env.
func TestSpawn_Interactive_GatewayBearerRidesTheUnattendedTokenPair(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	workdir := t.TempDir()
	t.Setenv(claudeConfigDirEnv, t.TempDir())

	const bearer = "sess-bearer-rides-token-pair"
	p := newFakeInteractiveProvider(t, captureEnvArgvScriptWithToken)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyAnthropic,
			Model:   "served-model",
			Host:    agent.HostGateway,
			BaseURL: "http://127.0.0.1:1/v1",
			Env:     map[string]string{gatewayModalKeyName: bearer},
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	awaitPTYExit(t, h)

	env := readCapturedFile(t, workdir, "env.txt")
	for _, want := range []string{
		"ANTHROPIC_BASE_URL=http://127.0.0.1:1/v1",
		"ANTHROPIC_AUTH_TOKEN=" + bearer,
	} {
		if !strings.Contains(env, want) {
			t.Errorf("captured PTY-child env missing %q; got:\n%s", want, env)
		}
	}
	for _, line := range strings.Split(env, "\n") {
		if value, ok := strings.CutPrefix(line, "ANTHROPIC_API_KEY="); ok && value != "" {
			t.Errorf("modal-generating key name rides the interactive child env with a value: %q", line)
		}
		if strings.Contains(line, bearer) && !strings.HasPrefix(line, "ANTHROPIC_AUTH_TOKEN=") {
			t.Errorf("session bearer rides the child env outside the token pair: %q", line)
		}
	}
	argv := argvLines(readCapturedFile(t, workdir, "argv.txt"))
	if got, ok := flagValue(argv, "--model"); !ok || got != "served-model" {
		t.Errorf("--model = %q (present=%v), want the binding model; argv: %q", got, ok, argv)
	}
}

// captureEnvArgvScriptWithToken extends the shared capture script with the
// unattended token pair, so the gateway test can assert the bearer lands on
// the token name and nowhere else.
const captureEnvArgvScriptWithToken = `
printf '%s\n' "$@" > "$PWD/argv.txt"
{
  printf 'ANTHROPIC_BASE_URL=%s\n' "$ANTHROPIC_BASE_URL"
  printf 'ANTHROPIC_AUTH_TOKEN=%s\n' "$ANTHROPIC_AUTH_TOKEN"
  printf 'ANTHROPIC_API_KEY=%s\n' "$ANTHROPIC_API_KEY"
  printf 'CLAUDE_CODE_USE_BEDROCK=%s\n' "$CLAUDE_CODE_USE_BEDROCK"
  printf 'AWS_REGION=%s\n' "$AWS_REGION"
  printf 'CLAUDE_CODE_USE_VERTEX=%s\n' "$CLAUDE_CODE_USE_VERTEX"
  printf 'CLOUD_ML_REGION=%s\n' "$CLOUD_ML_REGION"
} > "$PWD/env.txt"
`

// TestApplyEndpoint_GatewayHostProjectsTokenPairNotKeyName pins the
// projection at the helper level across every bearer spelling a binding may
// carry: the bearer always lands on the unattended token name, the base URL
// always lands beside it, the model always wins, and the modal-generating
// key name never survives — wherever the bearer arrived from.
func TestApplyEndpoint_GatewayHostProjectsTokenPairNotKeyName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		bindingEnv map[string]string
		specEnv    map[string]string
		wantKey    string
	}{
		{name: "token name on the binding", bindingEnv: map[string]string{EnvAuthToken: "tok"}, wantKey: "tok"},
		{name: "key name on the binding is re-homed", bindingEnv: map[string]string{gatewayModalKeyName: "re-homed"}, wantKey: "re-homed"},
		{name: "binding token beats session key", bindingEnv: map[string]string{EnvAuthToken: "binding"}, specEnv: map[string]string{gatewayModalKeyName: "session"}, wantKey: "binding"},
		{name: "session token fills a keyless binding", specEnv: map[string]string{EnvAuthToken: "session"}, wantKey: "session"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := applyEndpoint(agent.Spec{
				Model: "spec-model",
				Env:   tc.specEnv,
				Endpoint: &agent.EndpointBinding{
					Company: agent.CompanyAnthropic,
					Model:   "served-model",
					Host:    agent.HostGateway,
					BaseURL: "http://127.0.0.1:1/v1",
					Env:     tc.bindingEnv,
				},
			})
			if err != nil {
				t.Fatalf("applyEndpoint: %v", err)
			}
			if got.Model != "served-model" {
				t.Errorf("Model = %q, want the binding model", got.Model)
			}
			if got.Env[EnvBaseURL] != "http://127.0.0.1:1/v1" {
				t.Errorf("base URL = %q, want the gateway surface", got.Env[EnvBaseURL])
			}
			if got.Env[EnvAuthToken] != tc.wantKey {
				t.Errorf("token = %q, want %q (env: %v)", got.Env[EnvAuthToken], tc.wantKey, got.Env)
			}
			if _, present := got.Env[gatewayModalKeyName]; present {
				t.Errorf("modal-generating key name survives the projection: %v", got.Env)
			}
		})
	}
}

// TestPickGatewayBearer_IgnoresUnrelatedCredentialNames pins the bearer
// allowlist: only the two bearer spellings are read, so an unrelated
// credential on the layer can never be presented as the session bearer.
// A generic "first non-empty value" fallback would pass every other
// gateway test and silently mis-route the session onto the wrong key.
func TestPickGatewayBearer_IgnoresUnrelatedCredentialNames(t *testing.T) {
	t.Parallel()
	if got := pickGatewayBearer(map[string]string{"SOME_OTHER_KEY": "unrelated"}); got != "" {
		t.Errorf("pickGatewayBearer(unrelated-only) = %q, want empty — unrelated keys must never read as the bearer", got)
	}
	if got := pickGatewayBearer(map[string]string{
		"SOME_OTHER_KEY":    "unrelated",
		gatewayModalKeyName: "re-homed",
	}); got != "re-homed" {
		t.Errorf("pickGatewayBearer(unrelated + key name) = %q, want the bearer spelling to win", got)
	}
}

// TestApplyEndpoint_GatewayHostWithOnlyUnrelatedKeysFailsLoudly proves a
// gateway binding carrying credentials under no bearer spelling fails the
// spawn like the bearer-less case — never by projecting an unrelated key
// onto the token pair.
func TestApplyEndpoint_GatewayHostWithOnlyUnrelatedKeysFailsLoudly(t *testing.T) {
	t.Parallel()
	_, err := applyEndpoint(agent.Spec{
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyAnthropic,
			Model:   "served-model",
			Host:    agent.HostGateway,
			BaseURL: "http://127.0.0.1:1/v1",
			Env:     map[string]string{"SOME_OTHER_KEY": "unrelated"},
		},
	})
	if err == nil {
		t.Fatal("applyEndpoint: want error for a gateway binding with no bearer spelling, got nil")
	}
}

// TestApplyEndpoint_GatewayHostWithoutBearerFailsLoudly proves a
// gateway-routed binding with no bearer anywhere fails the spawn instead of
// silently running against the default host — the loud refusal the
// unattended contract requires.
func TestApplyEndpoint_GatewayHostWithoutBearerFailsLoudly(t *testing.T) {
	t.Parallel()
	_, err := applyEndpoint(agent.Spec{
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyAnthropic,
			Model:   "served-model",
			Host:    agent.HostGateway,
			BaseURL: "http://127.0.0.1:1/v1",
		},
	})
	if err == nil {
		t.Fatal("applyEndpoint: want error for a bearer-less gateway binding, got nil")
	}
}

// TestApplyEndpoint_GatewayHostRejectsUnroutableSurface proves a gateway
// binding that fails the dispatch-wire shape check (non-loopback over plain
// http here) is refused before it can reach a child — never silently
// stripped.
func TestApplyEndpoint_GatewayHostRejectsUnroutableSurface(t *testing.T) {
	t.Parallel()
	_, err := applyEndpoint(agent.Spec{
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyAnthropic,
			Model:   "served-model",
			Host:    agent.HostGateway,
			BaseURL: "http://gateway.example/v1",
			Env:     map[string]string{EnvAuthToken: "tok"},
		},
	})
	if err == nil {
		t.Fatal("applyEndpoint: want error for a non-loopback plain-http gateway surface, got nil")
	}
}
