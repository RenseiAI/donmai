package afclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// preRenameDaemonYAML is the shape a daemon built before the rename decodes
// daemon.yaml into: it models only the `poolMaxDiskGb` spelling, and — like
// every reader on this path — decodes non-strictly, so an unrecognised
// `workareaMaxDiskGb` is dropped in silence rather than rejected.
//
// It exists so the rollback direction can be tested for real. The hazard is not
// what today's code does with the file; it is what an already-shipped binary
// does with a file today's code migrated.
type preRenameDaemonYAML struct {
	Capacity struct {
		MaxConcurrentSessions int `yaml:"maxConcurrentSessions"`
		PoolMaxDiskGb         int `yaml:"poolMaxDiskGb"`
	} `yaml:"capacity"`
}

// readAsPreRenameDaemon decodes path exactly as a pre-rename release would.
func readAsPreRenameDaemon(t *testing.T, path string) preRenameDaemonYAML {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out preRenameDaemonYAML
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode as a pre-rename daemon: %v", err)
	}
	return out
}

// TestWriteDaemonYAMLKeepsBothDiskKeysInLockStep proves a read-modify-write
// cycle emits the current `workareaMaxDiskGb` key *and* refreshes the
// deprecated `poolMaxDiskGb` alias to the same value.
//
// Writing only the current key would make the migration one-way: the file would
// then be unreadable by the release that wrote it before the rename, which
// drops the unknown key non-strictly and lands on 0 — the value this setting
// defines as "no limit". Leaving the legacy key untouched instead of refreshing
// it would be just as wrong in the other direction: a later cap change would
// leave a stale number behind for the rolled-back daemon to enforce.
func TestWriteDaemonYAMLKeepsBothDiskKeysInLockStep(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// seed is the daemon.yaml on disk before the write.
		seed string
		// set, when non-zero, is the new cap the caller applies before writing.
		set  int
		want int
	}{
		{
			name: "read-modify-write of a pre-rename file preserves the cap",
			seed: "machine:\n  id: legacy-machine\ncapacity:\n  maxConcurrentSessions: 6\n  poolMaxDiskGb: 100\n",
			want: 100,
		},
		{
			name: "changing the cap moves both spellings, leaving no stale alias",
			seed: "machine:\n  id: legacy-machine\ncapacity:\n  maxConcurrentSessions: 6\n  poolMaxDiskGb: 100\n",
			set:  250,
			want: 250,
		},
		{
			name: "an already-migrated file gains the alias back",
			seed: "machine:\n  id: legacy-machine\ncapacity:\n  maxConcurrentSessions: 6\n  workareaMaxDiskGb: 40\n",
			want: 40,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeTempYAML(t, tc.seed)
			cfg, err := ReadDaemonYAML(path)
			if err != nil {
				t.Fatalf("ReadDaemonYAML: %v", err)
			}
			if tc.set != 0 {
				cfg.Capacity.PoolMaxDiskGb = tc.set
			}
			if err := WriteDaemonYAML(path, cfg); err != nil {
				t.Fatalf("WriteDaemonYAML: %v", err)
			}

			raw, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			got := string(raw)
			wantCurrent := fmt.Sprintf("workareaMaxDiskGb: %d", tc.want)
			if !strings.Contains(got, wantCurrent) {
				t.Errorf("written file is missing %q:\n%s", wantCurrent, got)
			}
			wantLegacy := fmt.Sprintf("poolMaxDiskGb: %d", tc.want)
			if !strings.Contains(got, wantLegacy) {
				t.Errorf("written file is missing the deprecated alias %q; a daemon rolled back to a pre-%s release reads no cap at all:\n%s",
					wantLegacy, WorkareaAliasRemovalVersion, got)
			}
			// Unmodelled keys outside the capacity mapping must survive.
			if !strings.Contains(got, "legacy-machine") {
				t.Errorf("writer dropped an unmodelled key:\n%s", got)
			}

			// The current reader must still prefer the current key.
			after, err := ReadDaemonYAML(path)
			if err != nil {
				t.Fatalf("re-read: %v", err)
			}
			if after.Capacity.PoolMaxDiskGb != tc.want {
				t.Errorf("disk envelope after write = %d, want %d", after.Capacity.PoolMaxDiskGb, tc.want)
			}
		})
	}
}

