package sessionshim_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
	"github.com/RenseiAI/donmai/sessionshim"
)

// lookupFrom turns a map into the env-lookup shape LaunchFromEnv takes.
func lookupFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLaunchRoundTripsThroughTheEnvironment(t *testing.T) {
	t.Parallel()

	want := sessionshim.Launch{
		Identity:     sessionshim.Identity{OrgID: "org-9", SessionID: "sess-9"},
		RegistryDir:  "/tmp/shims",
		Orphan:       sessionshim.DefaultOrphanPolicy(),
		ProcessEpoch: 7,
	}
	got, err := sessionshim.LaunchFromEnv(lookupFrom(want.Env()))
	if err != nil {
		t.Fatalf("LaunchFromEnv: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestNoLaunchIsTheOrdinaryCaseNotAFailure(t *testing.T) {
	t.Parallel()

	// A worker started without the contract stays on the pre-shim path. This is
	// §D11's migration law in one assertion: shipping the code must not change
	// who owns a terminal until a controller says so.
	for _, gate := range []string{"", "0", "true", "yes"} {
		_, err := sessionshim.LaunchFromEnv(lookupFrom(map[string]string{sessionshim.EnvOwnership: gate}))
		if !errors.Is(err, sessionshim.ErrNoLaunch) {
			t.Errorf("LaunchFromEnv with gate %q = %v, want ErrNoLaunch", gate, err)
		}
	}
	if _, err := sessionshim.LaunchFromEnv(nil); !errors.Is(err, sessionshim.ErrNoLaunch) {
		t.Errorf("LaunchFromEnv(nil) = %v, want ErrNoLaunch", err)
	}
}

func TestASelectedButMalformedLaunchFailsClosed(t *testing.T) {
	t.Parallel()

	// Once the gate is set, a bad field is an ERROR and never a default. A worker
	// that quietly fell back to direct ownership after being told to be a shim
	// would leave its controller adopting nothing while believing the session was
	// shim-backed — a terminal that silently is not durable.
	base := sessionshim.Launch{
		Identity:     sessionshim.Identity{OrgID: "o", SessionID: "s"},
		RegistryDir:  "/tmp/shims",
		Orphan:       sessionshim.DefaultOrphanPolicy(),
		ProcessEpoch: 1,
	}.Env()

	cases := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{"missing org", func(m map[string]string) { m[sessionshim.EnvOrgID] = "" }, "orgId"},
		{"missing session", func(m map[string]string) { m[sessionshim.EnvSessionID] = "" }, "sessionId"},
		{"path separator in session", func(m map[string]string) { m[sessionshim.EnvSessionID] = "a/b" }, "path separator"},
		{"missing registry dir", func(m map[string]string) { m[sessionshim.EnvRegistryDir] = "" }, sessionshim.EnvRegistryDir},
		{"unparsable epoch", func(m map[string]string) { m[sessionshim.EnvProcessEpoch] = "later" }, sessionshim.EnvProcessEpoch},
		{"missing orphan deadline", func(m map[string]string) { m[sessionshim.EnvOrphanDeadlineMS] = "" }, sessionshim.EnvOrphanDeadlineMS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			tc.mutate(env)
			_, err := sessionshim.LaunchFromEnv(lookupFrom(env))
			if err == nil {
				t.Fatalf("LaunchFromEnv accepted a malformed launch (%s)", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

func TestLaunchRevalidatesTheOrphanInequality(t *testing.T) {
	t.Parallel()

	// §D8 makes the inequality a precondition for ADMITTING a session, checked by
	// whoever is about to run one — not a courtesy the launcher performs on the
	// shim's behalf. The two processes are separately configurable, and a shim
	// that trusted a bound it never checked could outlive an external release
	// threshold and produce double execution.
	unsafe := sessionshim.Launch{
		Identity:    sessionshim.Identity{OrgID: "o", SessionID: "s"},
		RegistryDir: "/tmp/shims",
		Orphan: sessionshim.OrphanPolicy{
			Deadline:                 90 * time.Second,
			TerminationGrace:         5 * time.Second,
			PropagationMargin:        30 * time.Second,
			ExternalReleaseThreshold: 60 * time.Second,
		},
		ProcessEpoch: 1,
	}
	_, err := sessionshim.LaunchFromEnv(lookupFrom(unsafe.Env()))
	if !errors.Is(err, sessionshim.ErrOrphanPolicyUnsafe) {
		t.Fatalf("LaunchFromEnv on an unsafe orphan policy = %v, want ErrOrphanPolicyUnsafe", err)
	}
}

func TestEveryLaunchKeyIsRefusedToTheHarnessChild(t *testing.T) {
	t.Parallel()

	// runtime/env matches this contract by PREFIX and deliberately does not
	// import this package (sessionshim -> ptyhost -> runtime/env would close an
	// import cycle). This test is the pin that keeps the two halves from drifting:
	// a key added here that the runner-only boundary does not refuse would reach
	// the harness child, handing a workload the address of its own supervisor.
	keys := sessionshim.EnvKeys()
	if len(keys) == 0 {
		t.Fatal("EnvKeys is empty")
	}
	for _, key := range keys {
		if !runtimeenv.IsRunnerOnly(key) {
			t.Errorf("runtimeenv.IsRunnerOnly(%q) = false; the launch contract must never reach a harness child", key)
		}
		if !sessionshim.IsEnvKey(key) {
			t.Errorf("IsEnvKey(%q) = false for a key EnvKeys returned", key)
		}
	}
	if sessionshim.IsEnvKey("PATH") {
		t.Error(`IsEnvKey("PATH") = true`)
	}
	// Env() must render exactly the declared key set — no more, no less.
	env := sessionshim.Launch{
		Identity:    sessionshim.Identity{OrgID: "o", SessionID: "s"},
		RegistryDir: "/tmp/x", Orphan: sessionshim.DefaultOrphanPolicy(),
	}.Env()
	if len(env) != len(keys) {
		t.Fatalf("Env() rendered %d keys, EnvKeys declares %d", len(env), len(keys))
	}
	for _, key := range keys {
		if _, ok := env[key]; !ok {
			t.Errorf("Env() omitted declared key %s", key)
		}
	}
}

func TestStartFromEnvRequiresAUsableRegistryDirectory(t *testing.T) {
	t.Parallel()

	// The registry directory is created 0700 when absent, but a path that cannot
	// be a directory must fail the launch rather than leave a shim with nowhere to
	// publish — an unannounced shim is indistinguishable from one that never
	// started, and §D4 requires every survivor to be accounted for.
	_, err := sessionshim.StartFromEnv(sessionshim.Launch{
		Identity:    sessionshim.Identity{OrgID: "o", SessionID: "s"},
		RegistryDir: "/dev/null/not-a-directory",
		Orphan:      sessionshim.DefaultOrphanPolicy(),
	}, ptyhost.Spec{Command: []string{"/bin/sh", "-c", "exit 0"}}, "/tmp")
	if err == nil {
		t.Fatal("StartFromEnv accepted an unusable registry directory")
	}
}
