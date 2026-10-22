package muse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/testexec"
)

// TestCheckHostSessionLoginFile pins the offline leg of the host login
// check against fixture credential files: a shape-valid login passes; a
// missing file, an empty file, a non-JSON body, a body with no providers,
// and a meta entry with no mechanism all refuse. RED proof: accept an
// empty mechanism and the logged-out refusal vanishes.
func TestCheckHostSessionLoginFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		missing bool
		wantErr bool
	}{
		{name: "shape-valid login passes", body: `{"schema_version":1,"providers":{"meta":{"mechanism":"oauth","storage":"keychain"}}}`, wantErr: false},
		{name: "missing file refuses", missing: true, wantErr: true},
		{name: "empty file refuses", body: "", wantErr: true},
		{name: "non-JSON body refuses", body: "not json", wantErr: true},
		{name: "no providers refuses", body: `{"schema_version":1}`, wantErr: true},
		{name: "meta entry without mechanism refuses", body: `{"schema_version":1,"providers":{"meta":{}}}`, wantErr: true},
		{name: "empty mechanism refuses", body: `{"schema_version":1,"providers":{"meta":{"mechanism":""}}}`, wantErr: true},
		{name: "other provider only refuses", body: `{"schema_version":1,"providers":{"other":{"mechanism":"oauth"}}}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "auth.json")
			if !tt.missing {
				if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := checkHostSessionLoginFileAt(path)
			if tt.wantErr && err == nil {
				t.Error("expected refusal, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("expected pass, got %v", err)
			}
		})
	}
}

// TestResolveMuseAuthPath pins the launcher's credential resolution order:
// explicit path override first, then XDG, then HOME. Empty everywhere
// resolves to empty (itself a refusal downstream).
func TestResolveMuseAuthPath(t *testing.T) {
	t.Parallel()
	lookup := func(vars map[string]string) func(string) string {
		return func(key string) string { return vars[key] }
	}
	tests := []struct {
		name string
		vars map[string]string
		want string
	}{
		{
			name: "explicit override wins",
			vars: map[string]string{museAuthPathEnv: "/custom/auth.json", "XDG_CONFIG_HOME": "/xdg", "HOME": "/home"},
			want: "/custom/auth.json",
		},
		{
			name: "xdg beats home",
			vars: map[string]string{"XDG_CONFIG_HOME": "/xdg", "HOME": "/home"},
			want: filepath.Join("/xdg", "muse", "auth.json"),
		},
		{
			name: "home default",
			vars: map[string]string{"HOME": "/home"},
			want: filepath.Join("/home", ".config", "muse", "auth.json"),
		},
		{
			name: "nothing resolves to empty",
			vars: map[string]string{},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveMuseAuthPath(lookup(tt.vars)); got != tt.want {
				t.Errorf("resolveMuseAuthPath = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestProbeHostSessionLogin pins the native leg's verdict mapping through
// the injected environment: a model-validation failure proves the login,
// a credential-resolution failure refuses, and anything else (including a
// zero exit, which an unresolvable model can never produce) refuses.
func TestProbeHostSessionLogin(t *testing.T) {
	if testing.Short() {
		t.Skip("native probe test shells out to testdata scripts")
	}
	writeProbe := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "fake-muse")
		script := "#!/bin/bash\n" + body + "\n"
		if err := testexec.WriteFile(path, []byte(script), 0o600); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o755); err != nil { //nolint:gosec // test fixture needs exec bit
			t.Fatal(err)
		}
		return path
	}
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name:    "model rejection proves the login",
			body:    `echo "agent loop failed: model failed: model ` + hostLoginProbeModel + ` does not exist or you lack access" >&2; exit 1`,
			wantErr: false,
		},
		{
			name:    "missing credentials refuse",
			body:    `echo "missing meta credentials: run ` + "`muse login`" + `" >&2; exit 1`,
			wantErr: true,
		},
		{
			name:    "zero exit refuses",
			body:    `exit 0`,
			wantErr: true,
		},
		{
			name:    "unexpected failure refuses",
			body:    `echo "boom" >&2; exit 3`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := writeProbe(t, tt.body)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := probeHostSessionLoginWithEnv(ctx, bin, []string{"PATH=" + os.Getenv("PATH"), "NO_COLOR=1"})
			if tt.wantErr && err == nil {
				t.Error("expected refusal, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("expected pass, got %v", err)
			}
		})
	}
}

// TestCheckHostSessionLogin_ExplicitKeySkipsProbe pins that a session
// carrying its own API key satisfies the check without touching the login
// file or spawning a probe: the key is the session's route.
func TestCheckHostSessionLogin_ExplicitKeySkipsProbe(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A relative binary would refuse on its own; with a key it must pass
	// before any binary validation runs.
	if err := checkHostSessionLogin(ctx, "relative-binary", "session-key"); err != nil {
		t.Errorf("explicit session key must satisfy the login check, got %v", err)
	}
}

// TestCheckHostSessionLogin_RefusesWithoutLogin pins the fail-closed
// posture end to end: no key + a credential path pointing at nothing
// refuses, and the error is the login refusal (which spawnInteractive
// wraps with the typed not-authenticated cause).
func TestCheckHostSessionLogin_RefusesWithoutLogin(t *testing.T) {
	t.Setenv(museAuthPathEnv, filepath.Join(t.TempDir(), "absent-auth.json"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := checkHostSessionLogin(ctx, "/bin/false", "")
	if err == nil {
		t.Fatal("expected refusal with no login, got nil")
	}
}

// TestSpawn_Interactive_RefusesWithoutLogin drives the PRODUCTION Spawn
// entry point with an interactive spec and no usable login: the spawn
// must fail with the typed not-authenticated cause, before any PTY child
// starts. RED proof: skip the login check in spawnInteractive and this
// spawn proceeds to PTY bring-up instead of refusing.
func TestSpawn_Interactive_RefusesWithoutLogin(t *testing.T) {
	p := newFakeInteractiveProvider(t, `echo "tui up"`)
	t.Setenv(museAuthPathEnv, filepath.Join(t.TempDir(), "absent-auth.json"))

	_, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:      "hello",
		Cwd:         t.TempDir(),
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err == nil {
		t.Fatal("expected spawn refusal with no login, got nil")
	}
	if !isNotAuthenticatedErr(err) {
		t.Errorf("expected the typed not-authenticated cause, got %v", err)
	}
}

// TestSpawn_Interactive_ParentTerminalDoesNotLeak pins that the launching
// process's TERM/COLORTERM never reach the PTY child unless the Spec sets
// them: the PTY host supplies the interactive defaults. RED proof: compose
// the child env over the raw parent and the parent TERM promotes into an
// explicit override (caught by the registry-driven env/geometry test).
func TestSpawn_Interactive_ParentTerminalDoesNotLeak(t *testing.T) {
	t.Parallel()
	parent := []string{"TERM=dumb", "COLORTERM=", "PATH=/usr/bin:/bin", "HOME=/home/op"}
	got := withoutInteractiveParentTerminal(parent)
	for _, entry := range got {
		if strings.HasPrefix(entry, "TERM=") || strings.HasPrefix(entry, "COLORTERM=") {
			t.Fatalf("parent terminal descriptor leaked: %q (in %q)", entry, got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("dropped non-terminal entries: %q", got)
	}
}

// TestSpawn_Interactive_AdmitsBareSpecThroughTheCompiler pins that a bare
// interactive spec (no tool requirements) passes admission against the new
// interactive profiles: PrepareHarness must return a ready receipt naming
// the interactive prompt profile, not a denial. RED proof: remove the
// interactive profiles from the manifest and admission denies with "no
// profile for requested session mode".
func TestSpawn_Interactive_AdmitsBareSpecThroughTheCompiler(t *testing.T) {
	t.Parallel()
	spec, err := agent.PrepareHarness(agent.Spec{
		Prompt:      "hello",
		Cwd:         "/tmp",
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	}, (&Provider{}).Manifest())
	if err != nil {
		t.Fatalf("bare interactive spec must be admitted, got %v", err)
	}
	if spec.PromptReceipt == nil || spec.PromptReceipt.ProfileID != "muse/interactive/pty-v1" {
		t.Errorf("prompt receipt = %+v, want the interactive PTY profile", spec.PromptReceipt)
	}
	if spec.ToolLifecycleReceipt == nil || spec.ToolLifecycleReceipt.ProfileID != "muse/interactive/tool-lifecycle-v1" {
		t.Errorf("tool receipt = %+v, want the interactive tool-lifecycle profile", spec.ToolLifecycleReceipt)
	}
}

// TestSpawn_Interactive_DeniesStampedToolSpec pins the D6 truth: a spec
// stamping MCP servers (or an allow list, or a permission config) denies
// at admission on the interactive profile — the TUI exposes no channel
// for them, so spawning unenforced would lie. RED proof: declare an MCP
// delivery on the interactive profile and the denial vanishes.
func TestSpawn_Interactive_DeniesStampedToolSpec(t *testing.T) {
	t.Parallel()
	for _, mutate := range []struct {
		name string
		spec agent.Spec
	}{
		{"mcp servers", agent.Spec{MCPServers: []agent.MCPServerConfig{{Name: "s", Type: "http", URL: "https://example.com/mcp"}}}},
		{"allowed tools", agent.Spec{AllowedTools: []string{"Read"}}},
		{"disallowed tools", agent.Spec{DisallowedTools: []string{"Write"}}},
		{"permission config", agent.Spec{PermissionConfig: &agent.PermissionConfig{DefaultDecision: "deny"}}},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			t.Parallel()
			mutate.spec.Prompt = "hello"
			mutate.spec.Cwd = "/tmp"
			mutate.spec.Interactive = &agent.InteractiveSpec{Cols: 80, Rows: 24}
			if _, err := agent.PrepareHarness(mutate.spec, (&Provider{}).Manifest()); err == nil {
				t.Errorf("stamped interactive spec (%s) must deny at admission, got nil", mutate.name)
			}
		})
	}
}

// TestSpawn_Interactive_MissingBinarySurfacesUnavailable pins the
// install-first ordering in spawnInteractive: a session with no usable
// binary refuses with the install cause (ErrProviderUnavailable from
// the version check), NOT the login refusal — missing install and
// missing login stay separate typed causes. RED proof: drop the
// CheckHostSessionLogin call from spawnInteractive and the spawn
// refuses with the login cause instead.
func TestSpawn_Interactive_MissingBinarySurfacesUnavailable(t *testing.T) {
	p := &Provider{museBin: filepath.Join(t.TempDir(), "absent-muse")}
	t.Setenv(museAuthPathEnv, filepath.Join(t.TempDir(), "absent-auth.json"))

	_, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:      "hello",
		Cwd:         t.TempDir(),
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err == nil {
		t.Fatal("expected spawn refusal with no binary, got nil")
	}
	if !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Errorf("expected the install cause (ErrProviderUnavailable), got %v", err)
	}
	if isNotAuthenticatedErr(err) {
		t.Errorf("missing binary must not surface the login cause, got %v", err)
	}
}

func isNotAuthenticatedErr(err error) bool {
	for err != nil {
		if err == agent.ErrNotAuthenticated {
			return true
		}
		if u, ok := err.(interface{ Unwrap() []error }); ok {
			for _, inner := range u.Unwrap() {
				if isNotAuthenticatedErr(inner) {
					return true
				}
			}
			return false
		}
		if u1, ok := err.(interface{ Unwrap() error }); ok {
			err = u1.Unwrap()
			continue
		}
		return strings.Contains(err.Error(), "not authenticated")
	}
	return false
}

// TestProbeHostSessionLogin_RequiresProbeModelID pins the F3 verdict:
// a "does not exist" line that is NOT the probe's own model rejection
// refuses. A credential-store failure ("keychain item does not exist")
// or a rejection naming some other model proves nothing about the
// login, so the check must not count it. RED proof: drop the
// hostLoginProbeModel requirement from the verdict and both cases
// below prove the login instead of refusing.
func TestProbeHostSessionLogin_RequiresProbeModelID(t *testing.T) {
	if testing.Short() {
		t.Skip("native probe test shells out to testdata scripts")
	}
	writeProbe := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "fake-muse")
		script := "#!/bin/bash\n" + body + "\n"
		if err := testexec.WriteFile(path, []byte(script), 0o600); err != nil { //nolint:gosec // test fixture
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o755); err != nil { //nolint:gosec // test fixture needs exec bit
			t.Fatal(err)
		}
		return path
	}
	tests := []struct {
		name string
		body string
	}{
		{
			name: "credential store does-not-exist refuses",
			body: `echo "keychain item does not exist" >&2; exit 1`,
		},
		{
			name: "rejection naming another model refuses",
			body: `echo "agent loop failed: model failed: model some-other-model does not exist or you lack access" >&2; exit 1`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := writeProbe(t, tt.body)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := probeHostSessionLoginWithEnv(ctx, bin, []string{"PATH=" + os.Getenv("PATH"), "NO_COLOR=1"}); err == nil {
				t.Errorf("a does-not-exist line without the probe model id %q must refuse, got nil", hostLoginProbeModel)
			}
		})
	}
}

// TestProbeHostSessionLogin_ModelIDOnOtherLineRefuses pins the F3a
// per-line verdict: when the probe's own model id appears on one line
// (for example a startup banner naming it) and a credential-store
// "does not exist" lands on ANOTHER line, the login is NOT proven.
// The model id and the rejection must share a line — the host rejecting
// the model THIS probe asked for. RED proof: match the model id against
// the whole combined output instead of per line and this output proves
// the login.
func TestProbeHostSessionLogin_ModelIDOnOtherLineRefuses(t *testing.T) {
	if testing.Short() {
		t.Skip("native probe test shells out to testdata scripts")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-muse")
	script := "#!/bin/bash\n" +
		`echo "using model ` + hostLoginProbeModel + `"` + "\n" +
		`echo "keychain item does not exist" >&2; exit 1` + "\n"
	if err := testexec.WriteFile(bin, []byte(script), 0o600); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o755); err != nil { //nolint:gosec // test fixture needs exec bit
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := probeHostSessionLoginWithEnv(ctx, bin, []string{"PATH=" + os.Getenv("PATH"), "NO_COLOR=1"}); err == nil {
		t.Errorf("model id on another line + credential-store failure must refuse, got nil")
	}
}

// TestProbeHostSessionLogin_OversizeOutputRefuses pins the probe's
// output bound (M21): a child that buries the login-proven verdict past
// 16 KiB of junk refuses instead of proving the login — the verdict
// text is present in the child's full output, but the bound truncates
// it away. RED proof: remove the bound and the full output carries the
// marker, so the probe passes instead of refusing.
func TestProbeHostSessionLogin_OversizeOutputRefuses(t *testing.T) {
	if testing.Short() {
		t.Skip("native probe test shells out to testdata scripts")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-muse")
	script := "#!/bin/bash\n" +
		`for ((i=0; i<2048; i++)); do echo "junk-padding-line-$i-padding-padding-padding"; done` + "\n" +
		`echo "agent loop failed: model failed: model ` + hostLoginProbeModel + ` does not exist or you lack access" >&2; exit 1` + "\n"
	if err := testexec.WriteFile(bin, []byte(script), 0o600); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o755); err != nil { //nolint:gosec // test fixture needs exec bit
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := probeHostSessionLoginWithEnv(ctx, bin, []string{"PATH=" + os.Getenv("PATH"), "NO_COLOR=1"}); err == nil {
		t.Error("oversize probe output must refuse (bound truncates the verdict away), got nil")
	}
}

// TestProbeHostSessionLogin_ScrubsAmbientMetaKey pins the M17 guard
// through the production probe entry point: with an ambient META_API_KEY
// in the test process, the fake `exec` dumps the environment it
// actually ran under, and the key must be absent — the probe inherits
// only the config locators. The dump also pins the allowlist shape
// (PATH and NO_COLOR ride along). RED proof: build the probe child env
// from os.Environ() instead of hostSessionProbeEnv() and the ambient
// key reaches the child.
func TestProbeHostSessionLogin_ScrubsAmbientMetaKey(t *testing.T) {
	if testing.Short() {
		t.Skip("native probe test shells out to testdata scripts")
	}
	t.Setenv("META_API_KEY", "ambient-operator-key")
	dir := t.TempDir()
	dump := filepath.Join(dir, "probe.env")
	bin := filepath.Join(dir, "fake-muse")
	script := "#!/bin/bash\n" +
		`env > "` + dump + `"` + "\n" +
		`echo "missing meta credentials" >&2; exit 1` + "\n"
	if err := testexec.WriteFile(bin, []byte(script), 0o600); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o755); err != nil { //nolint:gosec // test fixture needs exec bit
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The probe refuses (missing credentials) — the assertion below is
	// about the environment the child RAN under, not the verdict.
	if err := probeHostSessionLogin(ctx, bin); err == nil {
		t.Fatal("expected the missing-credentials refusal, got nil")
	}
	raw, err := os.ReadFile(dump) //nolint:gosec // test temp path
	if err != nil {
		t.Fatalf("read probe child env dump: %v", err)
	}
	for line := range strings.Lines(string(raw)) {
		if strings.HasPrefix(line, "META_API_KEY=") {
			t.Errorf("ambient META_API_KEY reached the probe child: %q", line)
		}
	}
	if !strings.Contains(string(raw), "PATH=") {
		t.Errorf("probe child env lost PATH (allowlist shape changed):\n%s", raw)
	}
	if !strings.Contains(string(raw), "NO_COLOR=1") {
		t.Errorf("probe child env lost NO_COLOR (allowlist shape changed):\n%s", raw)
	}
}
