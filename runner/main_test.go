package runner

import (
	"fmt"
	"math"
	"os"
	"testing"

	"golang.org/x/term"
)

// testRoleEnv selects a re-exec child role. The suite re-runs its own binary
// as a PTY child so a test can drive a REAL raw-mode reader rather than a
// cooked-mode shell, which is the only way to tell a Return key from a Ctrl-J.
const testRoleEnv = "DONMAI_RUNNER_TEST_ROLE"

// Marker lines the raw-key child emits. They are deliberately distinct so a
// test asserting "the notice arrived as a TURN" cannot be satisfied by the
// bytes merely reaching the PTY.
const (
	rawKeysReady     = "RAWKEYS-READY"
	rawKeysSubmit    = "SUBMIT:"         // a Return key committed the accumulated line
	rawKeysNotSubmit = "NON-SUBMIT-KEY:" // a distinct key that did NOT commit
)

func TestMain(m *testing.M) {
	switch os.Getenv(testRoleEnv) {
	case "rawkeys":
		rawKeysChild()
	case "":
		os.Exit(m.Run())
	default:
		fmt.Fprintln(os.Stderr, "unknown "+testRoleEnv)
		os.Exit(2)
	}
}

// rawKeysChild models the input layer of a raw-mode TUI closely enough to
// answer the one question the existing harnesses could not: which byte
// SUBMITS a turn?
//
// It puts the PTY slave into raw mode itself — so ICRNL is off and no line
// discipline translates anything — then reads one byte at a time and treats
// CR and LF as DIFFERENT KEYS, which is exactly what a raw-mode keypress
// parser does (CR is the Return key; LF is the Ctrl-J chord). Only CR commits
// the accumulated line. A cooked-mode `read` loop cannot make this
// distinction, because the line discipline terminates the read on either.
//
// What this fixture does NOT do is prove any particular third-party REPL's
// key bindings; it proves that the byte we send is the byte the application
// sees, that CR and LF are separable there, and that the runner sends the one
// a Return key produces.
func rawKeysChild() {
	// os.File.Fd returns a uintptr and term.MakeRaw wants an int. On a 64-bit
	// host uintptr spans twice int's positive range, so the conversion is a
	// real (if unreachable) truncation — bound it rather than silence the
	// checker, since a truncated descriptor would put the WRONG fd into raw
	// mode and the fixture would then lie about which byte submits a turn.
	raw := os.Stdin.Fd()
	if raw > uintptr(math.MaxInt) {
		fmt.Fprintln(os.Stderr, "stdin fd out of range:", raw)
		os.Exit(3)
	}
	fd := int(raw)
	restore, err := term.MakeRaw(fd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "raw mode:", err)
		os.Exit(3)
	}
	rawKeysLoop()
	_ = term.Restore(fd, restore)
	os.Exit(0)
}

// rawKeysLoop reads keys until Ctrl-C or EOF. Split out of rawKeysChild so
// the terminal is restored on the way out (os.Exit would skip a defer).
func rawKeysLoop() {
	// OPOST is off in raw mode, so every line ends CR LF explicitly.
	out := func(s string) { _, _ = fmt.Fprint(os.Stdout, s+"\r\n") }
	out(rawKeysReady)

	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if n == 0 || err != nil {
			return
		}
		switch b := buf[0]; b {
		case '\r':
			out(rawKeysSubmit + string(line))
			line = line[:0]
		case 0x03: // Ctrl-C ends the fixture
			return
		default:
			if b < 0x20 || b == 0x7f {
				// Any other control byte — LF included — is a distinct key
				// that leaves the line uncommitted.
				out(fmt.Sprintf("%s0x%02x", rawKeysNotSubmit, b))
				continue
			}
			line = append(line, b)
		}
	}
}
