package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// This file is the allowlist suite for the pi child's exec environment
// (child_env.go). pi renames its process at startup, and a same-user process
// listing then renders the leading entries of the exec-time environment
// block as its command line — so every exec-environment entry is public to
// every same-user process. The suite pins that no secret reaches that block
// under ANY name, including names nobody has listed yet, on both spawn
// lanes; that every secret still reaches the session through the route it
// belongs to; and that the allowlist itself carries nothing secret-shaped.

// sentinelRoute is where a session secret must arrive — never in the exec
// environment.
type sentinelRoute int

const (
	// routeCredential: a model credential. Only the credential file's
	// credentials section carries it.
	routeCredential sentinelRoute = iota
	// routeDeferred: a binding the agent's tools may need. Only the
	// credential file's environment section carries it; the policy extension
	// restores it into pi's in-process environment.
	routeDeferred
	// routeRefused: nothing carries it, in any form.
	routeRefused
)

// childEnvSentinel names one secret-carrying variable and where it must
// arrive.
type childEnvSentinel struct {
	name  string
	route sentinelRoute
}

// childEnvSentinels is the sentinel table: secrets under every family of
// name a session can carry, plus names nobody has listed yet. Each test
// writes a distinct value per name and layer, so a failure names exactly
// which value leaked.
var childEnvSentinels = []childEnvSentinel{
	// The injected provider's key and the per-session gateway bearer.
	{PiKeyEnvVar, routeCredential},
	{gatewayBearerEnvVar, routeCredential},
	// The gateway key and BYOK provider keys.
	{"AI_GATEWAY_API_KEY", routeCredential},
	{"ANTHROPIC_API_KEY", routeCredential},
	{"OPENAI_API_KEY", routeCredential},
	{"GEMINI_API_KEY", routeCredential},
	{"OPENROUTER_API_KEY", routeCredential},
	{"XAI_API_KEY", routeCredential},
	{"HF_TOKEN", routeCredential},
	// Bedrock/Vertex/Azure-style cell credentials.
	{"AWS_ACCESS_KEY_ID", routeCredential},
	{"AWS_SECRET_ACCESS_KEY", routeCredential},
	{"AWS_BEARER_TOKEN_BEDROCK", routeCredential},
	{"GOOGLE_API_KEY", routeCredential},
	{"GOOGLE_APPLICATION_CREDENTIALS", routeCredential},
	{"AZURE_OPENAI_API_KEY", routeCredential},
	// The worker's runtime bearer and the generic custom-binding spellings.
	{"WORKER_AUTH_TOKEN", routeRefused},
	{"API_KEY", routeRefused},
	{"CUSTOM_SECRET_KEY", routeRefused},
	// The gateway's upstream credential and other host model auth.
	{runtimeenv.GatewayUpstreamAPIKeyEnv, routeRefused},
	{"ANTHROPIC_AUTH_TOKEN", routeRefused},
	{"OPENCLAW_GATEWAY_TOKEN", routeRefused},
	// Supervisor controls.
	{"ATTACH_TOKEN", routeRefused},
	{"DONMAI_CONTROL_TOKEN", routeRefused},
	// A name inside the harness's own namespace.
	{"DONMAI_PI_FUTURE_SECRET", routeRefused},
	// Tool credentials and session tokens the agent's tools may need.
	{"GITHUB_TOKEN", routeDeferred},
	{"GH_TOKEN", routeDeferred},
	{"LINEAR_API_KEY", routeDeferred},
	{"SESSION_READ_TOKEN", routeDeferred},
	// Names nobody has listed yet — including one with no secret shape at
	// all and one in lower case.
	{"FUTURE_VENDOR_API_KEY", routeDeferred},
	{"ACME_SESSION_BEARER", routeDeferred},
	{"OPAQUE_VALUE_7", routeDeferred},
	{"zz_lowercase_secret", routeDeferred},
}

