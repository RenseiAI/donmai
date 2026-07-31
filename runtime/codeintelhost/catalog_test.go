package codeintelhost

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewCatalogValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		repos   []CatalogRepository
		wantErr bool
	}{
		{"valid single", []CatalogRepository{{ID: "row-1", RepositoryPathID: "github:acme/widgets", ProjectID: "proj-1", Source: "https://example.test/repo.git"}}, false},
		{"missing id", []CatalogRepository{{RepositoryPathID: "github:acme/widgets", ProjectID: "proj-1", Source: "https://example.test/repo.git"}}, true},
		{"missing pathId", []CatalogRepository{{ID: "row-1", ProjectID: "proj-1", Source: "https://example.test/repo.git"}}, true},
		{"missing projectId", []CatalogRepository{{ID: "row-1", RepositoryPathID: "github:acme/widgets", Source: "https://example.test/repo.git"}}, true},
		{"missing source", []CatalogRepository{{ID: "row-1", RepositoryPathID: "github:acme/widgets", ProjectID: "proj-1"}}, true},
		{
			"duplicate pathId",
			[]CatalogRepository{
				{ID: "row-1", RepositoryPathID: "github:acme/widgets", ProjectID: "proj-1", Source: "https://example.test/a.git"},
				{ID: "row-2", RepositoryPathID: "github:acme/widgets", ProjectID: "proj-2", Source: "https://example.test/b.git"},
			},
			true,
		},
		{"empty catalog", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewCatalog(tc.repos)
			if (err != nil) != tc.wantErr {
				t.Errorf("NewCatalog() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestCatalogLookup(t *testing.T) {
	t.Parallel()
	const repositoryPathID = "github:acme/widgets"
	cat, err := NewCatalog([]CatalogRepository{
		{ID: "row-1", RepositoryPathID: repositoryPathID, ProjectID: "proj-1", Source: "https://example.test/repo.git", Git: &CatalogGit{SSHKey: "/keys/id_ed25519"}},
	})
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	if cat.Len() != 1 {
		t.Errorf("Len() = %d, want 1", cat.Len())
	}

	got, err := cat.Lookup(repositoryPathID)
	if err != nil {
		t.Fatalf("Lookup(%q) error = %v", repositoryPathID, err)
	}
	if got.ID != "row-1" || got.Source != "https://example.test/repo.git" || got.Git == nil || got.Git.SSHKey != "/keys/id_ed25519" {
		t.Errorf("Lookup(%q) = %+v, mismatch", repositoryPathID, got)
	}

	_, err = cat.Lookup("missing-repo")
	if !errors.Is(err, ErrRepositoryNotFound) {
		t.Errorf("Lookup(missing) error = %v, want ErrRepositoryNotFound", err)
	}
}

func TestLoadCatalogDecodesOnlyRepositoriesSection(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	// A realistic daemon.yaml carries machine/capacity/orchestrator top-level
	// keys this host must not require. LoadCatalog must decode only
	// `repositories:` and ignore the rest.
	const doc = `
apiVersion: v1
kind: DaemonConfig
machine:
  id: machine-1
capacity:
  maxConcurrentSessions: 4
orchestrator:
  url: https://example.test/orchestrator
repositories:
  - id: row-1
    pathId: github:acme/widgets
    projectId: proj-1
    source: https://example.test/repo.git
    git:
      credentialHelper: /usr/local/bin/my-helper
      sshKey: /keys/id_ed25519
  - id: row-2
    pathId: github:acme/gadgets
    projectId: proj-2
    source: git@example.test:org/repo2.git
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write catalog file: %v", err)
	}

	cat, err := LoadCatalog(path)
	if err != nil {
		t.Fatalf("LoadCatalog() error = %v", err)
	}
	if cat.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", cat.Len())
	}
	repo1, err := cat.Lookup("github:acme/widgets")
	if err != nil {
		t.Fatalf("Lookup(github:acme/widgets) error = %v", err)
	}
	if repo1.ID != "row-1" || repo1.Git == nil || repo1.Git.CredentialHelper != "/usr/local/bin/my-helper" {
		t.Errorf("github:acme/widgets config = %+v, mismatch", repo1)
	}
}

func TestLoadCatalogMissingFile(t *testing.T) {
	t.Parallel()
	_, err := LoadCatalog(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Error("LoadCatalog() error = nil, want error for missing file")
	}
}

// TestNewCatalogRejectsEmbeddedCredentialsInSource proves NewCatalog rejects
// an http(s) source with embedded URL userinfo (ErrInsecureSource) and that
// the returned error text never echoes the credential-bearing source value —
// the catalog file itself may be world-readable, logged, or checked into a
// repo.
func TestNewCatalogRejectsEmbeddedCredentialsInSource(t *testing.T) {
	t.Parallel()
	const secretSource = "https://svc-user:sup3r-s3cr3t-token@example.test/repo.git"
	cases := []struct {
		name    string
		source  string
		wantErr bool
	}{
		{"https with password", secretSource, true},
		{"https with username only", "https://svc-user@example.test/repo.git", true},
		{"http with password", "http://svc-user:sup3r-s3cr3t-token@example.test/repo.git", true},
		{"https without userinfo", "https://example.test/repo.git", false},
		{"scp-like ssh source unexamined", "git@example.test:org/repo.git", false},
		{"ssh scheme without userinfo", "ssh://example.test/org/repo.git", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewCatalog([]CatalogRepository{{ID: "row-1", RepositoryPathID: "github:acme/widgets", ProjectID: "proj-1", Source: tc.source}})
			if (err != nil) != tc.wantErr {
				t.Errorf("NewCatalog(source=%q) error = %v, wantErr %v", tc.source, err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrInsecureSource) {
				t.Errorf("NewCatalog(source=%q) error = %v, want ErrInsecureSource", tc.source, err)
			}
		})
	}

	_, err := NewCatalog([]CatalogRepository{{ID: "row-1", RepositoryPathID: "github:acme/widgets", ProjectID: "proj-1", Source: secretSource}})
	if err == nil {
		t.Fatal("NewCatalog() error = nil, want ErrInsecureSource")
	}
	if strings.Contains(err.Error(), "svc-user") || strings.Contains(err.Error(), "sup3r-s3cr3t-token") {
		t.Errorf("NewCatalog() error = %q, must not echo the embedded credential", err.Error())
	}
}

// TestLoadCatalogRejectsEmbeddedCredentialsInSource is the LoadCatalog-level
// equivalent of TestNewCatalogRejectsEmbeddedCredentialsInSource: a catalog
// YAML file with an embedded credential in a repository source must be
// rejected, and the file path in LoadCatalog's own wrapping context must not
// smuggle the secret in either (only the path is added, never the source).
func TestLoadCatalogRejectsEmbeddedCredentialsInSource(t *testing.T) {
	t.Parallel()
	const secretMarker = "sup3r-s3cr3t-token"
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	doc := "repositories:\n  - id: row-1\n    pathId: github:acme/widgets\n    projectId: proj-1\n    source: https://svc-user:" + secretMarker + "@example.test/repo.git\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write catalog file: %v", err)
	}

	_, err := LoadCatalog(path)
	if !errors.Is(err, ErrInsecureSource) {
		t.Fatalf("LoadCatalog() error = %v, want ErrInsecureSource", err)
	}
	if strings.Contains(err.Error(), secretMarker) {
		t.Errorf("LoadCatalog() error = %q, must not echo the embedded credential", err.Error())
	}
}
