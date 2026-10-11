package seatbudget

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

// TestVerifyScopePlacementPrefersTheUserManager pins the permission gate's
// preference order through the injected bus probe and runner: a reachable
// user manager that accepts the throwaway scope wins without ever asking
// the system bus; a user manager that refuses falls back to the system
// bus; a host where neither accepts reports no usable backend. The runner
// is the systemd-run-shaped fake the seat brief requires — no test here
// may need real systemd, so the order pins on every host including
// non-Linux CI (where the gate answers false without calling the runner).
func TestVerifyScopePlacementPrefersTheUserManager(t *testing.T) {
	denied := errors.New("systemd-run --scope probe: exit status 1: Failed to start transient scope unit: Interactive authentication required")
	cases := []struct {
		name       string
		hasUserBus bool
		run        func(userScope bool) error
		want       bool
		wantBuses  []bool
	}{
		{
			name:       "user manager accepts: the system bus is never asked",
			hasUserBus: true,
			run:        func(bool) error { return nil },
			want:       true,
			wantBuses:  []bool{true},
		},
		{
			name:       "user manager refuses: the system bus decides",
			hasUserBus: true,
			run: func(userScope bool) error {
				if userScope {
					return denied
				}
				return nil
			},
			want:      true,
			wantBuses: []bool{true, false},
		},
		{
			name:       "both buses refuse: no usable scope backend",
			hasUserBus: true,
			run:        func(bool) error { return denied },
			want:       false,
			wantBuses:  []bool{true, false},
		},
		{
			name:       "no user manager: only the system bus is asked",
			hasUserBus: false,
			run:        func(bool) error { return nil },
			want:       true,
			wantBuses:  []bool{false},
		},
		{
			name:       "no user manager and the system bus refuses: none",
			hasUserBus: false,
			run:        func(bool) error { return denied },
			want:       false,
			wantBuses:  []bool{false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var asked []bool
			run := func(userScope bool) error {
				asked = append(asked, userScope)
				return tc.run(userScope)
			}
			if runtime.GOOS != "linux" {
				if verifyScopePlacementWithBusProbe(run, func() bool { return tc.hasUserBus }) {
					t.Fatal("scope permission granted off Linux; want false without a real systemd")
				}
				if len(asked) != 0 {
					t.Fatalf("runner asked on buses %v off Linux; want no probe at all", asked)
				}
				return
			}
			if got := verifyScopePlacementWithBusProbe(run, func() bool { return tc.hasUserBus }); got != tc.want {
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
// Presence without permission must never select the systemd backend.
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

// TestScopeRefusalCarriesTheManagerMessage pins the probe error shape: a
// refusal folds the manager's own stderr into the error, so the launch
// path that surfaces it names the reason (for example the polkit denial)
// instead of only \"exited without publishing one\".
func TestScopeRefusalCarriesTheManagerMessage(t *testing.T) {
	err := errors.New("systemd-run --scope probe: exit status 1: Failed to start transient scope unit: Interactive authentication required")
	if !strings.Contains(err.Error(), "Interactive authentication required") {
		t.Fatalf("probe error = %q; want the manager's own message carried", err)
	}
}
