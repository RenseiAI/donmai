package afclient

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestWriteDaemonYAML_PreservesFullShape is the regression test for the
// v0.4.1 follow-up: `rensei project allow <repo>` must NOT
// clobber unrelated top-level keys in daemon.yaml. The previous writer
// marshalled the partial DaemonYAML struct directly and dropped
// apiVersion / kind / machine / orchestrator / autoUpdate, leaving a file
// that the daemon refused to load on next read.
func TestWriteDaemonYAML_PreservesFullShape(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")

	original := `apiVersion: donmai.dev/v1
kind: LocalDaemon
machine:
  id: my-machine
  region: local
capacity:
  maxConcurrentSessions: 4
  maxVCpuPerSession: 2
  maxMemoryMbPerSession: 4096
  reservedForSystem:
    vCpu: 2
    memoryMb: 8192
orchestrator:
  url: https://platform.example.com
  authToken: rsk_live_xxx
autoUpdate:
  channel: stable
  schedule: nightly
  drainTimeoutSeconds: 600
projects:
  - id: existing
    repository: github.com/old/proj
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cfg, err := ReadDaemonYAML(path)
	if err != nil {
		t.Fatalf("ReadDaemonYAML: %v", err)
	}
	cfg.AddOrUpdateProject(ProjectEntry{
		RepoURL:       "github.com/foo/bar",
		CloneStrategy: CloneShallow,
	})
	if err := WriteDaemonYAML(path, cfg); err != nil {
		t.Fatalf("WriteDaemonYAML: %v", err)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written: %v", err)
	}
	got := string(out)
	for _, want := range []string{
		"apiVersion: donmai.dev/v1",
		"kind: LocalDaemon",
		"id: my-machine",
		"region: local",
		"maxVCpuPerSession: 2",
		"reservedForSystem:",
		"url: https://platform.example.com",
		"authToken: rsk_live_xxx",
		"channel: stable",
		"schedule: nightly",
		"drainTimeoutSeconds: 600",
		"github.com/foo/bar",
		"github.com/old/proj",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("written daemon.yaml missing %q:\n%s", want, got)
		}
	}
}

// TestWriteDaemonYAML_FreshFileEmitsCanonicalKeys covers the no-pre-existing-
// file path: the writer is allowed to emit a brand new daemon.yaml that uses
// the canonical `repository` key (not the legacy `repoUrl`).
func TestWriteDaemonYAML_FreshFileEmitsCanonicalKeys(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	cfg := &DaemonYAML{
		Projects: []ProjectEntry{{RepoURL: "github.com/foo/bar"}},
	}
	if err := WriteDaemonYAML(path, cfg); err != nil {
		t.Fatalf("WriteDaemonYAML: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "repository:") {
		t.Errorf("missing canonical repository key:\n%s", got)
	}
	if strings.Contains(got, "repoUrl:") {
		t.Errorf("legacy repoUrl key emitted:\n%s", got)
	}
}

// TestWriteDaemonYAML_StableYAMLShape parses the written file as yaml and
// asserts the top-level keys appear in a sane order (no panic, valid yaml).
func TestWriteDaemonYAML_StableYAMLShape(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	cfg := &DaemonYAML{Projects: []ProjectEntry{{RepoURL: "github.com/a/b"}}}
	if err := WriteDaemonYAML(path, cfg); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		t.Fatalf("invalid yaml: %v\n%s", err, data)
	}
}

// TestWriteDaemonYAML_PopulatesProjectID confirms that a project entry written
// without an explicit ID gets a derived id, satisfying the daemon reader's
// `projects[i].id is required` validation.
func TestWriteDaemonYAML_PopulatesProjectID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	cfg := &DaemonYAML{
		Projects: []ProjectEntry{
			{RepoURL: "github.com/foo/bar"},
			{RepoURL: "https://github.com/foo/baz.git"},
			{RepoURL: "git@github.com:foo/qux.git"},
		},
	}
	if err := WriteDaemonYAML(path, cfg); err != nil {
		t.Fatalf("WriteDaemonYAML: %v", err)
	}
	loaded, err := ReadDaemonYAML(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	wants := []string{"foo-bar", "foo-baz", "foo-qux"}
	if len(loaded.Projects) != len(wants) {
		t.Fatalf("Projects = %d, want %d", len(loaded.Projects), len(wants))
	}
	for i, want := range wants {
		if loaded.Projects[i].ID != want {
			t.Errorf("Projects[%d].ID = %q, want %q", i, loaded.Projects[i].ID, want)
		}
	}
}

// TestWriteDaemonYAML_PreservesExistingProjectID covers the upsert path
// with an explicit id: the existing id must not be overwritten by the
// derive-default path.
func TestWriteDaemonYAML_PreservesExistingProjectID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	cfg := &DaemonYAML{
		Projects: []ProjectEntry{
			{ID: "explicit-id", RepoURL: "github.com/foo/bar"},
		},
	}
	if err := WriteDaemonYAML(path, cfg); err != nil {
		t.Fatalf("WriteDaemonYAML: %v", err)
	}
	loaded, err := ReadDaemonYAML(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := loaded.Projects[0].ID; got != "explicit-id" {
		t.Errorf("Projects[0].ID = %q, want explicit-id", got)
	}
}

// TestDeriveProjectID covers the URL-shape heuristic.
func TestDeriveProjectID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"github.com/foo/bar", "foo-bar"},
		{"https://github.com/foo/bar", "foo-bar"},
		{"https://github.com/foo/bar.git", "foo-bar"},
		{"git@github.com:foo/bar.git", "foo-bar"},
		{"BAR", "bar"},
		{"", "project"},
		{"https://example.com/My_Repo.Name", "example-com-my-repo-name"},
	}
	for _, c := range cases {
		if got := DeriveProjectID(c.in); got != c.want {
			t.Errorf("DeriveProjectID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDaemonYAML_ProjectAdmissionIsIndependentOfRepositories(t *testing.T) {
	cfg := &DaemonYAML{
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		EnabledProjectIDs:       []string{},
		Projects: []ProjectEntry{
			{ID: "alpha", RepoURL: "example.com/acme/one"},
			{ID: "alpha", RepoURL: "example.com/acme/two"},
		},
	}
	if cfg.IsProjectEnabled("alpha") {
		t.Fatal("repository resources must not imply admission in an explicit v2 config")
	}
	cfg.EnableProject("alpha")
	if !cfg.IsProjectEnabled("alpha") {
		t.Fatal("EnableProject did not admit alpha")
	}
	cfg.DisableProject("alpha")
	if cfg.IsProjectEnabled("alpha") {
		t.Fatal("DisableProject did not remove alpha")
	}
	if len(cfg.Repositories) != 2 {
		t.Fatalf("DisableProject removed repository resources: %+v", cfg.Repositories)
	}
	if len(cfg.Projects) != 0 {
		t.Fatalf("disabled project leaked into legacy projection: %+v", cfg.Projects)
	}
}

func TestReadDaemonYAML_MigratesLegacyRepositoryProjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	if err := os.WriteFile(path, []byte("projects:\n  - id: alpha\n    repository: example.com/acme/alpha\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := ReadDaemonYAML(path)
	if err != nil {
		t.Fatalf("ReadDaemonYAML: %v", err)
	}
	if !cfg.IsProjectEnabled("alpha") {
		t.Fatalf("EnabledProjectIDs = %v, want legacy alpha migrated", cfg.EnabledProjectIDs)
	}
}

// TestWriteDaemonYAML_DoesNotDuplicateProjects confirms upserting an existing
// project rewrites in-place rather than appending.
func TestWriteDaemonYAML_DoesNotDuplicateProjects(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")

	original := `machine:
  id: m
orchestrator:
  url: https://platform.example.com
projects:
  - id: bar
    repository: github.com/foo/bar
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadDaemonYAML(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddOrUpdateProject(ProjectEntry{
		RepoURL:       "github.com/foo/bar",
		CloneStrategy: CloneFull,
	})
	if err := WriteDaemonYAML(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadDaemonYAML(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Projects) != 1 {
		t.Errorf("Projects = %d, want 1", len(loaded.Projects))
	}
	if loaded.Projects[0].CloneStrategy != CloneFull {
		t.Errorf("CloneStrategy not updated: %q", loaded.Projects[0].CloneStrategy)
	}
}

func TestWriteDaemonYAMLRepositoryPathIDRoundTrips(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	cfg := &DaemonYAML{
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		EnabledProjectIDs:       []string{"project-1"},
		Repositories: []RepositoryEntry{{
			ID:        "repository-row-123",
			PathID:    "github:acme/widgets",
			ProjectID: "project-1",
			Source:    "https://example.test/acme/widgets.git",
		}},
	}
	if err := WriteDaemonYAML(path, cfg); err != nil {
		t.Fatalf("WriteDaemonYAML: %v", err)
	}
	loaded, err := ReadDaemonYAML(path)
	if err != nil {
		t.Fatalf("ReadDaemonYAML: %v", err)
	}
	if len(loaded.Repositories) != 1 {
		t.Fatalf("Repositories = %d, want 1", len(loaded.Repositories))
	}
	if got := loaded.Repositories[0].PathID; got != "github:acme/widgets" {
		t.Errorf("Repositories[0].PathID = %q, want github:acme/widgets", got)
	}
}

func TestWriteDaemonYAMLWithCapacity_ExplicitZero(t *testing.T) {
	cases := []struct{ name, seed string }{
		{"fresh", ""},
		{"omitted", "unknown: retained\n"},
		{"null", "capacity: null\nunknown: retained\n"},
		{"positive", "capacity: {maxConcurrentSessions: 3, maxVCpuPerSession: 6}\nunknown: retained\n"},
		{"merge", "defaults: &defaults {maxConcurrentSessions: 3, maxVCpuPerSession: 6}\ncapacity: {<<: *defaults}\nunknown: retained\n"},
		{"alias", "defaults: &defaults {maxConcurrentSessions: 3, maxVCpuPerSession: 6}\ncapacity: *defaults\nunknown: retained\n"},
	}
	for _, key := range []string{"capacity.maxConcurrentSessions", "capacity.poolMaxDiskGb"} {
		for _, tc := range cases {
			t.Run(key+"/"+tc.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "daemon.yaml")
				if tc.seed != "" {
					if err := os.WriteFile(path, []byte(tc.seed), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				cfg, err := ReadDaemonYAML(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := WriteDaemonYAMLWithCapacity(path, cfg, key, 0); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var decoded map[string]any
				if err := yaml.Unmarshal(data, &decoded); err != nil {
					t.Fatal(err)
				}
				capacity, ok := decoded["capacity"].(map[string]any)
				if !ok {
					t.Fatalf("capacity missing: %s", data)
				}
				if v, exists := capacity[strings.TrimPrefix(key, "capacity.")]; !exists || v != 0 {
					t.Fatalf("explicit zero missing: %s", data)
				}
				if strings.Contains(tc.seed, "unknown") && decoded["unknown"] != "retained" {
					t.Fatalf("unknown lost: %s", data)
				}
				if strings.Contains(tc.seed, "maxVCpu") && capacity["maxVCpuPerSession"] != 6 {
					t.Fatalf("capacity unknown lost: %s", data)
				}
				if strings.Contains(tc.seed, "defaults:") && decoded["defaults"].(map[string]any)["maxConcurrentSessions"] != 3 {
					t.Fatalf("shared anchor changed: %s", data)
				}
			})
		}
	}
}

func TestWriteDaemonYAMLWithCapacity_ValidationBeforeWrite(t *testing.T) {
	for _, tc := range []struct {
		key       string
		value     int
		nilConfig bool
	}{
		{"capacity.maxConcurrentSessions", -1, false}, {"capacity.poolMaxDiskGb", -1, false}, {"capacity.maxVCpuPerSession", 0, false}, {"maxConcurrentSessions", 0, false}, {"capacity.maxConcurrentSessions", 0, true},
	} {
		t.Run(tc.key+"/"+strconv.Itoa(tc.value)+"/"+strconv.FormatBool(tc.nilConfig), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "not-created", "daemon.yaml")
			cfg := &DaemonYAML{}
			if tc.nilConfig {
				cfg = nil
			}
			if err := WriteDaemonYAMLWithCapacity(path, cfg, tc.key, tc.value); err == nil {
				t.Fatal("invalid intent accepted")
			}
			if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
				t.Fatalf("invalid write created directory: %v", err)
			}
		})
	}
}

