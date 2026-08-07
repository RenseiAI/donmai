package afclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// daemon.yaml's workarea disk envelope was renamed from `poolMaxDiskGb` to
// `workareaMaxDiskGb`. These tests cover the alias that keeps an operator's
// existing file working, on both the read and the write side.

func writeTempYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestReadDaemonYAMLAcceptsLegacyDiskKey proves an unmigrated file still yields
// its disk envelope.
//
// The decoder is non-strict: without the alias the key is dropped in silence
// and the field defaults to 0, which this setting defines as "no limit". The
// visible symptom would be LRU eviction quietly switching off and the disk
// filling, with no error anywhere.
func TestReadDaemonYAMLAcceptsLegacyDiskKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want int
	}{
		{
			name: "legacy key alone",
			body: "capacity:\n  maxConcurrentSessions: 6\n  poolMaxDiskGb: 100\n",
			want: 100,
		},
		{
			name: "current key alone",
			body: "capacity:\n  maxConcurrentSessions: 6\n  workareaMaxDiskGb: 250\n",
			want: 250,
		},
		{
			name: "both present, current key wins",
			body: "capacity:\n  maxConcurrentSessions: 6\n  poolMaxDiskGb: 100\n  workareaMaxDiskGb: 250\n",
			want: 250,
		},
		{
			name: "neither present",
			body: "capacity:\n  maxConcurrentSessions: 6\n",
			want: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := ReadDaemonYAML(writeTempYAML(t, tc.body))
			if err != nil {
				t.Fatalf("ReadDaemonYAML: %v", err)
			}
			if cfg.Capacity.PoolMaxDiskGb != tc.want {
				t.Errorf("disk envelope = %d, want %d", cfg.Capacity.PoolMaxDiskGb, tc.want)
			}
			if cfg.Capacity.MaxConcurrentSessions != 6 {
				t.Errorf("maxConcurrentSessions = %d, want 6; sibling keys must survive the alias path",
					cfg.Capacity.MaxConcurrentSessions)
			}
		})
	}
}

// TestWriteDaemonYAMLMigratesLegacyDiskKey proves a read-modify-write cycle
// rewrites the legacy key rather than leaving two spellings that can disagree.
func TestWriteDaemonYAMLMigratesLegacyDiskKey(t *testing.T) {
	t.Parallel()

	path := writeTempYAML(t,
		"machine:\n  id: legacy-machine\ncapacity:\n  maxConcurrentSessions: 6\n  poolMaxDiskGb: 100\n")

	cfg, err := ReadDaemonYAML(path)
	if err != nil {
		t.Fatalf("ReadDaemonYAML: %v", err)
	}
	if err := WriteDaemonYAML(path, cfg); err != nil {
		t.Fatalf("WriteDaemonYAML: %v", err)
	}

	raw, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "workareaMaxDiskGb: 100") {
		t.Errorf("written file did not carry the current key with the preserved value:\n%s", got)
	}
	if strings.Contains(got, "poolMaxDiskGb") {
		t.Errorf("written file still carries the legacy key; a half-migrated file can disagree with itself:\n%s", got)
	}
	// Unmodelled keys outside the capacity mapping must survive the write.
	if !strings.Contains(got, "legacy-machine") {
		t.Errorf("writer dropped an unmodelled key:\n%s", got)
	}
	// And the migrated value must round-trip.
	after, err := ReadDaemonYAML(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if after.Capacity.PoolMaxDiskGb != 100 {
		t.Errorf("disk envelope after migration = %d, want 100", after.Capacity.PoolMaxDiskGb)
	}
}

// TestWriteDaemonYAMLKeepsLegacyDiskKeyWhenNothingReplacesIt guards the
// migration's precondition: the legacy key is only removed once its value has
// actually been re-emitted under the current name. Deleting it unconditionally
// would discard an operator's disk cap on any write that does not carry one.
func TestWriteDaemonYAMLKeepsLegacyDiskKeyWhenNothingReplacesIt(t *testing.T) {
	t.Parallel()

	path := writeTempYAML(t,
		"machine:\n  id: legacy-machine\ncapacity:\n  maxConcurrentSessions: 6\n  poolMaxDiskGb: 100\n")

	// A caller that models no disk envelope at all — the value must not be
	// silently dropped from the file.
	if err := WriteDaemonYAML(path, &DaemonYAML{
		Capacity: CapacityConfig{MaxConcurrentSessions: 9},
	}); err != nil {
		t.Fatalf("WriteDaemonYAML: %v", err)
	}

	after, err := ReadDaemonYAML(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if after.Capacity.PoolMaxDiskGb != 100 {
		t.Errorf("disk envelope = %d, want 100 preserved", after.Capacity.PoolMaxDiskGb)
	}
	if after.Capacity.MaxConcurrentSessions != 9 {
		t.Errorf("maxConcurrentSessions = %d, want 9", after.Capacity.MaxConcurrentSessions)
	}
}
