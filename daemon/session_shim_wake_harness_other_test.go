//go:build !darwin && !linux

package daemon

// configureWakeHarnessTerminal has no termios implementation on this
// platform: the wake harness runs with the terminal mode left as spawned.
// The `ack:` answers still land and every assertion matches on them, so
// this is a fidelity difference, not a correctness one.
func configureWakeHarnessTerminal(_ wakeFixtureMode) {}
