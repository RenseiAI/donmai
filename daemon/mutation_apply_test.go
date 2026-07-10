package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// newTestDaemonWithProjects builds a minimal Daemon backed by a real
// daemon.yaml in a temp dir so applyOneMutation's WriteConfig path
// exercises the atomic rename. No registration, no heartbeat, no spawner
// — those are not required for the unit-level applier tests.
func newTestDaemonWithProjects(t *testing.T, initial []ProjectConfig) (*Daemon, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "daemon.yaml")

	cfg := &Config{
		APIVersion: "v1",
		Kind:       "DaemonConfig",
		Machine:    MachineConfig{ID: "test-machine"},
		Capacity:   CapacityConfig{MaxConcurrentSessions: 1},
		Orchestrator: OrchestratorConfig{
			URL:       "https://example.test",
			AuthToken: "stub",
		},
		Projects: initial,
	}
	if err := WriteConfig(cfgPath, cfg); err != nil {
		t.Fatalf("seed yaml: %v", err)
	}

	d := &Daemon{
		opts:   Options{ConfigPath: cfgPath},
		config: cfg,
	}
	return d, cfgPath
}

// readYamlProjects reads the persisted daemon.yaml and returns its
// projects[] slice. Used to verify the atomic write actually happened.
func readYamlProjects(t *testing.T, path string) []ProjectConfig {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read yaml: %v", err)
	}
	var parsed Config
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal yaml: %v", err)
	}
	return parsed.Projects
}

func readYamlConfig(t *testing.T, path string) Config {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read yaml: %v", err)
	}
	var parsed Config
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal yaml: %v", err)
	}
	return parsed
}

func mustParams(t *testing.T, v any) json.RawMessage {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return out
}

func TestApplyPendingMutations_ProjectAdd(t *testing.T) {
	t.Parallel()
	d, path := newTestDaemonWithProjects(t, nil)

	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{
			ID:     "dmut_add_alpha",
			Op:     "project.add",
			Params: mustParams(t, map[string]string{"id": "alpha", "repository": "github.com/x/alpha"}),
		},
	})

	if len(applied) != 1 || applied[0] != "dmut_add_alpha" {
		t.Fatalf("applied = %v, want [dmut_add_alpha]", applied)
	}
	if len(failures) != 0 {
		t.Fatalf("failures = %v, want none", failures)
	}

	got := readYamlProjects(t, path)
	if len(got) != 1 || got[0].ID != "alpha" || got[0].Repository != "github.com/x/alpha" {
		t.Errorf("yaml after add = %+v, want [{alpha, github.com/x/alpha}]", got)
	}
}

func TestApplyPendingMutations_ProjectAdd_Idempotent(t *testing.T) {
	t.Parallel()
	d, _ := newTestDaemonWithProjects(t, []ProjectConfig{
		{ID: "alpha", Repository: "github.com/x/alpha"},
	})

	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{
			ID:     "dmut_dup",
			Op:     "project.add",
			Params: mustParams(t, map[string]string{"id": "alpha", "repository": "github.com/x/alpha"}),
		},
	})
	if len(applied) != 1 {
		t.Fatalf("applied = %v, want [dmut_dup] (idempotent success)", applied)
	}
	if len(failures) != 0 {
		t.Fatalf("failures = %v, want none on idempotent add", failures)
	}
}

func TestApplyPendingMutations_ProjectRemove(t *testing.T) {
	t.Parallel()
	d, path := newTestDaemonWithProjects(t, []ProjectConfig{
		{ID: "alpha", Repository: "github.com/x/alpha"},
		{ID: "beta", Repository: "github.com/x/beta"},
	})

	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{
			ID:     "dmut_remove_alpha",
			Op:     "project.remove",
			Params: mustParams(t, map[string]string{"id": "alpha"}),
		},
	})
	if len(applied) != 1 || len(failures) != 0 {
		t.Fatalf("applied/failures = %v/%v, want [dmut_remove_alpha]/[]", applied, failures)
	}

	got := readYamlProjects(t, path)
	if len(got) != 1 || got[0].ID != "beta" {
		t.Errorf("yaml after remove = %+v, want [{beta,...}]", got)
	}
}

func TestApplyPendingMutations_ProjectRemove_NotPresent(t *testing.T) {
	t.Parallel()
	d, _ := newTestDaemonWithProjects(t, []ProjectConfig{
		{ID: "alpha", Repository: "github.com/x/alpha"},
	})

	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{
			ID:     "dmut_remove_missing",
			Op:     "project.remove",
			Params: mustParams(t, map[string]string{"id": "ghost"}),
		},
	})
	if len(applied) != 1 || applied[0] != "dmut_remove_missing" {
		t.Fatalf("applied = %v, want [dmut_remove_missing] (idempotent)", applied)
	}
	if len(failures) != 0 {
		t.Fatalf("failures = %v, want none", failures)
	}
}

