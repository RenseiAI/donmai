//go:build unix

package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// guardTestTools skips the test unless the guard's fixed tool search path
// holds the inspection tools the guard refuses closed without. The check
// mirrors the guard's own TOOL_DIRS default rather than the test's PATH,
// so a host that could not scope anything skips instead of failing.
func guardTestTools(t *testing.T) {
	t.Helper()
	dirs := []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"}
	for _, tool := range []string{"pgrep", "lsof"} {
		found := false
		for _, d := range dirs {
			if st, err := os.Stat(filepath.Join(d, tool)); err == nil && !st.IsDir() {
				found = true
				break
			}
		}
		if !found {
			t.Skipf("guard inspection tool %s not in the fixed tool path", tool)
		}
	}
}

// installTestGuard installs a fresh runner-owned guard for one test. The
// directory is removed when the test ends.
func installTestGuard(t *testing.T) signalGuardLayout {
	t.Helper()
	layout, err := installSignalGuard()
	if err != nil {
		t.Fatalf("installSignalGuard: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(layout.dir) })
	return layout
}

// guardTestEnv builds the session env a guarded session carries: a minimal
// system PATH behind the guard directory plus the scope variables.
func guardTestEnv(layout signalGuardLayout, worktree string, extra map[string]string) []string {
	t := map[string]string{"PATH": "/usr/bin:/bin"}
	env := signalGuardEnv(t, layout, worktree, "")
	out := []string{}
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// startGuardSleeper starts `sleep 300` with argv[0] set to name and cwd
// dir, and waits until the guard's own pgrep form sees it. exec -a gives
// the process a distinctive command line on every platform; the process
// NAME used for name matching is calibrated per platform with
// guardSleeperComm. trapIgnoreTERM makes the sleeper ignore SIGTERM, so a
// test can tell TERM apart from KILL by survival.
func startGuardSleeper(t *testing.T, dir, name string, trapIgnoreTERM bool) *exec.Cmd {
	t.Helper()
	argv := "exec -a " + name + " /bin/sleep 300"
	if trapIgnoreTERM {
		argv = "trap '' TERM; " + argv
	}
	cmd := exec.Command("bash", "-c", argv) //nolint:gosec // G204: test fixture, argv is a hard-coded sleeper with a generated marker name.
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper %q in %s: %v", name, dir, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		pg, err := exec.Command("pgrep", "-f", "--", name).CombinedOutput() //nolint:gosec // G204: test fixture, the marker comes from our own started process.
		if err == nil && len(strings.Fields(string(pg))) > 0 {
			return cmd
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("sleeper %q (pid %d) never visible to pgrep -f", name, cmd.Process.Pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// guardSleeperComm reads the process NAME the platform reports for cmd:
// the token name matching (pkill without -f, killall) keys on. It is the
// argv[0] on macOS and the executed basename ("sleep") on Linux, so name
// tests calibrate to it instead of assuming either spelling.
func guardSleeperComm(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()
	out, err := exec.Command("ps", "-p", strconv.Itoa(cmd.Process.Pid), "-o", "comm=").CombinedOutput() //nolint:gosec // G204: test fixture, pid comes from our own started process.
	if err != nil {
		t.Fatalf("ps comm of pid %d: %v (%s)", cmd.Process.Pid, err, out)
	}
	comm := strings.TrimSpace(string(out))
	if comm == "" {
		t.Fatalf("empty comm for pid %d", cmd.Process.Pid)
	}
	return comm
}

// waitGuardGone waits until pid leaves the process table (a TERM-killed
// child lingers as a zombie until reaped, so a bare signal-0 check would
// lie) and reaps it.
func waitGuardGone(t *testing.T, cmd *exec.Cmd, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		psOut, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(cmd.Process.Pid)).CombinedOutput() //nolint:gosec // G204: test fixture, pid comes from our own started process.
		stat := strings.TrimSpace(string(psOut))
		if stat == "" || strings.HasPrefix(stat, "Z") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) survived the scoped pattern kill (stat %q)", what, cmd.Process.Pid, stat)
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Wait()
}

// assertGuardAlive fails when the process is gone.
func assertGuardAlive(t *testing.T, cmd *exec.Cmd, what string) {
	t.Helper()
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("%s (pid %d) is dead: %v", what, cmd.Process.Pid, err)
	}
}

// readRefusals returns the guard's refusal log, or "" when nothing was
// refused yet.
func readRefusals(t *testing.T, layout signalGuardLayout) string {
	t.Helper()
	body, err := os.ReadFile(layout.eventLog)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read refusal log: %v", err)
	}
	return string(body)
}

// TestSignalGuardScopesPatternKillToSession is the acceptance fixture for
// session-scoped pattern kills: a session pattern-kills a command line
// that matches both a process inside its worktree and an identical
// process outside it. The guard is driven through a wrapper script whose
// own command line carries the marker, so the guard's ancestry genuinely
// matches the pattern: the run proves the guard skips its own ancestry,
// kills its own process, refuses the outsider, and reports a typed
// refusal event.
func TestSignalGuardScopesPatternKillToSession(t *testing.T) {
	t.Parallel()
	guardTestTools(t)

	worktree := t.TempDir()
	outside := t.TempDir()
	marker := "gPkNrw1"

	// Two identical sleepers with the same distinctive command line, one
	// in each directory.
	own := startGuardSleeper(t, worktree, marker, false)
	outsider := startGuardSleeper(t, outside, marker, false)

	layout := installTestGuard(t)
	env := guardTestEnv(layout, worktree, nil)

	// The wrapper script lives in a directory named for the marker, so
	// the wrapper shell's own command line matches the pattern while it
	// is an ancestor of the guard: without the ancestry skip the guard
	// would signal its own parent (whose cwd is the in-scope worktree)
	// and the harness below would die with it.
	parentDir := filepath.Join(t.TempDir(), marker)
	if err := os.MkdirAll(parentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	parentPath := filepath.Join(parentDir, "parent.sh")
	parentBody := "#!/bin/sh\n\"" + layout.pkill + "\" -f \"" + marker + "\"\n"
	if err := os.WriteFile(parentPath, []byte(parentBody), 0o755); err != nil { //nolint:gosec // G306: a stub executable must carry the exec bit.
		t.Fatal(err)
	}
	parent := exec.Command("sh", parentPath) //nolint:gosec // G204: test fixture, path comes from t.TempDir.
	parent.Dir = worktree
	parent.Env = env
	parentOut, err := parent.CombinedOutput()
	if err != nil {
		t.Fatalf("guard pkill: %v (output %q)", err, parentOut)
	}

	// The session's own sleeper is dead.
	waitGuardGone(t, own, "session sleeper")
	// The outsider is untouched.
	assertGuardAlive(t, outsider, "outsider sleeper")
	// The wrapper parent survived: the guard skipped its own ancestry
	// instead of signalling an in-scope parent. (CombinedOutput already
	// reaped it; a guard kill would have left a signal-terminated
	// ProcessState.)
	if !parent.ProcessState.Success() {
		t.Fatalf("guard signalled its own harness ancestor: %v", parent.ProcessState)
	}

	// The refusal names the outsider and neither the session's own
	// process nor the skipped ancestor.
	logBody := readRefusals(t, layout)
	if !strings.Contains(logBody, strconv.Itoa(outsider.Process.Pid)) {
		t.Fatalf("refusal log missing the outsider pid %d: %q", outsider.Process.Pid, logBody)
	}
	for _, pid := range []int{own.Process.Pid, parent.Process.Pid} {
		if strings.Contains(logBody, strconv.Itoa(pid)) {
			t.Fatalf("refusal log names in-scope pid %d: %q", pid, logBody)
		}
	}

	// The drain reports the refusal as a typed event, mirrors it to
	// events.jsonl, and clears the log.
	eventsDir := t.TempDir()
	sink := &recordingSink{}
	r := minimalRunner(t)
	r.reportSignalGuardRefusals(context.Background(), layout, eventsDir, sink)
	found := false
	for _, ev := range sink.events {
		if sys, ok := ev.(agent.SystemEvent); ok && sys.Subtype == SystemSubtypeSignalGuardRefused &&
			strings.Contains(sys.Message, strconv.Itoa(outsider.Process.Pid)) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no signal_guard_refused event naming pid %d among %d reported", outsider.Process.Pid, len(sink.events))
	}
	mirror, err := os.ReadFile(filepath.Join(eventsDir, ".agent", "events.jsonl"))
	if err != nil || !strings.Contains(string(mirror), SystemSubtypeSignalGuardRefused) {
		t.Fatalf("events.jsonl mirror missing the refusal event: %v %q", err, mirror)
	}
	if _, err := os.Stat(layout.eventLog); !os.IsNotExist(err) {
		t.Fatalf("refusal log not cleared after drain")
	}
}

// TestKillallGuardScopesNameToSession is the killall acceptance case: a
// session kill by process name kills its own process and refuses the
// identical outsider, and the refusal is logged.
func TestKillallGuardScopesNameToSession(t *testing.T) {
	t.Parallel()
	guardTestTools(t)

	worktree := t.TempDir()
	outside := t.TempDir()
	name := "gKaNrw1"

	own := startGuardSleeper(t, worktree, name, false)
	outsider := startGuardSleeper(t, outside, name, false)
	target := guardSleeperComm(t, own)

	layout := installTestGuard(t)
	kill := exec.Command(layout.killall, target) //nolint:gosec // G204: test fixture, args are hard-coded literals plus the installed guard path.
	kill.Dir = worktree
	kill.Env = guardTestEnv(layout, worktree, nil)
	out, err := kill.CombinedOutput()
	if err != nil {
		t.Fatalf("guard killall: %v (output %q)", err, out)
	}

	waitGuardGone(t, own, "session process")
	assertGuardAlive(t, outsider, "outsider process")
	if logBody := readRefusals(t, layout); !strings.Contains(logBody, strconv.Itoa(outsider.Process.Pid)) {
		t.Fatalf("refusal log missing the outsider pid %d: %q", outsider.Process.Pid, logBody)
	}
}

// TestSignalGuardSiblingWorktreeOutOfScope pins the separator-aware
// boundary: a process in "<worktree>-evil" is outside the session scope
// even though its path carries the worktree as a string prefix. Both
// guard modes are exercised through the shared scope check.
func TestSignalGuardSiblingWorktreeOutOfScope(t *testing.T) {
	t.Parallel()
	guardTestTools(t)

	base := t.TempDir()
	worktree := filepath.Join(base, "sess")
	sibling := worktree + "-evil"
	for _, dir := range []string{worktree, sibling} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	name := "gSibBnd1"

	own := startGuardSleeper(t, worktree, name, false)
	sib := startGuardSleeper(t, sibling, name, false)

	layout := installTestGuard(t)
	env := guardTestEnv(layout, worktree, nil)

	kill := exec.Command(layout.pkill, "-f", name) //nolint:gosec // G204: test fixture, args are hard-coded literals plus the installed guard path.
	kill.Dir = worktree
	kill.Env = env
	if out, err := kill.CombinedOutput(); err != nil {
		t.Fatalf("guard pkill: %v (output %q)", err, out)
	}
	waitGuardGone(t, own, "session process")
	assertGuardAlive(t, sib, "sibling-worktree process")
	if logBody := readRefusals(t, layout); !strings.Contains(logBody, strconv.Itoa(sib.Process.Pid)) {
		t.Fatalf("refusal log missing the sibling pid %d: %q", sib.Process.Pid, logBody)
	}

	// The same boundary through the killall name leg.
	name2 := "gSibBnd2"
	own2 := startGuardSleeper(t, worktree, name2, false)
	sib2 := startGuardSleeper(t, sibling, name2, false)
	kill2 := exec.Command(layout.killall, guardSleeperComm(t, sib2)) //nolint:gosec // G204: test fixture, the name comes from our own started process.
	kill2.Dir = worktree
	kill2.Env = guardTestEnv(layout, worktree, nil)
	if out, err := kill2.CombinedOutput(); err != nil {
		t.Fatalf("guard killall: %v (output %q)", err, out)
	}
	waitGuardGone(t, own2, "session process (killall)")
	assertGuardAlive(t, sib2, "sibling-worktree process (killall)")
	if logBody := readRefusals(t, layout); !strings.Contains(logBody, strconv.Itoa(sib2.Process.Pid)) {
		t.Fatalf("refusal log missing the sibling pid %d: %q", sib2.Process.Pid, logBody)
	}
}

// TestSignalGuardWorkareaRootLeg pins the second directory leg: a process
// under the workarea root but outside the worktree is in scope.
func TestSignalGuardWorkareaRootLeg(t *testing.T) {
	t.Parallel()
	guardTestTools(t)

	base := t.TempDir()
	worktree := filepath.Join(base, "repo")
	leaf := filepath.Join(base, "other-leaf")
	for _, dir := range []string{worktree, leaf} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	name := "gWaRoot1"

	inRoot := startGuardSleeper(t, leaf, name, false)

	layout := installTestGuard(t)
	env := guardTestEnv(layout, worktree, map[string]string{"DONMAI_SESSION_WORKAREA_ROOT": base})
	kill := exec.Command(layout.pkill, "-f", name) //nolint:gosec // G204: test fixture, args are hard-coded literals plus the installed guard path.
	kill.Dir = worktree
	kill.Env = env
	if out, err := kill.CombinedOutput(); err != nil {
		t.Fatalf("guard pkill: %v (output %q)", err, out)
	}
	waitGuardGone(t, inRoot, "workarea-root process")
	if logBody := readRefusals(t, layout); logBody != "" {
		t.Fatalf("in-scope workarea kill logged a refusal: %q", logBody)
	}
}

// TestSignalGuardPatternForms pins the pkill argument grammar: every
// operand is a scoped pattern with or without -f (pkill takes no PID
// operands), combined shorts such as -9f are modelled, and any flag the
// guard cannot model exactly (-i, -u, -n, -s, lowercase clusters) is
// refused closed with both processes left alive.
func TestSignalGuardPatternForms(t *testing.T) {
	t.Parallel()
	guardTestTools(t)

	user := strconv.Itoa(os.Getuid())
	cases := []struct {
		name       string
		args       func(name, comm string) []string
		wantOwn    string // "dead" or "alive"
		wantRC     int    // 0 signals (possibly refused some), 1 refuses/nothing
		wantRefuse bool   // the outsider refusal or the flag refusal is logged
		trapper    bool   // the own sleeper ignores TERM (tells TERM from KILL)
	}{
		{name: "bare name", args: func(_, comm string) []string { return []string{comm} }, wantOwn: "dead", wantRC: 0, wantRefuse: true},
		{name: "dash f", args: func(name, _ string) []string { return []string{"-f", name} }, wantOwn: "dead", wantRC: 0, wantRefuse: true},
		{name: "signal then f", args: func(name, _ string) []string { return []string{"-9", "-f", name} }, wantOwn: "dead", wantRC: 0, wantRefuse: true},
		{name: "combined signal f", args: func(name, _ string) []string { return []string{"-9f", name} }, wantOwn: "dead", wantRC: 0, wantRefuse: true},
		{name: "exact name", args: func(_, comm string) []string { return []string{"-x", comm} }, wantOwn: "dead", wantRC: 0, wantRefuse: true},
		// Exact-full matches the whole command line, so the pattern is
		// the argv the sleeper was started with.
		{name: "exact full", args: func(name, _ string) []string { return []string{"-f", "-x", name + " 300"} }, wantOwn: "dead", wantRC: 0, wantRefuse: true},
		{name: "term leaves trapper", trapper: true, args: func(name, _ string) []string { return []string{"-f", name} }, wantOwn: "alive", wantRC: 0, wantRefuse: true},
		{name: "kill takes trapper", trapper: true, args: func(name, _ string) []string { return []string{"-9", "-f", name} }, wantOwn: "dead", wantRC: 0, wantRefuse: true},
		{name: "ignore case refused", args: func(name, _ string) []string { return []string{"-f", "-i", name} }, wantOwn: "alive", wantRC: 1, wantRefuse: true},
		{name: "combined fi refused", args: func(name, _ string) []string { return []string{"-fi", name} }, wantOwn: "alive", wantRC: 1, wantRefuse: true},
		{name: "user selector refused", args: func(name, _ string) []string { return []string{"-u", user, "-f", name} }, wantOwn: "alive", wantRC: 1, wantRefuse: true},
		{name: "newest refused", args: func(name, _ string) []string { return []string{"-n", "-f", name} }, wantOwn: "alive", wantRC: 1, wantRefuse: true},
		{name: "session selector refused", args: func(name, _ string) []string { return []string{"-s", name} }, wantOwn: "alive", wantRC: 1, wantRefuse: true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			worktree := t.TempDir()
			outside := t.TempDir()
			name := "gMx" + strconv.Itoa(i) + "z1"
			own := startGuardSleeper(t, worktree, name, tc.trapper)
			outsider := startGuardSleeper(t, outside, name, false)
			comm := guardSleeperComm(t, own)

			layout := installTestGuard(t)
			kill := exec.Command(layout.pkill, tc.args(name, comm)...) //nolint:gosec // G204: test fixture, the marker comes from our own started process.
			kill.Dir = worktree
			kill.Env = guardTestEnv(layout, worktree, nil)
			out, err := kill.CombinedOutput()
			rc := 0
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					rc = exit.ExitCode()
				} else {
					t.Fatalf("guard pkill: %v (output %q)", err, out)
				}
			}
			if rc != tc.wantRC {
				t.Fatalf("exit = %d (output %q), want %d", rc, out, tc.wantRC)
			}
			if tc.wantOwn == "dead" {
				waitGuardGone(t, own, "session process")
			} else {
				assertGuardAlive(t, own, "session process")
			}
			assertGuardAlive(t, outsider, "outsider process")
			logBody := readRefusals(t, layout)
			if tc.wantRefuse && logBody == "" {
				t.Fatalf("expected a refusal in the log, got none (output %q)", out)
			}
			if tc.wantRefuse && !strings.Contains(logBody, strconv.Itoa(outsider.Process.Pid)) &&
				!strings.Contains(logBody, "unsupported flag") {
				t.Fatalf("refusal log names neither the outsider nor a flag: %q", logBody)
			}
		})
	}
}