// sentinelValue is the distinct value a sentinel carries on one layer.
// Every value starts with "sentinel-", which no allowlisted value in a test
// process carries.
func sentinelValue(layer, name string) string {
	return "sentinel-" + layer + "-" + strings.ToLower(name)
}

// sentinelSpecEnv is the sentinel table on the trusted Spec.Env layer.
func sentinelSpecEnv() map[string]string {
	env := make(map[string]string, len(childEnvSentinels))
	for _, s := range childEnvSentinels {
		env[s.name] = sentinelValue("spec", s.name)
	}
	return env
}

// setParentSentinels writes the sentinel table into this process's own
// environment — the parent every pi child would otherwise inherit.
func setParentSentinels(t *testing.T) {
	t.Helper()
	for _, s := range childEnvSentinels {
		t.Setenv(s.name, sentinelValue("parent", s.name))
	}
}

// deferredEntry returns the environment-section entry for name.
func deferredEntry(parts childEnvPartition, name string) (sessionCredential, bool) {
	for _, d := range parts.deferred {
		if d.Env == name {
			return d, true
		}
	}
	return sessionCredential{}, false
}

// deferredValue returns the environment-section value for name, or "".
func deferredValue(parts childEnvPartition, name string) string {
	d, _ := deferredEntry(parts, name)
	return d.Value
}

// harnessComposedName reports whether name is one the harness composes onto
// the child itself (never copied from the parent or Spec.Env).
func harnessComposedName(name string) bool {
	switch name {
	case piCodingAgentDirEnvVar, piCodingAgentSessionDirEnvVar, piOfflineEnvVar, piSkipVersionCheckEnvVar:
		return true
	}
	return strings.HasPrefix(name, "DONMAI_PI_")
}

// assertExecEnvCarriesNoSentinel fails when any exec-environment entry holds
// a sentinel value, or when any entry is neither allowlisted nor composed by
// the harness itself — the structural half: a name outside both sets is a
// passthrough the allowlist did not admit.
func assertExecEnvCarriesNoSentinel(t *testing.T, lane string, env map[string]string) {
	t.Helper()
	if len(env) == 0 {
		t.Fatalf("%s exec environment is empty; the assertion would be vacuous", lane)
	}
	for name, value := range env {
		if strings.Contains(value, "sentinel-") {
			t.Errorf("%s exec environment carries a sentinel: %s=%q", lane, name, value)
		}
		if !childEnvAllowed(name, value) && !harnessComposedName(name) {
			t.Errorf("%s exec environment carries %s, which is neither allowlisted nor harness-composed", lane, name)
		}
	}
}

// envSliceMap turns a KEY=VALUE slice into a map (last entry wins, as exec
// resolves duplicates).
func envSliceMap(entries []string) map[string]string {
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if i := strings.IndexByte(e, '='); i > 0 {
			out[e[:i]] = e[i+1:]
		}
	}
	return out
}

// assertSentinelRoutes pins where every sentinel lands in the credential
// file: model credentials from Spec.Env in the credentials section only,
// tool bindings in the environment section only (from either layer), and
// refused names nowhere. credentials is the credentials section, environment
// the environment section.
func assertSentinelRoutes(t *testing.T, credentials, environment map[string]string) {
	t.Helper()
	for _, s := range childEnvSentinels {
		specValue, parentValue := sentinelValue("spec", s.name), sentinelValue("parent", s.name)
		if v, ok := environment[s.name]; ok && s.route != routeDeferred {
			t.Errorf("%s (%q) rides the environment section; only tool bindings belong there", s.name, v)
		}
		if v, ok := credentials[s.name]; ok && s.route != routeCredential {
			t.Errorf("%s (%q) rides the credentials section; only model credentials belong there", s.name, v)
		}
		switch s.route {
		case routeCredential:
			if credentials[s.name] != specValue {
				t.Errorf("model credential %s = %q in the credentials section, want the session's own %q", s.name, credentials[s.name], specValue)
			}
		case routeDeferred:
			if environment[s.name] != specValue {
				t.Errorf("tool binding %s = %q in the environment section, want the session's own %q", s.name, environment[s.name], specValue)
			}
		}
		for _, section := range []map[string]string{credentials, environment} {
			for name, v := range section {
				if v == parentValue && s.route != routeDeferred {
					t.Errorf("the parent's %s rides the credential file under %s", s.name, name)
				}
			}
		}
	}
}

