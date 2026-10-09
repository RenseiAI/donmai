//go:build !darwin && !linux

package daemon

// disableShimEchoTerminalEcho has no termios implementation on this platform:
// the echo harness runs with terminal echo left on. The `ack:` answers still
// land and every assertion matches on them, so this is a fidelity difference,
// not a correctness one.
func disableShimEchoTerminalEcho() {}
