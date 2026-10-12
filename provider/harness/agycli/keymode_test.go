package agycli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// The keyed-spawn contract (keymode.go), pinned end to end through the fake
// `agy` wrapper:
//
//   - a non-empty Spec.Env[GEMINI_API_KEY] selects the keyed path: the child
//     sees GEMINI_API_KEY in its environment (TestKeyedSpawn_InjectsKeyIntoChildEnv),
//     runs under a per-session HOME holding ONLY the API-key-mode
//     settings.json (TestKeyedSpawn_IsolatedHomeHoldsKeyedSettings), and the
//     operator's real state home is never written
//     (TestKeyedSpawn_LeavesOperatorStateHomeAlone);
//   - the isolated HOME is removed when the session ends
//     (TestKeyedSpawn_CleansUpIsolatedHome);
//   - without the key the child runs the host-session path: ambient HOME,
//     no injected key (TestHostSpawn_KeepsAmbientHomeAndNoKey);
//   - the key never rides argv on either path
//     (TestKeyedSpawn_KeyNeverRidesArgv);
//   - the injected-env-keys declaration covers the live keyed composition
//     (TestKeyEnvNames_CoversKeyedComposition).
//
// RED proofs: blank the settings write in prepareKeyedHome and the
// settings-content test fails; drop GEMINI_API_KEY from overlayKeyedEnv and
// the child-env test fails; point the keyed child at the ambient HOME and
// the isolation test fails; skip keyedHomeCleanup in readLoop and the
// cleanup test fails; interpolate the key into buildArgs and the argv test
// fails; add a name to keyedHomeEnv without transcribing it in keyEnvNames
// and the declaration test fails.

func keyedTestProvider(t *testing.T, script string, opts Options) *Provider {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	opts.DisableTranscriptEnrichment = true
	return newFakeProvider(t, script, opts)
}

func drainKeyedResult(t *testing.T, h agent.Handle) agent.ResultEvent {
	t.Helper()
	defer func() { _ = h.Stop(context.Background()) }()
	for _, ev := range collectEvents(t, h) {
		if res, ok := ev.(agent.ResultEvent); ok {
			return res
		}
	}
	t.Fatal("no terminal ResultEvent")
	return agent.ResultEvent{}
}

