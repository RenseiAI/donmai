package afcli

import (
	"errors"
	"runtime"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/daemon"
)

// TestNeedsSetupExitCodeMapsRefusalToNoRestartStatus pins the exit
// contract the supervisor reads: the typed setup refusal maps to the
// hold status for this host's supervisor (the setup status the systemd
// unit holds failed, the clean status the launchd plist parks), while
// every other error maps to nothing (the caller exits 1 and restarts).
// It drives NeedsSetupExitCode, the function main uses — reverting main's
// NeedsSetupExitCode branch back to a flat exit 1 reopens the restart
// storm this status closes.
func TestNeedsSetupExitCodeMapsRefusalToNoRestartStatus(t *testing.T) {
	wantHold := setupHoldExitCode(runtime.GOOS)
	code, ok := NeedsSetupExitCode(afclient.ErrNeedsSetup)
	if !ok || code != wantHold {
		t.Fatalf("NeedsSetupExitCode(setup refusal) = (%d, %v), want (%d, true)", code, ok, wantHold)
	}
	wrapped := errors.Join(errors.New("daemon start: boom"), afclient.ErrNeedsSetup)
	if code, ok := NeedsSetupExitCode(wrapped); !ok || code != wantHold {
		t.Fatalf("NeedsSetupExitCode(wrapped refusal) = (%d, %v), want (%d, true)", code, ok, wantHold)
	}
	if _, ok := NeedsSetupExitCode(errors.New("boom")); ok {
		t.Fatal("NeedsSetupExitCode(plain error) = true, want false (plain failures still restart)")
	}
	if _, ok := NeedsSetupExitCode(nil); ok {
		t.Fatal("NeedsSetupExitCode(nil) = true, want false")
	}
	_ = daemon.ExitCodeRestart
}

// TestSetupHoldExitCodeKeepsLinuxOnTheSetupStatus pins the systemd half
// of the hold contract: on Linux the refusal keeps the setup status the
// unit names in RestartPreventExitStatus, so the unit waits failed
// instead of restarting every RestartSec. Returning the clean status
// here would park the unit as exited rather than failed and orphan the
// directive the unit file pins.
func TestSetupHoldExitCodeKeepsLinuxOnTheSetupStatus(t *testing.T) {
	if got := setupHoldExitCode("linux"); got != afclient.ExitCodeNeedsSetup {
		t.Fatalf("setupHoldExitCode(linux) = %d, want the setup status %d the unit holds failed", got, afclient.ExitCodeNeedsSetup)
	}
	if afclient.ExitCodeNeedsSetup != 4 {
		t.Fatalf("ExitCodeNeedsSetup = %d, want 4 (the status the unit holds failed)", afclient.ExitCodeNeedsSetup)
	}
}

// TestSetupHoldExitCodeParksMacOSClean pins the launchd half of the hold
// contract: on macOS the refusal exits clean, the one status the plist's
// KeepAlive (SuccessfulExit=false, Crashed=true) does not respawn — a
// non-zero exit restarts about every ThrottleInterval, so a fresh host
// would spin until setup completes. The plist shape is pinned by the
// launchd KeepAlive test; this pins the binary half. Reverting the
// darwin arm back to the setup status reopens the respawn loop.
func TestSetupHoldExitCodeParksMacOSClean(t *testing.T) {
	if got := setupHoldExitCode("darwin"); got != 0 {
		t.Fatalf("setupHoldExitCode(darwin) = %d, want 0 (the clean status launchd parks instead of respawning)", got)
	}
}
