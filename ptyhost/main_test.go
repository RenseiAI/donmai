package ptyhost

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"golang.org/x/term"
)

// TestMain dispatches re-exec child roles before running the suite. The CPR
// conformance test (§12, Appendix A) re-execs this test binary with
// PTYHOST_TEST_ROLE=cpr so the child runs under a real ptyhost PTY.
func TestMain(m *testing.M) {
	switch os.Getenv("PTYHOST_TEST_ROLE") {
	case "cpr":
		cprChild()
	case "":
		os.Exit(m.Run())
	default:
		fmt.Fprintln(os.Stderr, "unknown PTYHOST_TEST_ROLE")
		os.Exit(2)
	}
}

// cprChild runs under a PTY: it puts the terminal in raw mode, emits a
// Cursor-Position-Report request (CSI 6n) and a Primary-Device-Attributes
// request (CSI c), and blocks reading each reply. It exits 0 only if the host VT
// answered both correctly on the PTY master (§12: the host VT is the terminal-
// query responder). Any failure exits non-zero.
func cprChild() {
	fd, err := fdToInt(os.Stdin.Fd())
	if err != nil {
		fmt.Fprintln(os.Stderr, "file descriptor:", err)
		os.Exit(3)
	}
	if _, err := term.MakeRaw(fd); err != nil {
		fmt.Fprintln(os.Stderr, "makeraw:", err)
		os.Exit(3)
	}
	// NB: no deferred term.Restore — this child ends every path with os.Exit
	// (which skips defers) and the PTY is torn down by the host regardless.

	// CPR: expect ESC [ <row> ; <col> R with row,col >= 1.
	_, _ = os.Stdout.WriteString("\x1b[6n")
	reply, err := readUntil(os.Stdin, 'R', 3*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cpr read: %v\n", err)
		os.Exit(4)
	}
	if !validCPR(reply) {
		fmt.Fprintf(os.Stderr, "bad CPR: %q\n", reply)
		os.Exit(5)
	}

	// DA: expect ESC [ ? … c.
	_, _ = os.Stdout.WriteString("\x1b[c")
	da, err := readUntil(os.Stdin, 'c', 3*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "da read: %v\n", err)
		os.Exit(6)
	}
	if !bytes.Contains(da, []byte("\x1b[?")) {
		fmt.Fprintf(os.Stderr, "bad DA: %q\n", da)
		os.Exit(7)
	}
	os.Exit(0)
}

// readUntil reads from f until the terminator byte is seen or the deadline
// elapses. It runs the (blocking) read on a goroutine so a missing reply times
// out rather than hanging (the child process exits right after, so the parked
// read is reaped).
func readUntil(f *os.File, term byte, d time.Duration) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var acc []byte
		buf := make([]byte, 64)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				acc = append(acc, buf[:n]...)
			}
			if bytes.IndexByte(acc, term) >= 0 || err != nil {
				ch <- result{acc, err}
				return
			}
		}
	}()
	select {
	case r := <-ch:
		return r.b, r.err
	case <-time.After(d):
		return nil, errors.New("timeout")
	}
}

// validCPR parses ESC [ <row> ; <col> R and checks row,col >= 1.
func validCPR(b []byte) bool {
	i := bytes.Index(b, []byte("\x1b["))
	if i < 0 {
		return false
	}
	rest := b[i+2:]
	end := bytes.IndexByte(rest, 'R')
	if end < 0 {
		return false
	}
	parts := bytes.SplitN(rest[:end], []byte(";"), 2)
	if len(parts) != 2 {
		return false
	}
	row, ok1 := atoiPositive(parts[0])
	col, ok2 := atoiPositive(parts[1])
	return ok1 && ok2 && row >= 1 && col >= 1
}

func atoiPositive(b []byte) (int, bool) {
	if len(b) == 0 {
		return 0, false
	}
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}
