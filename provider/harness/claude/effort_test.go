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
		{name: "unrecognised value selects the model default", effort: agent.EffortLevel("none"), want: "auto"},
		{name: "another harness naming selects the model default", effort: agent.EffortLevel("minimal"), want: "auto"},
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

// TestForceEffortEnv pins the stamped-headless gate: only a session carrying
// an execution-security stamp that is not interactive gets the forced effort
// environment variable. Interactive sessions are stamped too, so the stamp
// alone is not enough — the operator keeps their own effort control there.
func TestForceEffortEnv(t *testing.T) {
	t.Parallel()
	stamped := &agent.ExecutionSecurity{Version: 1, Levels: agent.IndexZeroExecutionSecurityLevels()}
	cases := []struct {
		name string
		spec agent.Spec
		want bool
	}{
		{name: "stamped headless forces", spec: agent.Spec{ExecutionSecurity: stamped}, want: true},
		{name: "stamped interactive does not force", spec: agent.Spec{ExecutionSecurity: stamped, Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24}}, want: false},
		{name: "unstamped standalone does not force", spec: agent.Spec{}, want: false},
		{name: "unstamped interactive does not force", spec: agent.Spec{Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24}}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := forceEffortEnv(tc.spec); got != tc.want {
				t.Errorf("forceEffortEnv(%+v) = %t, want %t", tc.spec, got, tc.want)
			}
		})
	}
}

// TestSpawn_Interactive_DoesNotForceEffortEnv drives Spawn end to end through
// the PTY lane with a fake claude that records its argv and whether
// CLAUDE_CODE_EFFORT_LEVEL is set at all: a stamped interactive session keeps
// the --effort flag when a level is known, but the environment variable is
// never forced — neither to the configured level nor to "auto". Restoring
// the unconditional withEffortEnv call in Spawn turns this red.
func TestSpawn_Interactive_DoesNotForceEffortEnv(t *testing.T) {
	t.Parallel()
	stamped := &agent.ExecutionSecurity{Version: 1, Levels: agent.IndexZeroExecutionSecurityLevels()}
	cases := []struct {
		effort   agent.EffortLevel
		wantFlag bool
		wantVal  string
	}{
		{effort: agent.EffortMax, wantFlag: true, wantVal: "max"},
		{effort: ""},
	}
	for _, tc := range cases {
		t.Run("effort="+string(tc.effort), func(t *testing.T) {
			t.Parallel()
			workdir := t.TempDir()
			p := newFakeInteractiveProvider(t, `
if [ -z "${CLAUDE_CODE_EFFORT_LEVEL+x}" ]; then
  printf 'unset' > "$PWD/effort-env"
else
  printf 'set:%s' "$CLAUDE_CODE_EFFORT_LEVEL" > "$PWD/effort-env"
fi
printf '%s\n' "$@" > "$PWD/argv"
`)
			h, err := p.Spawn(context.Background(), agent.Spec{
				Prompt:            "hello",
				Cwd:               workdir,
				Effort:            tc.effort,
				ExecutionSecurity: stamped,
				Interactive:       &agent.InteractiveSpec{Cols: 80, Rows: 24},
			})
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			waitForExit(t, h)
			gotEnv, err := os.ReadFile(filepath.Join(workdir, "effort-env"))
			if err != nil {
				t.Fatalf("read recorded effort env: %v", err)
			}
			if string(gotEnv) != "unset" {
				t.Errorf("child %s = %q, want unset (never forced on interactive sessions)", effortEnvVar, gotEnv)
			}
			argv, err := os.ReadFile(filepath.Join(workdir, "argv"))
			if err != nil {
				t.Fatalf("read recorded argv: %v", err)
			}
			argvLines := strings.Split(string(argv), "\n")
			hasFlag := slices.Contains(argvLines, "--effort")
			if hasFlag != tc.wantFlag {
				t.Errorf("child argv --effort present=%t, want %t: %q", hasFlag, tc.wantFlag, argv)
			}
			if tc.wantFlag {
				i := slices.Index(argvLines, "--effort")
				if i+1 >= len(argvLines) || argvLines[i+1] != tc.wantVal {
					t.Errorf("child argv --effort value = %q, want %q: %q", argvLines[i+1], tc.wantVal, argv)
				}
			}
		})
	}
}