// TestKillallGuardNameForms pins the killall grammar: every name operand
// is scoped (a trailing unknown name must not route to the unscoped real
// tool), an all-unknown invocation exits 1 without signalling, and
// platform-divergent flags (-s) are refused closed.
func TestKillallGuardNameForms(t *testing.T) {
	t.Parallel()
	guardTestTools(t)

	user := strconv.Itoa(os.Getuid())
	cases := []struct {
		name    string
		args    func(comm string) []string
		wantOwn string
		wantRC  int
	}{
		{name: "single name", args: func(comm string) []string { return []string{comm} }, wantOwn: "dead", wantRC: 0},
		{name: "signal name", args: func(comm string) []string { return []string{"-9", comm} }, wantOwn: "dead", wantRC: 0},
		{name: "trailing unknown scoped", args: func(comm string) []string { return []string{comm, "nosuchproc990"} }, wantOwn: "dead", wantRC: 0},
		{name: "all unknown no match", args: func(_ string) []string { return []string{"nosuch1", "nosuch2"} }, wantOwn: "alive", wantRC: 1},
		// A quoted multi-word name is one operand that matches nothing: it
		// must not split into two lookups (the first would kill the
		// session's own process) nor glob-expand. Both stay alive.
		{name: "spaced name matches nothing", args: func(comm string) []string { return []string{comm + " with-space"} }, wantOwn: "alive", wantRC: 1},
		{name: "signal flag refused", args: func(comm string) []string { return []string{"-s", "KILL", comm} }, wantOwn: "alive", wantRC: 1},
		{name: "user flag refused", args: func(comm string) []string { return []string{"-u", user, comm} }, wantOwn: "alive", wantRC: 1},
		{name: "quiet flag refused", args: func(comm string) []string { return []string{"-q", comm} }, wantOwn: "alive", wantRC: 1},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			worktree := t.TempDir()
			outside := t.TempDir()
			name := "gKx" + strconv.Itoa(i) + "z1"
			own := startGuardSleeper(t, worktree, name, false)
			outsider := startGuardSleeper(t, outside, name, false)
			comm := guardSleeperComm(t, own)

			layout := installTestGuard(t)
			kill := exec.Command(layout.killall, tc.args(comm)...) //nolint:gosec // G204: test fixture, the name comes from our own started process.
			kill.Dir = worktree
			kill.Env = guardTestEnv(layout, worktree, nil)
			out, err := kill.CombinedOutput()
			rc := 0
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					rc = exit.ExitCode()
				} else {
					t.Fatalf("guard killall: %v (output %q)", err, out)
				}
			}
			if rc != tc.wantRC {
				t.Fatalf("exit = %d (output %q), want %d", rc, out, tc.wantRC)
			}
			if tc.wantOwn == "dead" {
				waitGuardGone(t, own, "session process")
				assertGuardAlive(t, outsider, "outsider process")
				if logBody := readRefusals(t, layout); !strings.Contains(logBody, strconv.Itoa(outsider.Process.Pid)) {
					t.Fatalf("refusal log missing the outsider pid %d: %q", outsider.Process.Pid, logBody)
				}
			} else {
				assertGuardAlive(t, own, "session process")
				assertGuardAlive(t, outsider, "outsider process")
			}
		})
	}
}

