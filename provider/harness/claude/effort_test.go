package claude

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TestWithEffortEnv pins the environment half of the effort contract: the
// session's configured effort is written to CLAUDE_CODE_EFFORT_LEVEL, and with
// none configured the variable selects the model's own default ("auto") — so
// neither the operator's saved effortLevel nor an inherited value can stand in
// for configuration. The input map is never mutated.
func TestWithEffortEnv(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		env    map[string]string
		effort agent.EffortLevel
		want   string
	}{
		{name: "not configured selects the model default", effort: "", want: "auto"},
		{name: "configured max", effort: agent.EffortMax, want: "max"},
		{name: "configured xhigh", effort: agent.EffortXHigh, want: "xhigh"},
		{name: "configured low", effort: agent.EffortLow, want: "low"},
		{name: "inherited value is replaced when not configured", env: map[string]string{effortEnvVar: "max"}, effort: "", want: "auto"},
		{name: "inherited value is replaced by the configured one", env: map[string]string{effortEnvVar: "low"}, effort: agent.EffortHigh, want: "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before := envFingerprint(tc.env)
			got := withEffortEnv(tc.env, tc.effort)
			if got[effortEnvVar] != tc.want {
				t.Errorf("%s = %q, want %q", effortEnvVar, got[effortEnvVar], tc.want)
			}
			if envFingerprint(tc.env) != before {
				t.Errorf("withEffortEnv mutated its input: %v", tc.env)
			}
		})
	}
	kept := withEffortEnv(map[string]string{"ANTHROPIC_BASE_URL": "https://example.test"}, agent.EffortHigh)
	if kept["ANTHROPIC_BASE_URL"] != "https://example.test" {
		t.Errorf("withEffortEnv dropped an unrelated key: %v", kept)
	}
}

func envFingerprint(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k+"="+m[k])
	}
	slices.Sort(keys)
	return strings.Join(keys, ",")
}

// TestInteractiveArgs_EffortMatchesHeadless pins the interactive REPL's
// --effort flag to the headless mapping: a configured effort reaches both
// spawn modes identically, and none emits no flag in either.
func TestInteractiveArgs_EffortMatchesHeadless(t *testing.T) {
	t.Parallel()
	effortOf := func(argv []string) (string, bool) {
		i := slices.Index(argv, "--effort")
		if i < 0 || i+1 >= len(argv) {
			return "", false
		}
		return argv[i+1], true
	}
	for _, effort := range []agent.EffortLevel{agent.EffortHigh, agent.EffortXHigh, agent.EffortMax} {
		spec := agent.Spec{Prompt: "ship it", Effort: effort}
		headless, _ := buildArgs(spec, "", "")
		headlessEffort, ok := effortOf(headless)
		if !ok || headlessEffort != string(effort) {
			t.Fatalf("headless buildArgs effort = %q (present=%t), want %q", headlessEffort, ok, effort)
		}
		interactive := interactiveArgs(spec)
		got, ok := effortOf(interactive)
		if !ok || got != string(effort) {
			t.Errorf("interactiveArgs effort = %q (present=%t), want %q: %q", got, ok, effort, interactive)
		}
		if interactive[len(interactive)-1] != "ship it" {
			t.Errorf("positional prompt is no longer last: %q", interactive)
		}
	}
	if argv := interactiveArgs(agent.Spec{Prompt: "ship it"}); slices.Contains(argv, "--effort") {
		t.Errorf("interactiveArgs emitted --effort with none configured: %q", argv)
	}
}

// TestSpawn_Interactive_FixesEffortEnv drives Spawn end to end through the PTY
// lane with a fake claude that records CLAUDE_CODE_EFFORT_LEVEL and its argv:
// the configured effort reaches the child's environment and flag, and an
// unconfigured session runs with "auto" (the model default) and no flag.
func TestSpawn_Interactive_FixesEffortEnv(t *testing.T) {
	t.Parallel()
	cases := []struct {
		effort   agent.EffortLevel
		wantEnv  string
		wantFlag bool
	}{
		{effort: agent.EffortMax, wantEnv: "max", wantFlag: true},
		{effort: "", wantEnv: "auto"},
	}
	for _, tc := range cases {
		t.Run("effort="+string(tc.effort), func(t *testing.T) {
			t.Parallel()
			workdir := t.TempDir()
			p := newFakeInteractiveProvider(t, `
printf '%s' "$CLAUDE_CODE_EFFORT_LEVEL" > "$PWD/effort-env"
printf '%s\n' "$@" > "$PWD/argv"
`)
			h, err := p.Spawn(context.Background(), agent.Spec{
				Prompt:      "hello",
				Cwd:         workdir,
				Effort:      tc.effort,
				Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
			})
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			waitForExit(t, h)
			gotEnv, err := os.ReadFile(filepath.Join(workdir, "effort-env"))
			if err != nil {
				t.Fatalf("read recorded effort env: %v", err)
			}
			if string(gotEnv) != tc.wantEnv {
				t.Errorf("child %s = %q, want %q", effortEnvVar, gotEnv, tc.wantEnv)
			}
			argv, err := os.ReadFile(filepath.Join(workdir, "argv"))
			if err != nil {
				t.Fatalf("read recorded argv: %v", err)
			}
			hasFlag := slices.Contains(strings.Split(string(argv), "\n"), "--effort")
			if hasFlag != tc.wantFlag {
				t.Errorf("child argv --effort present=%t, want %t: %q", hasFlag, tc.wantFlag, argv)
			}
		})
	}
}

func waitForExit(t *testing.T, h agent.Handle) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case _, ok := <-h.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the fake claude to exit")
		}
	}
}

// TestOneShotEnv_FixesEffort pins the one-shot lane to the same rule: the
// request's effort, or the model default when it carries none, with or
// without a bound endpoint.
func TestOneShotEnv_FixesEffort(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  agent.OneShotRequest
		want string
	}{
		{name: "no endpoint, no effort", req: agent.OneShotRequest{}, want: "auto"},
		{name: "no endpoint, max", req: agent.OneShotRequest{Effort: agent.EffortMax}, want: "max"},
		{name: "direct endpoint, xhigh", req: agent.OneShotRequest{Effort: agent.EffortXHigh, Endpoint: &agent.EndpointBinding{Host: agent.HostDirect}}, want: "xhigh"},
	}
	for _, tc := range cases {
		if got := oneShotEnv(tc.req)[effortEnvVar]; got != tc.want {
			t.Errorf("%s: oneShotEnv %s = %q, want %q", tc.name, effortEnvVar, got, tc.want)
		}
	}
}
