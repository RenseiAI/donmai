package seatbudget

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// TestVerifyScopePlacementPrefersTheUserManager pins the permission gate's
// preference order through the injected bus probe and runner: a reachable
// user manager that accepts the throwaway scope wins without ever asking
// the system bus; a user manager that refuses falls back to the system
// bus; a host where neither accepts reports no usable backend. The runner
// is the systemd-run-shaped fake — no test here may need real systemd, so
// the order pins on every host including non-Linux CI (where the gate
// answers false without calling the runner).
func TestVerifyScopePlacementPrefersTheUserManager(t *testing.T) {
	const refusal = "Failed to start transient scope unit: Interactive authentication required"
	cases := []struct {
		name       string
		hasUserBus bool
		refuseUser bool
		refuseSys  bool
		want       bool
		wantBuses  []bool
	}{
		{
			name:       "user manager accepts: the system bus is never asked",
			hasUserBus: true,
			want:       true,
			wantBuses:  []bool{true},
		},
		{
			name:       "user manager refuses: the system bus decides",
			hasUserBus: true,
			refuseUser: true,
			want:       true,
			wantBuses:  []bool{true, false},
		},
		{
			name:       "both buses refuse: no usable scope backend",
			hasUserBus: true,
			refuseUser: true,
			refuseSys:  true,
			want:       false,
			wantBuses:  []bool{true, false},
		},
		{
			name:       "no user manager: only the system bus is asked",
			hasUserBus: false,
			want:       true,
			wantBuses:  []bool{false},
		},
		{
			name:       "no user manager and the system bus refuses: none",
			hasUserBus: false,
			refuseSys:  true,
			want:       false,
			wantBuses:  []bool{false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var asked []bool
			run := func(userScope bool, unit string) error {
				asked = append(asked, userScope)
				refuse := tc.refuseSys
				if userScope {
					refuse = tc.refuseUser
				}
				if !refuse {
					return nil
				}
				// The refusal renders through the real constructor's
				// error shape, the same fold production returns.
				return runScopeProbeWithCommand(userScope, unit, scopeProbeCommand,
					func(string, ...string) ([]byte, error) {
						return []byte(refusal + "\n"), errors.New("exit status 1")
					})
			}
			unit := func() string { return "test-probe.scope" }
			if runtime.GOOS != "linux" {
				if verifyScopePlacementWithUnitBusProbe(run, unit, func() bool { return tc.hasUserBus }) {
					t.Fatal("scope permission granted off Linux; want false without a real systemd")
				}
				if len(asked) != 0 {
					t.Fatalf("runner asked on buses %v off Linux; want no probe at all", asked)
				}
				return
			}
			if got := verifyScopePlacementWithUnitBusProbe(run, unit, func() bool { return tc.hasUserBus }); got != tc.want {
				t.Fatalf("verify = %v; want %v", got, tc.want)
			}
			if len(asked) != len(tc.wantBuses) {
				t.Fatalf("probed buses = %v; want %v", asked, tc.wantBuses)
			}
			for i := range asked {
				if asked[i] != tc.wantBuses[i] {
					t.Fatalf("probed buses = %v; want %v", asked, tc.wantBuses)
				}
			}
		})
	}
}

// TestSelectPlacementWithScopeProbeRefusesAPermissionDeniedHost pins the
// placement gate: a systemd host whose manager refuses this user a scope
// on every reachable bus selects none — the launch gate then refuses up
// front with the missing backend named instead of dying opaquely past it.
// Presence without permission must never select the systemd backend. The
// verdict half pins through the injected gate (no live probe); the
// agreement half pins below through the cached production entry.
func TestSelectPlacementWithScopeProbeRefusesAPermissionDeniedHost(t *testing.T) {
	if got := selectPlacementWithScopeProbe(true, false, func() bool { return false }); got != PlacementNone {
		t.Fatalf("placement = %q on a permission-denied systemd host; want none", got)
	}
	if got := selectPlacementWithScopeProbe(true, true, func() bool { return true }); got != PlacementSystemd {
		t.Fatalf("placement = %q on a permitted systemd host; want systemd", got)
	}
	if got := selectPlacementWithScopeProbe(false, true, func() bool { return false }); got != PlacementCgroupFS {
		t.Fatalf("placement = %q without systemd; want the named systemd-less placement", got)
	}
	if got := selectPlacementWithScopeProbe(false, false, func() bool { return true }); got != PlacementNone {
		t.Fatalf("placement = %q with no backend at all; want none", got)
	}
}

