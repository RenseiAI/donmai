//go:build unix

package runner

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestInstallSignalGuardWritesExecutableScripts(t *testing.T) {
	t.Parallel()
	worktree := t.TempDir()
	layout, err := installSignalGuard(worktree)
	if err != nil {
		t.Fatalf("installSignalGuard: %v", err)
	}
	for _, path := range []string{layout.pkill, layout.killall} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s is not executable (mode %v)", path, info.Mode())
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
}

func TestInstallSignalGuardRefusesEmptyWorktree(t *testing.T) {
	t.Parallel()
	if _, err := installSignalGuard(""); err == nil {
		t.Fatalf("installSignalGuard(\"\") succeeded, want an error")
	}
}

func TestSignalGuardEnvLeadsPATH(t *testing.T) {
	t.Parallel()
	worktree := t.TempDir()
	layout, err := installSignalGuard(worktree)
	if err != nil {
		t.Fatalf("installSignalGuard: %v", err)
	}
	env := signalGuardEnv(map[string]string{"PATH": "/usr/bin:/bin"}, layout, "/workarea-root", 1234)
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
		t.Fatalf("workarea root = %q", env[signalGuardWorkareaRootEnv])
	}
	if env[signalGuardProcessGroupEnv] != "1234" {
		t.Fatalf("pgid = %q, want 1234", env[signalGuardProcessGroupEnv])
	}
	if env[signalGuardEventLogEnv] != layout.eventLog {
		t.Fatalf("event log = %q, want %q", env[signalGuardEventLogEnv], layout.eventLog)
	}

	// Unknown pgid leaves the group leg unset rather than stamping zero.
	env = signalGuardEnv(map[string]string{}, layout, "", 0)
	if _, ok := env[signalGuardProcessGroupEnv]; ok {
		t.Fatalf("zero pgid stamped %q", env[signalGuardProcessGroupEnv])
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
	worktree := t.TempDir()
	layout, err := installSignalGuard(worktree)
	if err != nil {
		t.Fatalf("installSignalGuard: %v", err)
	}
	lines := "refused pkill -f 'x': pid(s) 11 22 outside session scope (worktree /w)\n\nrefused killall 'y': pid(s) 33 outside session scope (worktree /w)\n"
	if err := os.WriteFile(layout.eventLog, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	r := minimalRunner(t)
	r.reportSignalGuardRefusals(context.Background(), worktree, sink)
	if len(sink.events) != 2 {
		t.Fatalf("reported %d events, want 2", len(sink.events))
	}
	for _, ev := range sink.events {
		sys, ok := ev.(agent.SystemEvent)
		if !ok || sys.Subtype != SystemSubtypeSignalGuardRefused {
			t.Fatalf("event = %#v, want a signal_guard_refused SystemEvent", ev)
		}
	}
	if _, err := os.Stat(layout.eventLog); !os.IsNotExist(err) {
		t.Fatalf("refusal log not cleared after drain")
	}
	// Draining with no log is a silent no-op.
	r.reportSignalGuardRefusals(context.Background(), worktree, sink)
	if len(sink.events) != 2 {
		t.Fatalf("empty drain reported events")
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
	specEnv := map[string]string{}
	r.recordSessionProcessGroup(statePath, "sess-1", os.Getpid(), specEnv)
	pgid := specEnv[signalGuardProcessGroupEnv]
	if pgid == "" {
		t.Fatalf("group not stamped into the session env")
	}
	stored, err := r.store.Read(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if got := itoaForTest(stored.SessionProcessGroup); got != pgid {
		t.Fatalf("state group %q != env group %q", got, pgid)
	}
	// An unresolvable pid leaves env and state untouched.
	before := len(specEnv)
	r.recordSessionProcessGroup(statePath, "sess-1", -1, specEnv)
	if len(specEnv) != before {
		t.Fatalf("invalid pid mutated the session env")
	}
}
