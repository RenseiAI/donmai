package afcli

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
)

// The `pool` noun named four different things; the daemon's warm-workarea
// surface moved to the `workarea` noun. These tests exercise the CLI aliases
// that keep an operator's existing invocation working, not the new spellings.

// runHostCmdSplit runs the host command tree with stdout and stderr captured
// separately. The shared newTestHostCmd helper merges them into one buffer,
// which cannot distinguish "the notice was printed" from "the notice
// contaminated the JSON on stdout" — and keeping `--json | jq` parseable is the
// whole reason the notice goes to stderr.
func runHostCmdSplit(t *testing.T, mock daemonDoer, args []string) (stdout, stderr string, err error) {
	t.Helper()
	factory := func(_ afclient.DaemonConfig) daemonDoer { return mock }
	ds := func() afclient.DataSource { return afclient.NewMockClient() }
	cmd := newHostCmdWithFactory(factory, ds, Config{HostBinaryVersion: "test"})
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

// TestStatsPoolFlagAliasStillParses proves `host stats --pool` still selects
// the workarea section.
//
// Without the alias cobra rejects the unknown flag outright, so the failure
// mode is a hard parse error on an invocation that shipped for many releases.
func TestStatsPoolFlagAliasStillParses(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{statsResp: fixtureStatsResp()}
	stdout, stderr, err := runHostCmdSplit(t, mock, []string{"stats", "--pool"})
	if err != nil {
		t.Fatalf("execute `stats --pool`: %v (stderr: %s)", err, stderr)
	}
	if !mock.statsWithPool {
		t.Error("--pool did not select the workarea section; the flag alias is not bound to the same variable")
	}
	if !strings.Contains(stderr, "--workarea") {
		t.Errorf("stderr = %q, want it to name the --workarea replacement", stderr)
	}
	if !strings.Contains(stderr, afclient.WorkareaAliasRemovalVersion) {
		t.Errorf("stderr = %q, want it to declare removal version %q",
			stderr, afclient.WorkareaAliasRemovalVersion)
	}
	if strings.Contains(stdout, "deprecated") {
		t.Errorf("deprecation notice leaked onto stdout: %q", stdout)
	}
}

// TestStatsWorkareaFlagIsCurrent pins the other half: the current flag selects
// the same section and announces nothing.
func TestStatsWorkareaFlagIsCurrent(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{statsResp: fixtureStatsResp()}
	_, stderr, err := runHostCmdSplit(t, mock, []string{"stats", "--workarea"})
	if err != nil {
		t.Fatalf("execute `stats --workarea`: %v", err)
	}
	if !mock.statsWithPool {
		t.Error("--workarea did not select the workarea section")
	}
	if strings.Contains(stderr, "deprecated") {
		t.Errorf("the current flag announced itself as deprecated: %q", stderr)
	}
}

// TestSetLegacyCapacityKeyAliasStillAccepted proves
// `host set capacity.poolMaxDiskGb <n>` is still accepted, is translated to the
// current key before it reaches the daemon, and persists to the same field.
func TestSetLegacyCapacityKeyAliasStillAccepted(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{
		setCapResp: &afclient.SetCapacityResponse{
			OK:      true,
			Key:     afclient.WorkareaMaxDiskGbKey,
			Value:   "64",
			Message: "updated",
		},
	}
	yamlPath := t.TempDir() + "/daemon.yaml"
	stdout, stderr, err := runHostCmdSplit(t, mock, []string{
		"set", afclient.LegacyWorkareaMaxDiskGbKey, "64",
		"--config", yamlPath,
	})
	if err != nil {
		t.Fatalf("execute `set %s`: %v (stderr: %s)", afclient.LegacyWorkareaMaxDiskGbKey, err, stderr)
	}
	if mock.setCapKey != afclient.WorkareaMaxDiskGbKey {
		t.Errorf("daemon received key %q, want it canonicalized to %q",
			mock.setCapKey, afclient.WorkareaMaxDiskGbKey)
	}
	if mock.setCapValue != "64" {
		t.Errorf("daemon received value %q, want %q", mock.setCapValue, "64")
	}
	if !strings.Contains(stderr, afclient.WorkareaMaxDiskGbKey) {
		t.Errorf("stderr = %q, want it to name the replacement key", stderr)
	}
	if !strings.Contains(stderr, afclient.WorkareaAliasRemovalVersion) {
		t.Errorf("stderr = %q, want it to declare removal version %q",
			stderr, afclient.WorkareaAliasRemovalVersion)
	}
	if strings.Contains(stdout, "deprecated") {
		t.Errorf("deprecation notice leaked onto stdout: %q", stdout)
	}

	// The alias must persist to the same on-disk field the current key writes,
	// not merely be accepted and dropped.
	cfg, readErr := afclient.ReadDaemonYAML(yamlPath)
	if readErr != nil {
		t.Fatalf("read written config: %v", readErr)
	}
	if cfg.Capacity.PoolMaxDiskGb != 64 {
		t.Errorf("written disk envelope = %d, want 64", cfg.Capacity.PoolMaxDiskGb)
	}
}

// TestSetLegacyCapacityKeyKeepsJSONParseable proves the deprecation notice does
// not break `--json | jq` for a caller still using the alias.
func TestSetLegacyCapacityKeyKeepsJSONParseable(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{
		setCapResp: &afclient.SetCapacityResponse{
			OK: true, Key: afclient.WorkareaMaxDiskGbKey, Value: "8", Message: "updated",
		},
	}
	stdout, stderr, err := runHostCmdSplit(t, mock, []string{
		"set", afclient.LegacyWorkareaMaxDiskGbKey, "8",
		"--config", t.TempDir() + "/daemon.yaml",
		"--json",
	})
	if err != nil {
		t.Fatalf("execute: %v (stderr: %s)", err, stderr)
	}
	var resp afclient.SetCapacityResponse
	if unmarshalErr := json.Unmarshal([]byte(stdout), &resp); unmarshalErr != nil {
		t.Fatalf("stdout is not valid JSON while the alias is in use: %v\nstdout: %s", unmarshalErr, stdout)
	}
	if !resp.OK {
		t.Error("OK = false, want true")
	}
	if stderr == "" {
		t.Error("no deprecation notice was emitted for the alias")
	}
}

// TestWorkareaAliasRemovalVersionIsConcrete enforces the discipline itself: an
// alias whose removal version is "the next release" is unfalsifiable, and that
// is how the previous alias generation outlived its stated window.
func TestWorkareaAliasRemovalVersionIsConcrete(t *testing.T) {
	t.Parallel()

	semver := regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	if !semver.MatchString(afclient.WorkareaAliasRemovalVersion) {
		t.Errorf("WorkareaAliasRemovalVersion = %q, want a concrete vX.Y.Z",
			afclient.WorkareaAliasRemovalVersion)
	}
	notice := afclient.DeprecatedSurfaceNotice("--pool", "--workarea")
	for _, want := range []string{"--pool", "--workarea", afclient.WorkareaAliasRemovalVersion} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice %q missing %q", notice, want)
		}
	}
}

