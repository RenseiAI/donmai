package daemon

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// TestComposeEnv_RefusesRunnerOnlyFromCallerMaps pins the PROVENANCE of the
// injection declaration.
//
// SessionSpec.Env is copied verbatim from the orchestrator's poll work item, so
// without this filter a work item carrying
// DONMAI_INJECTED_ENV_KEYS=ANTHROPIC_API_KEY would re-admit the DAEMON's own
// operator-exported provider key into a harness child — a leak no value of
// item.Env could reach before the declaration existed. OnPreSpawn runs after
// this composition and remains the only author.
func TestComposeEnv_RefusesRunnerOnlyFromCallerMaps(t *testing.T) {
	t.Parallel()

	refused := []string{
		runtimeenv.InjectedEnvKeysVar,
		runtimeenv.GatewayUpstreamEnvKeysVar,
		"ATTACH_TOKEN",
		"ATTACH_TOKEN_FILE",
		"ATTACH_URL",
		"DONMAI_SESSION_SHIM_REGISTRY_DIR",
	}

	for _, key := range refused {
		t.Run(key, func(t *testing.T) {
			t.Parallel()

			// Every caller-supplied layer, not just spec.Env: BaseEnv is the
			// embedder's static map and daemonOwnedEnv is the daemon's own, and
			// neither is the right place to author a runner-owned control.
			for layer, parts := range map[string][]map[string]string{
				"spec.Env":       {nil, {key: "hostile", "KEEP": "yes"}, nil},
				"BaseEnv":        {{key: "hostile", "KEEP": "yes"}, nil, nil},
				"daemonOwnedEnv": {nil, nil, {key: "hostile", "KEEP": "yes"}},
			} {
				got := composeEnv(parts...)
				if envHasKey(got, key) {
					t.Errorf("%s layer: %q reached the composed worker env", layer, key)
				}
				if !envHasEntry(got, "KEEP=yes") {
					t.Errorf("%s layer: ordinary entries must still pass through (got %v)", layer, got)
				}
			}
		})
	}
}

// TestComposeEnv_OrdinaryEntriesStillMerge is the control: the filter must not
// disturb the layering it was inserted into.
func TestComposeEnv_OrdinaryEntriesStillMerge(t *testing.T) {
	t.Parallel()

	got := composeEnv(
		map[string]string{"A": "base", "B": "base"},
		map[string]string{"B": "spec"},
		map[string]string{"C": "daemon"},
	)
	for _, want := range []string{"A=base", "B=spec", "C=daemon"} {
		if !envHasEntry(got, want) {
			t.Errorf("composeEnv() = %v, missing %q", got, want)
		}
	}
}

// parentSupervisorKeys are supervisor controls a daemon process can carry in
// its OWN environment (exported by the operator's shell or service manager).
// None of them may reach a spawned session.
var parentSupervisorKeys = []string{
	afclient.ControlTokenEnv,
	afclient.ControlTokenFileEnv,
	"ATTACH_TOKEN",
	"ATTACH_URL",
	runtimeenv.InjectedEnvKeysVar,
	"DONMAI_SESSION_SHIM_ACCEPTANCE_TOKEN_FILE",
}

// parentOrdinaryEntry is an ordinary inherited variable: the control that the
// parent filter still passes the rest of the daemon's environment through.
const (
	parentOrdinaryKey   = "DONMAI_TEST_PARENT_ORDINARY"
	parentOrdinaryEntry = parentOrdinaryKey + "=keep"
)

// setParentSupervisorEnv places every parentSupervisorKeys entry and one
// ordinary variable in this test process's environment, which is the daemon
// parent environment composeEnv inherits.
func setParentSupervisorEnv(t *testing.T) {
	t.Helper()
	for _, key := range parentSupervisorKeys {
		t.Setenv(key, "parent-"+strings.ToLower(key))
	}
	t.Setenv(parentOrdinaryKey, "keep")
}

// TestComposeEnv_StripsRunnerOnlyFromParentEnv pins that the daemon's own
// inherited environment goes through the runner-only filter: a daemon started
// with the control token (or its file override) exported must not hand either
// to the sessions it spawns.
func TestComposeEnv_StripsRunnerOnlyFromParentEnv(t *testing.T) {
	setParentSupervisorEnv(t)

	got := composeEnv(nil, nil, nil)
	for _, key := range parentSupervisorKeys {
		if envHasKey(got, key) {
			t.Errorf("parent env %q reached the composed worker env", key)
		}
	}
	if !envHasEntry(got, parentOrdinaryEntry) {
		t.Errorf("ordinary parent entry %q must still pass through", parentOrdinaryEntry)
	}
}

