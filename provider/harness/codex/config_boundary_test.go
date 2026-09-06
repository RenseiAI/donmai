package codex

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexConfigBoundaryRejectsSymlinkedConfigWithoutFollowingIt(t *testing.T) {
	root := t.TempDir()
	boundary, err := newCodexConfigBoundary(root, false)
	if err != nil {
		t.Fatalf("new boundary: %v", err)
	}
	t.Cleanup(func() { _ = boundary.remove() })
	target := filepath.Join(root, "must-survive")
	const targetBody = "not provider-owned"
	if err := os.WriteFile(target, []byte(targetBody), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Remove(boundary.configPath); err != nil {
		t.Fatalf("remove owned config fixture: %v", err)
	}
	if err := os.Symlink(target, boundary.configPath); err != nil {
		t.Fatalf("symlink config fixture: %v", err)
	}
	if err := boundary.validate(); err == nil {
		t.Fatal("symlinked config unexpectedly passed validation")
	}
	if err := boundary.remove(); err != nil {
		t.Fatalf("remove boundary: %v", err)
	}
	body, err := os.ReadFile(target)
	if err != nil || string(body) != targetBody {
		t.Fatalf("boundary cleanup followed config symlink: body=%q err=%v", body, err)
	}
}

func TestCodexConfigBoundaryRemovalPinsParentIdentityAndPersistsFailure(t *testing.T) {
	root := t.TempDir()
	boundary, err := newCodexConfigBoundary(root, false)
	if err != nil {
		t.Fatalf("new boundary: %v", err)
	}
	otherInfo, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatalf("stat other parent: %v", err)
	}
	boundary.parentInfo = otherInfo

	first := boundary.remove()
	if first == nil || !strings.Contains(first.Error(), "parent identity changed") {
		t.Fatalf("first remove error = %v, want pinned-parent refusal", first)
	}
	second := boundary.remove()
	if second == nil || second.Error() != first.Error() {
		t.Fatalf("second remove error = %v, want persistent %v", second, first)
	}
	if _, err := os.Stat(boundary.home); err != nil {
		t.Fatalf("refused boundary unexpectedly removed: %v", err)
	}
}

func TestCodexConfigBoundaryRemovalRetainsHomesWithRollouts(t *testing.T) {
	for _, tc := range []struct {
		name        string
		rolloutName string
		wantHome    bool
	}{
		{name: "ordinary cleanup", rolloutName: "other.jsonl", wantHome: false},
		{name: "resumable rollout", rolloutName: "rollout-2026-09-06T12-00-00-thread-live.jsonl", wantHome: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boundary, err := newCodexConfigBoundary(t.TempDir(), false)
			if err != nil {
				t.Fatalf("new boundary: %v", err)
			}
			stateDir := filepath.Join(boundary.home, codexSessionStateSubdir, "2026", "09", "06")
			if err := os.MkdirAll(stateDir, 0o700); err != nil {
				t.Fatalf("create state directory: %v", err)
			}
			if err := os.WriteFile(filepath.Join(stateDir, tc.rolloutName), []byte(`{"type":"session_meta"}`), 0o600); err != nil {
				t.Fatalf("write session state: %v", err)
			}
			if err := boundary.remove(); err != nil {
				t.Fatalf("boundary remove: %v", err)
			}
			_, statErr := os.Stat(boundary.home)
			if got := statErr == nil; got != tc.wantHome {
				t.Fatalf("home exists after cleanup = %t, want %t (err=%v)", got, tc.wantHome, statErr)
			}
		})
	}
}

