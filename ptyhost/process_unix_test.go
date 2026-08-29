//go:build unix

package ptyhost

import (
	"syscall"
	"testing"
)

// TestSignalNameUsesTheSharedVocabulary pins this package's mapping to the one
// attachwire owns, plus the two cases attachwire deliberately cannot carry: a
// zero signal, and a signal whose NUMBER differs across the unixes.
func TestSignalNameUsesTheSharedVocabulary(t *testing.T) {
	for _, tc := range []struct {
		sig  syscall.Signal
		want string
	}{
		{sig: 0, want: ""},
		{sig: syscall.SIGTERM, want: "SIGTERM"},
		{sig: syscall.SIGKILL, want: "SIGKILL"},
		{sig: syscall.SIGINT, want: "SIGINT"},
		{sig: syscall.SIGBUS, want: "SIGBUS"},
	} {
		if got := signalName(tc.sig); got != tc.want {
			t.Errorf("signalName(%v) = %q, want %q", tc.sig, got, tc.want)
		}
	}
	if got := signalName(syscall.SIGUSR2); got == "" || got[:3] != "SIG" {
		t.Errorf("signalName(SIGUSR2) = %q, want a SIG-prefixed fallback", got)
	}
}
