package afcli

import "testing"

// TestFormatStartupWorkerLine covers REN-1445: the daemon startup log used to
// print `[daemon] worker-id worker-test-machine-stub` in stub mode, which
// misled operators into thinking the daemon had registered with the platform.
// The new helper annotates stub ids and returns "" when no id is assigned.
func TestFormatStartupWorkerLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"real platform id", "wkr_60eb0a2f35124d56", "[daemon] worker-id wkr_60eb0a2f35124d56"},
		{"stub id flagged", "worker-test-machine-stub", "[daemon] worker-id worker-test-machine-stub (stub registration — not registered with platform)"},
		{"another stub", "worker-host-stub", "[daemon] worker-id worker-host-stub (stub registration — not registered with platform)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := formatStartupWorkerLine(c.in); got != c.want {
				t.Errorf("formatStartupWorkerLine(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
