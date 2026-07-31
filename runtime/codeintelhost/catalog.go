package codeintelhost

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// CatalogGit mirrors daemon.ProjectGit's yaml shape: operator-owned Git
// credential configuration for one catalog repository. Both fields are
// optional; an absent CredentialHelper leaves the ambient git configuration
// untouched, and an absent SSHKey leaves the ambient SSH agent/config
// untouched.
type CatalogGit struct {
	CredentialHelper string `yaml:"credentialHelper,omitempty"`
	SSHKey           string `yaml:"sshKey,omitempty"`
}

// CatalogRepository is one admitted repository resource. It reuses the
// daemon's ~/.donmai/daemon.yaml `repositories[]` entry shape (id,
// projectId, source, git.credentialHelper, git.sshKey) but is decoded
// independently: this host requires none of daemon.yaml's machine,
// capacity, or orchestrator fields.
type CatalogRepository struct {
	ID        string      `yaml:"id"`
	ProjectID string      `yaml:"projectId"`
	Source    string      `yaml:"source"`
	Git       *CatalogGit `yaml:"git,omitempty"`
}

// catalogDocument is the minimal top-level shape this package decodes from
// the catalog YAML file — only the `repositories:` key. Any other top-level
// key present (machine, capacity, orchestrator, ...) in a shared
// daemon.yaml is ignored rather than rejected, so a single file may serve
// both the daemon and this host.
type catalogDocument struct {
	Repositories []CatalogRepository `yaml:"repositories"`
}

// Catalog is the resolved, validated set of repositories this host may
// serve, indexed by repositoryPathId.
type Catalog struct {
	byID map[string]CatalogRepository
}

// LoadCatalog reads and validates the repository catalog at path.
func LoadCatalog(path string) (*Catalog, error) {
	data, err := readConfinedFile(path)
	if err != nil {
		return nil, fmt.Errorf("read repository catalog %q: %w", path, err)
	}
	var doc catalogDocument
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse repository catalog %q: %w", path, err)
	}
	cat, err := NewCatalog(doc.Repositories)
	if err != nil {
		return nil, fmt.Errorf("repository catalog %q: %w", path, err)
	}
	return cat, nil
}

// readConfinedFile reads path's contents through an os.Root opened on path's
// cleaned parent directory, so the read cannot be steered outside that
// directory (e.g. via a symlink or ".." component in an operator-configured
// path) — the confinement os.OpenRoot (Go 1.24+) provides in place of a bare
// os.ReadFile(variablePath) call.
func readConfinedFile(path string) ([]byte, error) {
	dir := filepath.Clean(filepath.Dir(path))
	base := filepath.Base(path)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", dir, err)
	}
	defer func() { _ = root.Close() }()
	return root.ReadFile(base)
}

// NewCatalog validates repos and builds a Catalog. Each entry requires a
// non-empty id, projectId, and source; ids must be unique.
func NewCatalog(repos []CatalogRepository) (*Catalog, error) {
	byID := make(map[string]CatalogRepository, len(repos))
	for i, r := range repos {
		if r.ID == "" {
			return nil, fmt.Errorf("repositories[%d]: id is required", i)
		}
		if r.ProjectID == "" {
			return nil, fmt.Errorf("repositories[%d]: projectId is required", i)
		}
		if r.Source == "" {
			return nil, fmt.Errorf("repositories[%d]: source is required", i)
		}
		if err := validateSource(r.Source); err != nil {
			// Deliberately omit r.Source from this error: it is the value
			// under suspicion of carrying an embedded credential.
			return nil, fmt.Errorf("repositories[%d]: %w", i, err)
		}
		if _, dup := byID[r.ID]; dup {
			return nil, fmt.Errorf("repositories[%d]: duplicate id %q", i, r.ID)
		}
		byID[r.ID] = r
	}
	return &Catalog{byID: byID}, nil
}

// validateSource rejects an http(s) source that embeds URL userinfo (e.g.
// https://user:token@host/repo.git): the catalog file itself may be
// world-readable, checked into a repo, or captured in a log/support bundle,
// so a credential embedded in the URL is a leak wherever the catalog is. Any
// other scheme (git@host:path, ssh://, a local filesystem path, …) is left
// unexamined — CatalogGit.CredentialHelper/SSHKey is the supported
// credential-configuration path for those. A source that does not parse as a
// URL at all (e.g. scp-like syntax) is treated as having nothing to check.
func validateSource(source string) error {
	u, err := url.Parse(source)
	if err != nil {
		return nil
	}
	if (u.Scheme == "http" || u.Scheme == "https") && u.User != nil {
		return ErrInsecureSource
	}
	return nil
}

// Lookup resolves a repositoryPathId to its catalog entry. It returns
// ErrRepositoryNotFound (wrapped) when id is not admitted.
func (c *Catalog) Lookup(id string) (CatalogRepository, error) {
	r, ok := c.byID[id]
	if !ok {
		return CatalogRepository{}, fmt.Errorf("%w: %q", ErrRepositoryNotFound, id)
	}
	return r, nil
}

// Len reports the number of admitted repositories.
func (c *Catalog) Len() int {
	return len(c.byID)
}