// An empty filtered environment must remain an explicit empty child env.
// A nil exec.Cmd.Env would silently re-inherit the supervisor's control token.
func TestComposeEnv_EmptyFilteredParentDoesNotReinherit(t *testing.T) {
	const (
		producer = "compose-env-empty-producer"
		assert   = "compose-env-empty-assert"
	)
	phase := ""
	for _, arg := range os.Args {
		if arg == producer || arg == assert {
			phase = arg
		}
	}
	if phase == assert {
		if os.Getenv(afclient.ControlTokenEnv) != "" {
			t.Fatal("worker inherited the supervisor control token")
		}
		return
	}
	if phase == producer {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestComposeEnv_EmptyFilteredParentDoesNotReinherit$", "--", assert)
		cmd.Env = composeEnv(nil, nil, nil)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("explicitly empty worker environment was not preserved: %v: %s", err, output)
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestComposeEnv_EmptyFilteredParentDoesNotReinherit$", "--", producer)
	cmd.Env = []string{afclient.ControlTokenEnv + "=synthetic-control-token"}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated supervisor environment probe failed: %v: %s", err, output)
	}
}

// TestSpawnPaths_ParentControlTokenNeverReachesSession drives both spawn
// paths with a token-bearing daemon parent: the direct path's exec'd child
// reports what it actually sees, and the shim path's launcher records the env
// it is handed.
func TestSpawnPaths_ParentControlTokenNeverReachesSession(t *testing.T) {
	setParentSupervisorEnv(t)

	t.Run("direct", func(t *testing.T) {
		capture := newCaptureWriter()
		s := NewWorkerSpawner(SpawnerOptions{
			Projects:              []ProjectConfig{{ID: "p", Repository: "github.com/a/b"}},
			MaxConcurrentSessions: 1,
			WorkerCommand: []string{
				"/bin/sh", "-c",
				`printf 'probe ctl=%s ctlfile=%s ordinary=%s\n' "${DONMAI_CONTROL_TOKEN-unset}" "${DONMAI_CONTROL_TOKEN_FILE-unset}" "${DONMAI_TEST_PARENT_ORDINARY-unset}"; exit 0`,
			},
			StdoutPrefixWriter: capture,
		})
		ended := sessionEnds(s)
		if _, err := s.AcceptWork(SessionSpec{SessionID: "sess-parent-direct", Repository: "github.com/a/b", Ref: "main"}); err != nil {
			t.Fatalf("AcceptWork: %v", err)
		}
		lines := waitForLine(t, capture, "probe ")
		waitSessionEnd(t, ended)
		const want = "probe ctl=unset ctlfile=unset ordinary=keep"
		found := false
		for _, l := range lines {
			if strings.Contains(l, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("direct child env probe = %v, want a line containing %q", lines, want)
		}
	})

	t.Run("shim", func(t *testing.T) {
		var (
			mu     sync.Mutex
			gotEnv []string
		)
		launched := make(chan struct{}, 1)
		s := NewWorkerSpawner(SpawnerOptions{
			Projects:              []ProjectConfig{{ID: "p", Repository: "github.com/a/b"}},
			MaxConcurrentSessions: 1,
			WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
			ShimOwns:              func(SessionSpec) bool { return true },
			ShimSpawn: func(spec SessionSpec, _ ProjectConfig, env []string) (*SessionHandle, error) {
				mu.Lock()
				gotEnv = append([]string(nil), env...)
				mu.Unlock()
				select {
				case launched <- struct{}{}:
				default:
				}
				return &SessionHandle{SessionID: spec.SessionID, State: SessionRunning}, nil
			},
		})
		if _, err := s.AcceptWork(SessionSpec{SessionID: "sess-parent-shim", Repository: "github.com/a/b", Ref: "main"}); err != nil {
			t.Fatalf("AcceptWork: %v", err)
		}
		select {
		case <-launched:
		case <-time.After(spawnerWaitTimeout):
			t.Fatal("timed out waiting for the shim launcher")
		}
		mu.Lock()
		env := append([]string(nil), gotEnv...)
		mu.Unlock()
		for _, key := range parentSupervisorKeys {
			if envHasKey(env, key) {
				t.Errorf("shim launch env carries parent %q", key)
			}
		}
		if !envHasEntry(env, parentOrdinaryEntry) {
			t.Errorf("shim launch env lost ordinary parent entry %q", parentOrdinaryEntry)
		}
	})
}

func envHasKey(entries []string, key string) bool {
	prefix := key + "="
	for _, entry := range entries {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func envHasEntry(entries []string, want string) bool {
	for _, entry := range entries {
		if entry == want {
			return true
		}
	}
	return false
}
