package claude

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// captureEnvArgvScript is the fake-claude body every test in this file uses.
// It dumps its own argv (one element per line — printf cycles the format
// string once per positional parameter, so an argument containing spaces,
// e.g. a --settings JSON blob, still lands on exactly one line) and a fixed
// set of endpoint-routing env vars to files in the session's cwd, then exits
// immediately; no PTY interaction is needed for these assertions.
const captureEnvArgvScript = `
printf '%s\n' "$@" > "$PWD/argv.txt"
{
  printf 'ANTHROPIC_BASE_URL=%s\n' "$ANTHROPIC_BASE_URL"
  printf 'CLAUDE_CODE_USE_BEDROCK=%s\n' "$CLAUDE_CODE_USE_BEDROCK"
  printf 'AWS_REGION=%s\n' "$AWS_REGION"
  printf 'CLAUDE_CODE_USE_VERTEX=%s\n' "$CLAUDE_CODE_USE_VERTEX"
  printf 'CLOUD_ML_REGION=%s\n' "$CLOUD_ML_REGION"
} > "$PWD/env.txt"
`

// awaitPTYExit drains h.Events() until the channel closes, so the capture
// files the fake scripts in this file write are guaranteed present on disk
// by the time the test reads them.
func awaitPTYExit(t *testing.T, h agent.Handle) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case _, ok := <-h.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for fake claude exit")
		}
	}
}

