package afcli

import (
	"errors"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/daemon"
)

// TestNeedsSetupExitCodeMapsRefusalToNoRestartStatus pins the exit
// contract the supervisor reads: the typed setup refusal maps to the
// status the unit holds failed (RestartPreventExitStatus), while every
// other error maps to nothing (the caller exits 1 and restarts). It
// drives NeedsSetupExitCode, the function main uses — reverting main's
// NeedsSetupExitCode branch back to a flat exit 1 reopens the restart
// storm this status closes.
func TestNeedsSetupExitCodeMapsRefusalToNoRestartStatus(t *testing.T) {
	code, ok := NeedsSetupExitCode(afclient.ErrNeedsSetup)
	if !ok || code != afclient.ExitCodeNeedsSetup {
		t.Fatalf("NeedsSetupExitCode(setup refusal) = (%d, %v), want (%d, true)", code, ok, afclient.ExitCodeNeedsSetup)
	}
	if afclient.ExitCodeNeedsSetup != 4 {
		t.Fatalf("ExitCodeNeedsSetup = %d, want 4 (the status the unit holds failed)", afclient.ExitCodeNeedsSetup)
	}
	wrapped := errors.Join(errors.New("daemon start: boom"), afclient.ErrNeedsSetup)
	if code, ok := NeedsSetupExitCode(wrapped); !ok || code != 4 {
		t.Fatalf("NeedsSetupExitCode(wrapped refusal) = (%d, %v), want (4, true)", code, ok)
	}
	if _, ok := NeedsSetupExitCode(errors.New("boom")); ok {
		t.Fatal("NeedsSetupExitCode(plain error) = true, want false (plain failures still restart)")
	}
	if _, ok := NeedsSetupExitCode(nil); ok {
		t.Fatal("NeedsSetupExitCode(nil) = true, want false")
	}
	_ = daemon.ExitCodeRestart
}