// TestHostPlacementAgreesAcrossCalls pins the B1 agreement rule through the
// PRODUCTION entry point: every HostPlacement call in a process answers
// alike, because the permission verdict caches per process instead of
// re-execing the probe per call. Off Linux the answer is never systemd;
// the suite asserts agreement, not a particular backend, so it pins on
// every host without a real systemd.
func TestHostPlacementAgreesAcrossCalls(t *testing.T) {
	first := HostPlacement()
	for range 3 {
		if got := HostPlacement(); got != first {
			t.Fatalf("HostPlacement = %q then %q; want every call in a process to agree", first, got)
		}
	}
	if runtime.GOOS != "linux" && first == PlacementSystemd {
		t.Fatalf("HostPlacement = systemd off Linux; want no systemd backend there")
	}
}

// TestScopeRefusalCarriesTheManagerMessage pins the probe error shape
// through the REAL constructor: a fake systemd-run on PATH emits a refusal
// to stderr, and the returned error folds the manager's own line in — so
// the launch path that surfaces it names the reason (for example the
// polkit denial) instead of only "exited without publishing one".
// Dropping the stderr fold from the constructor fails this test.
func TestScopeRefusalCarriesTheManagerMessage(t *testing.T) {
	const refusal = "Failed to start transient scope unit: Interactive authentication required"
	dir := t.TempDir()
	helper := filepath.Join(dir, "systemd-run")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho '"+refusal+"' >&2\nexit 1\n"), 0o700); err != nil { //nolint:gosec // G306: test fixture: the fake manager the refusal shape pins against
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath)
	err := runScopeProbeOnUnit(false, "test-refusal.scope")
	if err == nil {
		t.Fatal("refused probe returned nil; want the manager's refusal carried")
	}
	if !strings.Contains(err.Error(), refusal) {
		t.Fatalf("probe error = %q; want the manager's own message carried", err)
	}
}

// TestScopeProbeUnitIsUniquePerAttempt pins the F2 collision rule: every
// rendered probe unit is distinct — pid plus a per-process counter — so
// concurrent probes (parallel packages, a spawn racing a status poll)
// never share the fixed name two systemd-run --unit calls race on.
func TestScopeProbeUnitIsUniquePerAttempt(t *testing.T) {
	seen := map[string]struct{}{}
	for range 64 {
		name := scopeProbeUnit()
		if !strings.HasPrefix(name, scopeProbePrefix+"-") || !strings.HasSuffix(name, ".scope") {
			t.Fatalf("probe unit = %q; want the %q prefix with a .scope suffix", name, scopeProbePrefix)
		}
		if _, dup := seen[name]; dup {
			t.Fatalf("probe unit %q rendered twice; want a fresh name per attempt", name)
		}
		seen[name] = struct{}{}
	}
	var wg sync.WaitGroup
	ch := make(chan string, 64)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 8 {
				ch <- scopeProbeUnit()
			}
		}()
	}
	wg.Wait()
	close(ch)
	for name := range ch {
		if _, dup := seen[name]; dup {
			t.Fatalf("concurrent probe unit %q collided; want unique names across goroutines", name)
		}
		seen[name] = struct{}{}
	}
}