// TestStatsRendersWorkareaSectionFromEitherResponseKey covers the response-field
// alias at the surface that consumes it: the renderer.
//
// `DaemonStatsResponse` carries the workarea-cache snapshot under the current
// `workarea` key and the deprecated `pool` key for the life of the alias, and a
// given daemon may send either or both depending on its build. Both fields are
// `omitempty`, so a renderer that reads one struct field directly prints
// nothing at all against a daemon that populated the other — the section simply
// vanishes, with a zero exit status and no diagnostic. Reading through
// DaemonStatsResponse.WorkareaStats is what makes the invocation survive the
// skew in both directions.
func TestStatsRendersWorkareaSectionFromEitherResponseKey(t *testing.T) {
	t.Parallel()

	snapshot := func() *afclient.WorkareaPoolStats {
		return &afclient.WorkareaPoolStats{
			TotalMembers:     5,
			ReadyMembers:     3,
			AcquiredMembers:  2,
			TotalDiskUsageMb: 1024,
		}
	}

	cases := []struct {
		name string
		// shape mutates the canned response into the wire shape a particular
		// daemon build emits.
		shape       func(*afclient.DaemonStatsResponse)
		wantSection bool
	}{
		{
			name: "daemon built before the rename sends only `pool`",
			shape: func(r *afclient.DaemonStatsResponse) {
				r.Pool = snapshot()
			},
			wantSection: true,
		},
		{
			name: "daemon in the alias window sends both keys",
			shape: func(r *afclient.DaemonStatsResponse) {
				r.SetWorkareaStats(snapshot())
			},
			wantSection: true,
		},
		{
			name: "daemon after the alias is removed sends only `workarea`",
			shape: func(r *afclient.DaemonStatsResponse) {
				r.Workarea = snapshot()
			},
			wantSection: true,
		},
		{
			name:        "section not requested",
			shape:       func(*afclient.DaemonStatsResponse) {},
			wantSection: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp := fixtureStatsResp()
			tc.shape(resp)
			mock := &mockDaemon{statsResp: resp}
			stdout, _, err := runHostCmdSplit(t, mock, []string{"stats", "--workarea"})
			if err != nil {
				t.Fatalf("execute `stats --workarea`: %v", err)
			}
			if !tc.wantSection {
				if strings.Contains(stdout, "Workarea pool:") {
					t.Errorf("rendered a workarea section from an empty response:\n%s", stdout)
				}
				return
			}
			if !strings.Contains(stdout, "Workarea pool:") {
				t.Errorf("no workarea section rendered; the section silently vanishes against this daemon:\n%s", stdout)
			}
			if !strings.Contains(stdout, "1024") {
				t.Errorf("workarea section is missing the disk usage from the response:\n%s", stdout)
			}
		})
	}
}
