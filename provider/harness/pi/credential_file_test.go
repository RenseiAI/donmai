package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/gateway"
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
// never a credential value. Credential names the session does not carry are
// shadowed with an explicit EMPTY override (so the PTY host cannot inherit a
// supervisor-declared parent value); an empty read is absent to the child,
// never a usable key.
//
// RED proof: return spec unstripped from stripSessionCredentialEnv and the
// value assertions fail with the sentinel.
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
		if isSessionCredentialEnv(k+"="+v) && v != "" {
			t.Fatalf("interactiveChildEnv carries a credential-named entry with a value: %q=%q", k, v)
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

// TestGatewayBearerRidesFileNotChildEnv is the gateway-binding regression
// test: a gateway binding carrying its per-session bearer under Endpoint.Env
// must deliver that bearer through the session credential file, never the
// child env, on BOTH lanes. applyEndpoint mirrors the binding key onto
// PiKeyEnvVar, so the env copy of the bearer is redundant exposure — the
// renamed pi child renders it to same-user listings either way.
//
// Drives the production entry points: applyEndpoint (the binding merge),
// composeChildEnv (headless lane) and interactiveChildEnv (PTY lane), plus a
// live headless Spawn against the fake recorder asserting the same.
//
// RED proof: drop the bearer from sessionCredentialEntries and the bearer
// assertions below fail with the sentinel quoted in the child env.
func TestGatewayBearerRidesFileNotChildEnv(t *testing.T) {
	// Not parallel: asserts on inherited-env stripping behavior.
	const bearer = "gw-bearer-must-not-ride-child-env"
	binding := &agent.EndpointBinding{
		Company:  agent.CompanyOpenAI,
		Model:    "served-model",
		BaseURL:  "http://127.0.0.1:8080/v1",
		Host:     agent.HostGateway,
		Protocol: agent.ProtoOpenAIChat,
		Env:      map[string]string{gatewayBearerEnvVar: bearer},
	}
	projected, err := applyEndpoint(agent.Spec{Cwd: t.TempDir(), Endpoint: binding})
	if err != nil {
		t.Fatalf("applyEndpoint rejected a routable gateway binding: %v", err)
	}

	// The projected spec carries the bearer twice: verbatim under its binding
	// name and mirrored onto the injected-provider key.
	if projected.Env[gatewayBearerEnvVar] != bearer {
		t.Fatalf("binding bearer missing from projected spec env: %v", projected.Env)
	}
	if projected.Env[PiKeyEnvVar] != bearer {
		t.Fatalf("binding key not mirrored onto %s: %v", PiKeyEnvVar, projected.Env)
	}

	// The credential file fans both copies out; the headless lane strips both.
	entries := sessionCredentialEntries(projected)
	byName := make(map[string]string, len(entries))
	for _, e := range entries {
		byName[e.Env] = e.Value
	}
	if byName[gatewayBearerEnvVar] != bearer {
		t.Errorf("credential file %s = %q, want the bearer", gatewayBearerEnvVar, byName[gatewayBearerEnvVar])
	}
	if byName[PiKeyEnvVar] != bearer {
		t.Errorf("credential file %s = %q, want the mirrored bearer", PiKeyEnvVar, byName[PiKeyEnvVar])
	}

	layout := newSessionLayout(t.TempDir())
	headless := composeChildEnv(projected, layout, "sess-token")
	for _, e := range headless {
		if isSessionCredentialEnv(e) {
			t.Fatalf("gateway bearer rides the headless child env by name: %q", e)
		}
		if i := strings.IndexByte(e, '='); i >= 0 && strings.Contains(e[i+1:], bearer) {
			t.Fatalf("gateway bearer rides the headless child env by value: %q", e)
		}
	}
	if !hasEnvVal(headless, piHandshakeEnvVar, "sess-token") {
		t.Errorf("handshake token missing; the env under test is not the headless lane")
	}

	// Interactive lane: same bearer, same strip, through its own override map.
	// Session-absent credential names are shadowed empty (see the unit
	// companion above); only a non-empty value is a leak.
	interactive := interactiveChildEnv(projected, layout)
	for k, v := range interactive {
		if isSessionCredentialEnv(k+"="+v) && v != "" {
			t.Fatalf("gateway bearer rides the interactive child env by name: %q", k)
		}
		if strings.Contains(v, bearer) {
			t.Fatalf("gateway bearer rides the interactive child env by value: %q=%q", k, v)
		}
	}
}

// TestHeadlessSpawn_GatewayBearerAbsentFromChildEnv drives a live headless
// Spawn with a gateway binding carrying the bearer and asserts the recorded
// child env carries the credential file's path but neither bearer copy.
// The spawn is expected to fail (the fake never answers the handshake);
// what matters is the child was exec'd with the production env.
func TestHeadlessSpawn_GatewayBearerAbsentFromChildEnv(t *testing.T) {
	if os.Getenv("SHELL") == "" && os.Getenv("OS") != "" {
		t.Skip("fake pi recorder needs /bin/sh")
	}
	const bearer = "gw-bearer-must-not-ride-child-env"
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
			Prompt: "prove gateway bearer rides file not env",
			Endpoint: &agent.EndpointBinding{
				Company:  agent.CompanyOpenAI,
				Model:    "served-model",
				BaseURL:  "http://127.0.0.1:8080/v1",
				Host:     agent.HostGateway,
				Protocol: agent.ProtoOpenAIChat,
				Env:      map[string]string{gatewayBearerEnvVar: bearer},
			},
		})
		spawnErr <- err
	}()

	_ = waitForArgvRecord(t, filepath.Join(dir, "argv"), 20*time.Second)
	envRaw := waitForArgvRecord(t, filepath.Join(dir, "child-env"), 20*time.Second)
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
	if got[gatewayBearerEnvVar] != bearer {
		t.Errorf("credential file %s = %q, want the bearer", gatewayBearerEnvVar, got[gatewayBearerEnvVar])
	}
	if got[PiKeyEnvVar] != bearer {
		t.Errorf("credential file %s = %q, want the mirrored bearer", PiKeyEnvVar, got[PiKeyEnvVar])
	}

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
		if isSessionCredentialEnv(entry) {
			t.Fatalf("gateway binding bearer rides the spawned child env by name: %q", entry)
		}
		if i := strings.IndexByte(entry, '='); i >= 0 && strings.Contains(entry[i+1:], bearer) {
			t.Fatalf("gateway binding bearer rides the spawned child env by value: %q", entry)
		}
	}
	if _, statErr := os.Stat(credPath); !os.IsNotExist(statErr) {
		t.Errorf("session credential file still exists after Stop: %s", credPath)
	}
}