// TestChildEnvAllowlist_SentinelTableNeverInExecEnv is the unit-level
// sentinel proof for BOTH lanes: the full sentinel table on the trusted
// Spec.Env layer AND in the inherited parent environment yields an exec
// environment with no sentinel value and no name outside the allowlist,
// while every sentinel lands on its own route in the credential file.
//
// RED proof: make childEnvAllowed admit every name (or re-add any one
// table name to childEnvAllowedNames) and the exec assertions fail with the
// sentinel quoted.
func TestChildEnvAllowlist_SentinelTableNeverInExecEnv(t *testing.T) {
	// Not parallel: writes the sentinel table into the process environment.
	setParentSentinels(t)
	spec := agent.Spec{Cwd: t.TempDir(), Env: sentinelSpecEnv()}
	layout := newSessionLayout(t.TempDir())

	assertExecEnvCarriesNoSentinel(t, "headless", envSliceMap(composeChildEnv(spec, layout, "sess-token")))
	assertExecEnvCarriesNoSentinel(t, "interactive", interactiveChildEnv(spec, layout))

	credentials := map[string]string{}
	for _, e := range sessionCredentialEntries(spec) {
		credentials[e.Env] = e.Value
	}
	for lane, parts := range map[string]childEnvPartition{
		"headless":    sessionChildEnv(spec),
		"interactive": interactiveChildEnvParts(spec),
	} {
		environment := map[string]string{}
		for _, d := range parts.deferred {
			environment[d.Env] = d.Value
		}
		t.Run(lane, func(t *testing.T) {
			assertSentinelRoutes(t, credentials, environment)
		})
	}
}

// TestChildEnvAllowlist_ParentOnlySentinelsNeverReachExecEnv covers a
// session whose spec carries nothing: every sentinel lives only in the
// inherited parent environment. None may reach either lane's exec
// environment; only the tool bindings may reach the session at all,
// through the environment section.
func TestChildEnvAllowlist_ParentOnlySentinelsNeverReachExecEnv(t *testing.T) {
	// Not parallel: writes the sentinel table into the process environment.
	setParentSentinels(t)
	spec := agent.Spec{Cwd: t.TempDir()}
	layout := newSessionLayout(t.TempDir())

	assertExecEnvCarriesNoSentinel(t, "headless", envSliceMap(composeChildEnv(spec, layout, "sess-token")))
	assertExecEnvCarriesNoSentinel(t, "interactive", interactiveChildEnv(spec, layout))
	if entries := sessionCredentialEntries(spec); len(entries) != 0 {
		t.Errorf("a spec with no credential filed %d credential entries", len(entries))
	}
	for _, s := range childEnvSentinels {
		got := deferredValue(sessionChildEnv(spec), s.name)
		switch {
		case s.route == routeDeferred && got != sentinelValue("parent", s.name):
			t.Errorf("parent tool binding %s deferred as %q, want it delivered through the environment section", s.name, got)
		case s.route != routeDeferred && got != "":
			t.Errorf("parent %s rides the environment section (%q)", s.name, got)
		}
	}
}

// TestPartitionChildEnv_LosesNothingButRefusals pins that the partition
// routes EVERY entry the shared Composer produces: each one lands in the
// exec half, in the deferred half, or is refused by name — so narrowing
// the exec environment never silently drops a binding a tool needs.
func TestPartitionChildEnv_LosesNothingButRefusals(t *testing.T) {
	// Not parallel: writes the sentinel table into the process environment.
	setParentSentinels(t)
	spec := agent.Spec{Cwd: t.TempDir(), Env: sentinelSpecEnv()}
	parts := sessionChildEnv(spec)
	exec := envSliceMap(parts.exec)
	for _, entry := range runtimeenv.NewComposer().Compose(envSliceToMap(os.Environ()), spec) {
		i := strings.IndexByte(entry, '=')
		if i <= 0 {
			continue
		}
		name, value := entry[:i], entry[i+1:]
		if childEnvRefused(name) {
			continue
		}
		if v, ok := exec[name]; ok && v == value {
			continue
		}
		if d, ok := deferredEntry(parts, name); ok && d.Value == value {
			continue
		}
		t.Errorf("composed entry %s was dropped without a refusal", name)
	}
}