func readCapturedFile(t *testing.T, workdir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(workdir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// argvLines splits the captured argv dump back into one string per original
// argument (see captureEnvArgvScript's doc comment for why this is
// line-based rather than whitespace-based).
func argvLines(captured string) []string {
	trimmed := strings.TrimRight(captured, "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// flagValue returns the argument immediately following flag in argv, mirrors
// the identical helper pattern used throughout interactive_test.go.
func flagValue(argv []string, flag string) (string, bool) {
	i := slices.Index(argv, flag)
	if i < 0 || i+1 >= len(argv) {
		return "", false
	}
	return argv[i+1], true
}

// TestSpawn_Interactive_EndpointBindingReachesPTYChild is the RED-first
// regression proof for the interactive endpoint projection defect (sibling
// of #323, one field over): a resolved Spec.Endpoint binding already reached
// the headless CLI subprocess (claude.go's spawn() called applyEndpoint),
// but the identical Spec silently lost the binding when Spec.Interactive
// routed it through spawnInteractive instead — applyEndpoint only ran inside
// spawn(), which interactive mode never reaches. Table-driven across every
// endpoint.go serving host that projects env (direct/bedrock/vertex),
// proving the binding's env knobs AND its Model override both now survive
// onto the PTY child.
func TestSpawn_Interactive_EndpointBindingReachesPTYChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}

	tests := []struct {
		name      string
		specModel string
		endpoint  *agent.EndpointBinding
		wantModel string // "" means --model must be ABSENT
		wantEnv   map[string]string
	}{
		{
			name:      "direct host: base URL projects and the binding's model wins",
			specModel: "claude-haiku", // must lose to Endpoint.Model
			endpoint: &agent.EndpointBinding{
				Company: agent.CompanyAnthropic,
				Model:   "claude-sonnet-4-5",
				Host:    agent.HostDirect,
				BaseURL: "https://gateway.invalid/v1",
			},
			wantModel: "claude-sonnet-4-5",
			wantEnv:   map[string]string{"ANTHROPIC_BASE_URL": "https://gateway.invalid/v1"},
		},
		{
			name: "bedrock host: CLI knob flips and the region derives",
			endpoint: &agent.EndpointBinding{
				Company: agent.CompanyAnthropic,
				Host:    agent.HostBedrock,
				Region:  "us-east-1",
			},
			wantEnv: map[string]string{
				"CLAUDE_CODE_USE_BEDROCK": "1",
				"AWS_REGION":              "us-east-1",
			},
		},
		{
			name: "vertex host: CLI knob flips and the region derives",
			endpoint: &agent.EndpointBinding{
				Company: agent.CompanyAnthropic,
				Host:    agent.HostVertex,
				Region:  "us-central1",
			},
			wantEnv: map[string]string{
				"CLAUDE_CODE_USE_VERTEX": "1",
				"CLOUD_ML_REGION":        "us-central1",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			workdir := t.TempDir()
			p := newFakeInteractiveProvider(t, captureEnvArgvScript)

			h, err := p.Spawn(context.Background(), agent.Spec{
				Model:       tc.specModel,
				Cwd:         workdir,
				Endpoint:    tc.endpoint,
				Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
			})
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			t.Cleanup(func() { _ = h.Stop(context.Background()) })
			awaitPTYExit(t, h)

			argv := argvLines(readCapturedFile(t, workdir, "argv.txt"))
			gotModel, hasModel := flagValue(argv, "--model")
			switch {
			case tc.wantModel == "" && hasModel:
				t.Errorf("--model = %q, want absent (argv: %q)", gotModel, argv)
			case tc.wantModel != "" && (!hasModel || gotModel != tc.wantModel):
				t.Errorf("--model = %q (present=%v), want %q (argv: %q)", gotModel, hasModel, tc.wantModel, argv)
			}

			env := readCapturedFile(t, workdir, "env.txt")
			for k, want := range tc.wantEnv {
				line := k + "=" + want
				if !strings.Contains(env, line) {
					t.Errorf("captured PTY-child env missing %q; got:\n%s", line, env)
				}
			}
		})
	}
}

// TestSpawn_Interactive_NilEndpointLeavesEnvRouteAndModelUnchanged pins the
// two-sided guarantee the packet requires: (1) a nil Spec.Endpoint ⇒
// applyEndpoint is a no-op, so an interactive spawn's --model and env stay
// byte-identical to pre-fix behavior (Spec.Model flows straight through,
// untouched by the new projection call); and (2) the ALREADY-SHIPPED
// claude-gateway credential route — a raw ANTHROPIC_BASE_URL riding
// Spec.Env directly, set by the platform's credential-snapshot env fan-out,
// with no Endpoint binding involved at all — is NOT regressed by moving
// applyEndpoint ahead of the interactive/headless split: it reaches the PTY
// child exactly as it did before this change, and no binding-derived knob
// (CLAUDE_CODE_USE_BEDROCK / CLAUDE_CODE_USE_VERTEX) appears out of nowhere.
func TestSpawn_Interactive_NilEndpointLeavesEnvRouteAndModelUnchanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}

	workdir := t.TempDir()
	p := newFakeInteractiveProvider(t, captureEnvArgvScript)

	h, err := p.Spawn(context.Background(), agent.Spec{
		Model: "claude-haiku",
		Cwd:   workdir,
		Env:   map[string]string{"ANTHROPIC_BASE_URL": "https://shipped-env-route.invalid/v1"},
		// Endpoint intentionally nil: no binding was resolved for this
		// session; today's shipped claude-gateway route rides Spec.Env
		// alone.
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	awaitPTYExit(t, h)

	argv := argvLines(readCapturedFile(t, workdir, "argv.txt"))
	gotModel, ok := flagValue(argv, "--model")
	if !ok || gotModel != "claude-haiku" {
		t.Errorf("--model = %q (present=%v), want %q — nil Endpoint must not touch Spec.Model", gotModel, ok, "claude-haiku")
	}

	env := readCapturedFile(t, workdir, "env.txt")
	if !strings.Contains(env, "ANTHROPIC_BASE_URL=https://shipped-env-route.invalid/v1") {
		t.Errorf("shipped env route (raw Spec.Env) did not reach the PTY child unchanged; got:\n%s", env)
	}
	for _, unwanted := range []string{"CLAUDE_CODE_USE_BEDROCK=1", "CLAUDE_CODE_USE_VERTEX=1"} {
		if strings.Contains(env, unwanted) {
			t.Errorf("nil-Endpoint spawn unexpectedly set %q out of nowhere; got:\n%s", unwanted, env)
		}
	}
}

// TestSpawn_Interactive_MismatchedEndpointCompanyFailsLoudly mirrors
// TestProvider_Spawn_EndpointCompanyMismatchFails (endpoint_test.go, the
// headless lane) on the interactive lane: a binding this harness cannot
// route must fail Spawn outright, never silently fall back to the default
// host — proving errors surface truthfully on both spawn modes now that
// applyEndpoint runs ahead of the split for both.
func TestSpawn_Interactive_MismatchedEndpointCompanyFailsLoudly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}

	p := newFakeInteractiveProvider(t, captureEnvArgvScript)

	_, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         t.TempDir(),
		Endpoint:    &agent.EndpointBinding{Company: agent.CompanyOpenAI, Host: agent.HostDirect},
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err == nil {
		t.Fatal("Spawn: want error for a non-anthropic endpoint company on the interactive lane, got nil")
	}
}
