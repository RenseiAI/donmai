//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package daemon

import (
	"errors"

	"github.com/RenseiAI/donmai/sessionshim"
)

// osShimLaunchProcess on a platform without POSIX sessions is a liveness probe
// only. Those platforms have no per-session shim at all — sessionshim.Start
// refuses with ErrShimUnsupported before one is ever launched (see
// configureShimProcess) — so this exists to keep the package building, not to
// carry a lane that runs there.
type osShimLaunchProcess struct {
	identity sessionshim.ProcessIdentity
}

func newShimLaunchProcess(started sessionshim.ProcessIdentity) shimLaunchProcess {
	return osShimLaunchProcess{identity: started}
}

func (p osShimLaunchProcess) Alive() (bool, error) { return p.identity.Alive() }

func (p osShimLaunchProcess) StopAndReap() error {
	return errors.New("session shim: stopping an abandoned launch is unsupported on this operating system")
}
