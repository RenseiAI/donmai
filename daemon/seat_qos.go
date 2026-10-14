package daemon

import (
	"runtime"

	"github.com/RenseiAI/donmai/daemon/seatqos"
)

// seatQoSCommandPrefix renders the per-seat scheduling-class wrapper for
// one seat command on this OS. Only the macOS task-policy tool renders a
// wrapper today: it applies the QoS clamp (utility or background) to the
// seat's worker process, and every process in the seat inherits it. The
// daemon itself never runs under the wrapper — only its seat children do.
//
// The default class returns no prefix: seats spawn exactly as before. Any
// other OS returns no prefix for any class: the setting is accepted and
// reported everywhere, but only macOS has the primitive that makes it
// real. Callers gate on the empty prefix, never on the class, so the
// default path stays byte-identical to unwrapped spawns.
func seatQoSCommandPrefix(class seatqos.Class) []string {
	return seatQoSCommandPrefixForGOOS(class, runtime.GOOS)
}

// seatQoSCommandPrefixForGOOS is seatQoSCommandPrefix parametrised by GOOS
// so tests pin the platform gate on any host. Production passes
// runtime.GOOS.
func seatQoSCommandPrefixForGOOS(class seatqos.Class, goos string) []string {
	if !seatqos.Wraps(class) {
		return nil
	}
	if goos != "darwin" {
		return nil
	}
	return []string{"/usr/sbin/taskpolicy", "-c", string(seatqos.Effective(class))}
}

// wrapSeatCommandWithQoS prepends the scheduling-class wrapper to one seat
// command. A nil prefix returns the command unchanged.
func wrapSeatCommandWithQoS(command []string, prefix []string) []string {
	if len(prefix) == 0 || len(command) == 0 {
		return command
	}
	return append(append([]string(nil), prefix...), command...)
}
