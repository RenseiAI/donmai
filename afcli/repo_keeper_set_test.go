package afcli

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
)

// TestHostSetRepoKeeperKeys drives the production `host set` entry point for
// each repoKeeper.* key and proves the value persists to daemon.yaml with a
// drain-aware-restart notice.
func TestHostSetRepoKeeperKeys(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ key, value string }{
		{"repoKeeper.enabled", "1"},
		{"repoKeeper.maxDiskGb", "5"},
		{"repoKeeper.fetchIntervalSeconds", "45"},
		{"repoKeeper.authorizationWindowSeconds", "120"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()
			mock := &mockDaemon{setCapResp: &afclient.SetCapacityResponse{OK: true, Key: tc.key, Value: tc.value, Message: "persisted"}}
			tmpDir := t.TempDir()
			buf, err := newTestHostCmd(mock, []string{"set", tc.key, tc.value, "--config", tmpDir + "/daemon.yaml"})
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if !strings.Contains(buf.String(), "restart") {
				t.Errorf("output %q does not say a restart is needed", buf.String())
			}
			cfg, readErr := afclient.ReadDaemonYAML(tmpDir + "/daemon.yaml")
			if readErr != nil {
				t.Fatalf("ReadDaemonYAML: %v", readErr)
			}
			switch tc.key {
			case "repoKeeper.enabled":
				if !cfg.RepoKeeper.Enabled {
					t.Errorf("repoKeeper.enabled not persisted")
				}
			case "repoKeeper.maxDiskGb":
				if cfg.RepoKeeper.MaxDiskGb != 5 {
					t.Errorf("maxDiskGb = %d, want 5", cfg.RepoKeeper.MaxDiskGb)
				}
			case "repoKeeper.fetchIntervalSeconds":
				if cfg.RepoKeeper.FetchIntervalSeconds != 45 {
					t.Errorf("fetchIntervalSeconds = %d, want 45", cfg.RepoKeeper.FetchIntervalSeconds)
				}
			case "repoKeeper.authorizationWindowSeconds":
				if cfg.RepoKeeper.AuthorizationWindowSeconds != 120 {
					t.Errorf("authorizationWindowSeconds = %d, want 120", cfg.RepoKeeper.AuthorizationWindowSeconds)
				}
			}
		})
	}
}

// TestApplyRepoKeeperSet proves the in-memory mapping behind `host set`: each
// repoKeeper.* key lands on its own field and nowhere else.
func TestApplyRepoKeeperSet(t *testing.T) {
	t.Parallel()

	cfg := &afclient.DaemonYAML{}
	applyRepoKeeperSet(cfg, "repoKeeper.enabled", 1)
	if !cfg.RepoKeeper.Enabled {
		t.Error("enabled not applied")
	}
	if cfg.Capacity.MaxConcurrentSessions != 0 {
		t.Errorf("enabled leaked into capacity: %+v", cfg.Capacity)
	}
	applyRepoKeeperSet(cfg, "repoKeeper.maxDiskGb", 5)
	if cfg.RepoKeeper.MaxDiskGb != 5 {
		t.Errorf("maxDiskGb = %d, want 5", cfg.RepoKeeper.MaxDiskGb)
	}
	applyRepoKeeperSet(cfg, "repoKeeper.fetchIntervalSeconds", 45)
	if cfg.RepoKeeper.FetchIntervalSeconds != 45 {
		t.Errorf("fetchIntervalSeconds = %d, want 45", cfg.RepoKeeper.FetchIntervalSeconds)
	}
	applyRepoKeeperSet(cfg, "repoKeeper.authorizationWindowSeconds", 120)
	if cfg.RepoKeeper.AuthorizationWindowSeconds != 120 {
		t.Errorf("authorizationWindowSeconds = %d, want 120", cfg.RepoKeeper.AuthorizationWindowSeconds)
	}
}

// TestHostSetRepoKeeperEnabledRejectsOutOfRange proves `host set
// repoKeeper.enabled 2` fails loudly instead of persisting a meaningless value.
func TestHostSetRepoKeeperEnabledRejectsOutOfRange(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{}
	if _, err := newTestHostCmd(mock, []string{"set", "repoKeeper.enabled", "2"}); err == nil {
		t.Fatal("expected error for repoKeeper.enabled=2")
	}
	if _, err := newTestHostCmd(mock, []string{"set", "repoKeeper.maxDiskGb", "-1"}); err == nil {
		t.Fatal("expected error for negative repoKeeper.maxDiskGb")
	}
}

// TestFormatRepoKeeperStat proves the stats renderer carries counts and bytes
// with no scope values or URLs.
func TestFormatRepoKeeperStat(t *testing.T) {
	t.Parallel()

	got := formatRepoKeeperStat(fixtureStatsResp())
	if got != "disabled" {
		t.Errorf("nil keeper renders %q, want disabled", got)
	}
	resp := fixtureStatsResp()
	resp.RepoKeeper = &afclient.RepoKeeperStats{MirrorCount: 3, LastFetchAt: "2026-10-08T00:00:00Z", RevokedCount: 1, Bytes: 42}
	got = formatRepoKeeperStat(resp)
	for _, want := range []string{"3", "1", "42"} {
		if !strings.Contains(got, want) {
			t.Errorf("render %q missing %q", got, want)
		}
	}
}
