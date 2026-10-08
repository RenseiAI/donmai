//go:build !linux

package confinement

// RunLandlockStageFromEnv runs the Landlock stage when the stage marker is
// set. The Linux mount-namespace backend is the only backend with a stage;
// on other operating systems there is nothing to run, so it never handles.
func RunLandlockStageFromEnv() (handled bool, exitCode int) {
	return false, 0
}

// abstractSocketsScoped reports whether abstract unix sockets outside the
// boundary are closed: they are a Linux feature, and no other backend has
// them to close.
func abstractSocketsScoped() (bool, string) {
	return false, "abstract unix sockets are a Linux feature"
}
