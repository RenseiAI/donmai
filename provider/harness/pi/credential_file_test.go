package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// This file is the child-environment regression suite for the session
// credential file: no secret value may reach the pi harness child's
// ENVIRONMENT on any spawn path. pi renames itself to a short process name
// at startup, and a same-user process listing then renders the child's whole
// environment as if it were its command line — so a key composed into the
// child env is readable host-wide. Credentials ride an owner-only file in
// the session state root instead; only the file's PATH rides the env.
//
// The sentinel stands in for every real credential (the injected-provider
// key, the provider-native mirrors, the snapshot keys). Each test drives a
// production spawn entry point and asserts two halves: the sentinel reaches
// the session (the harness still authenticates — via the credential file
// and, on the native route, the session auth.json) and NO child-env entry
// carries it.

// credentialFileSentinel is the canary credential every test in this file
// injects. It must reach the session through FILES and must never appear in
// the child's env — the same rule argvSecretSentinel pins for argv.
const credentialFileSentinel = "sentinel-credential-must-not-ride-child-env"

// TestHeadlessChildEnv_CarriesNoCredentialValue drives a real
// Provider.Spawn against a fake `pi` binary with a sentinel credential on
// Spec.Env and asserts the recorded child env carries no trace of the
// sentinel — while the session credential file beside it carries the full
// value. The spawn is expected to fail (the fake never answers the policy
// handshake); what matters is the child was exec'd first, with the
// production env, which the recorder files prove.
//
// RED proof: compose the unstripped spec into the child env (drop the
// stripSessionCredentialEnv call in composeChildEnv) and the env assertion
// fails with the sentinel quoted in the failure output.
func TestHeadlessChildEnv_CarriesNoCredentialValue(t *testing.T) {
	// Not parallel: spawns a real child process with a fixed recording dir.
	if os.Getenv("SHELL") == "" && os.Getenv("OS") != "" {
		t.Skip("fake pi recorder needs /bin/sh")
	}
	dir := t.TempDir()
	fake := fakeArgvRecorderPi(t, dir)

	p, err := New(Options{
		PiBin:            fake,
		HandshakeTimeout: 30 * time.Second,
		VersionProbe:     func(context.Context, string) (string, error) { return PinnedVersion, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	workdir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	spawnErr := make(chan error, 1)
	go func() {
		_, err := p.Spawn(ctx, agent.Spec{
			Cwd:    workdir,
			Prompt: "prove child env carries no credential",
			Env: map[string]string{
				PiKeyEnvVar:          credentialFileSentinel,
				"AI_GATEWAY_API_KEY": credentialFileSentinel,
				"ANTHROPIC_API_KEY":  credentialFileSentinel,
			},
		})
		spawnErr <- err
	}()

	// The fake records its env as its first act; EITHER recorder file
	// proves the child was exec'd with the production argv+env (the two
	// writes race, so wait for both). Read the credential file BEFORE Stop
	// reaps the session (Stop removes it).
	_ = waitForArgvRecord(t, filepath.Join(dir, "argv"), 20*time.Second)
	envRaw := waitForArgvRecord(t, filepath.Join(dir, "child-env"), 20*time.Second)
	// below triggers Stop through the handshake gate's ctx.Done branch.
	var credPath string
	for _, entry := range strings.Split(envRaw, "\n") {
		if strings.HasPrefix(entry, credentialFileEnvVar+"=") {
			credPath = strings.TrimPrefix(entry, credentialFileEnvVar+"=")
		}
	}
	if credPath == "" {
		t.Fatalf("child env carries no %s pointer; the session has no credential rail:\n%s", credentialFileEnvVar, envRaw)
	}
	got, err := readSessionCredentialFile(credPath)
	if err != nil {
		t.Fatalf("read session credential file: %v", err)
	}
	for _, key := range []string{PiKeyEnvVar, "AI_GATEWAY_API_KEY", "ANTHROPIC_API_KEY"} {
		if got[key] != credentialFileSentinel {
			t.Errorf("credential file %s = %q, want the sentinel", key, got[key])
		}
	}
	if fi, statErr := os.Stat(credPath); statErr != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("credential file mode is not owner-only")
	}

	// The child is still sleeping inside the fake; cancel so Spawn's
	// handshake gate (and its Stop) reaps it before the test ends.
	cancel()
	select {
	case err := <-spawnErr:
		if err == nil {
			t.Fatal("Spawn against a handshake-silent fake unexpectedly succeeded")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Spawn did not return after context cancellation")
	}

	for _, entry := range strings.Split(envRaw, "\n") {
		if entry == "" {
			continue
		}
		if i := strings.IndexByte(entry, '='); i >= 0 && strings.Contains(entry[i+1:], credentialFileSentinel) {
			t.Fatalf("harness child env carries the sentinel credential value: %q", entry)
		}
	}
	// Stop removed the file at session end: no secret lingers on disk.
	if _, statErr := os.Stat(credPath); !os.IsNotExist(statErr) {
		t.Errorf("session credential file still exists after Stop: %s", credPath)
	}
}

// TestNativeProviderAuthFile_CoversNativeRouteWithoutChildEnv drives the
// native-route credential through pi's own store: a spec carrying a
// provider-native key (plus the injected key) produces a session auth.json
// holding ONLY the native entry — the injected key is never duplicated into
// pi's store — and removal deletes it. A keyless spec writes nothing, so
// pi's own resolution for that provider is untouched.
//
// RED proof: include PiKeyEnvVar in the native entries (or write the file
// for a keyless spec) and the assertions fail.
func TestNativeProviderAuthFile_CoversNativeRouteWithoutChildEnv(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	layout := sessionLayout{
		root:      filepath.Join(dir, ".pi-x"),
		agentHome: filepath.Join(dir, ".pi-x", "agent-home"),
	}
	if err := os.MkdirAll(layout.agentHome, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := agent.Spec{Env: map[string]string{
		"ZAI_API_KEY": "native-secret",
		PiKeyEnvVar:   "injected-secret",
	}}
	path, err := writeNativeProviderAuthFile(layout, spec)
	if err != nil {
		t.Fatalf("writeNativeProviderAuthFile: %v", err)
	}
	if path == "" {
		t.Fatal("want an auth.json path for a native-credentialed spec")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // test-owned session path
	if err != nil {
		t.Fatalf("read session auth.json: %v", err)
	}
	if strings.Contains(string(raw), "injected-secret") {
		t.Fatalf("injected key duplicated into the native store: %s", raw)
	}
	if !strings.Contains(string(raw), `"zai"`) || !strings.Contains(string(raw), "native-secret") {
		t.Fatalf("native entry missing from the session auth.json: %s", raw)
	}
	if fi, statErr := os.Stat(path); statErr != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("session auth.json mode is not owner-only")
	}
	removeNativeProviderAuthFile(layout)
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("session auth.json still exists after removal: %s", path)
	}

	// Keyless spec: no file, empty path.
	empty, err := writeNativeProviderAuthFile(layout, agent.Spec{})
	if err != nil {
		t.Fatalf("writeNativeProviderAuthFile(keyless): %v", err)
	}
	if empty != "" {
		t.Errorf("keyless spec produced an auth.json at %s; want none", empty)
	}
}

// TestStop_RemovesSessionCredentialFiles drives the production Stop path
// with the launch-wired onStop hook: a session layout holding both files
// loses both when the handle stops. The headless spawn test above already
// proves a live Stop removes the credential file end to end; this pins the
// auth.json half at the same seam.
//
// RED proof: drop the onStop hook in pi.go launch and this hook never runs.
func TestStop_RemovesSessionCredentialFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	layout := sessionLayout{
		root:      filepath.Join(dir, ".pi-x"),
		agentHome: filepath.Join(dir, ".pi-x", "agent-home"),
	}
	if err := os.MkdirAll(layout.agentHome, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := agent.Spec{Env: map[string]string{
		PiKeyEnvVar:   credentialFileSentinel,
		"ZAI_API_KEY": credentialFileSentinel,
	}}
	credPath, err := writeSessionCredentialFile(layout, sessionCredentialEntries(spec))
	if err != nil {
		t.Fatalf("writeSessionCredentialFile: %v", err)
	}
	authPath, err := writeNativeProviderAuthFile(layout, spec)
	if err != nil {
		t.Fatalf("writeNativeProviderAuthFile: %v", err)
	}
	if credPath == "" || authPath == "" {
		t.Fatalf("want both session files, got cred=%q auth=%q", credPath, authPath)
	}
	// Wire exactly as pi.go launch does, then drive the production Stop.
	_, h, err := spawnScripted(t, agent.Spec{Cwd: t.TempDir()}, handshakeEvent("h1"),
		getStateResponse("ses_stop_cleanup")+event(map[string]any{"type": "agent_settled"}))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	handle, ok := h.(*Handle)
	if !ok {
		t.Fatalf("scripted handle is %T, want *Handle", h)
	}
	handle.onStop = func() {
		removeSessionCredentialFile(layout)
		removeNativeProviderAuthFile(layout)
	}
	drain(t, h)
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for _, path := range []string{credPath, authPath} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("session file still exists after Stop: %s", path)
		}
	}
	// Stop is idempotent: a second call removes nothing and errors nothing.
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// TestInteractiveChildEnv_CarriesNoCredentialValue is the PTY-lane
// companion: a real interactive Spawn against a fake `pi` records its env,
// which must carry the credential file's path but no credential value.
// The PTY cleanup removes the file when the child exits.
//
// RED proof: drop the stripSessionCredentialEnv call in interactiveChildEnv
// and the env assertion fails with the sentinel.
func TestInteractiveChildEnv_CarriesNoCredentialValue(t *testing.T) {
	workdir := t.TempDir()
	p := newFakeInteractivePiProvider(t, `
{
  env > "$PWD/pty-env.txt"
  printf '%s' "$`+credentialFileEnvVar+`" > "$PWD/pty-credpath.txt"
}
`)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		Env: map[string]string{
			PiKeyEnvVar:          credentialFileSentinel,
			"AI_GATEWAY_API_KEY": credentialFileSentinel,
		},
		Endpoint: &agent.EndpointBinding{
			Company:  agent.CompanyAnthropic,
			Model:    "agg-vendor/claude-3-haiku",
			Protocol: agent.ProtoOpenAIChat,
			Host:     agent.HostDirect,
			BaseURL:  "https://ai-gateway.invalid/v1",
			Env:      map[string]string{"OPENAI_API_KEY": credentialFileSentinel},
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	awaitPTYExit(t, h)

	envRaw := readCapturedFile(t, workdir, "pty-env.txt")
	for _, entry := range strings.Split(envRaw, "\n") {
		if entry == "" {
			continue
		}
		if i := strings.IndexByte(entry, '='); i >= 0 && strings.Contains(entry[i+1:], credentialFileSentinel) {
			t.Fatalf("interactive child env carries the sentinel credential value: %q", entry)
		}
	}
	credPath := strings.TrimSpace(readCapturedFile(t, workdir, "pty-credpath.txt"))
	if credPath == "" {
		t.Fatalf("interactive child env carries no %s pointer:\n%s", credentialFileEnvVar, envRaw)
	}
	// The PTY cleanup removed the file when the child exited.
	if _, statErr := os.Stat(credPath); !os.IsNotExist(statErr) {
		t.Errorf("session credential file still exists after PTY exit: %s", credPath)
	}
}

// TestComposeChildEnv_StripsCredentialValues pins the same invariant one
// layer down, without spawning: composeChildEnv over a spec carrying the
// full snapshot set must emit no entry whose VALUE holds a credential,
// while every non-credential binding survives. The catalog-miss fallback
// and the native-route mirror both write credentials onto Spec.Env, so the
// strip must cover PiKeyEnvVar AND every provider-native var.
//
// RED proof: return spec unstripped from stripSessionCredentialEnv and every
// credential row below fails.
func TestComposeChildEnv_StripsCredentialValues(t *testing.T) {
	t.Parallel()
	layout := newSessionLayout(t.TempDir())
	spec := agent.Spec{
		Cwd: t.TempDir(),
		Env: map[string]string{
			PiKeyEnvVar:           credentialFileSentinel,
			"AI_GATEWAY_API_KEY":  credentialFileSentinel,
			"ZAI_API_KEY":         credentialFileSentinel,
			"OPENROUTER_API_KEY":  credentialFileSentinel,
			"OPENAI_API_KEY":      credentialFileSentinel,
			"ANTHROPIC_API_KEY":   credentialFileSentinel,
			"GEMINI_API_KEY":      credentialFileSentinel,
			"DONMAI_HARMLESS_VAR": "keep",
		},
	}
	env := composeChildEnv(spec, layout, "sess-token")
	if sessionCredentialValuePresent(env, credentialFileSentinel) {
		t.Fatalf("composeChildEnv emitted the sentinel into the child env: %q", env)
	}
	for _, entry := range env {
		if isSessionCredentialEnv(entry) {
			t.Fatalf("composeChildEnv emitted a credential-named entry: %q (full env: %q)", entry, env)
		}
	}
	if !hasEnvVal(env, "DONMAI_HARMLESS_VAR", "keep") {
		t.Errorf("non-credential binding was dropped; the strip is too aggressive: %q", env)
	}
	if !hasEnvVal(env, piHandshakeEnvVar, "sess-token") {
		t.Errorf("handshake token missing; the env under test is not the headless lane")
	}

	// The stripped values are exactly what the credential file carries.
	entries := sessionCredentialEntries(spec)
	if len(entries) == 0 {
		t.Fatal("sessionCredentialEntries is empty for a credential-bearing spec")
	}
	for _, e := range entries {
		if e.Value != credentialFileSentinel {
			t.Errorf("credential entry %s = %q, want the sentinel", e.Env, e.Value)
		}
	}
}

// TestInteractiveChildEnvMap_StripsCredentialValues is the interactive-lane
// unit companion: the override map carries the pin and the harmless var,
// never a credential value.
//
// RED proof: return spec unstripped from stripSessionCredentialEnv and the
// credential assertions fail.
func TestInteractiveChildEnvMap_StripsCredentialValues(t *testing.T) {
	t.Parallel()
	layout := newSessionLayout(t.TempDir())
	got := interactiveChildEnv(agent.Spec{
		Cwd: t.TempDir(),
		Env: map[string]string{
			PiKeyEnvVar:           credentialFileSentinel,
			"AI_GATEWAY_API_KEY":  credentialFileSentinel,
			"DONMAI_HARMLESS_VAR": "keep",
		},
		Endpoint: &agent.EndpointBinding{
			Model: "agg-vendor/claude-3-haiku", Protocol: agent.ProtoOpenAIChat,
			Host: agent.HostDirect, BaseURL: "https://ai-gateway.invalid/v1",
		},
	}, layout)
	for k, v := range got {
		if strings.Contains(v, credentialFileSentinel) {
			t.Fatalf("interactiveChildEnv[%q] carries the sentinel credential value", k)
		}
		if isSessionCredentialEnv(k + "=" + v) {
			t.Fatalf("interactiveChildEnv carries a credential-named entry %q", k)
		}
	}
	if got["DONMAI_HARMLESS_VAR"] != "keep" {
		t.Errorf("non-credential binding was dropped: %v", got)
	}
	if got[piBaseURLEnvVar] != "https://ai-gateway.invalid/v1" {
		t.Errorf("provider pin base URL missing: %v", got)
	}
}

// TestCredentialSnapshotParity_CredentialValuesRideFilesNotEnv re-pins the
// snapshot parity contract under the new rail: the session's own credential
// values reach it through FILES (the credential file plus the session
// auth.json) with cell values intact, and NO credential-named entry rides
// the child env — neither from Spec.Env nor inherited from the parent
// (supervisor re-admission and ambient host keys included). Non-credential
// snapshot bindings still ride env, and the blocklist/runner-only contract
// still holds.
//
// RED proof: return spec unstripped from stripSessionCredentialEnv and the
// child-env assertion fails with the first snapshot value.
func TestCredentialSnapshotParity_CredentialValuesRideFilesNotEnv(t *testing.T) {
	// Not parallel: mutates process env.
	const hostCanary = "host-canary-must-not-leak"
	t.Setenv("ANTHROPIC_API_KEY", hostCanary)
	t.Setenv("OPENAI_API_KEY", hostCanary)
	t.Setenv("GEMINI_API_KEY", hostCanary)
	t.Setenv("ATTACH_TOKEN", "runner-control-must-not-reach-pi")

	snapshot := snapshotSpecEnv()
	snapshot["ATTACH_TOKEN"] = "spec-layer-runner-control-must-not-reach-pi"

	layout := newSessionLayout(t.TempDir())
	projected, err := applyEndpoint(agent.Spec{
		Cwd:      t.TempDir(),
		Env:      snapshot,
		Endpoint: gatewayBinding("served-model"),
	})
	if err != nil {
		t.Fatalf("applyEndpoint rejected a routable gateway binding: %v", err)
	}
	env := composeChildEnv(projected, layout, "sess-token")
	for _, e := range env {
		if isSessionCredentialEnv(e) {
			t.Fatalf("credential-named entry rides the child env: %q", e)
		}
		if strings.Contains(e, hostCanary) {
			t.Fatalf("blocklisted host credential leaked into pi child env: %q", e)
		}
	}
	// Non-credential snapshot bindings still ride env.
	if !hasEnvVal(env, "ANTHROPIC_BASE_URL", "cell-ANTHROPIC_BASE_URL") {
		t.Errorf("non-credential snapshot binding ANTHROPIC_BASE_URL missing from child env")
	}
	if hasEnvKey(env, "ATTACH_TOKEN") {
		t.Errorf("runner-only ATTACH_TOKEN reached the pi child env from either layer")
	}

	// The same values land in the credential file, keyed by env name.
	entries := sessionCredentialEntries(projected)
	byName := make(map[string]string, len(entries))
	for _, e := range entries {
		byName[e.Env] = e.Value
	}
	for _, k := range credentialSnapshotKeys {
		switch k {
		case "ANTHROPIC_BASE_URL", "GITHUB_TOKEN", "GH_TOKEN", "LINEAR_API_KEY", "CODEX_API_KEY":
			continue // non-model credentials: out of the session-file rail's scope
		}
		if byName[k] != "cell-"+k {
			t.Errorf("credential file %s = %q, want %q", k, byName[k], "cell-"+k)
		}
	}
	if !hasEnvVal(env, piHandshakeEnvVar, "sess-token") {
		t.Errorf("handshake token missing; the env under test is not the headless lane")
	}
}