// TestKillallGuardTwoNames pins that every name operand is scoped, not
// just the last: two real names each keep an in-scope and an out-of-scope
// process, and both outsiders survive.
func TestKillallGuardTwoNames(t *testing.T) {
	t.Parallel()
	guardTestTools(t)

	worktree := t.TempDir()
	outside := t.TempDir()
	ownA := startGuardSleeper(t, worktree, "gK2a1", false)
	outA := startGuardSleeper(t, outside, "gK2a1", false)
	ownB := startGuardSleeper(t, worktree, "gK2b1", false)
	outB := startGuardSleeper(t, outside, "gK2b1", false)
	commA := guardSleeperComm(t, ownA)
	commB := guardSleeperComm(t, ownB)

	layout := installTestGuard(t)
	kill := exec.Command(layout.killall, commA, commB) //nolint:gosec // G204: test fixture, names come from our own started processes.
	kill.Dir = worktree
	kill.Env = guardTestEnv(layout, worktree, nil)
	if out, err := kill.CombinedOutput(); err != nil {
		t.Fatalf("guard killall: %v (output %q)", err, out)
	}
	waitGuardGone(t, ownA, "session process A")
	waitGuardGone(t, ownB, "session process B")
	assertGuardAlive(t, outA, "outsider process A")
	assertGuardAlive(t, outB, "outsider process B")
	logBody := readRefusals(t, layout)
	for _, pid := range []int{outA.Process.Pid, outB.Process.Pid} {
		if !strings.Contains(logBody, strconv.Itoa(pid)) {
			t.Fatalf("refusal log missing outsider pid %d: %q", pid, logBody)
		}
	}
}