// TestInteractiveSpawn_DeclaredParentCredentialAbsentFromChildEnv is the B1
// regression test: a supervisor-declared inherited credential (the
// InjectedEnvKeysVar declaration that re-admits a blocklisted name into the
// worker) must not reach the interactive PTY child. The PTY host layers the
// override map onto the parent env and inherits unmentioned names, so the
// interactive lane shadows every session-absent credential name with an
// explicit empty override — the counterpart of the headless lane's
// post-merge strip. Drives the production interactive Spawn entry point
// against a fake binary that records its env.
//
// RED proof: drop the shadow loop from interactiveChildEnv and the recorded
// child env carries the declared parent value.
func TestInteractiveSpawn_DeclaredParentCredentialAbsentFromChildEnv(t *testing.T) {
	// Not parallel: mutates process env and spawns a real PTY child.
	const parentSecret = "parent-secret-declared-must-not-reach-pi"
	t.Setenv("ANTHROPIC_API_KEY", parentSecret)
	t.Setenv("DONMAI_INJECTED_ENV_KEYS", "ANTHROPIC_API_KEY")
	workdir := t.TempDir()
	p := newFakeInteractivePiProvider(t, `
{
  env > "$PWD/pty-env.txt"
}
`)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
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
		if i := strings.IndexByte(entry, '='); i >= 0 && strings.Contains(entry[i+1:], parentSecret) {
			t.Fatalf("declared parent credential reached the interactive child env: %q", entry)
		}
	}
	// The shadow override is what keeps it out: the name is present but
	// empty, which reads as absent to the child — never the parent's value.
	found := false
	for _, entry := range strings.Split(envRaw, "\n") {
		if entry == "ANTHROPIC_API_KEY=" {
			found = true
		}
	}
	if !found {
		t.Errorf("interactive child env carries no empty ANTHROPIC_API_KEY shadow; the parent value is one unshadowed name away:\n%s", envRaw)
	}
}

