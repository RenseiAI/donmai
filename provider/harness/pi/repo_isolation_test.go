package pi

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// This file pins the pi lane's repo-content isolation (the FU4 half): repo
// content under the session working directory must not steer the session,
// and project-local agent configuration must not override the harness's
// own provider registration.
//
// The posture under test is structural, not adversarial-luck: the child
// reads its session state from the per-session state root
// (--session-dir, PI_CODING_AGENT_DIR, PI_CODING_AGENT_SESSION_DIR — all
// harness-composed, all pointing beside the checkout), its provider pin
// from CLI argv (--provider/--model) plus the harness-composed
// DONMAI_PI_* env, and its environment from the allowlist (ExactEnv —
// nothing inherited past it). Repo content at the legacy in-checkout
// directory or a planted config-home redirect therefore has no rail into
// the child. Each test drives the PRODUCTION interactive Spawn against a
// fake binary and reads back what the child actually received.

// captureIsolationScript records the argv and the complete child env, plus
// the resolved config/session-home values, for the isolation assertions.
const captureIsolationScript = `
printf '%s\n' "$@" > "$PWD/argv.txt"
{
  env
  printf 'RESOLVED_AGENTDIR:%s\n' "$PI_CODING_AGENT_DIR"
  printf 'RESOLVED_SESSIONDIR:%s\n' "$PI_CODING_AGENT_SESSION_DIR"
} > "$PWD/pty-env.txt"
`

// TestSpawn_Interactive_RepoAgentDirCannotPreseedSessionState proves repo
// content cannot pre-seed the per-session agent dir: a checked-out tree
// carrying a planted agent-home/auth.json (with a foreign key) and planted
// settings at the legacy in-checkout directory leaves the child's resolved
// config home, session dir, and provider pin untouched. RED proof: point
// the harness at the planted directory and the foreign key surfaces.
func TestSpawn_Interactive_RepoAgentDirCannotPreseedSessionState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	workdir := t.TempDir()
	planted := filepath.Join(workdir, ".pi", "agent-home")
	if err := os.MkdirAll(planted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planted, "auth.json"), []byte(`{"evil":{"type":"api_key","key":"planted-repo-key"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, ".pi", "settings.json"), []byte(`{"model":"planted-repo-model"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	p := newFakeInteractivePiProvider(t, captureIsolationScript)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Model:       "session-model",
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	awaitPTYExit(t, h)

	env := readCapturedFile(t, workdir, "pty-env.txt")
	for _, planted := range []string{"planted-repo-key", "planted-repo-model"} {
		if strings.Contains(env, planted) {
			t.Errorf("repo-planted content %q reaches the interactive child", planted)
		}
	}
	var agentDir, sessionDir string
	for _, line := range strings.Split(env, "\n") {
		if v, ok := strings.CutPrefix(line, "RESOLVED_AGENTDIR:"); ok {
			agentDir = v
		}
		if v, ok := strings.CutPrefix(line, "RESOLVED_SESSIONDIR:"); ok {
			sessionDir = v
		}
	}
	if !strings.HasPrefix(agentDir, sessionStateRoot(agent.Spec{Cwd: workdir})) {
		t.Errorf("resolved agent dir %q is not under the session state root — a planted checkout dir could steer it", agentDir)
	}
	if !strings.HasPrefix(sessionDir, sessionStateRoot(agent.Spec{Cwd: workdir})) {
		t.Errorf("resolved session dir %q is not under the session state root", sessionDir)
	}
	argv := argvLines(readCapturedFile(t, workdir, "argv.txt"))
	if model, ok := flagValue(argv, "--model"); !ok || model != "session-model" {
		t.Errorf("--model = %q (present=%v), want the session model, not repo content; argv: %q", model, ok, argv)
	}
}

// TestSpawn_Interactive_PlantedConfigHomeRedirectIsIgnored proves a
// project-local config-home redirect cannot override the provider
// registration: Spec.Env carrying a planted PI_CODING_AGENT_DIR is
// replaced by the harness-composed per-session agent home, so the child
// never reads the planted directory. RED proof: copy Spec.Env verbatim
// into the child env and the planted path surfaces.
func TestSpawn_Interactive_PlantedConfigHomeRedirectIsIgnored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	workdir := t.TempDir()
	planted := filepath.Join(workdir, ".pi", "planted-home")
	if err := os.MkdirAll(planted, 0o755); err != nil {
		t.Fatal(err)
	}

	p := newFakeInteractivePiProvider(t, captureIsolationScript)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Model:       "session-model",
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		Env:         map[string]string{piCodingAgentDirEnvVar: planted},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	awaitPTYExit(t, h)

	env := readCapturedFile(t, workdir, "pty-env.txt")
	if strings.Contains(env, planted) {
		t.Errorf("planted %s=%q reaches the interactive child; the harness-composed home must win", piCodingAgentDirEnvVar, planted)
	}
	var agentDir string
	for _, line := range strings.Split(env, "\n") {
		if v, ok := strings.CutPrefix(line, "RESOLVED_AGENTDIR:"); ok {
			agentDir = v
		}
	}
	if agentDir == planted {
		t.Errorf("resolved agent dir is the planted path %q; the harness-composed home must win", planted)
	}
	if !strings.HasPrefix(agentDir, sessionStateRoot(agent.Spec{Cwd: workdir})) {
		t.Errorf("resolved agent dir %q is not under the session state root", agentDir)
	}
}
