package testexec

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// TestWriteFile_ExecImmediatelyUnderConcurrentForks writes and execs scripts
// while other goroutines continuously fork. Without the fork lock this fails
// intermittently with ETXTBSY on Linux.
func TestWriteFile_ExecImmediatelyUnderConcurrentForks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	dir := t.TempDir()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = exec.Command("/bin/sh", "-c", "exit 0").Run()
				}
			}
		}()
	}
	defer func() { close(stop); wg.Wait() }()

	for i := range 40 {
		path := filepath.Join(dir, "fake"+strings.Repeat("x", i%3)+string(rune('a'+i%26)))
		if err := WriteFile(path, []byte("#!/bin/sh\necho ok\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(path).Output() //nolint:gosec // executing the fixture just written is the point
		if err != nil {
			t.Fatalf("exec %s: %v", path, err)
		}
		if strings.TrimSpace(string(out)) != "ok" {
			t.Fatalf("unexpected output %q", out)
		}
		_ = os.Remove(path)
	}
}