// TestScopeNameCollisionRetriesUnderAFreshName pins the collision branch
// through the REAL loop: a first attempt that fails only with "already
// exists" is retried under a fresh name, and the retry's acceptance
// decides — a permitted host never flips to none on a sibling's unit.
func TestScopeNameCollisionRetriesUnderAFreshName(t *testing.T) {
	// The retry loop is GOOS-independent (verifyScopeBusRefusal takes the
	// runner and unit renderer as parameters): the test stages a first
	// attempt that fails only with "already exists" and pins that the
	// retry under a fresh name decides — on every host, no real systemd.
	var units []string
	calls := 0
	run := func(unit string) error {
		units = append(units, unit)
		calls++
		if calls == 1 {
			return fmt.Errorf("systemd-run --scope probe: %w: %s", errors.New("exit status 1"), "Failed to start transient scope unit: Unit test-probe.scope already exists.")
		}
		return nil
	}
	seq := 0
	unit := func() string {
		seq++
		return fmt.Sprintf("test-collision-%d.scope", seq)
	}
	if err := verifyScopeBusRefusal(run, unit); err != nil {
		t.Fatalf("collision-then-accept = %v; want the retry's acceptance to decide", err)
	}
	if len(units) != 2 || units[0] == units[1] {
		t.Fatalf("probed units = %v; want the retry under a fresh name", units)
	}
}

// TestScopeCollisionOnEveryAttemptIsNotARefusal pins the exhaustion branch:
// when every attempt collides, the bus never answered, so the outcome is a
// non-nil "name unavailable" error rather than a refusal — and its line
// extracts to nothing, so the gate never learns a collision as a no.
func TestScopeCollisionOnEveryAttemptIsNotARefusal(t *testing.T) {
	run := func(string) error {
		return fmt.Errorf("systemd-run --scope probe: %w: %s", errors.New("exit status 1"), "Failed to start transient scope unit: Unit x.scope already exists.")
	}
	unit := func() string { return "test-collision.scope" }
	err := verifyScopeBusRefusal(run, unit)
	if err == nil {
		t.Fatal("all-collided bus returned nil; want the name-unavailable outcome")
	}
	if line := scopeRefusalLine(err); line != "" {
		t.Fatalf("collision-only error extracts line %q; want nothing (a collision is not a refusal)", line)
	}
}

// TestProbeScopeRefusalOnReportsTheFirstRefusal pins the reason replay
// through the REAL function: the first bus that really refuses contributes
// its manager line, and a collision-only bus contributes nothing.
func TestProbeScopeRefusalOnReportsTheFirstRefusal(t *testing.T) {
	const refusal = "Failed to start transient scope unit: Interactive authentication required"
	if runtime.GOOS != "linux" {
		if denied, line := probeScopeRefusalOn(nil, nil, func() bool { return true }); denied || line != "" {
			t.Fatalf("refusal replay off Linux = (%v, %q); want no refusal without a real systemd", denied, line)
		}
		// probeScopeRefusalOn gates on GOOS the same way the probe loop
		// does: the acceptance/refusal matrix below pins on Linux CI.
		// Off Linux the branch above is the whole pin.
		return
	}
	run := func(userScope bool, unit string) error {
		_ = unit
		if userScope {
			return fmt.Errorf("systemd-run --scope probe: %w: %s", errors.New("exit status 1"), refusal)
		}
		return nil
	}
	unit := func() string { return "test-replay.scope" }
	denied, line := probeScopeRefusalOn(run, unit, func() bool { return true })
	if denied {
		t.Fatalf("refusal replay = denied with line %q; want the user bus's acceptance to win (no refusal)", line)
	}
	runBoth := func(userScope bool, unit string) error {
		_ = unit
		_ = userScope
		return fmt.Errorf("systemd-run --scope probe: %w: %s", errors.New("exit status 1"), refusal)
	}
	denied, line = probeScopeRefusalOn(runBoth, unit, func() bool { return true })
	if !denied || !strings.Contains(line, "Interactive authentication required") {
		t.Fatalf("refusal replay = (%v, %q); want the first bus's manager line", denied, line)
	}
}
