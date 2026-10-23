package sessionshim

import (
	"os"
	"os/exec"
	"testing"
)

// TestProcessIdentityForPinsALiveChild covers the pin-then-prove pairing the
// tombstone path depends on: an identity taken while a process is running
// answers Alive() truthfully afterwards, and a PID with the WRONG start time is
// reported gone rather than live — the anti-reuse question §D2 requires.
func TestProcessIdentityForPinsALiveChild(t *testing.T) {
	t.Parallel()
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := ProcessIdentityFor(child.Process.Pid)
	if err != nil {
		_ = child.Process.Kill()
		t.Fatal(err)
	}
	if identity.PID != child.Process.Pid || identity.StartedAt <= 0 {
		t.Fatalf("pinned identity = %+v, want the child's pid and a positive start time", identity)
	}
	alive, err := identity.Alive()
	if err != nil || !alive {
		t.Fatalf("pinned live child reported alive=%t err=%v", alive, err)
	}
	// A reused pid is a different incarnation: mutate the drift-free key
	// when the platform carries one (Linux ticks plus boot ID), else the
	// Unix-nanosecond start time. Mutating only StartedAt on Linux would
	// leave the ticks plus boot ID identical and must still verify — the
	// wall clock is not part of the identity.
	reused := identity
	if reused.StartTicks > 0 && reused.BootID != "" {
		reused.StartTicks++
	} else {
		reused.StartedAt++
	}
	if alive, err := reused.Alive(); err != nil || alive {
		t.Fatalf("a pid with a different start time reported alive=%t err=%v — a reused pid is a "+
			"DIFFERENT process and the recorded one is gone", alive, err)
	}

	_ = child.Process.Kill()
	_ = child.Wait()
	if alive, err := identity.Alive(); err != nil || alive {
		t.Fatalf("reaped child reported alive=%t err=%v", alive, err)
	}
}

func TestProcessIdentityForRefusesANonPositivePID(t *testing.T) {
	t.Parallel()
	if _, err := ProcessIdentityFor(0); err == nil {
		t.Fatal("ProcessIdentityFor(0) returned an identity")
	}
	if _, err := ProcessIdentityFor(-1); err == nil {
		t.Fatal("ProcessIdentityFor(-1) returned an identity")
	}
}

func TestSelfPinsThisProcess(t *testing.T) {
	t.Parallel()
	self, err := Self()
	if err != nil {
		t.Fatal(err)
	}
	if self.PID != os.Getpid() || self.StartedAt <= 0 {
		t.Fatalf("Self() = %+v, want this process with a positive start time", self)
	}
	alive, err := self.Alive()
	if err != nil || !alive {
		t.Fatalf("Self() reported alive=%t err=%v", alive, err)
	}
}
