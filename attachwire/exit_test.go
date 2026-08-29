package attachwire

import (
	"reflect"
	"testing"
)

func TestExitConventionHelpers(t *testing.T) {
	// SIGKILL = 9 -> exitCode 137.
	sig := NewSignalExit("SIGKILL", 9)
	if sig.ExitCode != 137 {
		t.Fatalf("128+9 = %d, want 137", sig.ExitCode)
	}
	if !sig.BySignal() {
		t.Fatalf("signal exit must report BySignal()")
	}
	if got := ExitCodeForSignal(15); got != 143 {
		t.Fatalf("ExitCodeForSignal(15) = %d, want 143", got)
	}
	signum, wasSignal := SignumFromExitCode(137)
	if !wasSignal || signum != 9 {
		t.Fatalf("SignumFromExitCode(137) = (%d,%v), want (9,true)", signum, wasSignal)
	}
	if _, wasSignal := SignumFromExitCode(0); wasSignal {
		t.Fatalf("exit code 0 must not be in the signal band")
	}
	if _, wasSignal := SignumFromExitCode(128); wasSignal {
		t.Fatalf("exit code 128 (signum 0) must not be in the signal band")
	}

	normal := NewNormalExit(0)
	if normal.BySignal() {
		t.Fatalf("normal exit must not report BySignal()")
	}
}

func TestExitRoundTrip(t *testing.T) {
	cases := []ExitPayload{
		NewNormalExit(0),
		NewNormalExit(1),
		NewSignalExit("SIGTERM", 15),
		{ExitCode: 42, Signal: ""},
	}
	for _, want := range cases {
		got, err := DecodeExit(want.Encode())
		if err != nil {
			t.Fatalf("DecodeExit: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Exit round trip: got %#v want %#v", got, want)
		}
	}
}

func TestExitTruncation(t *testing.T) {
	// signalLen claims 4 with no bytes.
	buf := []byte{0x89, 0x01, 0x04} // exitCode=137, signalLen=4, no signal
	if _, err := DecodeExit(buf); !IsFramingErr(err) {
		t.Fatalf("want framing error, got %v", err)
	}
}

// TestSignalNameSpellsTheConventionalNames pins the vocabulary every producer
// of an Exit payload or a terminal tombstone shares. Go's own Signal.String()
// yields "terminated"; the §12.2 convention is "SIGTERM", and two producers
// spelling one signal differently is a correlation that silently stops
// matching.
//
// The table is keyed by NUMBER, so it deliberately excludes the signals whose
// number differs across the unixes (SIGBUS is 7 on Linux and 10 on darwin);
// those stay with their platform-specific caller.
func TestSignalNameSpellsTheConventionalNames(t *testing.T) {
	for _, tc := range []struct {
		signum int
		want   string
	}{
		{signum: 0, want: ""},
		{signum: 1, want: "SIGHUP"},
		{signum: 2, want: "SIGINT"},
		{signum: 9, want: "SIGKILL"},
		{signum: 15, want: "SIGTERM"},
		{signum: 7, want: ""},
		{signum: 10, want: ""},
		{signum: 64, want: ""},
	} {
		if got := SignalName(tc.signum); got != tc.want {
			t.Errorf("SignalName(%d) = %q, want %q", tc.signum, got, tc.want)
		}
	}
}
