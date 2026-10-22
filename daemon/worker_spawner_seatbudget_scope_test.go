package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/daemon/seatbudget"
	"github.com/RenseiAI/donmai/internal/testexec"
	"github.com/RenseiAI/donmai/sessionshim"
)

// newScopeProbeLaunch builds the launch one scope-permission test drives
// through the production entry: the launch identity, the registry beside
// it, and the default orphan policy every ordinary launch carries.
func newScopeProbeLaunch(dir, sessionID string) sessionshim.Launch {
	return sessionshim.Launch{
		Identity:     sessionshim.Identity{OrgID: "test-org", SessionID: sessionID},
		RegistryDir:  dir + "/registry",
		Orphan:       sessionshim.DefaultOrphanPolicy(),
		ProcessEpoch: 1,
	}
}

// newScopeProbeRegistry opens the registry a scope-permission launch
// wrote beside its (refused or failed) seat.
func newScopeProbeRegistry(dir string) (*sessionshim.Registry, error) {
	return sessionshim.NewRegistry(dir)
}

// TestStartShimProcess_ProbeDeniedRefusesUpFront pins the permission gate at
// the PRODUCTION launch entry: a Linux host whose placement probe reports
// none — presence without creation permission — refuses the launch with the
// missing transient-scope backend named, and leaves no launch record behind.
// Reverting the gate to presence-only would let this launch start and die
// past it with only "exited without publishing one".
func TestStartShimProcess_ProbeDeniedRefusesUpFront(t *testing.T) {
	dir := t.TempDir()
	const refusal = "Failed to start transient scope unit: Interactive authentication required"
	denied := shimScopeProbe{goos: "linux", placement: func() seatbudget.Placement { return seatbudget.PlacementNone }, scopeReason: func() string { return refusal }, readLimits: noLiveScope}
	d := &Daemon{shimScope: denied}
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 2,
		SeatBudget:            SeatBudget{CPUs: 2, MemoryMB: 256, Mode: "enforced"},
		WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
		WorktreeParentDir:     dir,
	})
	launch := newScopeProbeLaunch(dir, "probe-denied-seat")
	if _, err := d.startShimProcess(SessionSpec{SessionID: "probe-denied-seat"}, launch, nil); err == nil {
		t.Fatal("startShimProcess on a permission-denied host succeeded; want the up-front refusal")
	} else {
		if !strings.Contains(err.Error(), "transient-scope") {
			t.Errorf("refusal %q does not name the missing transient-scope backend", err)
		}
		// The gate folds the probe's own refusal line into the error
		// (see ScopeRefusalReason): the accept error names the reason,
		// not only the placement.
		if !strings.Contains(err.Error(), refusal) {
			t.Errorf("refusal %q does not carry the manager's own refusal line", err)
		}
	}
	if registry, err := newScopeProbeRegistry(launch.RegistryDir); err == nil {
		if _, found, _ := registry.SeatLaunch(launch.Identity, 1); found {
			t.Error("a refused launch left a seat launch record behind")
		}
	}
}

// TestStartShimProcess_ScopeRefusalSurfacesTheManagerMessage pins the
// launch-time half: when the wrapped scope binary refuses the scope, the
// AcceptWork error carries the refusal line — not only "exited without
// publishing one". The fake scope binary prints the denial to stderr and
// exits before the worker ever runs, the way a polkit denial does; the
// launch entry's preserved child-log tail is what carries it out. The fake
// rides the process PATH (t.Setenv, the same seam installFakeSystemdRun
// uses): exec resolves command[0] against the process environment, not the
// launch env argument, so a PATH passed only in env never reaches the
// wrapper.
func TestStartShimProcess_ScopeRefusalSurfacesTheManagerMessage(t *testing.T) {
	dir := t.TempDir()
	fakes := filepath.Join(dir, "fakes")
	if err := os.MkdirAll(fakes, 0o700); err != nil {
		t.Fatal(err)
	}
	const refusal = "Failed to start transient scope unit: Interactive authentication required"
	// The two lines land separately in the child log; the tail assertion
	// below pins the full refusal text. (A single echo writing the whole
	// refusal at once races the immediate post-Start read on some shells,
	// so the test polls briefly for the line — see below.)
	fake := "#!/bin/sh\n" +
		"echo scope-refused >&2\n" +
		"echo \"" + refusal + "\" >&2\n" +
		"exit 1\n"
	if err := testexec.WriteFile(filepath.Join(fakes, "systemd-run"), []byte(fake), 0o700); err != nil { //nolint:gosec // G306: test-owned fake binary; the exec bit is the point (LookPath + exec need +x)
		t.Fatal(err)
	}
	t.Setenv("PATH", fakes+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := &Daemon{shimScope: linuxSystemdShimScope()}
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 2,
		SeatBudget:            SeatBudget{CPUs: 2, MemoryMB: 256, Mode: "enforced"},
		WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
		WorktreeParentDir:     dir,
	})
	launch := newScopeProbeLaunch(dir, "scope-refused-seat")
	started, err := d.startShimProcess(SessionSpec{SessionID: "scope-refused-seat"}, launch, nil)
	if err != nil {
		t.Fatalf("startShimProcess: %v", err)
	}
	if started.PID == 0 {
		t.Fatal("startShimProcess returned pid 0")
	}
	// The wrapper refused before the worker ran: the discovery wait ends at
	// once on the dead pid, and the launch caller's preserved child-log tail
	// carries the manager's own message onto the AcceptWork error.
	wait := shimDiscoveryWait{started: started}
	discoveryErr := wait.exitedError(os.ErrNotExist)
	if !strings.Contains(discoveryErr.Error(), "exited without publishing one") {
		t.Fatalf("discovery failure = %q; want the definite exited failure", discoveryErr)
	}
	// The wrapper exits at once, but the child's stderr write races the
	// reader: poll briefly for the refusal line rather than asserting on
	// a single immediate read.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if tail := redactAndReadShimChildLogTail(shimChildLogPath(launch.RegistryDir, launch.Identity)); strings.Contains(tail, refusal) {
			break
		}
		if time.Now().After(deadline) {
			tail := redactAndReadShimChildLogTail(shimChildLogPath(launch.RegistryDir, launch.Identity))
			t.Fatalf("preserved child-log tail = %q; want the manager's refusal line carried", tail)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
