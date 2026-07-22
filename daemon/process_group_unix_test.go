//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSpawner_ForceKillSessionProcessTree(t *testing.T) {
	grandchildPIDPath := filepath.Join(t.TempDir(), "grandchild.pid")
	command := fmt.Sprintf("sleep 30 & echo $! > %q; wait", grandchildPIDPath)
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{"/bin/sh", "-c", command},
	})
	ended := sessionEnds(s)
	handle, err := s.AcceptWork(SessionSpec{SessionID: "hard-kill", Repository: "github.com/a/b"})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	grandchildPID := waitPIDFile(t, grandchildPIDPath)
	parentPGID, err := syscall.Getpgid(handle.PID)
	if err != nil {
		t.Fatalf("parent getpgid: %v", err)
	}
	grandchildPGID, err := syscall.Getpgid(grandchildPID)
	if err != nil {
		t.Fatalf("grandchild getpgid: %v", err)
	}
	selfPGID, _ := syscall.Getpgid(os.Getpid())
	if parentPGID != handle.PID || grandchildPGID != parentPGID || parentPGID == selfPGID {
		t.Fatalf("groups: parent=%d child=%d childPID=%d self=%d", parentPGID, grandchildPGID, handle.PID, selfPGID)
	}

	if err := s.ForceKillSession("hard-kill"); err != nil {
		t.Fatalf("ForceKillSession: %v", err)
	}
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 0)
	// Ended is not delivered until the full daemon-owned group has gone away;
	// checking the group itself catches a leader-exited descendant surviving the
	// direct-child cmd.Wait path.
	if err := syscall.Kill(-handle.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("process group still exists after Ended: %v", err)
	}
	if err := s.ForceKillSession("hard-kill"); err != nil {
		t.Fatalf("duplicate force kill should be idempotent: %v", err)
	}
	if err := s.ForceKillSession("never-owned"); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("never-owned error = %v", err)
	}
}

func TestSpawner_ForceKillSessionLeavesSiblingProcessTreeAlive(t *testing.T) {
	pidDir := t.TempDir()
	command := `sleep 30 & echo $! > "$PID_DIR/$DONMAI_SESSION_ID.pid"; wait`
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 2,
		WorkerCommand:         []string{"/bin/sh", "-c", command},
		BaseEnv:               map[string]string{"PID_DIR": pidDir},
	})
	t.Cleanup(func() { _ = s.Drain(time.Second) })
	ended := sessionEnds(s)
	victim, err := s.AcceptWork(SessionSpec{SessionID: "victim", Repository: "github.com/a/b"})
	if err != nil {
		t.Fatalf("accept victim: %v", err)
	}
	sibling, err := s.AcceptWork(SessionSpec{SessionID: "sibling", Repository: "github.com/a/b"})
	if err != nil {
		t.Fatalf("accept sibling: %v", err)
	}
	victimChild := waitPIDFile(t, filepath.Join(pidDir, "victim.pid"))
	siblingChild := waitPIDFile(t, filepath.Join(pidDir, "sibling.pid"))

	victimPGID, err := syscall.Getpgid(victim.PID)
	if err != nil {
		t.Fatalf("victim getpgid: %v", err)
	}
	siblingPGID, err := syscall.Getpgid(sibling.PID)
	if err != nil {
		t.Fatalf("sibling getpgid: %v", err)
	}
	if victimPGID == siblingPGID || victimPGID != victim.PID || siblingPGID != sibling.PID {
		t.Fatalf("process groups not isolated: victim=%d sibling=%d", victimPGID, siblingPGID)
	}
	if childGroup, _ := syscall.Getpgid(victimChild); childGroup != victimPGID {
		t.Fatalf("victim child group = %d, want %d", childGroup, victimPGID)
	}
	if childGroup, _ := syscall.Getpgid(siblingChild); childGroup != siblingPGID {
		t.Fatalf("sibling child group = %d, want %d", childGroup, siblingPGID)
	}

	if err := s.ForceKillSession("victim"); err != nil {
		t.Fatalf("ForceKillSession victim: %v", err)
	}
	waitSessionEnd(t, ended)
	time.Sleep(50 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), 0); err != nil {
		t.Fatalf("test/daemon process did not survive: %v", err)
	}
	if err := syscall.Kill(sibling.PID, 0); err != nil {
		t.Fatalf("sibling parent did not survive: %v", err)
	}
	if err := syscall.Kill(siblingChild, 0); err != nil {
		t.Fatalf("sibling child did not survive: %v", err)
	}
	active := s.ActiveSessions()
	if len(active) != 1 || active[0].SessionID != "sibling" {
		t.Fatalf("active sessions after victim kill = %+v, want sibling only", active)
	}
	if !s.StopSession("sibling") {
		t.Fatal("cleanup failed to stop surviving sibling")
	}
}

