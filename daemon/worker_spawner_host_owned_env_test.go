package daemon

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// TestSessionEnv_WorkItemCannotOverrideHostConfinement pins that the pi
// confinement switch is the HOST's: a work item's environment is copied
// verbatim from the orchestrator and applied after the daemon's own, so a
// work item carrying DONMAI_PI_CONFINEMENT=off would otherwise turn a host
// that requires confinement into an unconfined pi seat for its session.
//
// Each case composes the worker environment exactly as both spawn paths do
// (sessionEnv) and runs a real child on it, so the answer is the value a
// worker process actually reads, after exec resolves the duplicate keys.
//
// RED: pass spec.Env to composeEnv unfiltered and the first two cases see
// "off".
func TestSessionEnv_WorkItemCannotOverrideHostConfinement(t *testing.T) {
	key := runtimeenv.PiConfinementEnv
	for _, tc := range []struct {
		name     string
		daemon   string // the daemon process's own environment; "" = unset
		baseEnv  map[string]string
		workItem string
		want     string
	}{
		{name: "daemon requires, work item says off", daemon: "required", workItem: "off", want: "required"},
		{name: "base env requires, work item says off", baseEnv: map[string]string{key: "required"}, workItem: "off", want: "required"},
		{name: "daemon requires, work item says nothing", daemon: "required", want: "required"},
		{name: "host says nothing, work item says required", workItem: "required", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(key, tc.daemon)
			if tc.daemon == "" {
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
			}
			spec := SessionSpec{SessionID: "sess-host-owned", Env: map[string]string{"KEEP": "yes"}}
			if tc.workItem != "" {
				spec.Env[key] = tc.workItem
			}
			s := NewWorkerSpawner(SpawnerOptions{BaseEnv: tc.baseEnv})
			env := s.sessionEnv(spec, nil)
			if !envHasEntry(env, "KEEP=yes") {
				t.Fatalf("ordinary work-item entries must still pass through (got %v)", env)
			}

			cmd := exec.Command("/bin/sh", "-c", `printf '%s' "${`+key+`-}"`) //nolint:gosec // G204: fixed shell; the variable name is a package constant.
			cmd.Env = env
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("child: %v", err)
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Errorf("worker sees %s=%q, want %q", key, got, tc.want)
			}
		})
	}
}

// TestHostOwnedConstants_MatchTheDaemon pins the shared names the pi
// harness reads (it cannot import this package) to the daemon's own.
func TestHostOwnedConstants_MatchTheDaemon(t *testing.T) {
	t.Parallel()
	if runtimeenv.DaemonControlURLEnv != EnvDaemonControlURL {
		t.Errorf("runtimeenv.DaemonControlURLEnv = %q, daemon states %q", runtimeenv.DaemonControlURLEnv, EnvDaemonControlURL)
	}
	if runtimeenv.DefaultDaemonControlPort != DefaultHTTPPort {
		t.Errorf("runtimeenv.DefaultDaemonControlPort = %d, daemon binds %d", runtimeenv.DefaultDaemonControlPort, DefaultHTTPPort)
	}
}

// TestAcceptWork_BothSpawnPathsKeepHostConfinement drives a work item that
// says DONMAI_PI_CONFINEMENT=off through both spawn paths of a daemon that
// requires it, and reads the environment each path actually hands its
// worker: the direct child (what OnPreSpawn receives) and the shim launch.
// It catches a spawn path that composes the worker environment without
// sessionEnv.
func TestAcceptWork_BothSpawnPathsKeepHostConfinement(t *testing.T) {
	key := runtimeenv.PiConfinementEnv
	t.Setenv(key, "required")
	workItem := map[string]string{key: "off"}

	lastValue := func(env []string) string {
		value := ""
		for _, entry := range env {
			if v, ok := strings.CutPrefix(entry, key+"="); ok {
				value = v
			}
		}
		return value
	}

	t.Run("direct", func(t *testing.T) {
		got := make(chan []string, 1)
		s := NewWorkerSpawner(SpawnerOptions{
			Projects:              []ProjectConfig{{ID: "p", Repository: "github.com/a/b"}},
			MaxConcurrentSessions: 1,
			WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
			OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
				select {
				case got <- append([]string(nil), env...):
				default:
				}
				return env, nil
			},
		})
		ended := sessionEnds(s)
		if _, err := s.AcceptWork(SessionSpec{SessionID: "sess-host-direct", Repository: "github.com/a/b", Ref: "main", Env: workItem}); err != nil {
			t.Fatalf("AcceptWork: %v", err)
		}
		var env []string
		select {
		case env = <-got:
		case <-time.After(spawnerWaitTimeout):
			t.Fatal("timed out waiting for the direct spawn")
		}
		waitSessionEnd(t, ended)
		if v := lastValue(env); v != "required" {
			t.Errorf("direct worker env %s=%q, want the host's %q", key, v, "required")
		}
	})

	t.Run("shim", func(t *testing.T) {
		got := make(chan []string, 1)
		s := NewWorkerSpawner(SpawnerOptions{
			Projects:              []ProjectConfig{{ID: "p", Repository: "github.com/a/b"}},
			MaxConcurrentSessions: 1,
			WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
			ShimOwns:              func(SessionSpec) bool { return true },
			ShimSpawn: func(spec SessionSpec, _ ProjectConfig, env []string) (*SessionHandle, error) {
				select {
				case got <- append([]string(nil), env...):
				default:
				}
				return &SessionHandle{SessionID: spec.SessionID, State: SessionRunning}, nil
			},
		})
		if _, err := s.AcceptWork(SessionSpec{SessionID: "sess-host-shim", Repository: "github.com/a/b", Ref: "main", Env: workItem}); err != nil {
			t.Fatalf("AcceptWork: %v", err)
		}
		var env []string
		select {
		case env = <-got:
		case <-time.After(spawnerWaitTimeout):
			t.Fatal("timed out waiting for the shim launch")
		}
		if v := lastValue(env); v != "required" {
			t.Errorf("shim launch env %s=%q, want the host's %q", key, v, "required")
		}
	})
}
