package pi

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestVersionProbeCommandStripsStartupInjectionWithoutExecutingCandidate(t *testing.T) {
	for key := range receiptUnsafeStartupEnv {
		t.Setenv(key, "benign-control-value")
	}
	for _, key := range []string{"DYLD_FUTURE_LOADER_CONTROL", "LD_FUTURE_LOADER_CONTROL"} {
		t.Setenv(key, "benign-control-value")
	}
	t.Setenv("DONMAI_HARMLESS_VAR", "preserved")
	cmd := versionProbeCommand(context.Background(), "/artifact/pi")
	if cmd.Dir != "/artifact" {
		t.Fatalf("version probe directory=%q, want selected binary directory", cmd.Dir)
	}
	if len(cmd.Args) != 2 || cmd.Args[0] != "/artifact/pi" || cmd.Args[1] != "--version" {
		t.Fatalf("version probe arguments=%q", cmd.Args)
	}
	for key := range receiptUnsafeStartupEnv {
		if hasEnvKey(cmd.Env, key) {
			t.Fatalf("version probe retained startup injection environment %s", key)
		}
	}
	for _, key := range []string{"DYLD_FUTURE_LOADER_CONTROL", "LD_FUTURE_LOADER_CONTROL"} {
		if hasEnvKey(cmd.Env, key) {
			t.Fatalf("version probe retained loader namespace environment %s", key)
		}
	}
	if !hasEnvVal(cmd.Env, "DONMAI_HARMLESS_VAR", "preserved") {
		t.Fatal("version probe dropped an unrelated host environment entry")
	}
}

func TestReceiptStartupContextRejectsRuntimeAndNativeLoaderEnvironment(t *testing.T) {
	for _, key := range []string{
		"BUN_OPTIONS",
		"BUN_BE_BUN",
		"NODE_OPTIONS",
		"NODE_PATH",
		"DYLD_FUTURE_LOADER_CONTROL",
		"LD_FUTURE_LOADER_CONTROL",
	} {
		t.Run(key, func(t *testing.T) {
			if got := measureReceiptStartupContext(t.TempDir(), []string{key + "=benign-control-value"}); got != nil {
				t.Fatalf("startup environment %s retained receipt admission", key)
			}
		})
	}
	if got := measureReceiptStartupContext(t.TempDir(), []string{"NODE_ENV=staging"}); got == nil {
		t.Fatal("ordinary NODE_ENV was misclassified as Node startup resolution configuration")
	}
}

func TestReceiptStartupContextUsesExactComposedHostAndSpecEnvironment(t *testing.T) {
	const harmlessOptions = "--smol"
	t.Setenv("BUN_OPTIONS", harmlessOptions)
	layout := newSessionLayout(t.TempDir())
	hostEnv := composeChildEnv(agent.Spec{Cwd: t.TempDir()}, layout, "token")
	if !hasEnvVal(hostEnv, "BUN_OPTIONS", harmlessOptions) {
		t.Fatal("control invalid: composed child environment did not retain host BUN_OPTIONS")
	}
	if measureReceiptStartupContext(t.TempDir(), hostEnv) != nil {
		t.Fatal("host BUN_OPTIONS retained receipt admission")
	}

	t.Setenv("BUN_OPTIONS", "")
	specEnv := composeChildEnv(agent.Spec{
		Cwd: t.TempDir(),
		Env: map[string]string{"BUN_OPTIONS": harmlessOptions},
	}, layout, "token")
	if !hasEnvVal(specEnv, "BUN_OPTIONS", harmlessOptions) {
		t.Fatal("control invalid: Spec.Env BUN_OPTIONS did not reach exact child environment")
	}
	if measureReceiptStartupContext(t.TempDir(), specEnv) != nil {
		t.Fatal("Spec.Env BUN_OPTIONS retained receipt admission")
	}
}

func TestReceiptStartupContextAllowsConfigFilesForAuthenticatedNoAutoloadBuild(t *testing.T) {
	for _, name := range []string{
		"bunfig.toml",
		".env",
		".env.local",
		".env.production",
		".env.development",
		".env.test",
		".env.staging",
	} {
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			if err := os.WriteFile(filepath.Join(cwd, name), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if got := measureReceiptStartupContext(cwd, []string{"PATH=/usr/bin", "NODE_ENV=staging"}); got == nil {
				t.Fatalf("inert config file %s withheld receipt admission from the authenticated no-autoload build", name)
			}
		})
	}
}

func TestReceiptStartupContextAllowsBenignPlatformWorktreeDotEnv(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, ".env.local"), []byte("E2E_PORT=4310\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := measureReceiptStartupContext(cwd, []string{"PATH=/usr/bin"}); got == nil {
		t.Fatal("benign E2E_PORT-only .env.local withheld the authenticated no-autoload profile")
	}
}

func TestReceiptStartupLeaseAllowsConfigAppearanceButDetectsEnvironmentAndCwdChange(t *testing.T) {
	cwd := t.TempDir()
	env := []string{"PATH=/usr/bin", "SAFE=value"}
	lease := measureReceiptStartupContext(cwd, env)
	if lease == nil || lease.revalidate() != nil || !lease.matchesEnv(env) {
		t.Fatal("safe startup context was not retained")
	}
	if lease.matchesEnv(append(env, "OTHER=value")) {
		t.Fatal("changed child environment matched retained startup lease")
	}
	if err := os.WriteFile(filepath.Join(cwd, "bunfig.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lease.revalidate(); err != nil {
		t.Fatalf("authenticated no-autoload build treated inert bunfig.toml appearance as startup code: %v", err)
	}
	moved := cwd + ".moved"
	if err := os.Rename(cwd, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := lease.revalidate(); err == nil {
		t.Fatal("replacement workarea directory retained startup admission")
	}
}

func TestUnsafeStartupContextWithholdsReceiptWithoutBlockingLegacySession(t *testing.T) {
	base, _, _ := testReceiptAdmission(t)
	admission, err := newReceiptAdmission(
		base.artifact,
		newSessionLayout(t.TempDir()),
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("unsafe startup context blocked legacy session construction: %v", err)
	}
	if admission != nil {
		t.Fatal("unsafe startup context retained receipt admission")
	}
}

func TestUnsafeStartupEnvironmentPreservesScriptedLegacySpawn(t *testing.T) {
	_, handle, err := spawnScripted(
		t,
		agent.Spec{
			Prompt:     "legacy session",
			Cwd:        t.TempDir(),
			Autonomous: true,
			Env: map[string]string{
				"BUN_OPTIONS":                "--smol",
				"DYLD_FUTURE_LOADER_CONTROL": "benign-control-value",
			},
		},
		handshakeEvent("startup-handshake"),
		getStateResponse("legacy-startup")+event(map[string]any{"type": "agent_settled"}),
	)
	if err != nil {
		t.Fatalf("legacy Spawn with unreviewed startup environment: %v", err)
	}
	if handle.(*Handle).receipt != nil {
		t.Fatal("legacy session with unreviewed startup environment retained receipt admission")
	}
	events := drain(t, handle)
	if len(events) == 0 {
		t.Fatal("legacy session produced no events")
	}
}
