//go:build unix

package runner

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TestSignalGuardScopesPatternKillToSession is the acceptance fixture for
// session-scoped pattern kills: a session pattern-kills a command line that
// matches both a process inside its worktree and an identical process
// outside it. The guard must kill its own process and refuse the outsider,
// and the refusal must be reported as a typed activity event.
func TestSignalGuardScopesPatternKillToSession(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not on PATH")
	}
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not on PATH")
	}

	worktree := t.TempDir()
	outside := t.TempDir()
	marker := "signal-guard-fixture-93847"

	// Two identical long-lived sleepers with the same distinctive command
	// line, one in each directory: a wrapper script whose own argv carries
	// the marker and then sleeps (the shell keeps its argv, so pgrep -f
	// matches the marker; an `exec sleep` would replace the argv and lose
	// it on macOS where the comm becomes "sleep").
	writeWrapper := func(dir string) {
		body := "#!/bin/sh\nsleep 300\n"
		//nolint:gosec // G306: a stub executable must carry the exec bit.
		if err := os.WriteFile(dir+"/marker-loop.sh", []byte(body), 0o755); err != nil {
			t.Fatalf("write fixture wrapper: %v", err)
		}
	}
	writeWrapper(worktree)
	writeWrapper(outside)
	startSleeper := func(dir string) *exec.Cmd {
		cmd := exec.Command("./marker-loop.sh", marker)
		cmd.Dir = dir
		if err := cmd.Start(); err != nil {
			t.Fatalf("start fixture sleeper in %s: %v", dir, err)
		}
		return cmd
	}
	own := startSleeper(worktree)
	t.Cleanup(func() { _ = own.Process.Kill(); _ = own.Wait() })
	outsider := startSleeper(outside)
	t.Cleanup(func() { _ = outsider.Process.Kill(); _ = outsider.Wait() })
	// Let both sleepers exec into sleep.
	time.Sleep(500 * time.Millisecond)

	layout, err := installSignalGuard(worktree)
	if err != nil {
		t.Fatalf("installSignalGuard: %v", err)
	}
	env := signalGuardEnv(map[string]string{}, layout, "", 0)

	kill := exec.Command(layout.pkill, "-f", marker) //nolint:gosec // G204: test fixture, args are hard-coded literals plus the installed guard path.
	kill.Dir = worktree
	kill.Env = []string{
		"PATH=/usr/bin:/bin",
		"DONMAI_SESSION_WORKTREE=" + env[signalGuardWorktreeEnv],
		"DONMAI_SIGNAL_GUARD_EVENT_LOG=" + env[signalGuardEventLogEnv],
	}
	out, err := kill.CombinedOutput()
	// The guard signals its own process and refuses the outsider.
	if err != nil {
		t.Fatalf("guard pkill: %v (output %q)", err, out)
	}

	// The session's own sleeper is dead. The guard signalled it with
	// TERM; the process may linger as an unreaped zombie (its parent,
	// this test, has not waited yet), so liveness is confirmed via the
	// process table, not signal 0: a zombie still answers signal 0.
	// Wait for the pid to vanish from pgrep, then reap it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		pg, _ := exec.Command("pgrep", "-f", marker+" ").CombinedOutput()
		_ = pg
		psOut, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(own.Process.Pid)).CombinedOutput() //nolint:gosec // G204: test fixture, pid comes from our own started process.
		stat := strings.TrimSpace(string(psOut))
		if stat == "" || strings.HasPrefix(stat, "Z") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session sleeper survived the scoped pattern kill (stat %q)", stat)
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = own.Wait()

	// The outsider is untouched.
	if err := outsider.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("outsider sleeper was killed by the session pattern kill: %v", err)
	}

	// The refusal was logged for the typed activity event drain.
	logBody, err := os.ReadFile(layout.eventLog)
	if err != nil {
		t.Fatalf("read refusal log: %v", err)
	}
	if !strings.Contains(string(logBody), "refused") {
		t.Fatalf("refusal log missing the refusal line: %q", logBody)
	}

	// The drain reports the refusal as a typed event and clears the log.
	sink := &recordingSink{}
	r := minimalRunner(t)
	r.reportSignalGuardRefusals(context.Background(), worktree, sink)
	if len(sink.events) == 0 {
		t.Fatalf("no typed refusal event reported")
	}
	found := false
	for _, ev := range sink.events {
		if sys, ok := ev.(agent.SystemEvent); ok && sys.Subtype == SystemSubtypeSignalGuardRefused {
			found = true
		}
	}
	if !found {
		t.Fatalf("no signal_guard_refused event among %d reported", len(sink.events))
	}
	if _, err := os.Stat(layout.eventLog); !os.IsNotExist(err) {
		t.Fatalf("refusal log not cleared after drain")
	}
}
