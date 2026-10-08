//go:build !linux

package confinement

// RunLandlockStageFromEnv runs the Landlock stage when the stage marker is
// set. The Linux mount-namespace backend is the only backend with a stage;
// on other operating systems there is nothing to run, so it never handles.
func RunLandlockStageFromEnv() (handled bool, exitCode int) {
	return false, 0
}