func TestCodexConfigBoundaryHardLinksHostSessionAuth(t *testing.T) {
	root := t.TempDir()
	hostHome := filepath.Join(root, "host")
	boundaryParent := filepath.Join(root, "boundaries")
	for _, dir := range []string{hostHome, boundaryParent} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Base(dir), err)
		}
	}
	hostAuth := filepath.Join(hostHome, codexAuthFileName)
	if err := os.WriteFile(hostAuth, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"before"}}`), 0o600); err != nil {
		t.Fatalf("write host auth fixture: %v", err)
	}

	boundary, err := newCodexConfigBoundary(boundaryParent, true)
	if err != nil {
		t.Fatalf("new boundary with host auth: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(boundary.home, codexAuthFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("auth was delivered before selected spawn: %v", err)
	}
	if err := boundary.linkHostSessionAuth(hostAuth); err != nil {
		t.Fatalf("link host auth at spawn boundary: %v", err)
	}
	linkedInfo, err := os.Stat(boundary.authPath)
	if err != nil {
		t.Fatalf("stat linked auth: %v", err)
	}
	hostInfo, err := os.Stat(hostAuth)
	if err != nil {
		t.Fatalf("stat host auth: %v", err)
	}
	if !os.SameFile(hostInfo, linkedInfo) {
		t.Fatal("isolated auth is a copy instead of the host credential inode")
	}
	if err := boundary.validate(); err != nil {
		t.Fatalf("validate boundary with host auth: %v", err)
	}
	configBody, err := os.ReadFile(boundary.configPath)
	if err != nil {
		t.Fatalf("read isolated config: %v", err)
	}
	if !strings.Contains(string(configBody), codexFileAuthConfig) {
		t.Fatalf("isolated config does not pin file auth: %q", configBody)
	}

	const refreshed = `{"auth_mode":"chatgpt","tokens":{"access_token":"after"}}`
	if err := os.WriteFile(boundary.authPath, []byte(refreshed), 0o600); err != nil {
		t.Fatalf("refresh through isolated auth: %v", err)
	}
	hostBody, err := os.ReadFile(hostAuth)
	if err != nil || string(hostBody) != refreshed {
		t.Fatalf("host auth did not observe refresh: body=%q err=%v", hostBody, err)
	}

	if err := boundary.remove(); err != nil {
		t.Fatalf("remove boundary: %v", err)
	}
	hostBody, err = os.ReadFile(hostAuth)
	if err != nil || string(hostBody) != refreshed {
		t.Fatalf("boundary cleanup removed or changed host auth: body=%q err=%v", hostBody, err)
	}
}

func TestCodexConfigBoundaryRejectsUnsafeHostSessionAuth(t *testing.T) {
	root := t.TempDir()
	boundary, err := newCodexConfigBoundary(root, true)
	if err != nil {
		t.Fatalf("new file-auth boundary: %v", err)
	}
	t.Cleanup(func() { _ = boundary.remove() })
	loose := filepath.Join(root, "loose-auth.json")
	//nolint:gosec // The fixture deliberately proves group-readable auth is rejected.
	if err := os.WriteFile(loose, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write loose auth: %v", err)
	}
	if err := boundary.linkHostSessionAuth(loose); err == nil || !strings.Contains(err.Error(), "group or other access") {
		t.Fatalf("loose host auth error = %v, want permissions rejection", err)
	}

	target := filepath.Join(root, "target-auth.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write target auth: %v", err)
	}
	symlink := filepath.Join(root, "symlink-auth.json")
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatalf("create auth symlink: %v", err)
	}
	if err := boundary.linkHostSessionAuth(symlink); err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("symlinked host auth error = %v, want symlink rejection", err)
	}
}

func TestCodexConfigBoundaryDetectsReplacedHostSessionAuth(t *testing.T) {
	root := t.TempDir()
	hostAuth := filepath.Join(root, codexAuthFileName)
	if err := os.WriteFile(hostAuth, []byte(`{"auth_mode":"chatgpt"}`), 0o600); err != nil {
		t.Fatalf("write host auth: %v", err)
	}
	boundary, err := newCodexConfigBoundary(root, true)
	if err != nil {
		t.Fatalf("new file-auth boundary: %v", err)
	}
	t.Cleanup(func() { _ = boundary.remove() })
	if err := boundary.linkHostSessionAuth(hostAuth); err != nil {
		t.Fatalf("link host auth: %v", err)
	}

	oldAuth := filepath.Join(root, "old-auth.json")
	if err := os.Rename(hostAuth, oldAuth); err != nil {
		t.Fatalf("move original host auth: %v", err)
	}
	if err := os.WriteFile(hostAuth, []byte(`{"auth_mode":"chatgpt","replacement":true}`), 0o600); err != nil {
		t.Fatalf("write replacement host auth: %v", err)
	}
	if err := boundary.validate(); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("validate after host auth replacement = %v, want identity rejection", err)
	}
}