// TestChildEnvAllowlistCarriesNoSecretShapedName refuses any allowlisted
// name that looks like a credential, that is a session model credential, or
// that the partition would refuse anyway. Every allowlisted value is
// printed by a same-user listing of the renamed child, so the allowlist may
// only ever name process context.
//
// RED proof: add "GH_TOKEN" (or any credential-shaped name) to
// childEnvAllowedNames and this test fails naming it.
func TestChildEnvAllowlistCarriesNoSecretShapedName(t *testing.T) {
	t.Parallel()
	var names []string
	for name := range childEnvAllowedNames {
		names = append(names, name)
	}
	for name := range childEnvProxyNames {
		names = append(names, name)
	}
	for name := range receiptUnsafeStartupEnv {
		names = append(names, name)
	}
	for _, name := range names {
		if runtimeenv.LooksSensitive(name) {
			t.Errorf("allowlisted name %s looks like a credential", name)
		}
		if isSessionCredentialName(name) {
			t.Errorf("allowlisted name %s is a session model credential", name)
		}
		if childEnvRefused(name) {
			t.Errorf("allowlisted name %s is refused by the partition; the allowlist entry is dead", name)
		}
	}
	for _, s := range childEnvSentinels {
		if childEnvAllowed(s.name, sentinelValue("spec", s.name)) {
			t.Errorf("sentinel name %s is allowlisted", s.name)
		}
	}
}

// TestChildEnvProxyAdmittedOnlyWithoutUserinfo pins the one value-dependent
// rule: a proxy route rides the exec environment only when it names no
// credentials; one that embeds userinfo rides the credential file instead.
func TestChildEnvProxyAdmittedOnlyWithoutUserinfo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"http://proxy.invalid:3128", true},
		{"proxy.invalid:3128", true},
		{"http://user:sentinel-proxy-password@proxy.invalid:3128", false},
		{"http://sentinel-proxy-token@proxy.invalid", false},
		{"user:sentinel@proxy.invalid:3128", false},
	} {
		for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "ALL_PROXY"} {
			if got := childEnvAllowed(name, tc.value); got != tc.want {
				t.Errorf("childEnvAllowed(%s, %q) = %v, want %v", name, tc.value, got, tc.want)
			}
		}
	}
	parts := partitionChildEnv(nil, agent.Spec{Env: map[string]string{
		"HTTPS_PROXY": "http://user:sentinel-proxy-password@proxy.invalid:3128",
	}})
	if hasEnvKey(parts.exec, "HTTPS_PROXY") {
		t.Errorf("a proxy URL with credentials rode the exec environment: %q", parts.exec)
	}
	if deferredValue(parts, "HTTPS_PROXY") == "" {
		t.Errorf("a proxy URL with credentials was dropped instead of deferred")
	}
}