// TestSpawn_Headless_ForcesEffortEnvWhenStamped drives Spawn end to end
// through the headless lane with a fake CLI that records whether
// CLAUDE_CODE_EFFORT_LEVEL is set: a stamped headless spawn carries the
// variable (the configured level, or "auto" when none is configured),
// while an unstamped standalone spawn leaves it unset so the operator's own
// effort control stands. Restoring the unconditional withEffortEnv call in
// Spawn turns the unstamped case red.
func TestSpawn_Headless_ForcesEffortEnvWhenStamped(t *testing.T) {
	t.Parallel()
	stamped := &agent.ExecutionSecurity{Version: 1, Levels: agent.IndexZeroExecutionSecurityLevels()}
	cases := []struct {
		name     string
		spec     agent.Spec
		wantEff  string // "unset", or the exact forced value
		wantFlag bool
	}{
		{name: "stamped headless with effort", spec: agent.Spec{Prompt: "hello", Effort: agent.EffortMax, ExecutionSecurity: stamped}, wantEff: "max", wantFlag: true},
		{name: "stamped headless without effort", spec: agent.Spec{Prompt: "hello", ExecutionSecurity: stamped}, wantEff: "auto"},
		{name: "unstamped standalone with effort", spec: agent.Spec{Prompt: "hello", Effort: agent.EffortMax}, wantEff: "unset", wantFlag: true},
		{name: "unstamped standalone without effort", spec: agent.Spec{Prompt: "hello"}, wantEff: "unset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			cli := writeFakeCLI(t, "fake-claude-effort.sh",
				"#!/bin/sh\n"+
					"if [ -z \"${CLAUDE_CODE_EFFORT_LEVEL+x}\" ]; then\n"+
					"  printf 'unset' > "+shQuote(filepath.Join(dir, "effort-env"))+"\n"+
					"else\n"+
					"  printf 'set:%s' \"$CLAUDE_CODE_EFFORT_LEVEL\" > "+shQuote(filepath.Join(dir, "effort-env"))+"\n"+
					"fi\n"+
					"printf '%s\\n' \"$@\" > "+shQuote(filepath.Join(dir, "argv"))+"\n"+
					`printf '{"type":"system","subtype":"init","session_id":"sess-effort-1"}\n'`+"\n"+
					`printf '{"type":"result","subtype":"success","is_error":false,"num_turns":1}\n'`+"\n")
			p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			tc.spec.Cwd = dir
			h, err := p.Spawn(t.Context(), tc.spec)
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			t.Cleanup(func() { _ = h.Stop(t.Context()) })
			_ = drainAllWithIdle(t, h.Events(), 5*time.Second, 45*time.Second)
			gotEnv, err := os.ReadFile(filepath.Join(dir, "effort-env"))
			if err != nil {
				t.Fatalf("read recorded effort env: %v", err)
			}
			want := tc.wantEff
			if want != "unset" {
				want = "set:" + want
			}
			if string(gotEnv) != want {
				t.Errorf("child %s = %q, want %q", effortEnvVar, gotEnv, want)
			}
			argv := readLines(t, filepath.Join(dir, "argv"))
			hasFlag := slices.Contains(argv, "--effort")
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

// TestOneShotEnv_NeverForcesEffort pins the one-shot lane's rule: one-shot
// calls carry no execution-security stamp, so the effort environment variable
// is never forced — with or without a bound endpoint. The request's --effort
// flag (see buildOneShotArgs) is the only effort signal. Restoring the
// withEffortEnv call in oneShotEnv turns this red.
func TestOneShotEnv_NeverForcesEffort(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  agent.OneShotRequest
	}{
		{name: "no endpoint, no effort", req: agent.OneShotRequest{}},
		{name: "no endpoint, max", req: agent.OneShotRequest{Effort: agent.EffortMax}},
		{name: "direct endpoint, xhigh", req: agent.OneShotRequest{Effort: agent.EffortXHigh, Endpoint: &agent.EndpointBinding{Host: agent.HostDirect}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, forced := oneShotEnv(tc.req)[effortEnvVar]; forced {
				t.Errorf("%s: oneShotEnv forces %s, want unset (one-shot calls carry no stamp)", tc.name, effortEnvVar)
			}
		})
	}
}

// TestOneShotEnv_ProjectsEndpointWithoutEffort pins that dropping the forced
// variable did not drop the endpoint projection: a bound endpoint still
// contributes its serving-host env, just without the effort variable.
func TestOneShotEnv_ProjectsEndpointWithoutEffort(t *testing.T) {
	t.Parallel()
	env := oneShotEnv(agent.OneShotRequest{
		Endpoint: &agent.EndpointBinding{
			Company: agent.CompanyAnthropic,
			Host:    agent.HostBedrock,
			Region:  "us-east-1",
			Env:     map[string]string{"AWS_ACCESS_KEY_ID": "AKIATEST"},
		},
	})
	if _, forced := env[effortEnvVar]; forced {
		t.Errorf("oneShotEnv forces %s, want unset", effortEnvVar)
	}
	if env[EnvUseBedrock] != "1" {
		t.Errorf("oneShotEnv lost the bedrock projection: %v", env)
	}
}
