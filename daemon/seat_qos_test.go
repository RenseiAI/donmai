package daemon

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/daemon/seatqos"
)

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