func TestApplyPendingMutations_ProjectEnableDisable(t *testing.T) {
	t.Parallel()
	d, path := newTestDaemonWithProjects(t, []ProjectConfig{
		{ID: "alpha", Repository: "example.com/acme/one"},
		{ID: "alpha", Repository: "example.com/acme/two"},
	})
	d.config.EnabledProjectIDs = []string{}

	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "enable", Op: "project.enable", Params: mustParams(t, map[string]string{"id": "alpha"})},
		{ID: "disable", Op: "project.disable", Params: mustParams(t, map[string]string{"id": "alpha"})},
	})
	if len(applied) != 2 || len(failures) != 0 {
		t.Fatalf("applied/failures = %v/%v, want two successes", applied, failures)
	}
	got := readYamlConfig(t, path)
	if got.ProjectAdmissionVersion != ProjectAdmissionVersionV2 {
		t.Errorf("ProjectAdmissionVersion = %d, want 2", got.ProjectAdmissionVersion)
	}
	if len(got.EnabledProjectIDs) != 0 {
		t.Errorf("EnabledProjectIDs = %v, want empty", got.EnabledProjectIDs)
	}
	if len(got.Repositories) != 2 {
		t.Errorf("Repositories = %+v, disable must retain repository resources", got.Repositories)
	}
	if len(got.Projects) != 0 {
		t.Errorf("Projects = %+v, disabled projects must be omitted from the legacy projection", got.Projects)
	}
}

func TestApplyPendingMutations_FirstV2MutationBacksUpLegacyConfigOnce(t *testing.T) {
	t.Parallel()
	d, path := newTestDaemonWithProjects(t, []ProjectConfig{{ID: "alpha", Repository: "example.com/acme/alpha"}})
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read original config: %v", err)
	}

	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "disable", Op: "project.disable", Params: mustParams(t, map[string]string{"id": "alpha"})},
		{ID: "enable", Op: "project.enable", Params: mustParams(t, map[string]string{"id": "alpha"})},
	})
	if len(applied) != 2 || len(failures) != 0 {
		t.Fatalf("applied/failures = %v/%v, want two successes", applied, failures)
	}
	backups, err := filepath.Glob(path + ".v1-backup-*")
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one", backups)
	}
	backup, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backup) != string(original) {
		t.Fatalf("backup differs from original legacy config\nbackup:\n%s\noriginal:\n%s", backup, original)
	}
	info, err := os.Stat(backups[0])
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("backup mode = %o, want 600", got)
	}
}

func TestApplyPendingMutations_LegacyProjectAddSupportsMultipleRepositories(t *testing.T) {
	t.Parallel()
	d, path := newTestDaemonWithProjects(t, []ProjectConfig{{ID: "alpha", Repository: "example.com/acme/one"}})

	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{{
		ID:     "add-second",
		Op:     "project.add",
		Params: mustParams(t, map[string]string{"id": "alpha", "repository": "example.com/acme/two"}),
	}})
	if len(applied) != 1 || len(failures) != 0 {
		t.Fatalf("applied/failures = %v/%v", applied, failures)
	}
	got := readYamlConfig(t, path)
	if len(got.Projects) != 2 {
		t.Fatalf("Projects = %+v, want two repository resources", got.Projects)
	}
	if len(got.Repositories) != 2 {
		t.Fatalf("Repositories = %+v, want two normalized repository resources", got.Repositories)
	}
	if len(got.EnabledProjectIDs) != 1 || got.EnabledProjectIDs[0] != "alpha" {
		t.Fatalf("EnabledProjectIDs = %v, want [alpha]", got.EnabledProjectIDs)
	}
}

func TestApplyPendingMutations_BadOp(t *testing.T) {
	t.Parallel()
	d, _ := newTestDaemonWithProjects(t, nil)

	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "dmut_bad", Op: "project.rename", Params: json.RawMessage(`{}`)},
	})
	if len(applied) != 0 {
		t.Errorf("applied = %v, want none", applied)
	}
	if len(failures) != 1 || failures[0].ID != "dmut_bad" {
		t.Fatalf("failures = %v, want one failure for dmut_bad", failures)
	}
	if !strings.Contains(failures[0].Error, "unsupported mutation op") {
		t.Errorf("failure.Error = %q, want unsupported-op message", failures[0].Error)
	}
}

func TestApplyPendingMutations_MissingParams(t *testing.T) {
	t.Parallel()
	d, _ := newTestDaemonWithProjects(t, nil)

	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "dmut_no_id", Op: "project.add", Params: mustParams(t, map[string]string{"repository": "x"})},
	})
	if len(applied) != 0 {
		t.Errorf("applied = %v, want none", applied)
	}
	if len(failures) != 1 {
		t.Fatalf("failures = %v, want one", failures)
	}
	if !strings.Contains(failures[0].Error, "requires id") {
		t.Errorf("failure.Error = %q, want id-required message", failures[0].Error)
	}
}

func TestApplyPendingMutations_MixedBatch(t *testing.T) {
	t.Parallel()
	d, path := newTestDaemonWithProjects(t, []ProjectConfig{
		{ID: "alpha", Repository: "github.com/x/alpha"},
	})

	applied, failures := d.applyPendingMutations(context.Background(), []PendingMutation{
		{ID: "dmut_add_beta", Op: "project.add", Params: mustParams(t, map[string]string{"id": "beta", "repository": "github.com/x/beta"})},
		{ID: "dmut_bad", Op: "project.add", Params: mustParams(t, map[string]string{"repository": "no-id"})},
		{ID: "dmut_remove_alpha", Op: "project.remove", Params: mustParams(t, map[string]string{"id": "alpha"})},
	})

	if len(applied) != 2 {
		t.Errorf("applied = %v, want 2 successes", applied)
	}
	if len(failures) != 1 || failures[0].ID != "dmut_bad" {
		t.Errorf("failures = %v, want one for dmut_bad", failures)
	}

	got := readYamlProjects(t, path)
	if len(got) != 1 || got[0].ID != "beta" {
		t.Errorf("final yaml = %+v, want [{beta,...}]", got)
	}
}