// TestHeadlessChildEnv_StripsKnownSecretValues is the B2 headless regression
// test: the finding's named sentinels (a custom binding key and the worker
// runtime bearer on Spec.Env) must not reach the headless pi child env by
// NAME or by VALUE. The closed producer set is pinned by the endpoint
// manifests: model credentials arrive only under endpoint-declared EnvKeys
// or the gateway bearer, both of which the file rail fans out — so the rail
// plus the value backstop below leave no named hole. Drives the production
// composeChildEnv entry point.
//
// RED proof: remove the bearer/custom-name coverage from the rail (or the
// value backstop) and the assertions fail with the sentinel quoted.
func TestHeadlessChildEnv_StripsKnownSecretValues(t *testing.T) {
	t.Parallel()
	const customSecret = "super-secret-custom-value"
	const workerSecret = "worker-secret-value"
	layout := newSessionLayout(t.TempDir())
	spec := agent.Spec{
		Cwd: t.TempDir(),
		Env: map[string]string{
			"API_KEY":             customSecret,
			"CUSTOM_SECRET_KEY":   customSecret,
			"WORKER_AUTH_TOKEN":   workerSecret,
			"DONMAI_HARMLESS_VAR": "keep",
		},
	}
	env := composeChildEnv(spec, layout, "sess-token")
	for _, e := range env {
		if i := strings.IndexByte(e, '='); i >= 0 && (strings.Contains(e[i+1:], customSecret) || strings.Contains(e[i+1:], workerSecret)) {
			t.Fatalf("secret value rides the headless child env: %q", e)
		}
	}
	if !hasEnvVal(env, "DONMAI_HARMLESS_VAR", "keep") {
		t.Errorf("non-credential binding was dropped from the headless lane: %q", env)
	}
}

// TestInteractiveChildEnv_StripsKnownSecretValues is the B2 interactive
// companion: the same named sentinels are absent from the PTY override map
// by NAME and by VALUE. Drives the production interactiveChildEnv entry
// point.
//
// RED proof: remove the bearer/custom-name coverage from the rail and the
// assertions fail with the sentinel quoted.
func TestInteractiveChildEnv_StripsKnownSecretValues(t *testing.T) {
	t.Parallel()
	const customSecret = "super-secret-custom-value"
	const workerSecret = "worker-secret-value"
	layout := newSessionLayout(t.TempDir())
	got := interactiveChildEnv(agent.Spec{
		Cwd: t.TempDir(),
		Env: map[string]string{
			"API_KEY":             customSecret,
			"CUSTOM_SECRET_KEY":   customSecret,
			"WORKER_AUTH_TOKEN":   workerSecret,
			"DONMAI_HARMLESS_VAR": "keep",
		},
	}, layout)
	for k, v := range got {
		if strings.Contains(v, customSecret) || strings.Contains(v, workerSecret) {
			t.Fatalf("secret value rides the interactive child env: %q=%q", k, v)
		}
	}
	if got["DONMAI_HARMLESS_VAR"] != "keep" {
		t.Errorf("non-credential binding was dropped from the interactive lane: %v", got)
	}
}

// TestGatewayBearerConstLinked pins F1: the pi-local bearer name is one
// compiler-checked definition with the gateway package's own TokenEnvVar,
// so a gateway rename breaks the build instead of silently re-opening the
// child-env hole. A literal re-spelling here would compile either way.
func TestGatewayBearerConstLinked(t *testing.T) {
	t.Parallel()
	if gatewayBearerEnvVar != gateway.TokenEnvVar {
		t.Fatalf("gatewayBearerEnvVar = %q, want gateway.TokenEnvVar (%q): the rail must track the gateway's own definition", gatewayBearerEnvVar, gateway.TokenEnvVar)
	}
}
