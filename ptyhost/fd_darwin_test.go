//go:build darwin

package ptyhost

import (
	"errors"
	"fmt"
	"testing"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func TestFDToIntDarwin(t *testing.T) {
	maxInt := uintptr(^uint(0) >> 1)

	for _, tc := range []struct {
		name string
		fd   uintptr
		want int
	}{
		{name: "zero", fd: 0, want: 0},
		{name: "maximum", fd: maxInt, want: int(maxInt)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fdToInt(tc.fd)
			if err != nil {
				t.Fatalf("fdToInt(%d): %v", tc.fd, err)
			}
			if got != tc.want {
				t.Fatalf("fdToInt(%d) = %d, want %d", tc.fd, got, tc.want)
			}
		})
	}

	overflow := maxInt + 1
	got, err := fdToInt(overflow)
	if got != 0 {
		t.Fatalf("fdToInt(%d) value = %d, want 0", overflow, got)
	}
	if !errors.Is(err, errFDOutOfRange) {
		t.Fatalf("fdToInt(%d) error = %v, want errFDOutOfRange", overflow, err)
	}
	wantError := fmt.Sprintf("file descriptor %d: file descriptor is outside the int range", overflow)
	if err.Error() != wantError {
		t.Fatalf("fdToInt(%d) error = %q, want %q", overflow, err, wantError)
	}

	if err := applyWinsize(overflow, 80, 24, 0, 0); err == nil || err.Error() != wantError {
		t.Fatalf("applyWinsize(%d) error = %v, want %q", overflow, err, wantError)
	}
	if got := echoModeOfFd(overflow); got != attachwire.EchoUnknown {
		t.Fatalf("echoModeOfFd(%d) = %d, want EchoUnknown", overflow, got)
	}
}

func TestEchoModeOfFdDarwin(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}
	defer func() {
		if err := master.Close(); err != nil {
			t.Errorf("close pty master: %v", err)
		}
	}()
	defer func() {
		if err := slave.Close(); err != nil {
			t.Errorf("close pty slave: %v", err)
		}
	}()

	slaveFD, err := fdToInt(slave.Fd())
	if err != nil {
		t.Fatalf("convert slave fd: %v", err)
	}
	original, err := unix.IoctlGetTermios(slaveFD, unix.TIOCGETA)
	if err != nil {
		t.Fatalf("read original termios: %v", err)
	}
	defer func() {
		if err := unix.IoctlSetTermios(slaveFD, unix.TIOCSETA, original); err != nil {
			t.Errorf("restore termios: %v", err)
		}
	}()

	for _, tc := range []struct {
		name string
		on   bool
		want uint8
	}{
		{name: "echo on", on: true, want: attachwire.EchoOn},
		{name: "echo off", on: false, want: attachwire.EchoOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode := *original
			if tc.on {
				mode.Lflag |= unix.ECHO
			} else {
				mode.Lflag &^= unix.ECHO
			}
			if err := unix.IoctlSetTermios(slaveFD, unix.TIOCSETA, &mode); err != nil {
				t.Fatalf("set termios: %v", err)
			}
			if got := echoModeOfFd(master.Fd()); got != tc.want {
				t.Fatalf("echoModeOfFd() = %d, want %d", got, tc.want)
			}
		})
	}
}
