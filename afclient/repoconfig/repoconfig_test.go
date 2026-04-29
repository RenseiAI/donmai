package repoconfig_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/afclient/repoconfig"
)

// writeConfig writes content to .agentfactory/config.yaml under dir.
func writeConfig(t *testing.T, dir, content string) {
	t.Helper()
	afDir := filepath.Join(dir, ".agentfactory")
	if err := os.MkdirAll(afDir, 0o750); err != nil { //nolint:gosec
		t.Fatalf("mkdir .agentfactory: %v", err)
	}
	path := filepath.Join(afDir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil { // #nosec G306
		t.Fatalf("write config.yaml: %v", err)
	}
}

func TestLoad_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := repoconfig.Load(dir)
	if !errors.Is(err, repoconfig.ErrConfigNotFound) {
		t.Fatalf("expected ErrConfigNotFound, got %v", err)
	}
}

func TestLoad_AllowedProjects(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `apiVersion: v1
kind: RepositoryConfig
repository: github.com/org/repo
allowedProjects:
  - Alpha
  - Beta
`)
	rc, err := repoconfig.Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rc.Repository != "github.com/org/repo" {
		t.Errorf("Repository = %q; want github.com/org/repo", rc.Repository)
	}
	if !rc.IsProjectAllowed("Alpha") {
		t.Error("expected Alpha to be allowed")
	}
	if !rc.IsProjectAllowed("Beta") {
		t.Error("expected Beta to be allowed")
	}
	if rc.IsProjectAllowed("Gamma") {
		t.Error("expected Gamma to be disallowed")
	}
}

func TestLoad_ProjectPaths_StringShorthand(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `apiVersion: v1
kind: RepositoryConfig
repository: github.com/org/monorepo
projectPaths:
  Social: apps/social
  Family: apps/family
sharedPaths:
  - packages/ui
`)
	rc, err := repoconfig.Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	allowed := rc.GetEffectiveAllowedProjects()
	if len(allowed) != 2 {
		t.Fatalf("expected 2 allowed projects, got %d: %v", len(allowed), allowed)
	}
	pc := rc.GetProjectConfig("Social")
	if pc == nil {
		t.Fatal("expected project config for Social")
	}
	if pc.Path != "apps/social" {
		t.Errorf("Social.Path = %q; want apps/social", pc.Path)
	}
}

func TestLoad_ProjectPaths_ObjectForm(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `apiVersion: v1
kind: RepositoryConfig
repository: github.com/org/monorepo
buildCommand: make build
projectPaths:
  iOS:
    path: apps/ios
    packageManager: none
    buildCommand: xcodebuild -scheme App
`)
	rc, err := repoconfig.Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pc := rc.GetProjectConfig("iOS")
	if pc == nil {
		t.Fatal("expected project config for iOS")
	}
	if pc.Path != "apps/ios" {
		t.Errorf("iOS.Path = %q; want apps/ios", pc.Path)
	}
	if pc.PackageManager != "none" {
		t.Errorf("iOS.PackageManager = %q; want none", pc.PackageManager)
	}
	if pc.BuildCommand != "xcodebuild -scheme App" {
		t.Errorf("iOS.BuildCommand = %q; want xcodebuild -scheme App", pc.BuildCommand)
	}
}

func TestLoad_MutuallyExclusive(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `apiVersion: v1
kind: RepositoryConfig
allowedProjects:
  - Alpha
projectPaths:
  Beta: apps/beta
`)
	_, err := repoconfig.Load(dir)
	if err == nil {
		t.Fatal("expected error for mutually exclusive allowedProjects + projectPaths")
	}
}

func TestLoad_WrongKind(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `apiVersion: v1
kind: SomethingElse
`)
	_, err := repoconfig.Load(dir)
	if err == nil {
		t.Fatal("expected error for wrong kind")
	}
}

func TestIsProjectAllowed_NoList(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `apiVersion: v1
kind: RepositoryConfig
`)
	rc, err := repoconfig.Load(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// When no list is set, all projects should be allowed.
	if !rc.IsProjectAllowed("Anything") {
		t.Error("expected all projects to be allowed when no allowlist is set")
	}
}
