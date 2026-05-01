package afclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestWriteDaemonYAML_PreservesFullShape is the regression test for the
// v0.4.1 follow-up to REN-1419: `rensei project allow <repo>` must NOT
// clobber unrelated top-level keys in daemon.yaml. The previous writer
// marshalled the partial DaemonYAML struct directly and dropped
// apiVersion / kind / machine / orchestrator / autoUpdate, leaving a file
// that the daemon refused to load on next read.
func TestWriteDaemonYAML_PreservesFullShape(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")

	original := `apiVersion: rensei.dev/v1
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
  url: https://app.rensei.ai
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
		"apiVersion: rensei.dev/v1",
		"kind: LocalDaemon",
		"id: my-machine",
		"region: local",
		"maxVCpuPerSession: 2",
		"reservedForSystem:",
		"url: https://app.rensei.ai",
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

// TestWriteDaemonYAML_DoesNotDuplicateProjects confirms upserting an existing
// project rewrites in-place rather than appending.
func TestWriteDaemonYAML_DoesNotDuplicateProjects(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")

	original := `machine:
  id: m
orchestrator:
  url: https://app.rensei.ai
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
