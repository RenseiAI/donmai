package afclient

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRepoKeeperYAML_RoundTrip proves the repoKeeper.* settings travel from
// daemon.yaml through ReadDaemonYAML into the struct and back through
// WriteDaemonYAML with no loss.
func TestRepoKeeperYAML_RoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	original := "machine:\n    id: keeper-test\n" +
		"capacity:\n    maxConcurrentSessions: 2\n" +
		"repoKeeper:\n    enabled: true\n    maxDiskGb: 5\n    fetchIntervalSeconds: 45\n    authorizationWindowSeconds: 120\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, err := ReadDaemonYAML(path)
	if err != nil {
		t.Fatalf("ReadDaemonYAML: %v", err)
	}
	if !cfg.RepoKeeper.Enabled || cfg.RepoKeeper.MaxDiskGb != 5 ||
		cfg.RepoKeeper.FetchIntervalSeconds != 45 || cfg.RepoKeeper.AuthorizationWindowSeconds != 120 {
		t.Fatalf("repoKeeper = %+v, want the seeded values", cfg.RepoKeeper)
	}
	if err := WriteDaemonYAML(path, cfg); err != nil {
		t.Fatalf("WriteDaemonYAML: %v", err)
	}
	reloaded, err := ReadDaemonYAML(path)
	if err != nil {
		t.Fatalf("ReadDaemonYAML(reload): %v", err)
	}
	if reloaded.RepoKeeper != cfg.RepoKeeper {
		t.Errorf("reload repoKeeper = %+v, want %+v", reloaded.RepoKeeper, cfg.RepoKeeper)
	}
}

// TestWriteDaemonYAMLWithCapacity_RepoKeeperKeys drives the production
// WriteDaemonYAMLWithCapacity entry point for each repoKeeper.* key and
// proves the value lands in the repoKeeper block without disturbing the
// capacity block.
func TestWriteDaemonYAMLWithCapacity_RepoKeeperKeys(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		key   string
		value int
	}{
		{"repoKeeper.enabled", 1},
		{"repoKeeper.maxDiskGb", 5},
		{"repoKeeper.fetchIntervalSeconds", 45},
		{"repoKeeper.authorizationWindowSeconds", 120},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "daemon.yaml")
			if err := os.WriteFile(path, []byte("capacity:\n    maxConcurrentSessions: 2\n"), 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}
			cfg, err := ReadDaemonYAML(path)
			if err != nil {
				t.Fatalf("ReadDaemonYAML: %v", err)
			}
			if err := WriteDaemonYAMLWithCapacity(path, cfg, tc.key, tc.value); err != nil {
				t.Fatalf("WriteDaemonYAMLWithCapacity: %v", err)
			}
			reloaded, err := ReadDaemonYAML(path)
			if err != nil {
				t.Fatalf("ReadDaemonYAML(reload): %v", err)
			}
			if reloaded.Capacity.MaxConcurrentSessions != 2 {
				t.Errorf("capacity.maxConcurrentSessions = %d, want 2 (undisturbed)", reloaded.Capacity.MaxConcurrentSessions)
			}
			var got int64
			var enabled bool
			switch tc.key {
			case "repoKeeper.enabled":
				enabled = reloaded.RepoKeeper.Enabled
			case "repoKeeper.maxDiskGb":
				got = reloaded.RepoKeeper.MaxDiskGb
			case "repoKeeper.fetchIntervalSeconds":
				got = reloaded.RepoKeeper.FetchIntervalSeconds
			case "repoKeeper.authorizationWindowSeconds":
				got = reloaded.RepoKeeper.AuthorizationWindowSeconds
			}
			if tc.key == "repoKeeper.enabled" {
				if !enabled {
					t.Errorf("repoKeeper.enabled not set by %s=%d", tc.key, tc.value)
				}
				return
			}
			if got != int64(tc.value) {
				t.Errorf("%s = %d, want %d", tc.key, got, tc.value)
			}
		})
	}
}