// TestSignalGuardMissingToolsRefusesClosed pins the fail-closed leg: with
// no inspection tools on the guard's fixed search path, both guard modes
// refuse without signalling anything.
func TestSignalGuardMissingToolsRefusesClosed(t *testing.T) {
	t.Parallel()
	guardTestTools(t)

	for _, mode := range []string{"pkill", "killall"} {
		t.Run(mode, func(t *testing.T) {
			worktree := t.TempDir()
			outside := t.TempDir()
			name := "gMt" + mode + "1"
			own := startGuardSleeper(t, worktree, name, false)
			outsider := startGuardSleeper(t, outside, name, false)

			layout := installTestGuard(t)
			extra := map[string]string{"DONMAI_SIGNAL_GUARD_TOOL_DIRS": t.TempDir()}
			bin := layout.pkill
			args := []string{"-f", name}
			if mode == "killall" {
				bin = layout.killall
				args = []string{guardSleeperComm(t, own)}
			}
			kill := exec.Command(bin, args...) //nolint:gosec // G204: test fixture, the marker comes from our own started process.
			kill.Dir = worktree
			kill.Env = guardTestEnv(layout, worktree, extra)
			out, err := kill.CombinedOutput()
			if err == nil {
				t.Fatalf("guard %s with no tools succeeded (output %q), want refusal", mode, out)
			}
			assertGuardAlive(t, own, "session process")
			assertGuardAlive(t, outsider, "outsider process")
			if logBody := readRefusals(t, layout); !strings.Contains(logBody, "scope inspection unavailable") {
				t.Fatalf("refusal log missing the fail-closed line: %q", logBody)
			}
		})
	}
}