func waitPIDFile(t *testing.T, path string) int {
	t.Helper()
	var pid int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		raw, readErr := os.ReadFile(path)
		if readErr == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
			if pid > 1 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid <= 1 {
		t.Fatalf("pid was not published at %s", path)
	}
	return pid
}

func TestSpawner_ReapsPipeSilentDescendantAfterLeaderExit(t *testing.T) {
	childPath := filepath.Join(t.TempDir(), "pipe-silent-child.pid")
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand: []string{"/bin/sh", "-c", fmt.Sprintf(
			`sleep 30 >/dev/null 2>&1 & echo $! > %q; exit 0`, childPath,
		)},
	})
	ended := sessionEnds(s)
	if _, err := s.AcceptWork(SessionSpec{SessionID: "pipe-silent", Repository: "github.com/a/b"}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	child := waitPIDFile(t, childPath)
	waitSessionEnd(t, ended)
	if err := syscall.Kill(child, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("pipe-silent descendant survived terminal reaping: %v", err)
	}
}

func TestSpawner_StopSessionGrantsTERMFlushWindow(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "term-flushed")
	ready := marker + ".ready"
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand: []string{"/bin/sh", "-c", fmt.Sprintf(
			`trap 'printf flushed > %q; exit 0' TERM; printf ready > %q; while :; do :; done`, marker, ready,
		)},
	})
	ended := sessionEnds(s)
	if _, err := s.AcceptWork(SessionSpec{SessionID: "term-flush", Repository: "github.com/a/b"}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	waitFile(t, ready)
	if !s.StopSession("term-flush") {
		t.Fatal("StopSession = false")
	}
	waitSessionEnd(t, ended)
	if raw, err := os.ReadFile(marker); err != nil || string(raw) != "flushed" {
		t.Fatalf("TERM flush marker = %q, %v; want flushed", raw, err)
	}
}

func TestSpawner_DrainGrantsTERMFlushWindow(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "drain-term-flushed")
	ready := marker + ".ready"
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand: []string{"/bin/sh", "-c", fmt.Sprintf(
			`trap 'printf flushed > %q; exit 0' TERM; printf ready > %q; while :; do :; done`, marker, ready,
		)},
	})
	ended := sessionEnds(s)
	if _, err := s.AcceptWork(SessionSpec{SessionID: "drain-term-flush", Repository: "github.com/a/b"}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	waitFile(t, ready)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.DrainContext(ctx)
	var incomplete *DrainIncompleteError
	if !errors.As(err, &incomplete) {
		t.Fatalf("DrainContext error = %v, want DrainIncompleteError", err)
	}
	waitSessionEnd(t, ended)
	if raw, err := os.ReadFile(marker); err != nil || string(raw) != "flushed" {
		t.Fatalf("TERM drain flush marker = %q, %v; want flushed", raw, err)
	}
	if err := s.DrainContext(context.Background()); err != nil {
		t.Fatalf("final DrainContext: %v", err)
	}
}

func TestSpawner_DrainEscalatesTERMIgnoringProcessTree(t *testing.T) {
	childPath := filepath.Join(t.TempDir(), "term-ignoring-child.pid")
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		WorkerCommand: []string{"/bin/sh", "-c", fmt.Sprintf(
			`trap '' TERM; sleep 30 & echo $! > %q; wait`, childPath,
		)},
	})
	ended := sessionEnds(s)
	handle, err := s.AcceptWork(SessionSpec{SessionID: "term-ignore", Repository: "github.com/a/b"})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	child := waitPIDFile(t, childPath)
	start := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = s.DrainContext(ctx)
	var incomplete *DrainIncompleteError
	if !errors.As(err, &incomplete) {
		t.Fatalf("DrainContext error = %v, want DrainIncompleteError", err)
	}
	waitSessionEnd(t, ended)
	if elapsed := time.Since(start); elapsed < sessionTerminationGrace/2 {
		t.Fatalf("TERM-ignoring tree reaped in %s, before bounded graceful window", elapsed)
	}
	if err := syscall.Kill(child, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("TERM-ignoring child survived escalation: %v", err)
	}
	if err := syscall.Kill(-handle.PID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("TERM-ignoring process group survived escalation: %v", err)
	}
}

func waitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("file was not published at %s", path)
}
