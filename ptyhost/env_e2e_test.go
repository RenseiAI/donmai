package ptyhost

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

func TestSpawnE2E_RuntimeBlocklistSurvivesPTYSpawn(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}

	t.Setenv("ANTHROPIC_API_KEY", "parent-secret")
	t.Setenv("GITHUB_TOKEN", "github-token")
	t.Setenv("HOME", "/tmp/ptyhost-home")
	t.Setenv("PATH", "/test/bin")

	// Match the runner's serving path: it composes the daemon environment before
	// the interactive harness passes the resulting env through to ptyhost.Spawn.
	composedEnv := runtimeenv.NewComposer().Compose(map[string]string{
		"ANTHROPIC_API_KEY": "parent-secret",
		"GITHUB_TOKEN":      "github-token",
		"HOME":              "/tmp/ptyhost-home",
		"PATH":              "/test/bin",
	}, agent.Spec{})

	resultPath := filepath.Join(t.TempDir(), "env-result")
	sess, err := Spawn(Spec{
		Command: []string{
			"/bin/sh", "-c",
			`if [ -z "$ANTHROPIC_API_KEY" ] && [ "$GITHUB_TOKEN" = github-token ] && [ "$HOME" = /tmp/ptyhost-home ] && [ "$PATH" = /test/bin ]; then printf clean > "$1"; else printf leaked > "$1"; fi`,
			"sh", resultPath,
		},
		Env: composedEnv,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = sess.Stop(context.Background()) })

	select {
	case <-sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("PTY child did not exit")
	}

	got, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read child env result: %v", err)
	}
	if string(got) != "clean" {
		t.Fatal("PTY child observed a blocked provider credential or lost a permitted env var")
	}
}

func TestSpawnE2E_ExplicitProviderCredentialOverridesFilteredParent(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}

	t.Setenv("ANTHROPIC_API_KEY", "parent-secret")

	composedEnv := runtimeenv.NewComposer().Compose(map[string]string{
		"ANTHROPIC_API_KEY": "parent-secret",
	}, agent.Spec{Env: map[string]string{
		"ANTHROPIC_API_KEY": "session-credential",
	}})

	resultPath := filepath.Join(t.TempDir(), "env-result")
	sess, err := Spawn(Spec{
		Command: []string{
			"/bin/sh", "-c",
			`if [ "$ANTHROPIC_API_KEY" = session-credential ]; then printf clean > "$1"; else printf leaked > "$1"; fi`,
			"sh", resultPath,
		},
		Env: composedEnv,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = sess.Stop(context.Background()) })

	select {
	case <-sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("PTY child did not exit")
	}

	got, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read child env result: %v", err)
	}
	if string(got) != "clean" {
		t.Fatal("PTY child did not receive the explicit provider credential")
	}
}

func TestSpawnE2E_ChildCannotObserveRunnerOnlyAttachControls(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}

	t.Setenv("ATTACH_TOKEN", "parent-secret")
	t.Setenv("ATTACH_TOKEN_FILE", "/parent/token")
	t.Setenv("ATTACH_URL", "wss://parent.invalid/v1/rooms/room-1")

	resultPath := filepath.Join(t.TempDir(), "env-result")
	sess, err := Spawn(Spec{
		Command: []string{
			"/bin/sh", "-c",
			`if [ "${ATTACH_TOKEN+x}${ATTACH_TOKEN_FILE+x}${ATTACH_URL+x}" = "" ]; then printf clean > "$1"; else printf leaked > "$1"; fi`,
			"sh", resultPath,
		},
		Env: []string{
			"ATTACH_TOKEN=override-secret",
			"ATTACH_TOKEN_FILE=/override/token",
			"ATTACH_URL=wss://override.invalid/v1/rooms/room-1",
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = sess.Stop(context.Background()) })

	select {
	case <-sess.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("PTY child did not exit")
	}

	got, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read child env result: %v", err)
	}
	if string(got) != "clean" {
		t.Fatal("PTY child observed runner-only attach controls")
	}
}
