//go:build !windows

package process_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/internal/process"
)

func TestPIDFile_WriteReadRoundtrip(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	tmp := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmp)

	pf, err := process.NewPIDFile("test-roundtrip")
	if err != nil {
		t.Fatalf("NewPIDFile: %v", err)
	}

	pid := os.Getpid()
	if err := pf.Write(pid); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := pf.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != pid {
		t.Errorf("Read() = %d, want %d", got, pid)
	}
}

func TestPIDFile_ReadMissingFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmp)

	pf, err := process.NewPIDFile("test-missing")
	if err != nil {
		t.Fatalf("NewPIDFile: %v", err)
	}

	_, err = pf.Read()
	if !errors.Is(err, process.ErrNotRunning) {
		t.Errorf("Read() with no file = %v, want ErrNotRunning", err)
	}
}

func TestPIDFile_ReadStalePID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmp)

	pf, err := process.NewPIDFile("test-stale")
	if err != nil {
		t.Fatalf("NewPIDFile: %v", err)
	}

	// Use a very large PID that is extremely unlikely to exist.
	dir := filepath.Dir(pf.Path())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	deadPID := 99999999
	data := []byte(strconv.Itoa(deadPID) + "\n")
	if err := os.WriteFile(pf.Path(), data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err = pf.Read()
	if !errors.Is(err, process.ErrStalePID) {
		t.Errorf("Read() with dead PID = %v, want ErrStalePID", err)
	}
}

func TestPIDFile_ReadInvalidContent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmp)

	pf, err := process.NewPIDFile("test-invalid")
	if err != nil {
		t.Fatalf("NewPIDFile: %v", err)
	}

	// Write invalid (non-integer) content.
	dir := filepath.Dir(pf.Path())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(pf.Path(), []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err = pf.Read()
	if err == nil {
		t.Error("Read() with invalid content = nil, want error")
	}
}

func TestPIDFile_RemoveIdempotent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmp)

	pf, err := process.NewPIDFile("test-remove")
	if err != nil {
		t.Fatalf("NewPIDFile: %v", err)
	}

	// Remove when file does not exist should not error.
	if err := pf.Remove(); err != nil {
		t.Errorf("Remove() on missing file = %v, want nil", err)
	}

	// Write then remove should succeed.
	if err := pf.Write(os.Getpid()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := pf.Remove(); err != nil {
		t.Errorf("Remove() after Write = %v, want nil", err)
	}

	// Remove again — still idempotent.
	if err := pf.Remove(); err != nil {
		t.Errorf("Remove() second time = %v, want nil", err)
	}
}

func TestPIDFile_PathXDG(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmp)

	pf, err := process.NewPIDFile("myservice")
	if err != nil {
		t.Fatalf("NewPIDFile: %v", err)
	}

	want := filepath.Join(tmp, "agentfactory", "myservice.pid")
	if pf.Path() != want {
		t.Errorf("Path() = %q, want %q", pf.Path(), want)
	}
}

func TestPIDFile_PathFallback(t *testing.T) {
	// Unset XDG_RUNTIME_DIR to force fallback.
	t.Setenv("XDG_RUNTIME_DIR", "")

	pf, err := process.NewPIDFile("myservice")
	if err != nil {
		t.Fatalf("NewPIDFile: %v", err)
	}

	if !strings.HasSuffix(pf.Path(), filepath.Join("agentfactory", "myservice.pid")) {
		t.Errorf("Path() = %q, expected to end with agentfactory/myservice.pid", pf.Path())
	}
}

func TestPIDFile_WriteCreatesMissingDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmp)

	pf, err := process.NewPIDFile("test-mkdirall")
	if err != nil {
		t.Fatalf("NewPIDFile: %v", err)
	}

	// The dir does not exist yet — Write should create it.
	if err := pf.Write(os.Getpid()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if _, err := os.Stat(pf.Path()); err != nil {
		t.Errorf("PID file not found after Write: %v", err)
	}
}
