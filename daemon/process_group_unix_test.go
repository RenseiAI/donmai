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

	var grandchildPID int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		raw, readErr := os.ReadFile(grandchildPIDPath)
		if readErr == nil {
			grandchildPID, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
			if grandchildPID > 1 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if grandchildPID <= 1 {
		t.Fatal("grandchild pid was not published")
	}
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