// TestSignalGuardFailingPgrepRefusesClosed pins the empty-match fail-closed
// leg against the exact B2 fallback: when the guard's own pgrep fails (not
// merely absent), the guard must refuse closed without re-execing the real
// tool. A stub pgrep that exits 2 reaches the empty-match leg past the
// missing-tools check; reverting that leg to `real_tool "$@"` would kill
// both sleepers and stay GREEN without this pin.
func TestSignalGuardFailingPgrepRefusesClosed(t *testing.T) {
	t.Parallel()
	guardTestTools(t)

	for _, mode := range []string{"pkill", "killall"} {
		t.Run(mode, func(t *testing.T) {
			worktree := t.TempDir()
			outside := t.TempDir()
			name := "gFp" + mode + "1"
			own := startGuardSleeper(t, worktree, name, false)
			outsider := startGuardSleeper(t, outside, name, false)

			layout := installTestGuard(t)
			stubDir := t.TempDir()
			stub := "#!/bin/sh\nexit 2\n"
			if err := os.WriteFile(stubDir+"/pgrep", []byte(stub), 0o755); err != nil { //nolint:gosec // G306: test stub must carry the exec bit.
				t.Fatal(err)
			}
			extra := map[string]string{"DONMAI_SIGNAL_GUARD_TOOL_DIRS": stubDir + " /usr/bin /bin /usr/sbin /sbin"}
			bin := layout.pkill
			args := []string{"-f", name}
			if mode == "killall" {
				bin = layout.killall
				args = []string{guardSleeperComm(t, own)}
			}
			kill := exec.Command(bin, args...) //nolint:gosec // G204: test fixture, the marker comes from our own started process.
			kill.Dir = worktree
			kill.Env = guardTestEnv(layout, worktree, extra)
			out, err := kill.CombinedOutput()
			if err == nil {
				t.Fatalf("guard %s with failing pgrep succeeded (output %q), want refusal", mode, out)
			}
			assertGuardAlive(t, own, "session process")
			assertGuardAlive(t, outsider, "outsider process")
			if !strings.Contains(string(out), "process match lookup failed") {
				t.Fatalf("stderr missing the fail-closed line: %q", out)
			}
			if logBody := readRefusals(t, layout); !strings.Contains(logBody, "process match lookup failed") {
				t.Fatalf("refusal log missing the fail-closed line: %q", logBody)
			}
		})
	}
}