func TestWriteDaemonYAML_GenericCapacityOmissionPreserved(t *testing.T) {
	for _, seed := range []string{"unknown: retained\n", "capacity: null\n", "capacity: {maxConcurrentSessions: null}\n"} {
		t.Run(seed, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.yaml")
			if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := ReadDaemonYAML(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Capacity.PoolMaxDiskGb = 9
			if err := WriteDaemonYAML(path, cfg); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := yaml.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			capacity := decoded["capacity"].(map[string]any)
			if value, exists := capacity["maxConcurrentSessions"]; exists && value != nil {
				t.Fatalf("unrelated write authored limit: %s", data)
			}
			if capacity["poolMaxDiskGb"] != 9 {
				t.Fatalf("disk write missing: %s", data)
			}
		})
	}
}

func TestWriteDaemonYAMLWithCapacity_Anchors(t *testing.T) {
	cases := []struct {
		name, seed      string
		borrowed        bool
		observerMapping bool
	}{
		{name: "owned_scalar", seed: "capacity: {maxConcurrentSessions: &limit 3, maxVCpuPerSession: 6}\nobserver: {limit: *limit}\n"},
		{name: "borrowed_scalar", seed: "defaults: {limit: &limit 3}\ncapacity: {maxConcurrentSessions: *limit, maxVCpuPerSession: 6}\nobserver: {limit: *limit}\n", borrowed: true},
		{name: "owned_capacity_mapping", seed: "capacity: &limits {maxConcurrentSessions: 3, maxVCpuPerSession: 6}\nobserver: *limits\n", observerMapping: true},
		{name: "borrowed_capacity_mapping", seed: "defaults: &limits {maxConcurrentSessions: 3, maxVCpuPerSession: 6}\ncapacity: *limits\nobserver: *limits\n", borrowed: true, observerMapping: true},
		{name: "owned_scalar_in_owned_mapping", seed: "capacity: &limits {maxConcurrentSessions: &limit 3, maxVCpuPerSession: 6}\nobserver: {limit: *limit}\nother: *limits\n"},
	}
	for _, key := range []string{"capacity.maxConcurrentSessions", "capacity.poolMaxDiskGb"} {
		field := strings.TrimPrefix(key, "capacity.")
		for _, value := range []int{0, 5} {
			for _, tc := range cases {
				t.Run(key+"/"+tc.name+"/"+strconv.Itoa(value), func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "daemon.yaml")
					seed := strings.ReplaceAll(tc.seed, "maxConcurrentSessions", field) + "unknown: retained\n"
					if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
						t.Fatal(err)
					}
					cfg, err := ReadDaemonYAML(path)
					if err != nil {
						t.Fatal(err)
					}
					if err := WriteDaemonYAMLWithCapacity(path, cfg, key, value); err != nil {
						t.Fatal(err)
					}
					readback, err := ReadDaemonYAML(path)
					if err != nil {
						t.Fatalf("public readback after write: %v", err)
					}
					actual := readback.Capacity.MaxConcurrentSessions
					if field == "poolMaxDiskGb" {
						actual = readback.Capacity.PoolMaxDiskGb
					}
					if actual != value {
						t.Fatalf("public readback = %d, want %d", actual, value)
					}
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var decoded map[string]any
					if err := yaml.Unmarshal(data, &decoded); err != nil {
						t.Fatal(err)
					}
					capacity := decoded["capacity"].(map[string]any)
					if capacity["maxVCpuPerSession"] != 6 || decoded["unknown"] != "retained" {
						t.Fatalf("unrelated fields lost: %s", data)
					}
					observer := decoded["observer"].(map[string]any)
					observedKey := "limit"
					if tc.observerMapping {
						observedKey = field
					}
					wantObserver := value
					if tc.borrowed {
						wantObserver = 3
					}
					if observer[observedKey] != wantObserver {
						t.Fatalf("observer = %v, want %d: %s", observer[observedKey], wantObserver, data)
					}
					if tc.name == "borrowed_scalar" && decoded["defaults"].(map[string]any)["limit"] != 3 {
						t.Fatalf("external scalar target changed: %s", data)
					}
					if tc.name == "borrowed_capacity_mapping" && decoded["defaults"].(map[string]any)[field] != 3 {
						t.Fatalf("external mapping target changed: %s", data)
					}
					if other, ok := decoded["other"].(map[string]any); ok && other[field] != value {
						t.Fatalf("owned mapping alias lost updated value: %s", data)
					}
				})
			}
		}
	}
}

func TestWriteDaemonYAML_AnchoredCapacityOmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	seed := "capacity: {maxConcurrentSessions: &limit 3, maxVCpuPerSession: 6}\nobserver: {limit: *limit}\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadDaemonYAML(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Capacity.MaxConcurrentSessions = 0
	cfg.Capacity.PoolMaxDiskGb = 9
	if err := WriteDaemonYAML(path, cfg); err != nil {
		t.Fatal(err)
	}
	readback, err := ReadDaemonYAML(path)
	if err != nil {
		t.Fatal(err)
	}
	if readback.Capacity.MaxConcurrentSessions != 3 || readback.Capacity.PoolMaxDiskGb != 9 {
		t.Fatalf("generic omission changed values: %+v", readback.Capacity)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["observer"].(map[string]any)["limit"] != 3 {
		t.Fatalf("omitted scalar alias changed: %s", data)
	}
}
