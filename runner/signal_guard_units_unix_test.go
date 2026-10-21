//go:build unix

package runner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestInstallSignalGuardCreatesRunnerOwnedDir pins the tighten-only
// install: the guard lives in a fresh runner-owned directory (never
// inside the session checkout), holding exactly the two installed shims,
// with an owner-only directory mode and no refusal log before any
// refusal.
func TestInstallSignalGuardCreatesRunnerOwnedDir(t *testing.T) {
	t.Parallel()
	worktree := t.TempDir()
	layout, err := installSignalGuard()
	if err != nil {
		t.Fatalf("installSignalGuard: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(layout.dir) })
	if layout.dir == "" || layout.dir == worktree {
		t.Fatalf("guard dir = %q, want a fresh directory outside the checkout", layout.dir)
	}
	info, err := os.Stat(layout.dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("guard dir mode = %v, want 0700", info.Mode())
	}
	entries, err := os.ReadDir(layout.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != "killall" || entries[1].Name() != "pkill" {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("guard dir holds %q, want exactly [killall pkill]", names)
	}
	for _, path := range []string{layout.pkill, layout.killall} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if fi.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s is not executable (mode %v)", path, fi.Mode())
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.HasPrefix(string(body), "#!/bin/sh") {
			t.Fatalf("%s missing sh shebang", path)
		}
	}
	if _, err := os.Stat(layout.eventLog); !os.IsNotExist(err) {
		t.Fatalf("refusal log should not exist before any refusal")
	}
	if _, err := os.Stat(layout.pgidFile); !os.IsNotExist(err) {
		t.Fatalf("pgid file should not exist before the harness spawns")
	}
}

// TestSignalGuardEnvLeadsPATH pins the session env overlay: the
// runner-owned guard directory leads PATH, the worktree and workarea
// root are stamped explicitly, and the process-group id is NOT stamped
// into the env snapshot (the group leg is delivered through the guard
// directory's session.pgid file, because the harness has not spawned
// when this env is composed).
func TestSignalGuardEnvLeadsPATH(t *testing.T) {
	t.Parallel()
	worktree := t.TempDir()
	layout, err := installSignalGuard()
	if err != nil {
		t.Fatalf("installSignalGuard: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(layout.dir) })
	env := signalGuardEnv(map[string]string{"PATH": "/usr/bin:/bin"}, layout, worktree, "/workarea-root")
	if !strings.HasPrefix(env["PATH"], layout.dir+string(os.PathListSeparator)) {
		t.Fatalf("PATH does not lead with the guard dir: %q", env["PATH"])
	}
	if !strings.HasSuffix(env["PATH"], "/usr/bin:/bin") {
		t.Fatalf("PATH lost the prior entries: %q", env["PATH"])
	}
	if env[signalGuardWorktreeEnv] != worktree {
		t.Fatalf("worktree = %q, want %q", env[signalGuardWorktreeEnv], worktree)
	}
	if env[signalGuardWorkareaRootEnv] != "/workarea-root" {
		t.Fatalf("workarea root = %q, want /workarea-root", env[signalGuardWorkareaRootEnv])
	}
	if env[signalGuardDirEnv] != layout.dir {
		t.Fatalf("guard dir = %q, want %q", env[signalGuardDirEnv], layout.dir)
	}
	if env[signalGuardEventLogEnv] != layout.eventLog {
		t.Fatalf("event log = %q, want %q", env[signalGuardEventLogEnv], layout.eventLog)
	}
	if _, ok := env[signalGuardProcessGroupEnv]; ok {
		t.Fatalf("process group stamped %q into the pre-spawn env; the group leg is file-delivered", env[signalGuardProcessGroupEnv])
	}
}

// TestSignalGuardEnvWithoutLayout pins the fail-safe wiring: an install
// failure leaves the session env alone (unguarded PATH) instead of
// poisoning it.
func TestSignalGuardEnvWithoutLayout(t *testing.T) {
	t.Parallel()
	env := signalGuardEnv(map[string]string{"PATH": "/usr/bin:/bin"}, signalGuardLayout{}, t.TempDir(), "")
	if env["PATH"] != "/usr/bin:/bin" {
		t.Fatalf("PATH = %q, want it untouched", env["PATH"])
	}
	if _, ok := env[signalGuardWorktreeEnv]; ok {
		t.Fatalf("worktree stamped without a guard layout")
	}
}

func itoaForTest(n int) string { return strconv.Itoa(n) }

func TestChainOnProcessSpawnedRunsBoth(t *testing.T) {
	t.Parallel()
	var calls []int
	first := func(pid int) { calls = append(calls, pid*10) }
	second := func(pid int) { calls = append(calls, pid) }
	chainOnProcessSpawned(first, second)(42)
	if len(calls) != 2 || calls[0] != 420 || calls[1] != 42 {
		t.Fatalf("chained calls = %v", calls)
	}
	if chainOnProcessSpawned(nil, second) == nil {
		t.Fatalf("nil existing returned nil")
	}
	if chainOnProcessSpawned(first, nil) == nil {
		t.Fatalf("nil after returned nil")
	}
}

func TestReportSignalGuardRefusalsDrainsAndClears(t *testing.T) {
	t.Parallel()
	layout, err := installSignalGuard()
	if err != nil {
		t.Fatalf("installSignalGuard: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(layout.dir) })
	eventsDir := t.TempDir()
	lines := "refused pkill -f 'x': pid(s) 11 22 outside session scope (worktree /w)\n\nrefused killall 'y': pid(s) 33 outside session scope (worktree /w)\n"
	if err := os.WriteFile(layout.eventLog, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	r := minimalRunner(t)
	r.reportSignalGuardRefusals(context.Background(), layout, eventsDir, sink)
	if len(sink.events) != 2 {
		t.Fatalf("reported %d events, want 2", len(sink.events))
	}
	for _, ev := range sink.events {
		sys, ok := ev.(agent.SystemEvent)
		if !ok || sys.Subtype != SystemSubtypeSignalGuardRefused {
			t.Fatalf("event = %#v, want a signal_guard_refused SystemEvent", ev)
		}
	}
	mirror, err := os.ReadFile(filepath.Join(eventsDir, ".agent", "events.jsonl"))
	if err != nil || strings.Count(string(mirror), SystemSubtypeSignalGuardRefused) != 2 {
		t.Fatalf("events.jsonl mirror = %q, want both refusal events", mirror)
	}
	if _, err := os.Stat(layout.eventLog); !os.IsNotExist(err) {
		t.Fatalf("refusal log not cleared after drain")
	}
	// Draining with no log is a silent no-op.
	r.reportSignalGuardRefusals(context.Background(), layout, eventsDir, sink)
	if len(sink.events) != 2 {
		t.Fatalf("empty drain reported events")
	}
	// Draining without a guard layout is a no-op (install failure path).
	r.reportSignalGuardRefusals(context.Background(), signalGuardLayout{}, eventsDir, sink)
	if len(sink.events) != 2 {
		t.Fatalf("layout-less drain reported events")
	}
}

func TestSignalGuardProtocolPartialMentionsKillByPID(t *testing.T) {
	t.Parallel()
	if !strings.Contains(injectSignalGuardProtocolPartial(""), "Kill by PID") {
		t.Fatalf("protocol partial missing the kill-by-PID rule")
	}
	if got := injectSignalGuardProtocolPartial("base"); !strings.Contains(got, "base") || !strings.Contains(got, "Kill by PID") {
		t.Fatalf("protocol partial did not append: %q", got)
	}
}

func TestRecordSessionProcessGroupPersistsGroup(t *testing.T) {
	t.Parallel()
	r := minimalRunner(t)
	statePath := t.TempDir()
	layout, err := installSignalGuard()
	if err != nil {
		t.Fatalf("installSignalGuard: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(layout.dir) })
	r.recordSessionProcessGroup(statePath, "sess-1", os.Getpid(), layout.dir)
	fileBody, err := os.ReadFile(layout.pgidFile)
	if err != nil {
		t.Fatalf("read guard pgid file: %v", err)
	}
	pgid := strings.TrimSpace(string(fileBody))
	if pgid == "" || pgid == "0" {
		t.Fatalf("group not written to the guard pgid file: %q", fileBody)
	}
	stored, err := r.store.Read(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if got := itoaForTest(stored.SessionProcessGroup); got != pgid {
		t.Fatalf("state group %q != guard file group %q", got, pgid)
	}
	// An unresolvable pid leaves the file, env and state untouched.
	before := fileBody
	r.recordSessionProcessGroup(statePath, "sess-1", -1, layout.dir)
	if after, _ := os.ReadFile(layout.pgidFile); string(after) != string(before) {
		t.Fatalf("invalid pid rewrote the guard pgid file")
	}
	// pid 0 (never observed) is refused the same way.
	if err := os.Remove(layout.pgidFile); err != nil {
		t.Fatal(err)
	}
	r.recordSessionProcessGroup(statePath, "sess-1", 0, layout.dir)
	if _, err := os.Stat(layout.pgidFile); !os.IsNotExist(err) {
		t.Fatalf("pid 0 wrote a guard pgid file")
	}
}
