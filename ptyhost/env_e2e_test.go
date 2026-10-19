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

// TestSpawnE2E_ChildDecodesUTF8WithoutALocaleFromTheDaemon spawns a REAL PTY
// child with a launchd-like environment (no LANG, LC_ALL or LC_CTYPE anywhere
// it can inherit) and asserts the child's own locale stack decodes multi-byte
// output as characters, not bytes: printf of the session's 8 box-and-quote
// glyphs piped to
// wc -m counts 8 characters. An LC_ALL=C control in the same parent counts 22
// (one per byte), so the assertion discriminates the codeset instead of merely
// proving the pipe works.
//
// The parent locale is scrubbed process-wide for the test (t.Setenv with the
// empty value unsets the name for the child on unix), and the parent path
// below is explicit about WHICH locale names are scrubbed: unset LANG,
// LC_ALL and LC_CTYPE in os.Unsetenv terms via Setenv("", ...). A bare LANG=
// also reaches composeEnv through the request Env on one sub-case, pinning
// that an empty locale does not suppress the LANG=C.UTF-8 default.
//
// RED proof: drop the LANG default from composeEnv and the default sub-cases
// fail with 22 instead of 8; force LANG=C.UTF-8 past an explicit LC_ALL=C and
// the control fails with 8 instead of 22.
func TestSpawnE2E_ChildDecodesUTF8WithoutALocaleFromTheDaemon(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}

	// Not parallel with the scrub below: the parent locale names are cleared
	// process-wide for the duration of the test.
	for _, key := range []string{"LANG", "LC_ALL", "LC_CTYPE", "LANGUAGE"} {
		t.Setenv(key, "")
	}

	tests := []struct {
		name string
		env  []string
		want string
	}{
		{
			name: "no locale from the daemon decodes UTF-8 as characters",
			env:  nil,
			want: "8",
		},
		{
			name: "bare LANG from the request still decodes UTF-8 as characters",
			env:  []string{"LANG="},
			want: "8",
		},
		{
			name: "explicit LC_ALL=C keeps single-byte decoding",
			env:  []string{"LC_ALL=C"},
			want: "22",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resultPath := filepath.Join(t.TempDir(), "wc-result")
			// printf emits the 8 glyphs (22 UTF-8 bytes: five box-drawing
			// runes, an em dash, a right single quote, an ASCII quote and
			// an ellipsis) without a trailing newline; wc -m counts
			// CHARACTERS under a UTF-8 locale (8) and BYTES under the C
			// locale (22). tr -d strips wc's padding and
			// the newline so the file holds exactly the count.
			sess, err := Spawn(Spec{
				Command: []string{
					"/bin/sh", "-c",
					`printf '\342\224\214\342\224\200\342\224\254\342\224\202\342\200\224\342\200\231\042\342\200\246' | wc -m | tr -d ' \n' > "$1"`,
					"sh", resultPath,
				},
				Env: tt.env,
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
				t.Fatalf("read child wc result: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("child wc -m = %q, want %q (request env %q)", got, tt.want, tt.env)
			}
		})
	}
}
