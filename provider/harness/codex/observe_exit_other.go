//go:build !windows && !darwin && !linux

package codex

import "time"

// Portable fallback observes the unreaped child, never calling Wait itself.
func observeOwnedProcessExit(pid int) error {
	for {
		state, err := ownedProcessGroupState(pid)
		if err != nil {
			return err
		}
		if !state.anchored || !state.leaderRunning {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}
