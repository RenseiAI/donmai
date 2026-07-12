//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package daemon

import (
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
	if got := s.ActiveCount(); got != 0 {
		t.Fatalf("ActiveCount = %d, want 0", got)
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