// TestKillallGuardUnknownDoesNotReexec pins that an all-unknown killall
// exits 1 without re-execing the real tool: the real killall reports "No
// matching processes" on stderr, while the guard stays silent. Reverting
// the empty-match leg to `real_tool "$@"` would print that line and stay
// GREEN without this pin.
func TestKillallGuardUnknownDoesNotReexec(t *testing.T) {
	t.Parallel()
	guardTestTools(t)

	worktree := t.TempDir()
	layout := installTestGuard(t)
	kill := exec.Command(layout.killall, "nosuchA991", "nosuchB992") //nolint:gosec // G204: test fixture, names are hard-coded literals plus the installed guard path.
	kill.Dir = worktree
	kill.Env = guardTestEnv(layout, worktree, nil)
	out, err := kill.CombinedOutput()
	if err == nil {
		t.Fatalf("guard killall with unknown names succeeded (output %q), want exit 1", out)
	}
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("exit = %v (output %q), want 1", err, out)
	}
	if strings.Contains(strings.ToLower(string(out)), "matching") || strings.Contains(strings.ToLower(string(out)), "no process") {
		t.Fatalf("guard re-execed the real tool (output %q), want silent fail-closed", out)
	}
}

// TestSignalGuardProcessGroupLeg pins the group scope leg end to end: the
// runner records the harness pid's group into the guard directory file,
// and the guard then kills an in-group process whose cwd is outside the
// worktree while refusing an out-of-group outsider.
func TestSignalGuardProcessGroupLeg(t *testing.T) {
	t.Parallel()
	guardTestTools(t)

	nowhere := t.TempDir()
	name := "gGrpLeg1"

	// The own process leads its own group (like a spawned harness with
	// Setpgid); the outsider shares the test's group.
	own := exec.Command("bash", "-c", "exec -a "+name+" /bin/sleep 300") //nolint:gosec // G204: test fixture, the marker is a hard-coded literal.
	own.Dir = nowhere
	own.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := own.Start(); err != nil {
		t.Fatalf("start grouped sleeper: %v", err)
	}
	t.Cleanup(func() {
		_ = own.Process.Kill()
		_ = own.Wait()
	})
	outsider := startGuardSleeper(t, nowhere, name, false)

	layout := installTestGuard(t)
	r := minimalRunner(t)
	statePath := t.TempDir()
	r.recordSessionProcessGroup(statePath, "sess-grp", own.Process.Pid, layout.dir)

	// The runner persisted the group to state and delivered it to the
	// guard directory file.
	stored, err := r.store.Read(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	pgid, err := syscall.Getpgid(own.Process.Pid)
	if err != nil {
		t.Fatalf("getpgid: %v", err)
	}
	if stored.SessionProcessGroup != pgid {
		t.Fatalf("state group = %d, want %d", stored.SessionProcessGroup, pgid)
	}
	fileBody, err := os.ReadFile(layout.pgidFile)
	if err != nil || strings.TrimSpace(string(fileBody)) != strconv.Itoa(pgid) {
		t.Fatalf("guard pgid file = %q, want %d", fileBody, pgid)
	}

	// Neither cwd is the worktree, so only the group leg can scope: the
	// in-group process dies, the outsider is refused.
	scopeElsewhere := t.TempDir()
	kill := exec.Command(layout.pkill, "-f", name) //nolint:gosec // G204: test fixture, the marker comes from our own started process.
	kill.Dir = scopeElsewhere
	kill.Env = guardTestEnv(layout, scopeElsewhere, nil)
	if out, err := kill.CombinedOutput(); err != nil {
		t.Fatalf("guard pkill: %v (output %q)", err, out)
	}
	waitGuardGone(t, own, "in-group process")
	assertGuardAlive(t, outsider, "out-of-group process")
	if logBody := readRefusals(t, layout); !strings.Contains(logBody, strconv.Itoa(outsider.Process.Pid)) {
		t.Fatalf("refusal log missing the outsider pid %d: %q", outsider.Process.Pid, logBody)
	}
}

// TestSignalGuardResolvesThroughSessionPATH pins the install end to end
// for the common harness door: a non-login POSIX shell carrying the
// session env resolves both names to the runner-provided guards, and a
// pattern kill issued by bare name through that shell is scoped (its own
// process dies, the outsider is refused). Login shells rebuild PATH
// through path_helper and bypass the guard; that door is a documented
// limitation, not covered here.
func TestSignalGuardResolvesThroughSessionPATH(t *testing.T) {
	t.Parallel()
	guardTestTools(t)
	worktree := t.TempDir()
	outside := t.TempDir()
	layout := installTestGuard(t)
	env := guardTestEnv(layout, worktree, nil)
	for _, name := range []string{"pkill", "killall"} {
		probe := exec.Command("sh", "-c", "command -v "+name) //nolint:gosec // G204: test fixture, name is pkill/killall.
		probe.Env = env
		out, err := probe.CombinedOutput()
		if err != nil {
			t.Fatalf("command -v %s: %v (%s)", name, err, out)
		}
		if got := strings.TrimSpace(string(out)); got != filepath.Join(layout.dir, name) {
			t.Fatalf("command -v %s = %q, want the session guard", name, got)
		}
	}
	marker := "gPathSh1"
	own := startGuardSleeper(t, worktree, marker, false)
	outsider := startGuardSleeper(t, outside, marker, false)
	byName := exec.Command("sh", "-c", "pkill -f "+marker) //nolint:gosec // G204: test fixture, the marker is a hard-coded literal.
	byName.Dir = worktree
	byName.Env = env
	if out, err := byName.CombinedOutput(); err != nil {
		t.Fatalf("shell pkill by name: %v (output %q)", err, out)
	}
	waitGuardGone(t, own, "session process")
	assertGuardAlive(t, outsider, "outsider process")
}

// TestSignalGuardRefusalDrainsThroughRun is the end-to-end pin for the
// install → env → guard → drain chain through a real Run: the agent's
// turn invokes the session's own guard binary (resolved from the session
// env captured at spawn), the guard refuses an out-of-scope process, and
// the run's terminal teardown drains that refusal into events.jsonl. The
// drain runs in a defer covering every terminal path, so deleting it (or
// the install, or the env overlay) leaves the mirror without the refusal
// and this test goes red.
func TestSignalGuardRefusalDrainsThroughRun(t *testing.T) {
	guardTestTools(t)

	const marker = "gE2eDrn1"
	var spawnEnv map[string]string
	provider := func(base agent.HarnessProvider) agent.Provider {
		scripted := &verdictScriptProvider{HarnessProvider: base, t: t, turns: []verdictScriptTurn{{
			manifest: passedManifest,
			text:     "WORK_RESULT:passed",
			during: func(t *testing.T, cwd string) {
				if spawnEnv == nil {
					t.Fatal("no session env captured at spawn")
				}
				guardDir := spawnEnv[signalGuardDirEnv]
				eventLog := spawnEnv[signalGuardEventLogEnv]
				worktree := spawnEnv[signalGuardWorktreeEnv]
				if guardDir == "" || eventLog == "" || worktree == "" {
					t.Fatalf("session env missing guard scope (dir %q log %q worktree %q)", guardDir, eventLog, worktree)
				}
				own := startGuardSleeper(t, cwd, marker, false)
				outsider := startGuardSleeper(t, t.TempDir(), marker, false)
				kill := exec.Command(filepath.Join(guardDir, "pkill"), "-f", marker) //nolint:gosec // G204: test fixture, the marker is a hard-coded literal.
				kill.Dir = cwd
				kill.Env = []string{
					"PATH=/usr/bin:/bin",
					"DONMAI_SESSION_WORKTREE=" + worktree,
					"DONMAI_SIGNAL_GUARD_DIR=" + guardDir,
					"DONMAI_SIGNAL_GUARD_EVENT_LOG=" + eventLog,
				}
				if out, err := kill.CombinedOutput(); err != nil {
					t.Fatalf("session guard pkill: %v (output %q)", err, out)
				}
				waitGuardGone(t, own, "session process")
				assertGuardAlive(t, outsider, "outsider process")
			},
		}}}
		return &spawnEnvCaptureProvider{verdictScriptProvider: scripted, env: &spawnEnv}
	}
	res, _ := runScriptedSession(t, scriptedSession{
		workType:     "development",
		skipSteering: true,
		repository:   "https://github.com/example/repo",
		provider:     provider,
		turns:        nil,
	})
	if res.Status != "completed" {
		t.Fatalf("Status = %q (%s: %s), want completed", res.Status, res.FailureMode, res.Error)
	}
	mirror, err := os.ReadFile(filepath.Join(res.WorktreePath, ".agent", "events.jsonl"))
	if err != nil {
		t.Fatalf("read events.jsonl: %v", err)
	}
	if !strings.Contains(string(mirror), SystemSubtypeSignalGuardRefused) {
		t.Fatalf("events.jsonl missing the drained refusal event:\n%s", mirror)
	}
}

// spawnEnvCaptureProvider records the harness Spec.Env at spawn and then
// plays the wrapped script. The turn hooks read the recorded env to find
// the session's own guard installation.
type spawnEnvCaptureProvider struct {
	*verdictScriptProvider
	env *map[string]string
}

func (p *spawnEnvCaptureProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	*p.env = spec.Env
	return p.verdictScriptProvider.Spawn(ctx, spec)
}
