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
	// Env() must render exactly the declared key set — no more, no less —
	// for EVERY launch. The seat facts always render: a plain launch (no
	// scope, no limits) carries the seat keys with empty/zero values, so a
	// restarted daemon recovers the record the shim republishes rather than
	// an absent field it cannot distinguish from an old launcher. Both
	// shapes must decode back to the launch they came from.
	plain := sessionshim.Launch{
		Identity:    sessionshim.Identity{OrgID: "o", SessionID: "s"},
		RegistryDir: "/tmp/x", Orphan: sessionshim.DefaultOrphanPolicy(),
	}
	env := plain.Env()
	for _, key := range keys {
		if _, ok := env[key]; !ok {
			t.Errorf("Env() omitted declared key %s", key)
		}
	}
	if back, err := sessionshim.LaunchFromEnv(lookupFrom(env)); err != nil || back != plain {
		t.Errorf("plain round trip = %+v, %v; want %+v", back, err, plain)
	}
	scoped := plain
	scoped.SeatScope = "donmai-seat-abc-1.scope"
	scoped.SeatCPUs = 2
	scoped.SeatMemoryMB = 512
	scoped.SeatIOWeight = 100
	scopedEnv := scoped.Env()
	for _, key := range keys {
		if _, ok := scopedEnv[key]; !ok {
			t.Errorf("scoped Env() omitted declared key %s", key)
		}
	}
	if back, err := sessionshim.LaunchFromEnv(lookupFrom(scopedEnv)); err != nil || back != scoped {
		t.Errorf("scoped round trip = %+v, %v; want %+v", back, err, scoped)
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

// TestSeatFactsRoundTripThroughLaunch pins the secret-free launch record:
// the scope unit and the launched limits travel the launch contract and
// decode back unchanged. A budgetless launch renders empty/zero seat values
// and decodes back to them; a wire that predates the seat keys entirely
// (an old launcher) decodes to no facts. A malformed limit fails closed.
func TestSeatFactsRoundTripThroughLaunch(t *testing.T) {
	t.Parallel()

	base := sessionshim.Launch{
		Identity:     sessionshim.Identity{OrgID: "o", SessionID: "s"},
		RegistryDir:  "/tmp/shims",
		Orphan:       sessionshim.DefaultOrphanPolicy(),
		ProcessEpoch: 3,
		SeatScope:    "donmai-seat-abc-3.scope",
		SeatCPUs:     2,
		SeatMemoryMB: 512,
		SeatIOWeight: 100,
	}
	got, err := sessionshim.LaunchFromEnv(lookupFrom(base.Env()))
	if err != nil {
		t.Fatalf("LaunchFromEnv: %v", err)
	}
	if got != base {
		t.Fatalf("round trip = %+v, want %+v", got, base)
	}
	// Absent seat keys decode to no facts — an old launcher — without error.
	// (Env() itself always renders the keys; only a hand-built wire that
	// predates them takes this path.)
	plain := sessionshim.Launch{
		Identity:     sessionshim.Identity{OrgID: "o", SessionID: "s"},
		RegistryDir:  "/tmp/shims",
		Orphan:       sessionshim.DefaultOrphanPolicy(),
		ProcessEpoch: 3,
	}
	env := plain.Env()
	for _, key := range []string{sessionshim.EnvSeatScope, sessionshim.EnvSeatCPUs, sessionshim.EnvSeatMemoryMB, sessionshim.EnvSeatIOWeight} {
		delete(env, key)
	}
	got, err = sessionshim.LaunchFromEnv(lookupFrom(env))
	if err != nil {
		t.Fatalf("LaunchFromEnv without seat keys: %v", err)
	}
	if got.SeatScope != "" || got.SeatCPUs != 0 || got.SeatMemoryMB != 0 || got.SeatIOWeight != 0 {
		t.Fatalf("launch without seat keys = %+v; want no seat facts", got)
	}
	// Malformed limits fail closed: a limit that silently became zero would
	// un-confine a seat while its record claimed otherwise.
	for _, key := range []string{sessionshim.EnvSeatCPUs, sessionshim.EnvSeatMemoryMB, sessionshim.EnvSeatIOWeight} {
		bad := base.Env()
		bad[key] = "many"
		if _, err := sessionshim.LaunchFromEnv(lookupFrom(bad)); err == nil {
			t.Errorf("LaunchFromEnv accepted %s=many", key)
		}
	}
	bad := base.Env()
	bad[sessionshim.EnvSeatIOWeight] = "20000"
	if _, err := sessionshim.LaunchFromEnv(lookupFrom(bad)); err == nil {
		t.Error("LaunchFromEnv accepted an IO weight above the record bound")
	}
}