// TestHeadlessSpawn_SentinelTableAbsentFromExecEnv drives the production
// headless Spawn against a fake `pi` that records the environment it was
// exec'd with. The full sentinel table rides both Spec.Env and the parent
// environment; the recorded exec environment must carry none of it, and
// the session credential file must carry each sentinel on its own route.
//
// RED proof: make childEnvAllowed admit every name and the exec assertion
// fails with the sentinels quoted.
func TestHeadlessSpawn_SentinelTableAbsentFromExecEnv(t *testing.T) {
	// Not parallel: writes the sentinel table into the process environment.
	if os.Getenv("SHELL") == "" && os.Getenv("OS") != "" {
		t.Skip("fake pi recorder needs /bin/sh")
	}
	setParentSentinels(t)
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

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	spawnErr := make(chan error, 1)
	go func() {
		_, err := p.Spawn(ctx, agent.Spec{
			Cwd:    t.TempDir(),
			Prompt: "prove no sentinel rides the exec environment",
			Env:    sentinelSpecEnv(),
		})
		spawnErr <- err
	}()

	_ = waitForArgvRecord(t, filepath.Join(dir, "argv"), 20*time.Second)
	envRaw := waitForArgvRecord(t, filepath.Join(dir, "child-env"), 20*time.Second)
	execEnv := envSliceMap(strings.Split(strings.TrimSpace(envRaw), "\n"))
	credentials, environment := readCredentialFileSections(t, execEnv[credentialFileEnvVar])
	cancel()
	select {
	case err := <-spawnErr:
		if err == nil {
			t.Fatal("Spawn against a handshake-silent fake unexpectedly succeeded")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Spawn did not return after context cancellation")
	}

	// The recorder's own shell adds PWD/SHLVL/_ on top of what it was exec'd
	// with; they carry no sentinel and are the shell's, not the harness's.
	for _, shellOwned := range []string{"PWD", "SHLVL", "_", "OLDPWD"} {
		delete(execEnv, shellOwned)
	}
	assertExecEnvCarriesNoSentinel(t, "headless spawn", execEnv)
	assertSentinelRoutes(t, credentials, environment)
}

// TestInteractiveSpawn_SentinelTableAbsentFromExecEnv is the PTY-lane
// companion: the production interactive Spawn against a fake `pi` that
// records its environment. The PTY host must spawn the child with exactly
// the lane's allowlisted environment — inheriting nothing from the parent —
// so neither the Spec.Env sentinels nor the parent's reach it, and no
// credential name is present even empty.
//
// RED proof: spawn without ExactEnv (spawnInteractive) and every parent
// sentinel rides back into the recorded environment.
func TestInteractiveSpawn_SentinelTableAbsentFromExecEnv(t *testing.T) {
	// Not parallel: writes the sentinel table into the process environment.
	setParentSentinels(t)
	workdir := t.TempDir()
	p := newFakeInteractivePiProvider(t, `
{
  env > "$PWD/pty-env.txt"
  if [ -n "$`+credentialFileEnvVar+`" ]; then cp "$`+credentialFileEnvVar+`" "$PWD/credential-file.json"; fi
}
`)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		Env:         sentinelSpecEnv(),
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	awaitPTYExit(t, h)

	execEnv := envSliceMap(strings.Split(strings.TrimSpace(readCapturedFile(t, workdir, "pty-env.txt")), "\n"))
	for _, shellOwned := range []string{"PWD", "SHLVL", "_", "OLDPWD"} {
		delete(execEnv, shellOwned)
	}
	assertExecEnvCarriesNoSentinel(t, "interactive spawn", execEnv)
	for _, s := range childEnvSentinels {
		if _, present := execEnv[s.name]; present {
			t.Errorf("interactive child defines %s; it must be absent, not shadowed", s.name)
		}
	}
	credentials, environment := readCredentialFileSections(t, filepath.Join(workdir, "credential-file.json"))
	assertSentinelRoutes(t, credentials, environment)
}

// readCredentialFileSections reads both sections of a session credential
// file.
func readCredentialFileSections(t *testing.T, path string) (credentials, environment map[string]string) {
	t.Helper()
	if strings.TrimSpace(path) == "" {
		t.Fatal("the child carries no credential file pointer; the session has no file rail")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the session-owned credential file under test
	if err != nil {
		t.Fatalf("read session credential file: %v", err)
	}
	if credentials, err = readSessionCredentialMap(raw); err != nil {
		t.Fatalf("decode credentials section: %v", err)
	}
	if environment, err = readSessionEnvironmentMap(raw); err != nil {
		t.Fatalf("decode environment section: %v", err)
	}
	return credentials, environment
}
