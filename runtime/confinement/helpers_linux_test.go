//go:build linux

package confinement

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shortTempDir returns a short temporary directory: the self-test binds a
// socket in its session tmp, and socket paths are limited to about a hundred
// bytes.
func shortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = removeAllForce(dir) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return resolved
}

// hostDirs returns a throwaway operator home and host state home plus a
// profile directory under the state home.
func hostDirs(t *testing.T) (home, stateHome, profileDir string) {
	t.Helper()
	base := shortTempDir(t, "dch")
	home = filepath.Join(base, "home")
	stateHome = filepath.Join(home, ".state")
	profileDir = filepath.Join(stateHome, "profiles")
	for _, dir := range []string{home, stateHome, profileDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	return home, stateHome, profileDir
}

func failureIDs(record SelfTestRecord) string {
	var ids []string
	for _, probe := range record.Failures() {
		ids = append(ids, fmt.Sprintf("%s/%s (expected %s, observed %s: %s)", probe.Mode, probe.ID, probe.Expected, probe.Observed, probe.Detail))
	}
	return strings.Join(ids, "\n")
}

// runSeatCheck writes the plan into work (a writable root of the boundary
// under test), runs launch with the test binary and the plan path, and
// returns the results by id. A run that leaves no results fails the test:
// a seat that never ran proves nothing.
func runSeatCheck(t *testing.T, work string, steps []seatCheckStep, launch func(self, planPath string) (string, error)) map[string]seatCheckResult {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	planPath := filepath.Join(work, "seat-plan.json")
	resultPath := filepath.Join(work, "seat-result.json")
	raw, err := json.Marshal(seatCheckPlan{ResultPath: resultPath, Steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	output, launchErr := launch(self, planPath)
	encoded, err := os.ReadFile(resultPath) //nolint:gosec // G304: the check's own result.
	if err != nil {
		if len(output) > 2048 {
			output = output[:2048]
		}
		t.Fatalf("the seat check left no results (launch: %v): %v\n%s", launchErr, err, output)
	}
	var decoded []seatCheckResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode seat check results: %v", err)
	}
	results := map[string]seatCheckResult{}
	for _, result := range decoded {
		results[result.ID] = result
	}
	for _, step := range steps {
		if _, ok := results[step.ID]; !ok {
			t.Fatalf("the seat check left no result for %q", step.ID)
		}
	}
	return results
}

// countingListener is a loopback TCP listener that counts the connections
// it accepted, so a dial that got through is observed from outside.
type countingListener struct {
	addr  string
	port  int
	count atomic.Int32
}

func newCountingListener(t *testing.T) *countingListener {
	t.Helper()
	listener, port, err := listenLoopback()
	if err != nil {
		t.Fatalf("loopback listener: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	l := &countingListener{addr: listener.Addr().String(), port: port}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			l.count.Add(1)
			_ = conn.Close()
		}
	}()
	return l
}

func (l *countingListener) accepted() int32 { return l.count.Load() }

// reached reports whether the listener accepted a connection, waiting
// briefly: a dial that completed may be accepted just after it returns.
func (l *countingListener) reached() bool {
	deadline := time.Now().Add(3 * time.Second)
	for l.accepted() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	return l.accepted() > 0
}