// TestPreRenameDaemonStillReadsCapFromMigratedFile is the rollback proof: a
// release built before the rename, reading a file this build wrote, must still
// find the operator's disk cap.
//
// Without the alias emission it decodes 0, and 0 means "no limit" — LRU
// eviction switches off and the workarea cache fills the disk, with nothing
// logged and no error returned. This fires on ordinary version skew, not just a
// deliberate downgrade: any host still running the previous build after another
// host's CLI has rewritten a shared-shape config, or a rollback to a pre-rename
// release after an incident.
func TestPreRenameDaemonStillReadsCapFromMigratedFile(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		seed string
		set  int
		want int
	}{
		{
			name: "cap that arrived under the legacy spelling",
			seed: "machine:\n  id: m\ncapacity:\n  maxConcurrentSessions: 6\n  poolMaxDiskGb: 100\n",
			want: 100,
		},
		{
			name: "cap that arrived under the current spelling",
			seed: "machine:\n  id: m\ncapacity:\n  maxConcurrentSessions: 6\n  workareaMaxDiskGb: 100\n",
			want: 100,
		},
		{
			name: "cap changed after the file was migrated",
			seed: "machine:\n  id: m\ncapacity:\n  maxConcurrentSessions: 6\n  poolMaxDiskGb: 100\n",
			set:  250,
			want: 250,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeTempYAML(t, tc.seed)
			cfg, err := ReadDaemonYAML(path)
			if err != nil {
				t.Fatalf("ReadDaemonYAML: %v", err)
			}
			if tc.set != 0 {
				cfg.Capacity.PoolMaxDiskGb = tc.set
			}
			if err := WriteDaemonYAML(path, cfg); err != nil {
				t.Fatalf("WriteDaemonYAML: %v", err)
			}

			old := readAsPreRenameDaemon(t, path)
			if old.Capacity.PoolMaxDiskGb != tc.want {
				t.Errorf("a pre-rename daemon reads a disk envelope of %d, want %d (0 means no limit: LRU eviction is off and the disk fills silently)",
					old.Capacity.PoolMaxDiskGb, tc.want)
			}
			if old.Capacity.MaxConcurrentSessions != 6 {
				t.Errorf("a pre-rename daemon reads maxConcurrentSessions = %d, want 6; the sibling keys must survive the write",
					old.Capacity.MaxConcurrentSessions)
			}
		})
	}
}

// TestWriteDaemonYAMLEmitsNoDiskKeyWhenUncapped pins the other half: an
// uncapped config must not start writing an explicit 0 under either spelling.
// Both are `omitempty`, and 0 already means "no limit" — materialising it would
// be a behaviour change dressed as a migration.
func TestWriteDaemonYAMLEmitsNoDiskKeyWhenUncapped(t *testing.T) {
	t.Parallel()

	path := writeTempYAML(t, "machine:\n  id: uncapped-machine\n")
	if err := WriteDaemonYAML(path, &DaemonYAML{
		Capacity: CapacityConfig{MaxConcurrentSessions: 6},
	}); err != nil {
		t.Fatalf("WriteDaemonYAML: %v", err)
	}

	raw, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got := string(raw); strings.Contains(got, "MaxDiskGb") {
		t.Errorf("writer materialised a disk envelope on an uncapped config:\n%s", got)
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

// TestDaemonStatsResponseWorkareaKeyPair covers the response-field alias from
// the client side: SetWorkareaStats must populate both spellings, and
// WorkareaStats must read whichever one a given daemon emitted.
//
// The fallback is the "new binary, daemon not yet restarted" skew — a client
// built after the rename talking to a daemon built before it, which sends only
// `pool`. Reading the struct field directly instead of the accessor would
// render an empty workarea section there, silently.
func TestDaemonStatsResponseWorkareaKeyPair(t *testing.T) {
	t.Parallel()

	t.Run("SetWorkareaStats emits both keys", func(t *testing.T) {
		t.Parallel()

		var resp DaemonStatsResponse
		resp.SetWorkareaStats(&WorkareaPoolStats{TotalMembers: 3, Members: []WorkareaPoolMember{}})

		raw, err := json.Marshal(&resp)
		if err != nil {
			t.Fatalf("marshal stats: %v", err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Fatalf("decode stats keys: %v", err)
		}
		current, hasCurrent := keys["workarea"]
		legacy, hasLegacy := keys["pool"]
		if !hasCurrent {
			t.Errorf("no `workarea` key emitted; 011's alias table names it the current spelling:\n%s", raw)
		}
		if !hasLegacy {
			t.Errorf("no `pool` key emitted; a client pinned before %s renders no workarea section:\n%s",
				WorkareaAliasRemovalVersion, raw)
		}
		if hasCurrent && hasLegacy && string(current) != string(legacy) {
			t.Errorf("the two spellings disagree:\n workarea = %s\n     pool = %s", current, legacy)
		}
	})

	t.Run("WorkareaStats reads whichever key is present", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			name string
			body string
			want int
		}{
			{
				name: "pre-rename daemon emits only the alias",
				body: `{"pool":{"totalMembers":7,"members":[]}}`,
				want: 7,
			},
			{
				name: "current daemon emits both",
				body: `{"workarea":{"totalMembers":7,"members":[]},"pool":{"totalMembers":7,"members":[]}}`,
				want: 7,
			},
			{
				name: "post-removal daemon emits only the current key",
				body: `{"workarea":{"totalMembers":7,"members":[]}}`,
				want: 7,
			},
			{
				name: "section not selected",
				body: `{}`,
				want: -1,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				var resp DaemonStatsResponse
				if err := json.Unmarshal([]byte(tc.body), &resp); err != nil {
					t.Fatalf("decode stats: %v", err)
				}
				got := resp.WorkareaStats()
				if tc.want < 0 {
					if got != nil {
						t.Errorf("WorkareaStats() = %+v, want nil", got)
					}
					return
				}
				if got == nil {
					t.Fatalf("WorkareaStats() = nil, want a section with %d members", tc.want)
				}
				if got.TotalMembers != tc.want {
					t.Errorf("WorkareaStats().TotalMembers = %d, want %d", got.TotalMembers, tc.want)
				}
			})
		}
	})
}
