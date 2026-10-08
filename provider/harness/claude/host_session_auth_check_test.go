package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func TestCheckHostSessionLogin_RestrictedNativeHome(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(home, "xdg")
	config := filepath.Join(home, "claude-config")
	t.Setenv("HOME", home)
	t.Setenv("USER", "probe-operator")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	for _, name := range []string{"GITHUB_TOKEN", "DONMAI_DAEMON_JWT", "NODE_OPTIONS", "HTTP_PROXY", "OPENAI_API_KEY"} {
		t.Setenv(name, "synthetic-must-not-inherit")
	}
	script := fmt.Sprintf(`
[ "$1" = auth ] && [ "$2" = status ] && [ "$3" = --json ] || exit 21
[ "$HOME" = %q ] || exit 22
[ "$XDG_CONFIG_HOME" = %q ] || exit 23
[ "$CLAUDE_CONFIG_DIR" = %q ] || exit 24
[ "$(pwd -P)" != "$HOME" ] || exit 25
[ "$USER" = "probe-operator" ] || exit 29
[ -n "$TMPDIR" ] && [ "$XDG_CACHE_HOME" = "$TMPDIR" ] || exit 26
[ -z "${GITHUB_TOKEN+x}" ] && [ -z "${DONMAI_DAEMON_JWT+x}" ] || exit 27
[ -z "${NODE_OPTIONS+x}" ] && [ -z "${HTTP_PROXY+x}" ] && [ -z "${OPENAI_API_KEY+x}" ] || exit 28
printf '{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}\n'
`, home, xdg, config)
	binary := writeFakeCLI(t, "synthetic-claude-login", script)
	if err := CheckHostSessionLogin(t.Context(), binary); err != nil {
		t.Fatalf("restricted native host login refused: %v", err)
	}
}

func TestCheckHostSessionLogin_RefusesUnverifiedStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, output := range []string{"{}", `{"loggedIn":true,"authMethod":"api_key","apiProvider":"firstParty"}`, `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"thirdParty"}`, strings.Repeat("x", hostLoginOutputMax+1)} {
		binary := writeFakeCLI(t, "synthetic-claude-login", "printf '%s\\n' '"+output+"'")
		if err := CheckHostSessionLogin(t.Context(), binary); !errors.Is(err, agent.ErrProviderUnavailable) {
			t.Fatalf("unverified status accepted: %v", err)
		}
	}
}

func TestCheckHostSessionLogin_BoundsOwnedChildAndCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owned process group requires unix")
	}
	t.Setenv("HOME", t.TempDir())
	binary := writeFakeCLI(t, "synthetic-claude-login", `
(sleep 0.4; printf survived > "$0.child-survived") &
printf '{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}\n'
`)
	if err := CheckHostSessionLogin(t.Context(), binary); err != nil {
		t.Fatalf("owned login status: %v", err)
	}
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(binary + ".fixture.child-survived"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned login child survived: %v", err)
	}
	busy := writeFakeCLI(t, "synthetic-claude-busy", "while :; do :; done")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := CheckHostSessionLogin(ctx, busy); !errors.Is(err, agent.ErrProviderUnavailable) {
		t.Fatalf("timed-out login status: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("login status held caller for %s", elapsed)
	}
}

func TestCheckHostSessionLogin_PreservesRelativeConfigLocators(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "relative-claude-config")
	t.Setenv("XDG_CONFIG_HOME", "relative-xdg-config")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	claudeConfig := filepath.Join(cwd, "relative-claude-config")
	xdgConfig := filepath.Join(cwd, "relative-xdg-config")
	script := fmt.Sprintf(`
[ "$1" = auth ] && [ "$2" = status ] && [ "$3" = --json ] || exit 21
[ "$HOME" = %q ] || exit 22
[ "$CLAUDE_CONFIG_DIR" = %q ] || exit 23
[ "$XDG_CONFIG_HOME" = %q ] || exit 24
[ "$(pwd -P)" != %q ] || exit 25
printf '{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}\n'
`, home, claudeConfig, xdgConfig, cwd)
	binary := writeFakeCLI(t, "synthetic-claude-relative-login", script)
	if err := CheckHostSessionLogin(t.Context(), binary); err != nil {
		t.Fatalf("relative native config locator lost: %v", err)
	}
}
