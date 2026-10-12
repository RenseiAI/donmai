package daemon

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/daemon/seatqos"
)

// errSeatQoSCaptureStop ends an AcceptWork capture test at the start
// seam: the wrapped argv is recorded, no child process runs.
var errSeatQoSCaptureStop = errors.New("seat QoS capture stop")

// TestSpawner_SeatQoS_WrapsWorkerThroughAcceptWork drives the PRODUCTION
// spawn path (AcceptWork) with the utility class and asserts the seat
// worker launches under the scheduling wrapper: the command the spawner
// starts carries the task-policy prefix ahead of the worker argv.
// Reverting the spawn to the unwrapped command fails here. The wrapper
// renders only on macOS (the only OS with the primitive), so this test
// asserts the platform-gated production renderer directly: on macOS it
// pins the wrapped argv, elsewhere it pins the passthrough.
func TestSpawner_SeatQoS_WrapsWorkerThroughAcceptWork(t *testing.T) {
	var gotCmd []string
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"/bin/worker", "agent", "run"},
		SeatQoS:               string(seatqos.Utility),
		StdoutPrefixWriter:    PrefixWriterFunc(func(_, _ string) {}),
		StderrPrefixWriter:    PrefixWriterFunc(func(_, _ string) {}),
	})
	s.startCommand = func(cmd *exec.Cmd) error {
		gotCmd = append([]string(nil), cmd.Args...)
		// Fail the start so AcceptWork returns before the child runs:
		// the wrapped argv is captured, no process is launched.
		return errSeatQoSCaptureStop
	}
	_, err := s.AcceptWork(SessionSpec{SessionID: "seat-qos-wrap", Repository: "github.com/a/b"})
	if err == nil || !strings.Contains(err.Error(), "seat QoS capture") {
		t.Fatalf("accept = %v; want the capture stop", err)
	}
	want := seatQoSCommandPrefixForGOOS(seatqos.Utility, runtime.GOOS)
	if len(want) == 0 {
		if strings.Join(gotCmd, " ") != "/bin/worker agent run" {
			t.Errorf("command = %v off macOS; want the bare worker argv", gotCmd)
		}
		return
	}
	joined := strings.Join(gotCmd, " ")
	if !strings.HasPrefix(joined, strings.Join(want, " ")+" ") {
		t.Errorf("command = %q; want the %q prefix ahead of the worker", joined, strings.Join(want, " "))
	}
	if !strings.HasSuffix(joined, "/bin/worker agent run") {
		t.Errorf("command = %q; want the worker argv after the prefix", joined)
	}
}

// TestSpawner_SeatQoS_DefaultSpawnsUnwrapped pins the opt-in default
// through AcceptWork: an unauthored class leaves the worker argv
// byte-identical on every OS.
func TestSpawner_SeatQoS_DefaultSpawnsUnwrapped(t *testing.T) {
	var gotCmd []string
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"/bin/worker", "agent", "run"},
		StdoutPrefixWriter:    PrefixWriterFunc(func(_, _ string) {}),
		StderrPrefixWriter:    PrefixWriterFunc(func(_, _ string) {}),
	})
	s.startCommand = func(cmd *exec.Cmd) error {
		gotCmd = append([]string(nil), cmd.Args...)
		return errSeatQoSCaptureStop
	}
	_, err := s.AcceptWork(SessionSpec{SessionID: "seat-qos-off", Repository: "github.com/a/b"})
	if err == nil || !strings.Contains(err.Error(), "seat QoS capture") {
		t.Fatalf("accept = %v; want the capture stop", err)
	}
	if strings.Join(gotCmd, " ") != "/bin/worker agent run" {
		t.Errorf("command = %v; want the bare worker argv with the default class", gotCmd)
	}
}

