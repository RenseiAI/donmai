//go:build unix

package afcli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStoredGitHubCLIRefusesMissingBinaryAndCancelledContext(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	if _, err := storedGitHubCLIToken(context.Background()); err == nil || !strings.Contains(err.Error(), "installed GitHub CLI") {
		t.Fatalf("missing native provider = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := storedGitHubCLIToken(ctx); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("cancelled lookup = %v", err)
	}
}

func TestStoredGitHubCLIRefusesUnsafeCredentialLocators(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	localGitHubFakeCLI(t, home, "printf 'native-token\\n'")
	t.Setenv("PATH", filepath.Join(home, ".local", "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_CONFIG_DIR", "relative/config")
	if _, err := storedGitHubCLIToken(context.Background()); err == nil || !strings.Contains(err.Error(), "GH_CONFIG_DIR") {
		t.Fatalf("relative config locator = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "gh-args")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid credential locator launched native CLI")
	}
}

func TestStoredGitHubCLIStopsOnlyOwnedDescendants(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	localGitHubFakeCLI(t, home, "printf '%s\\n' \"$$\" > \"$HOME/owned-leader-pid\"\nsleep 30 &\nprintf '%s\\n' \"$!\" > \"$HOME/owned-child-pid\"\nprintf 'native-child-token\\n'")
	t.Setenv("PATH", filepath.Join(home, ".local", "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	token, err := storedGitHubCLIToken(ctx)
	if err != nil || token != "native-child-token" {
		t.Fatalf("owned descendant lookup = %q, %v", token, err)
	}
	rawPID, err := os.ReadFile(filepath.Join(home, "owned-child-pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid fixture descendant PID %q, %v", rawPID, err)
	}
	rawLeader, err := os.ReadFile(filepath.Join(home, "owned-leader-pid"))
	if err != nil {
		t.Fatal(err)
	}
	leader, err := strconv.Atoi(strings.TrimSpace(string(rawLeader)))
	if err != nil || leader <= 0 {
		t.Fatalf("invalid fixture leader PID %q, %v", rawLeader, err)
	}
	// A just-killed child may briefly be a zombie under the container's PID 1.
	// Neither an absent process nor a zombie has a live writer or credential use.
	table, err := localGitHubPSTable() // fixed executable and fixed argv
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 || fields[0] != strconv.Itoa(pid) {
			continue
		}
		group, parseErr := strconv.Atoi(fields[2])
		if parseErr != nil || group != leader {
			t.Fatalf("fixture descendant lost owned group: pid=%d leader=%d status=%q", pid, leader, line)
		}
		if !strings.HasPrefix(fields[3], "Z") {
			t.Fatalf("owned fixture descendant remains live: pid=%d state=%q", pid, fields[3])
		}
	}
}

func TestStoredGitHubCLITimeoutRefusesAndQuiesces(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	localGitHubFakeCLI(t, home, "printf 'timeout-secret-token\\n'\nsleep 30")
	t.Setenv("PATH", filepath.Join(home, ".local", "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := storedGitHubCLIToken(ctx); err == nil || strings.Contains(err.Error(), "timeout-secret-token") {
		t.Fatalf("timed-out credential was accepted or disclosed: %v", err)
	}
}
