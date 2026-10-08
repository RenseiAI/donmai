package daemon

import (
	"runtime"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/runtime/confinement"
)

// confinementKernelFloor names the kernel floor for a full seat boundary:
// the scope layer needs Landlock ABI 6, which ships in Linux 6.12. Below
// it the seat still starts, but signals and abstract sockets outside stay
// reachable and the host never reports itself as confined.
const confinementKernelFloor = "Linux 6.12 (Landlock ABI 6) for the full boundary"

// confinementStatus is the daemon's OS-confinement self-check for /status
// and /doctor. It probes the running kernel through the confinement
// backend: where the scope layer is missing the report says degraded with
// the reason, never attested. Secret-free: it names the backend and the
// missing layer, never paths, digests or probe values.
//
// The probe is cheap (one Landlock version syscall) and uncached, so a
// status reader always sees this boot's kernel rather than a stale note.
func confinementStatus() *afclient.ConfinementStatus {
	backend := confinement.DefaultBackend()
	if backend == nil {
		return &afclient.ConfinementStatus{}
	}
	status := &afclient.ConfinementStatus{Backend: string(backend.Name())}
	if ok, why := confinement.ScopesAvailable(); !ok {
		status.Degraded = why
		status.Detail = "seat boundary is partial below " + confinementKernelFloor + ": same-user signals and abstract sockets outside stay reachable"
		return status
	}
	status.Attested = true
	if runtime.GOOS == "linux" {
		status.Detail = "seat boundary holds at " + confinementKernelFloor + " or newer"
	}
	return status
}
