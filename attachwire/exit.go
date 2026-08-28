package attachwire

// §3.1 Exit payload + §12.2 exit-code convention helpers.

// ExitSignalBase is the shell-convention offset for signal death: a process
// killed by signal N reports exitCode = ExitSignalBase + N (§12.2), e.g.
// SIGKILL (9) → 137.
const ExitSignalBase = 128

// ExitPayload is the §3.1 Exit layout: [exitCode][signalLen][signal]. The PTY
// child exited. On a normal exit Signal is empty and ExitCode is the process
// exit code; on signal death Signal carries the signal name and
// ExitCode = 128 + signum (§12.2). Exit is the final seq-bearing host frame.
type ExitPayload struct {
	ExitCode uint64
	Signal   string
}

// NewNormalExit builds an Exit for a normal process exit (§12.2): the given
// code, no signal.
func NewNormalExit(code uint64) ExitPayload {
	return ExitPayload{ExitCode: code}
}

// NewSignalExit builds an Exit for signal death (§12.2): Signal is the signal
// name (e.g. "SIGKILL") and ExitCode is set to 128 + signum.
func NewSignalExit(signal string, signum int) ExitPayload {
	return ExitPayload{ExitCode: ExitCodeForSignal(signum), Signal: signal}
}

// ExitCodeForSignal returns the shell-convention exit code for death by the
// given signal number: 128 + signum (§12.2).
func ExitCodeForSignal(signum int) uint64 {
	return uint64(ExitSignalBase + signum) //nolint:gosec // G115: signum is a POSIX signal number (1-64), so 128+signum is always positive and well under MaxInt64
}

// SignalName maps a POSIX signal number to the conventional name the §12.2
// Exit payload carries ("SIGTERM" rather than the Go runtime's "terminated"),
// or "" for zero and for a signal outside the mapped set.
//
// It lives here, beside the exit-code convention it pairs with, so that every
// producer of an Exit payload or a terminal tombstone spells one signal the
// same way. Taking an int rather than a syscall.Signal keeps the package
// buildable everywhere the wire types are.
func SignalName(signum int) string {
	switch signum {
	case 1:
		return "SIGHUP"
	case 2:
		return "SIGINT"
	case 3:
		return "SIGQUIT"
	case 4:
		return "SIGILL"
	case 6:
		return "SIGABRT"
	case 8:
		return "SIGFPE"
	case 9:
		return "SIGKILL"
	case 11:
		return "SIGSEGV"
	case 13:
		return "SIGPIPE"
	case 14:
		return "SIGALRM"
	case 15:
		return "SIGTERM"
	default:
		return ""
	}
}

// SignumFromExitCode inverts the §12.2 convention: if code is in the
// signal-death band (128 < code ≤ 128+255) it returns the signal number and
// true; otherwise it returns 0 and false. Note that a program may legitimately
// return a code in this band on a normal exit — the Signal field (see
// ExitPayload.BySignal) is the authoritative discriminator; this helper only
// applies the arithmetic.
func SignumFromExitCode(code uint64) (signum int, wasSignal bool) {
	if code > ExitSignalBase && code <= ExitSignalBase+255 {
		return int(code) - ExitSignalBase, true
	}
	return 0, false
}

// BySignal reports whether this Exit represents signal death, i.e. Signal is
// non-empty (§12.2 — the authoritative discriminator).
func (p ExitPayload) BySignal() bool { return p.Signal != "" }

// Encode serializes the Exit payload (§3.1).
func (p ExitPayload) Encode() []byte {
	buf := make([]byte, 0, 2*MaxVarintLen+len(p.Signal))
	buf = AppendUvarint(buf, p.ExitCode)
	buf = AppendUvarint(buf, uint64(len(p.Signal)))
	buf = append(buf, p.Signal...)
	return buf
}

// DecodeExit parses an Exit payload (§3.1).
func DecodeExit(payload []byte) (ExitPayload, error) {
	r := newReader(payload)
	code, err := r.uvarint()
	if err != nil {
		return ExitPayload{}, err
	}
	sig, err := r.lenPrefixed()
	if err != nil {
		return ExitPayload{}, err
	}
	if err := r.expectDone(); err != nil {
		return ExitPayload{}, err
	}
	return ExitPayload{ExitCode: code, Signal: string(sig)}, nil
}
