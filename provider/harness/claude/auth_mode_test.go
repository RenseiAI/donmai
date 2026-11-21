package claude

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// fakeAuthEnvEchoCLI writes a fake claude CLI that echoes, as its assistant
// text, the three credential-surface variables it received. ${VAR-unset}
// distinguishes an absent variable from an empty one, so a blanked-but-present
// key cannot pass for a removed one.
func fakeAuthEnvEchoCLI(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI uses /bin/sh; skip on windows")
	}
	return writeFakeCLI(t, "fake-claude-auth.sh",
		"#!/bin/sh\n"+
			`printf '{"type":"system","subtype":"init","session_id":"sess-auth-1"}\n'`+"\n"+
			`printf '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"%s|%s|%s"}]}}\n' "${ANTHROPIC_API_KEY-unset}" "${ANTHROPIC_AUTH_TOKEN-unset}" "${ANTHROPIC_BASE_URL-unset}"`+"\n"+
			`printf '{"type":"result","subtype":"success","is_error":false,"num_turns":1}\n'`+"\n")
}

// TestProvider_Spawn_AuthModeKeysCredentialSurface proves the child's
// credential surface follows the session's resolved auth mode end to end
// (Spawn → applyAuthMode → composed env → subprocess).
//
// The inherited layer reproduces a supervising daemon that injected a
// platform-resolved key and declared it (runtime/env.InjectedEnvKeysVar), next
// to an operator shell's undeclared token and base URL. A host-login session
// must see none of them; a keyed session must see its declared key and
// nothing ambient.
//
// Not parallel: the inherited layer is the real process environment.
func TestProvider_Spawn_AuthModeKeysCredentialSurface(t *testing.T) {
	cases := []struct {
		name string
		mode agent.AuthMode
		// injected is the supervisor-declared inherited key ("" = none).
		injected string
		// ambientKey is an undeclared inherited ANTHROPIC_API_KEY ("" = none);
		// mutually exclusive with injected.
		ambientKey string
		specEnv    map[string]string
		want       string
	}{
		{
			name:     "host-session strips a declared injection, ambient vars and the spec layer",
			mode:     agent.AuthHostSession,
			injected: "declared-platform-key",
			specEnv:  map[string]string{"ANTHROPIC_API_KEY": "spec-key", "ANTHROPIC_BASE_URL": "https://redirect.invalid"},
			want:     "unset|unset|unset",
		},
		{
			name:       "local strips an undeclared ambient key",
			mode:       agent.AuthLocal,
			ambientKey: "ambient-shell-key",
			want:       "unset|unset|unset",
		},
		{
			name:     "metered keeps the declared injected key and drops ambient vars",
			mode:     agent.AuthMetered,
			injected: "declared-metered-key",
			want:     "declared-metered-key|unset|unset",
		},
		{
			name:     "byok keeps the declared injected key and drops ambient vars",
			mode:     agent.AuthBYOK,
			injected: "declared-byok-key",
			want:     "declared-byok-key|unset|unset",
		},
		{
			name:       "byok spec-layer credential wins over an ambient key",
			mode:       agent.AuthBYOK,
			ambientKey: "ambient-shell-key",
			specEnv:    map[string]string{"ANTHROPIC_API_KEY": "declared-byok-key"},
			want:       "declared-byok-key|unset|unset",
		},
		{
			name:     "unstamped auth mode keeps historical behavior",
			injected: "declared-legacy-key",
			want:     "declared-legacy-key|unset|unset",
		},
	}

	cli := fakeAuthEnvEchoCLI(t)
	p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// An operator shell's token and base URL, never declared.
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "ambient-shell-token")
			t.Setenv("ANTHROPIC_BASE_URL", "https://ambient-proxy.invalid")
			switch {
			case tc.injected != "":
				t.Setenv("ANTHROPIC_API_KEY", tc.injected)
				t.Setenv(runtimeenv.InjectedEnvKeysVar, "ANTHROPIC_API_KEY")
			case tc.ambientKey != "":
				t.Setenv("ANTHROPIC_API_KEY", tc.ambientKey)
				t.Setenv(runtimeenv.InjectedEnvKeysVar, "")
			default:
				t.Setenv(runtimeenv.InjectedEnvKeysVar, "")
			}

			if got := spawnAndReadText(t, p, agent.Spec{Prompt: "echo auth", AuthMode: tc.mode, Env: tc.specEnv}); got != tc.want {
				t.Errorf("child credential surface = %q, want %q", got, tc.want)
			}
		})
	}
}

func spawnAndReadText(t *testing.T, p *Provider, spec agent.Spec) string {
	t.Helper()
	h, err := p.Spawn(t.Context(), spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(t.Context()) })
	deadline := time.After(30 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatal("events channel closed before assistant text")
			}
			if e, isText := ev.(agent.AssistantTextEvent); isText {
				return e.Text
			}
		case <-deadline:
			t.Fatal("timed out waiting for assistant text")
		}
	}
}

