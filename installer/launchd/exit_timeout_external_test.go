package launchd_test

import (
	"testing"

	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/installer/launchd"
)

// TestExitTimeOutCoversDrainDefault statically asserts the launchd exit
// window is at least the daemon's config-default graceful drain (plus
// margin), pinned against the real default rather than a mirrored literal —
// if the drain default grows past the plist's exit window, launchd would
// SIGKILL the job mid-drain again and this test fails first.
//
// It lives in the external launchd_test package so it can import daemon no
// matter how daemon's own dependencies on the installer packages evolve: an
// in-package test importing daemon would form an import cycle the moment daemon
// imports launchd.
func TestExitTimeOutCoversDrainDefault(t *testing.T) {
	drain := daemon.DefaultConfig().AutoUpdate.DrainTimeoutSeconds
	if drain <= 0 {
		t.Fatalf("daemon config-default DrainTimeoutSeconds = %d, want > 0", drain)
	}
	if launchd.ExitTimeOutSeconds < drain+30 {
		t.Errorf("ExitTimeOutSeconds = %d, want >= config-default drain %d + 30s escalation margin",
			launchd.ExitTimeOutSeconds, drain)
	}
}
