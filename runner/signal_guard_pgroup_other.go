//go:build !unix

package runner

func chainOnProcessSpawned(existing func(pid int), after func(pid int)) func(pid int) {
	if existing == nil {
		return after
	}
	if after == nil {
		return existing
	}
	return func(pid int) {
		existing(pid)
		after(pid)
	}
}

func (r *Runner) recordSessionProcessGroup(statePath, sessionID string, pid int, guardDir string) {
}
