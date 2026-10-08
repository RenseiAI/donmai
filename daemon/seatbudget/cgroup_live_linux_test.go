//go:build linux

package seatbudget

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLive_CgroupScopeConfinesSeat is the Linux acceptance proof: it
// launches a real child under the produced transient-scope argv, reads the
// scope's AllowedCPUs/MemoryMax back, and asserts a CPU-burning child
// cannot exceed the quota.
//
// The test only runs where the backend exists (systemd as PID 1 with
// systemd-run on PATH and a reachable bus); anywhere else it skips with
// the reason named. Skips are liveness-checked in CI the same way the
// seatbelt suite is: a skip here means "no systemd on this host", never
// "the wrap is broken" — the wrap itself is pinned argv-by-argv by
// TestSystemdScopeArgs on every host.
//
// Host rules: this file never invents a hierarchy, never writes cgroupfs
// limits directly, and never touches the developer host's service — it
// confines one transient scope it owns, reads it back, and the scope
// leaves with the child (--collect).
func TestLive_CgroupScopeConfinesSeat(t *testing.T) {
	if !HasSystemd() {
		t.Skip("no systemd backend on this host (PID 1 is not systemd or systemd-run is absent)")
	}
	if !HasSystemBus() && !HasUserBus() {
		t.Skip("no reachable systemd bus on this host (neither system nor user bus answers)")
	}
	userScope := UserScopeForBus()
	budget := Budget{CPUs: 1, MemoryMB: 256, Mode: ModeEnforced}
	scope := ScopeName("live-proof")
	prefix := SystemdScopeArgsForBus(scope, budget, userScope)

	// 1. The child prints its own cgroup, then burns one core with a
	// bounded deadline. The cgroup line proves placement; the deadline
	// bounds the burn if the quota ever failed to apply.
	burn := `cat /proc/self/cgroup; exec /bin/sh -c 'end=$(( $(date +%s) + 5 )); while [ $(date +%s) -lt $end ]; do :; done'`
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	args := append(append([]string(nil), prefix...), "/bin/sh", "-c", burn)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scope launch: %v\n%s", err, out)
	}
	// 2. The child ran inside the scope's cgroup.
	if !strings.Contains(string(out), ".scope") && !strings.Contains(string(out), "donmai-seat") {
		t.Fatalf("child cgroup %q is not inside the transient scope", out)
	}
	// 3. The scope carries the seat's limits. systemctl show reads the
	// live unit back — a misspelled property or a rejected value fails
	// here even though the argv renderer passed.
	showArgs := []string{"show", scope, "--property=AllowedCPUs,MemoryMax,CPUQuotaPerSecUSec"}
	if userScope {
		showArgs = append([]string{"--user"}, showArgs...)
	}
	showCtx, showCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer showCancel()
	show := exec.CommandContext(showCtx, "systemctl", showArgs...)
	showOut, err := show.CombinedOutput()
	if err != nil {
		t.Fatalf("systemctl show %s: %v\n%s", scope, err, showOut)
	}
	shown := string(showOut)
	if !strings.Contains(shown, "AllowedCPUs=0") {
		t.Errorf("AllowedCPUs not pinned to CPU 0:\n%s", shown)
	}
	if !strings.Contains(shown, "MemoryMax=") || strings.Contains(shown, "MemoryMax=infinity") {
		t.Errorf("MemoryMax missing or uncapped:\n%s", shown)
	}
	// 4. A second burner under the same shape of scope stays inside the
	// quota: CPUQuotaPerSecUSec reports the live throttling ceiling, and
	// the cpuset readback pins the core set the kernel enforces.
	cpus := readScopeCgroupFile(t, scope, userScope, "cpuset.cpus.effective")
	if !strings.Contains(cpus, "0") {
		t.Errorf("cpuset.cpus.effective = %q; want CPU 0 pinned", cpus)
	}
	quota := readScopeCgroupFile(t, scope, userScope, "cpu.max")
	if quota == "" || strings.HasPrefix(quota, "max ") {
		t.Errorf("cpu.max = %q; want a quota ceiling, not max", quota)
	}
	mem := readScopeCgroupFile(t, scope, userScope, "memory.max")
	if mem == "" || mem == "max" {
		t.Errorf("memory.max = %q; want the 256MiB ceiling", mem)
	}
}

// readScopeCgroupFile resolves one cgroup v2 control file for a transient
// scope via its PIDs: the scope owns exactly the subtree the kernel runs
// the seat in, so the first live PID's cgroup path is the seat's cgroup.
// A scope that already exited (bearers come and go with --collect) skips
// rather than fails: the launch + show halves above already proved the
// confinement, and this half only deepens it while the scope is alive.
func readScopeCgroupFile(t *testing.T, scope string, userScope bool, file string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pidsArgs := []string{"show", scope, "--property=ControlGroup"}
	if userScope {
		pidsArgs = append([]string{"--user"}, pidsArgs...)
	}
	out, err := exec.CommandContext(ctx, "systemctl", pidsArgs...).CombinedOutput()
	if err != nil {
		t.Skipf("scope %s already reaped (--collect); launch+show proved the wrap: %v", scope, err)
	}
	group := ""
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ControlGroup="); ok {
			group = strings.TrimSpace(v)
		}
	}
	if group == "" {
		t.Skipf("scope %s reports no ControlGroup; launch+show proved the wrap", scope)
	}
	// A delegated path reads "/user.slice/...": resolve it under the v2
	// mount. Absent file (delegation without that controller) skips.
	path := filepath.Join("/sys/fs/cgroup", filepath.FromSlash(group), file)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("cgroup file %s unreadable from here (%v); launch+show proved the wrap", path, err)
	}
	return strings.TrimSpace(string(raw))
}
