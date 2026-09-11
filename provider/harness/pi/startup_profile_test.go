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
	if !hasEnvVal(cmd.Env, "DONMAI_HARMLESS_VAR", "preserved") {
		t.Fatal("version probe dropped an unrelated host environment entry")
	}
}

func TestReceiptStartupContextRejectsRuntimeAndNativeLoaderEnvironment(t *testing.T) {
	for _, key := range []string{
		"BUN_OPTIONS",
		"BUN_BE_BUN",
		"NODE_OPTIONS",
		"DYLD_INSERT_LIBRARIES",
		"DYLD_LIBRARY_PATH",
		"DYLD_FRAMEWORK_PATH",
		"DYLD_FALLBACK_LIBRARY_PATH",
		"DYLD_FALLBACK_FRAMEWORK_PATH",
		"LD_PRELOAD",
		"LD_LIBRARY_PATH",
	} {
		t.Run(key, func(t *testing.T) {
			if got := measureReceiptStartupContext(t.TempDir(), []string{key + "=benign-control-value"}); got != nil {
				t.Fatalf("startup environment %s retained receipt admission", key)
			}
		})
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

func TestReceiptStartupContextRejectsBunAutoloadFiles(t *testing.T) {
	for _, name := range receiptAutoloadConfigNames {
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			if err := os.WriteFile(filepath.Join(cwd, name), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if got := measureReceiptStartupContext(cwd, []string{"PATH=/usr/bin"}); got != nil {
				t.Fatalf("autoload file %s retained receipt admission", name)
			}
		})
	}

	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, ".env.staging"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := measureReceiptStartupContext(cwd, []string{"NODE_ENV=staging"}); got != nil {
		t.Fatal("NODE_ENV-selected dotenv file retained receipt admission")
	}
	if got := measureReceiptStartupContext(t.TempDir(), []string{"NODE_ENV=../outside"}); got != nil {
		t.Fatal("path-shaped NODE_ENV retained receipt admission")
	}
}

func TestReceiptStartupContextWithholdsForBenignPlatformWorktreeDotEnv(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, ".env.local"), []byte("E2E_PORT=4310\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := measureReceiptStartupContext(cwd, []string{"PATH=/usr/bin"}); got != nil {
		t.Fatal("benign E2E_PORT-only .env.local unexpectedly retained receipt admission")
	}
}

func TestReceiptStartupLeaseDetectsConfigAppearanceAndEnvironmentChange(t *testing.T) {
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
	if err := lease.revalidate(); err == nil {
		t.Fatal("bunfig.toml appearing after startup measurement was accepted")
	}

	customCwd := t.TempDir()
	custom := measureReceiptStartupContext(customCwd, []string{"NODE_ENV=staging"})
	if custom == nil {
		t.Fatal("safe custom NODE_ENV startup context was not retained")
	}
	if err := os.WriteFile(filepath.Join(customCwd, ".env.staging"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := custom.revalidate(); err == nil {
		t.Fatal("NODE_ENV-selected dotenv appearing after measurement was accepted")
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
			Env:        map[string]string{"BUN_OPTIONS": "--smol"},
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
