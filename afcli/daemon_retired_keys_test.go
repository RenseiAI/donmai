package afcli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
)

// seedRetiredKeyDaemonConfig writes a daemon config carrying the retired
// key on two entries and returns its path.
func seedRetiredKeyDaemonConfig(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := `machine:
  id: cli-fixture
orchestrator:
  url: https://platform.example.com
projects:
  - id: one
    repository: github.com/foo/one
    cloneStrategy: shallow
  - id: two
    repository: github.com/foo/two
    cloneStrategy: full
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestDaemonUpdateStripsRetiredKeys proves the upgrade path strips the
// retired key from the config, preserves mode and every other field, and
// still performs the daemon action.
func TestDaemonUpdateStripsRetiredKeys(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := seedRetiredKeyDaemonConfig(t, dir, "daemon.yaml")

	mock := &mockDaemon{actionResp: fixtureActionResp()}
	factory := func(_ afclient.DaemonConfig) daemonDoer { return mock }
	ds := func() afclient.DataSource { return afclient.NewMockClient() }
	cmd := newHostCmdWithFactory(factory, ds, Config{HostBinaryVersion: "test"})
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"update", "--config", path})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("update: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "retired-key sweep:") {
		t.Errorf("update should print one sweep summary line; got:\n%s", out)
	}
	if strings.Count(out, "retired-key sweep:") != 1 {
		t.Errorf("update should print exactly one sweep summary line; got:\n%s", out)
	}
	if !strings.Contains(out, "update") {
		t.Errorf("update should still perform the daemon action; got:\n%s", out)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "cloneStrategy") {
		t.Errorf("update left the retired key behind:\n%s", raw)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600 (preserved)", info.Mode().Perm())
	}
	for _, want := range []string{"id: cli-fixture", "url: https://platform.example.com", "github.com/foo/one", "github.com/foo/two"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("update lost %q:\n%s", want, raw)
		}
	}
	// Idempotent: a second run strips nothing and writes no backup.
	cmd2 := newHostCmdWithFactory(factory, ds, Config{HostBinaryVersion: "test"})
	buf2 := &bytes.Buffer{}
	cmd2.SetOut(buf2)
	cmd2.SetErr(buf2)
	cmd2.SetArgs([]string{"update", "--config", path})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("second update: %v", err)
	}
	if !strings.Contains(buf2.String(), "no retired keys") {
		t.Errorf("second update should report nothing to do; got:\n%s", buf2.String())
	}
	matches, _ := filepath.Glob(path + ".sweep-backup-*")
	if len(matches) != 1 {
		t.Errorf("backups = %v; want exactly 1 (second sweep is a no-op)", matches)
	}
}

// TestDaemonDoctorFixStripsRetiredKeys proves `doctor --fix` strips the
// retired key and prints one summary line, even though the service itself
// is not installed (the sweep runs before the health verdict).
func TestDaemonDoctorFixStripsRetiredKeys(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	path := seedRetiredKeyDaemonConfig(t, tmp, "daemon.yaml")

	cmd := newDaemonDoctorCmd("donmai")
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--fix", "--config", path})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected the not-installed error; --fix must not mask the health verdict")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("expected not-installed error, got: %v", err)
	}
	out := buf.String()
	if strings.Count(out, "retired-key sweep:") != 1 {
		t.Errorf("doctor --fix should print exactly one sweep summary line; got:\n%s", out)
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(raw), "cloneStrategy") {
		t.Errorf("doctor --fix left the retired key behind:\n%s", raw)
	}
}

// TestDaemonDoctorWithoutFixLeavesRetiredKeys proves doctor without --fix
// never touches the config file.
func TestDaemonDoctorWithoutFixLeavesRetiredKeys(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	path := seedRetiredKeyDaemonConfig(t, tmp, "daemon.yaml")

	cmd := newDaemonDoctorCmd("donmai")
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(nil)
	_ = cmd.Execute()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "cloneStrategy") {
		t.Errorf("doctor without --fix must not touch the config:\n%s", raw)
	}
	if strings.Contains(buf.String(), "retired-key sweep:") {
		t.Errorf("doctor without --fix must not print a sweep line; got:\n%s", buf.String())
	}
}

// TestDaemonDoctorFixJSONMergesSweep proves --fix --json keeps valid JSON
// with the report fields at top level plus the sweep outcome.
func TestDaemonDoctorFixJSONMergesSweep(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	path := seedRetiredKeyDaemonConfig(t, tmp, "daemon.yaml")

	cmd := newDaemonDoctorCmd("donmai")
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--fix", "--json", "--config", path})
	_ = cmd.Execute()

	var report map[string]any
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("expected --fix --json to emit valid JSON; got: %v\noutput:\n%s", err, buf.String())
	}
	if _, ok := report["OS"]; !ok {
		t.Errorf("expected JSON to keep the OS field; got: %v", report)
	}
	sweep, ok := report["retiredKeySweep"].(map[string]any)
	if !ok {
		t.Fatalf("expected JSON to include retiredKeySweep; got: %v", report)
	}
	if sweep["keysRemoved"] != float64(2) {
		t.Errorf("keysRemoved = %v, want 2", sweep["keysRemoved"])
	}
}