func TestKeyedSpawn_InjectsKeyIntoChildEnv(t *testing.T) {
	t.Parallel()

	const sessionKey = "sess-key-injects-into-child-env"
	envPath := filepath.Join(t.TempDir(), "env.txt")
	p := keyedTestProvider(t, `env > "$ENV_CAPTURE"
echo '<<<DONMAI_RESULT>>>'
echo '{"status":"passed","summary":"done"}'
echo '<<<END_DONMAI_RESULT>>>'
`, Options{})

	h, err := spawnFake(context.Background(), t, p, agent.Spec{
		Prompt: "keyed task",
		Cwd:    t.TempDir(),
		Env: map[string]string{
			"ENV_CAPTURE":    envPath,
			EnvGeminiAPIKey:  sessionKey,
			"UNRELATED_SAFE": "kept",
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if res := drainKeyedResult(t, h); !res.Success {
		t.Fatalf("keyed run failed: %+v", res)
	}
	raw, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read child env capture: %v", err)
	}
	child := "\n" + string(raw) + "\n"
	if !strings.Contains(child, "\n"+EnvGeminiAPIKey+"="+sessionKey+"\n") {
		t.Fatalf("child env lacks %s=<session key>:\n%s", EnvGeminiAPIKey, raw)
	}
	if !strings.Contains(child, "\nUNRELATED_SAFE=kept\n") {
		t.Fatalf("child env dropped unrelated Spec.Env entry:\n%s", raw)
	}
}

func TestKeyedSpawn_IsolatedHomeHoldsKeyedSettings(t *testing.T) {
	t.Parallel()

	const sessionKey = "sess-key-isolated-home-settings"
	homePath := filepath.Join(t.TempDir(), "home.txt")
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	treePath := filepath.Join(t.TempDir(), "tree.txt")
	p := keyedTestProvider(t, `printf '%s' "$HOME" > "$HOME_CAPTURE"
cat "$HOME/.gemini/antigravity-cli/settings.json" > "$SETTINGS_CAPTURE"
ls -A "$HOME" > "$TREE_CAPTURE"
echo '<<<DONMAI_RESULT>>>'
echo '{"status":"passed","summary":"done"}'
echo '<<<END_DONMAI_RESULT>>>'
`, Options{})

	h, err := spawnFake(context.Background(), t, p, agent.Spec{
		Prompt: "keyed task",
		Cwd:    t.TempDir(),
		Env: map[string]string{
			"HOME_CAPTURE":     homePath,
			"SETTINGS_CAPTURE": settingsPath,
			"TREE_CAPTURE":     treePath,
			EnvGeminiAPIKey:    sessionKey,
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if res := drainKeyedResult(t, h); !res.Success {
		t.Fatalf("keyed run failed: %+v", res)
	}
	// The captures were written DURING the run (the isolated tree is
	// removed at session end — see the cleanup test).
	raw, err := os.ReadFile(homePath)
	if err != nil {
		t.Fatalf("read HOME capture: %v", err)
	}
	home := string(raw)
	if home == "" {
		t.Fatal("child HOME is empty")
	}
	if realHome, err := os.UserHomeDir(); err == nil && home == realHome {
		t.Fatalf("keyed child HOME = operator home %q (must be isolated)", home)
	}
	// The isolated HOME holds ONLY the API-key-mode settings.json.
	settingsRaw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read isolated settings.json capture: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(settingsRaw, &settings); err != nil {
		t.Fatalf("isolated settings.json does not parse: %v", err)
	}
	if settings["modelProvider"] != "gemini" {
		t.Fatalf("isolated settings.json = %s, want modelProvider gemini", settingsRaw)
	}
	treeRaw, err := os.ReadFile(treePath)
	if err != nil {
		t.Fatalf("read isolated HOME listing: %v", err)
	}
	if strings.TrimSpace(string(treeRaw)) != ".gemini" {
		t.Fatalf("isolated HOME holds %q, want only [.gemini]", treeRaw)
	}
}

func TestKeyedSpawn_LeavesOperatorStateHomeAlone(t *testing.T) {
	t.Parallel()

	// Point Options.StateHome at a stand-in "operator home" and prove the
	// keyed spawn writes nothing there: its settings file, brain dir, and
	// trust seed all live under the isolated HOME instead.
	operatorHome := t.TempDir()
	const sessionKey = "sess-key-leaves-operator-alone"
	p := keyedTestProvider(t, `echo '<<<DONMAI_RESULT>>>'
echo '{"status":"passed","summary":"done"}'
echo '<<<END_DONMAI_RESULT>>>'
`, Options{StateHome: operatorHome})

	cwd := t.TempDir()
	h, err := spawnFake(context.Background(), t, p, agent.Spec{
		Prompt: "keyed task",
		Cwd:    cwd,
		Env:    map[string]string{EnvGeminiAPIKey: sessionKey},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if res := drainKeyedResult(t, h); !res.Success {
		t.Fatalf("keyed run failed: %+v", res)
	}
	entries, err := os.ReadDir(operatorHome)
	if err != nil {
		t.Fatalf("read operator-home stand-in: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("keyed spawn wrote to the operator state home: %v", names)
	}
}

func TestKeyedSpawn_CleansUpIsolatedHome(t *testing.T) {
	t.Parallel()

	const sessionKey = "sess-key-cleanup"
	homePath := filepath.Join(t.TempDir(), "home.txt")
	p := keyedTestProvider(t, `printf '%s' "$HOME" > "$HOME_CAPTURE"
echo '<<<DONMAI_RESULT>>>'
echo '{"status":"passed","summary":"done"}'
echo '<<<END_DONMAI_RESULT>>>'
`, Options{})

	h, err := spawnFake(context.Background(), t, p, agent.Spec{
		Prompt: "keyed task",
		Cwd:    t.TempDir(),
		Env: map[string]string{
			"HOME_CAPTURE":  homePath,
			EnvGeminiAPIKey: sessionKey,
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if res := drainKeyedResult(t, h); !res.Success {
		t.Fatalf("keyed run failed: %+v", res)
	}
	raw, err := os.ReadFile(homePath)
	if err != nil {
		t.Fatalf("read HOME capture: %v", err)
	}
	//nolint:gosec // G703: path comes from the test's own capture file, not external input.
	if _, err := os.Stat(string(raw)); !os.IsNotExist(err) {
		t.Fatalf("isolated HOME %q still exists after session end (err=%v)", string(raw), err)
	}
}

func TestKeyedSpawn_KeyNeverRidesArgv(t *testing.T) {
	t.Parallel()

	const sessionKey = "sess-key-never-rides-argv"
	argvPath := filepath.Join(t.TempDir(), "argv.txt")
	p := keyedTestProvider(t, `printf '%s\n' "$@" > "$ARGV_CAPTURE"
echo '<<<DONMAI_RESULT>>>'
echo '{"status":"passed","summary":"done"}'
echo '<<<END_DONMAI_RESULT>>>'
`, Options{})

	h, err := spawnFake(context.Background(), t, p, agent.Spec{
		Prompt: "keyed task",
		Cwd:    t.TempDir(),
		Env: map[string]string{
			"ARGV_CAPTURE":  argvPath,
			EnvGeminiAPIKey: sessionKey,
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if res := drainKeyedResult(t, h); !res.Success {
		t.Fatalf("keyed run failed: %+v", res)
	}
	raw, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("read argv capture: %v", err)
	}
	if strings.Contains(string(raw), sessionKey) {
		t.Fatalf("keyed argv carries the session key: %q", raw)
	}
}

func TestHostSpawn_KeepsAmbientHomeAndNoKey(t *testing.T) {
	// Not parallel: mutates process env.

	// Without GEMINI_API_KEY the child runs the host-session path: the
	// ambient HOME is inherited untouched and no key is injected, even
	// when the parent process carries one (the blocklist strips it; only
	// the trusted Spec.Env layer selects the keyed path).
	t.Setenv(EnvGeminiAPIKey, "parent-key-must-not-leak")
	homePath := filepath.Join(t.TempDir(), "home.txt")
	envPath := filepath.Join(t.TempDir(), "env.txt")
	p := keyedTestProvider(t, `printf '%s' "$HOME" > "$HOME_CAPTURE"
env > "$ENV_CAPTURE"
echo '<<<DONMAI_RESULT>>>'
echo '{"status":"passed","summary":"done"}'
echo '<<<END_DONMAI_RESULT>>>'
`, Options{})

	h, err := spawnFake(context.Background(), t, p, agent.Spec{
		Prompt: "host task",
		Cwd:    t.TempDir(),
		Env: map[string]string{
			"HOME_CAPTURE": homePath,
			"ENV_CAPTURE":  envPath,
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if res := drainKeyedResult(t, h); !res.Success {
		t.Fatalf("host run failed: %+v", res)
	}
	home, err := os.ReadFile(homePath)
	if err != nil {
		t.Fatalf("read HOME capture: %v", err)
	}
	if got, want := string(home), os.Getenv("HOME"); got != want {
		t.Fatalf("host child HOME = %q, want ambient %q", got, want)
	}
	raw, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read child env capture: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, EnvGeminiAPIKey+"=") {
			t.Fatalf("host child env carries %s (parent leak or injection): %q", EnvGeminiAPIKey, line)
		}
	}
}

func TestKeyedSpawn_ForwardsCompatibleEndpoint(t *testing.T) {
	t.Parallel()

	const sessionKey = "sess-key-endpoint"
	const baseURL = "https://compatible.example/v1"
	envPath := filepath.Join(t.TempDir(), "env.txt")
	p := keyedTestProvider(t, `env > "$ENV_CAPTURE"
echo '<<<DONMAI_RESULT>>>'
echo '{"status":"passed","summary":"done"}'
echo '<<<END_DONMAI_RESULT>>>'
`, Options{})

	h, err := spawnFake(context.Background(), t, p, agent.Spec{
		Prompt: "keyed task",
		Cwd:    t.TempDir(),
		Env: map[string]string{
			"ENV_CAPTURE":          envPath,
			EnvGeminiAPIKey:        sessionKey,
			EnvGoogleGeminiBaseURL: baseURL,
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if res := drainKeyedResult(t, h); !res.Success {
		t.Fatalf("keyed run failed: %+v", res)
	}
	raw, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read child env capture: %v", err)
	}
	child := "\n" + string(raw) + "\n"
	if !strings.Contains(child, "\n"+EnvGoogleGeminiBaseURL+"="+baseURL+"\n") {
		t.Fatalf("child env lacks %s=%s:\n%s", EnvGoogleGeminiBaseURL, baseURL, raw)
	}
}

func TestKeyEnvNames_CoversKeyedComposition(t *testing.T) {
	t.Parallel()

	// The injected-env-keys declaration (keyEnvNames) must cover every
	// name the keyed spawn sets on the child beyond the inherited +
	// Spec.Env merge: the session key, the optional endpoint override,
	// and the whole HOME/XDG redirect layer. A new keyed name updates
	// this declaration in the same change instead of riding undeclared.
	home := t.TempDir()
	composed := []string{"PATH=/usr/bin", "HOME=/home/u"}
	keyed := overlayKeyedEnv(composed, home, "k", "https://compatible.example/v1")
	declared := make(map[string]struct{}, len(keyEnvNames))
	for _, n := range keyEnvNames {
		declared[n] = struct{}{}
	}
	composedNames := make(map[string]struct{}, len(composed))
	for _, e := range composed {
		if i := strings.IndexByte(e, '='); i >= 0 {
			composedNames[e[:i]] = struct{}{}
		}
	}
	var uncovered []string
	for _, e := range keyed {
		i := strings.IndexByte(e, '=')
		if i < 0 {
			continue
		}
		name := e[:i]
		if _, wasComposed := composedNames[name]; wasComposed {
			continue // overlaid value for an already-composed name (HOME)
		}
		if _, ok := declared[name]; !ok {
			uncovered = append(uncovered, name)
		}
	}
	if len(uncovered) > 0 {
		t.Fatalf("keyed child-env names outside the injected-env-keys declaration:\n%s\ntranscribe them in keyEnvNames (keymode.go)", strings.Join(uncovered, "\n"))
	}
	// And the declaration holds no dead names: every declared name must
	// actually be set by the keyed composition — except the optional
	// endpoint override, which is set only when the cell binds one.
	for _, withBase := range []bool{false, true} {
		base := ""
		if withBase {
			base = "https://compatible.example/v1"
		}
		got := overlayKeyedEnv(composed, home, "k", base)
		set := make(map[string]struct{}, len(got))
		for _, e := range got {
			if i := strings.IndexByte(e, '='); i >= 0 {
				set[e[:i]] = struct{}{}
			}
		}
		for _, n := range keyEnvNames {
			if n == EnvGoogleGeminiBaseURL && !withBase {
				continue // optional: set only when the cell binds one
			}
			if _, ok := set[n]; !ok {
				t.Fatalf("declared keyed name %q not set by the keyed composition (withBase=%v)", n, withBase)
			}
		}
	}
}

func TestPrepareKeyedHome_SettingsContent(t *testing.T) {
	t.Parallel()

	home, err := prepareKeyedHome()
	if err != nil {
		t.Fatalf("prepareKeyedHome: %v", err)
	}
	defer func() { _ = keyedHomeCleanup(home) }()
	raw, err := os.ReadFile(filepath.Join(home, keyedSettingsRelPath))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("settings do not parse: %v", err)
	}
	if len(settings) != 1 || settings["modelProvider"] != "gemini" {
		t.Fatalf("settings = %s, want exactly {\"modelProvider\":\"gemini\"}", raw)
	}
}

func TestKeyedSessionKey_Selection(t *testing.T) {
	t.Parallel()

	if _, ok := keyedSessionKey(nil); ok {
		t.Error("nil env selects keyed (want host-session)")
	}
	if _, ok := keyedSessionKey(map[string]string{}); ok {
		t.Error("empty env selects keyed (want host-session)")
	}
	if _, ok := keyedSessionKey(map[string]string{EnvGeminiAPIKey: ""}); ok {
		t.Error("empty key selects keyed (want host-session)")
	}
	if key, ok := keyedSessionKey(map[string]string{EnvGeminiAPIKey: "k"}); !ok || key != "k" {
		t.Errorf("keyed selection = %q, %v; want %q, true", key, ok, "k")
	}
}

func TestKeyedSpawn_RejectsMissingWorktree(t *testing.T) {
	t.Parallel()

	p := keyedTestProvider(t, "exit 0\n", Options{})
	_, err := spawnFake(context.Background(), t, p, agent.Spec{
		Prompt: "x",
		Cwd:    filepath.Join(t.TempDir(), "missing"),
		Env:    map[string]string{EnvGeminiAPIKey: "k"},
	})
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("Spawn error = %v, want ErrSpawnFailed", err)
	}
}
