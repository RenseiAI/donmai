//go:build !darwin && !linux

package daemon

// observeProcessPriority reports that this OS has no process-priority
// observation, so /status omits the field.
func observeProcessPriority() (processPriorityObservation, bool) {
	return processPriorityObservation{}, false
}