// TestApplyAuthMode_DoesNotMutateInputEnv pins the copy-on-strip contract:
// the caller's Spec.Env map survives a host-login strip intact.
func TestApplyAuthMode_DoesNotMutateInputEnv(t *testing.T) {
	t.Parallel()
	in := map[string]string{"ANTHROPIC_API_KEY": "k", "FOO": "bar"}
	out, unset := applyAuthMode(agent.Spec{AuthMode: agent.AuthHostSession, Env: in})
	if _, ok := out.Env["ANTHROPIC_API_KEY"]; ok || out.Env["FOO"] != "bar" {
		t.Errorf("stripped env = %v, want FOO only", out.Env)
	}
	if len(in) != 2 || in["ANTHROPIC_API_KEY"] != "k" {
		t.Errorf("input env mutated: %v", in)
	}
	if len(unset) != len(hostLoginStrippedEnv) {
		t.Errorf("unset names = %v, want %v", unset, hostLoginStrippedEnv)
	}
}

// hostLoginRoutingScript is a fake claude CLI that records, per turn, the
// Bedrock switch it sees in its process env and on its --settings layer
// (the CLI's winning layer), plus the API key, into $PWD/<turn>.txt.
const hostLoginRoutingScript = `
settings_env=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "--settings" ]; then
    settings_env="$arg"
  fi
  prev="$arg"
done
case " $* " in
  *' --resume '*) out="resume.txt";;
  *) out="spawn.txt";;
esac
pinned=$(printf '%s' "$settings_env" | python3 -c 'import json,sys
try:
    env = json.loads(sys.stdin.read()).get("env", {})
except Exception:
    env = {}
print(env.get("CLAUDE_CODE_USE_BEDROCK", "unset"))' 2>/dev/null)
[ -n "$pinned" ] || pinned=unset
printf 'process=%s settings=%s key=%s' "${CLAUDE_CODE_USE_BEDROCK-unset}" "$pinned" "${ANTHROPIC_API_KEY-unset}" > "$PWD/$out"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"sess-host-login-1"}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"usage":{}}'
`

// TestProvider_HostLoginStripSurvivesRoutingPin covers the routing pin: an
// ambient Bedrock setup always carries a region name, and a region name made
// the pin read the Bedrock switch back from the parent process onto the
// CLI's winning --settings layer, routing a host-session session to Bedrock
// despite the strip. The switch must be absent from the process env AND the
// --settings layer on the fresh spawn, on an Inject --resume turn, and on a
// Provider.Resume restore.
//
// Not parallel: the inherited layer is the real process environment.
func TestProvider_HostLoginStripSurvivesRoutingPin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI uses /bin/sh; skip on windows")
	}
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("ANTHROPIC_API_KEY", "declared-platform-key")
	t.Setenv(runtimeenv.InjectedEnvKeysVar, "ANTHROPIC_API_KEY")

	cli := writeFakeCLI(t, "fake-claude-host-login-routing.sh", "#!/bin/sh\n"+hostLoginRoutingScript)
	p, err := New(Options{Binary: cli, LookPath: func(string) (string, error) { return cli, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const want = "process=unset settings=unset key=unset"
	// The runner composes the worker's own env into Spec.Env, so an ambient
	// Bedrock setup arrives on the spec layer too.
	spec := func(workdir string) agent.Spec {
		return agent.Spec{
			Prompt:   "first turn",
			Cwd:      workdir,
			AuthMode: agent.AuthHostSession,
			Env:      map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "AWS_REGION": "us-east-1"},
		}
	}
	read := func(t *testing.T, workdir, file string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(workdir, file))
		if err != nil {
			t.Fatalf("fake CLI wrote no %s: %v", file, err)
		}
		return string(raw)
	}

	t.Run("spawn and inject", func(t *testing.T) {
		workdir := t.TempDir()
		h, err := p.Spawn(t.Context(), spec(workdir))
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		t.Cleanup(func() { _ = h.Stop(context.Background()) })
		waitForHeadlessTerminal(t, h)
		if err := h.Inject(t.Context(), "steer this turn"); err != nil {
			t.Fatalf("Inject: %v", err)
		}
		waitForHeadlessTerminal(t, h)
		for _, file := range []string{"spawn.txt", "resume.txt"} {
			if got := read(t, workdir, file); got != want {
				t.Errorf("%s: child saw %q, want %q", file, got, want)
			}
		}
	})

	t.Run("resume", func(t *testing.T) {
		workdir := t.TempDir()
		h, err := p.Resume(t.Context(), "sess-host-login-1", spec(workdir))
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		t.Cleanup(func() { _ = h.Stop(context.Background()) })
		waitForHeadlessTerminal(t, h)
		if got := read(t, workdir, "resume.txt"); got != want {
			t.Errorf("Resume: child saw %q, want %q", got, want)
		}
	})
}
