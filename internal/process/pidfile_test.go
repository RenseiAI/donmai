//go:build !windows

package process_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/internal/process"
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

	want := filepath.Join(tmp, "donmai", "myservice.pid")
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

	if !strings.HasSuffix(pf.Path(), filepath.Join("donmai", "myservice.pid")) {
		t.Errorf("Path() = %q, expected to end with donmai/myservice.pid", pf.Path())
	}
}

// writeLegacyPIDFile drops a PID file in the pre-rename (agentfactory)
// directory, simulating a process recorded by an older binary.
func writeLegacyPIDFile(t *testing.T, base, name string, pid int) string {
	t.Helper()
	dir := filepath.Join(base, "agentfactory")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll legacy dir: %v", err)
	}
	path := filepath.Join(dir, name+".pid")
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile legacy pid: %v", err)
	}
	return path
}

// Read must fall back to the legacy agentfactory directory during the
// rename migration window so an upgrade does not orphan a process
// started by an older binary.
func TestPIDFile_ReadFallsBackToLegacyDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmp)

	writeLegacyPIDFile(t, tmp, "legacy-live", os.Getpid())

	pf, err := process.NewPIDFile("legacy-live")
	if err != nil {
		t.Fatalf("NewPIDFile: %v", err)
	}
	got, err := pf.Read()
	if err != nil {
		t.Fatalf("Read() with legacy file = %v, want nil", err)
	}
	if got != os.Getpid() {
		t.Errorf("Read() = %d, want %d", got, os.Getpid())
	}
}

// A stale legacy record keeps the stale semantics on the fallback path.
func TestPIDFile_LegacyStalePID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmp)

	writeLegacyPIDFile(t, tmp, "legacy-stale", 99999999)

	pf, err := process.NewPIDFile("legacy-stale")
	if err != nil {
		t.Fatalf("NewPIDFile: %v", err)
	}
	if _, err := pf.Read(); !errors.Is(err, process.ErrStalePID) {
		t.Errorf("Read() with dead legacy PID = %v, want ErrStalePID", err)
	}
}

// The canonical file wins over the legacy one when both exist.
func TestPIDFile_CanonicalWinsOverLegacy(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmp)

	writeLegacyPIDFile(t, tmp, "both", 99999999) // stale legacy record

	pf, err := process.NewPIDFile("both")
	if err != nil {
		t.Fatalf("NewPIDFile: %v", err)
	}
	if err := pf.Write(os.Getpid()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := pf.Read()
	if err != nil {
		t.Fatalf("Read() = %v, want nil (canonical file wins)", err)
	}
	if got != os.Getpid() {
		t.Errorf("Read() = %d, want %d", got, os.Getpid())
	}
}

// Write clears the legacy record so a stale pre-rename file cannot
// resurface through the fallback after the canonical file is removed.
func TestPIDFile_WriteClearsLegacyRecord(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmp)

	legacyPath := writeLegacyPIDFile(t, tmp, "clears", 99999999)

	pf, err := process.NewPIDFile("clears")
	if err != nil {
		t.Fatalf("NewPIDFile: %v", err)
	}
	if err := pf.Write(os.Getpid()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("legacy pid file still present after Write: %v", err)
	}
}

// Remove clears both the canonical and legacy locations.
func TestPIDFile_RemoveClearsLegacyRecord(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmp)

	writeLegacyPIDFile(t, tmp, "remove-both", os.Getpid())

	pf, err := process.NewPIDFile("remove-both")
	if err != nil {
		t.Fatalf("NewPIDFile: %v", err)
	}
	if err := pf.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := pf.Read(); !errors.Is(err, process.ErrNotRunning) {
		t.Errorf("Read() after Remove = %v, want ErrNotRunning", err)
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