// TestSpawner_SeatQoS_OptsClassGovernsSpawn pins the constructor wiring:
// an embedder-composed SpawnerOptions.SeatQoS governs spawns without a
// SetSeatQoS call. Reverting the constructor to drop the opts class
// leaves this spawn unwrapped and fails here.
func TestSpawner_SeatQoS_OptsClassGovernsSpawn(t *testing.T) {
	var gotCmd []string
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"/bin/worker"},
		SeatQoS:               string(seatqos.Utility),
		StdoutPrefixWriter:    PrefixWriterFunc(func(_, _ string) {}),
		StderrPrefixWriter:    PrefixWriterFunc(func(_, _ string) {}),
	})
	s.startCommand = func(cmd *exec.Cmd) error {
		gotCmd = append([]string(nil), cmd.Args...)
		return errSeatQoSCaptureStop
	}
	_, err := s.AcceptWork(SessionSpec{SessionID: "seat-qos-opts", Repository: "github.com/a/b"})
	if err == nil || !strings.Contains(err.Error(), "seat QoS capture") {
		t.Fatalf("accept = %v; want the capture stop", err)
	}
	want := seatQoSCommandPrefixForGOOS(seatqos.Utility, runtime.GOOS)
	if len(want) == 0 {
		return
	}
	if !strings.HasPrefix(strings.Join(gotCmd, " "), strings.Join(want, " ")+" ") {
		t.Errorf("command = %v; want the opts-composed utility prefix", gotCmd)
	}
}

// TestSpawner_SeatQoS_SetRejectsUnknown pins the fail-closed setter: an
// unknown class is rejected and the previous class keeps governing
// spawns, so a bad config can never silently downgrade to default.
func TestSpawner_SeatQoS_SetRejectsUnknown(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
	})
	if err := s.SetSeatQoS("utility"); err != nil {
		t.Fatalf("SetSeatQoS(utility): %v", err)
	}
	if err := s.SetSeatQoS("nice"); err == nil {
		t.Fatal("SetSeatQoS(nice) = nil; want a rejection")
	}
	if got := s.seatQoSForSpawn(); got != seatqos.Utility {
		t.Errorf("class after rejected set = %q; want the previous utility", got)
	}
}

// TestSeatQoSCommandPrefix pins the wrapper matrix through the PRODUCTION
// prefix renderer: the default class never wraps on any OS, non-default
// classes wrap only on macOS, and the wrapper names the configured class.
func TestSeatQoSCommandPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		class seatqos.Class
		goos  string
		want  []string // nil means no wrapper
	}{
		{name: "default never wraps on macOS", class: seatqos.Default, goos: "darwin", want: nil},
		{name: "unset never wraps on macOS", class: "", goos: "darwin", want: nil},
		{name: "default never wraps on Linux", class: seatqos.Default, goos: "linux", want: nil},
		{name: "utility wraps on macOS", class: seatqos.Utility, goos: "darwin", want: []string{"/usr/sbin/taskpolicy", "-c", "utility"}},
		{name: "background wraps on macOS", class: seatqos.Background, goos: "darwin", want: []string{"/usr/sbin/taskpolicy", "-c", "background"}},
		{name: "utility does not wrap on Linux", class: seatqos.Utility, goos: "linux", want: nil},
		{name: "background does not wrap on Linux", class: seatqos.Background, goos: "linux", want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := seatQoSCommandPrefixForGOOS(tc.class, tc.goos)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("prefix(%q, %q) = %v; want %v", tc.class, tc.goos, got, tc.want)
			}
		})
	}
}

// TestWrapSeatCommandWithQoS pins the splice: the wrapper prepends, and an
// empty prefix returns the command unchanged (never an empty splice that
// would shift argv).
func TestWrapSeatCommandWithQoS(t *testing.T) {
	t.Parallel()
	cmd := []string{"/bin/worker", "--serve"}
	got := wrapSeatCommandWithQoS(cmd, []string{"/usr/sbin/taskpolicy", "-c", "utility"})
	if strings.Join(got, " ") != "/usr/sbin/taskpolicy -c utility /bin/worker --serve" {
		t.Errorf("wrapped = %v; want the taskpolicy prefix first", got)
	}
	if plain := wrapSeatCommandWithQoS(cmd, nil); strings.Join(plain, " ") != strings.Join(cmd, " ") {
		t.Errorf("unwrapped = %v; want the command unchanged", plain)
	}
}
