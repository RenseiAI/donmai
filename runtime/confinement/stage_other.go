//go:build !linux

package confinement

// RunLandlockStageFromEnv runs the Landlock stage when the stage marker is
// set. The Linux mount-namespace backend is the only backend with a stage;
// on other operating systems there is nothing to run, so it never handles.
func RunLandlockStageFromEnv() (handled bool, exitCode int) {
	return false, 0
}

// ScopesAvailable reports whether the scope layer is enforced on this
// host. Only the Linux mount-namespace backend has one; every other
// backend answers enforced (nothing missing), never degraded. Tests stub
// the portable scopesProbe (see confinement.go); here the verdict still
// reads through it, so the cache recheck stays stubbable on every host.
func ScopesAvailable() (bool, string) { return scopeVerdict(scopesProbe()) }

// defaultScopesProbe probes the running kernel for the scope layer. It is
// defined on every host so the portable tests stay buildable; only Linux
// kernels ever report below the floor, so anywhere else the answer is the
// floor itself (enforced by definition: nothing missing).
func defaultScopesProbe() int { return landlockScopeABI }

// abstractSocketsScoped reports whether abstract unix sockets outside the
// boundary are closed: they are a Linux feature, and no other backend has
// them to close.
func abstractSocketsScoped() (bool, string) {
	return false, "abstract unix sockets are a Linux feature"
}
