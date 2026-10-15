package ptyhost

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

func TestSpawnE2E_SessionReadTokenSurvivesNoPTYChild(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}

	// The worker consumed the per-session read credential during bootstrap;
	// a stale copy lingering in the parent environment must not reach an
	// interactive PTY child through the inherited layer.
	t.Setenv(runtimeenv.SessionReadTokenEnv, "stale-read-credential")

	resultPath := filepath.Join(t.TempDir(), "env-result")
	sess, err := Spawn(Spec{
		Command: []string{
			"/bin/sh", "-c",
			`if [ -z "${DONMAI_SESSION_READ_TOKEN+x}" ]; then printf clean > "$1"; else printf leaked > "$1"; fi`,
			"sh", resultPath,
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
		t.Fatal("PTY child observed the per-session read credential")
	}
}

// TestSpawnE2E_ChildDecodesUTF8WithoutALocaleFromTheDaemon spawns a real PTY
// child with the environment a launchd daemon hands it (no LANG, LC_ALL or
// LC_CTYPE) and has it count the characters of "┌─┬│—’“…" with `wc -m`, which
// decodes through the locale the way macOS pbcopy does. Eight characters
// means the child decodes UTF-8; 24 means it read bytes, which is how a
// harness's clipboard copy turned "┌" into "‚îå". The LC_ALL=C control proves
// the count follows the locale, so the assertion discriminates.
func TestSpawnE2E_ChildDecodesUTF8WithoutALocaleFromTheDaemon(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	count := func(env ...string) string {
		resultPath := filepath.Join(t.TempDir(), "wc-result")
		sess, err := Spawn(Spec{
			Command: []string{
				"/bin/sh", "-c", `printf '%s' '┌─┬│—’“…' | wc -m > "$1"`,
				"sh", resultPath,
			},
			Env:      append([]string{"PATH=/usr/bin:/bin"}, env...),
			ExactEnv: true,
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
			t.Fatalf("read child result: %v", err)
		}
		return strings.TrimSpace(string(got))
	}

	if got := count(); got != "8" {
		t.Errorf("child with no locale counted %s characters in an 8-character UTF-8 string, want 8", got)
	}
	if got := count("LC_ALL=C"); got != "24" {
		t.Errorf("control: child with LC_ALL=C counted %s, want 24 bytes; the count does not follow the locale", got)
	}
}
