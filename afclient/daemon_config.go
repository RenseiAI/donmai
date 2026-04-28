// Package afclient daemon_config.go — read/write ~/.rensei/daemon.yaml.
//
// The file is the source-of-truth for the daemon's project allowlist and
// credential configuration. The running daemon reloads on SIGHUP or restart;
// af project commands write atomically (tmp file + rename) to avoid corrupting
// the file while the daemon is live.
package afclient

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ── daemon.yaml types ─────────────────────────────────────────────────────────

// CredentialHelperKind enumerates the supported per-project credential sources.
type CredentialHelperKind string

const (
	// CredentialHelperOSXKeychain uses the macOS osxkeychain git credential helper.
	CredentialHelperOSXKeychain CredentialHelperKind = "osxkeychain"
	// CredentialHelperSSH uses a filesystem SSH key for git authentication.
	CredentialHelperSSH CredentialHelperKind = "ssh"
	// CredentialHelperPAT stores the name of an env-var containing the PAT.
	CredentialHelperPAT CredentialHelperKind = "pat"
	// CredentialHelperGH delegates to the `gh` CLI via `gh auth status`.
	CredentialHelperGH CredentialHelperKind = "gh"
)

// CloneStrategy controls how the daemon clones a repo for session workareas.
type CloneStrategy string

const (
	// CloneShallow performs a depth-1 clone (fast; no history).
	CloneShallow CloneStrategy = "shallow"
	// CloneFull performs a full clone (slower; full history).
	CloneFull CloneStrategy = "full"
	// CloneReference clones from an existing local mirror.
	CloneReference CloneStrategy = "reference-clone"
)

// CredentialHelper is the per-project credential configuration written into
// daemon.yaml under projects[].credentialHelper.
//
// Exactly one of the helper-specific fields is set, matching the Kind:
//   - osxkeychain: no extra fields needed; git handles it natively.
//   - ssh:         SSHKeyPath is the absolute path to the private key.
//   - pat:         EnvVarName is the env-var whose value is the PAT.
//   - gh:          no extra fields needed; `gh auth` is invoked.
//
// When Kind is empty the helper is unconfigured; the daemon will refuse work
// for this project until credentials are added via `af project credentials`.
type CredentialHelper struct {
	Kind       CredentialHelperKind `yaml:"kind,omitempty"       json:"kind,omitempty"`
	SSHKeyPath string               `yaml:"sshKeyPath,omitempty" json:"sshKeyPath,omitempty"`
	EnvVarName string               `yaml:"envVarName,omitempty" json:"envVarName,omitempty"`
}

// ProjectEntry is one entry in the daemon.yaml `projects` list.
type ProjectEntry struct {
	// RepoURL is the canonical remote URL, e.g. "github.com/foo/bar".
	RepoURL string `yaml:"repoUrl" json:"repoUrl"`
	// CloneStrategy controls how the daemon clones the repo. Default: shallow.
	CloneStrategy CloneStrategy `yaml:"cloneStrategy,omitempty" json:"cloneStrategy,omitempty"`
	// CredentialHelper is the credential source for this project.
	// A nil pointer means no credentials are configured (--no-credentials).
	CredentialHelper *CredentialHelper `yaml:"credentialHelper,omitempty" json:"credentialHelper,omitempty"`
}

// DaemonYAML is the in-memory representation of ~/.rensei/daemon.yaml.
// Only the fields relevant to the project command tree are modelled here;
// unknown top-level keys are preserved via the yaml decoder's pass-through.
type DaemonYAML struct {
	// Projects is the allowlist of repos the daemon will accept work for.
	Projects []ProjectEntry `yaml:"projects,omitempty"`
}

// ── default path ─────────────────────────────────────────────────────────────

// DefaultDaemonYAMLPath returns the canonical path to daemon.yaml, expanding ~.
func DefaultDaemonYAMLPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "~/.rensei/daemon.yaml"
	}
	return filepath.Join(home, ".rensei", "daemon.yaml")
}

// ── read / write ─────────────────────────────────────────────────────────────

// ReadDaemonYAML reads and parses daemon.yaml from path.
// If the file does not exist an empty DaemonYAML is returned without error,
// so callers can treat first-run as a no-op read followed by a write.
func ReadDaemonYAML(path string) (*DaemonYAML, error) {
	data, err := os.ReadFile(path) //nolint:gosec // caller-supplied path is intentional
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &DaemonYAML{}, nil
		}
		return nil, fmt.Errorf("read daemon config %q: %w", path, err)
	}
	var cfg DaemonYAML
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse daemon config %q: %w", path, err)
	}
	return &cfg, nil
}

// WriteDaemonYAML atomically writes cfg to path (tmp file + rename).
// The parent directory is created with 0o700 if it does not exist.
func WriteDaemonYAML(path string, cfg *DaemonYAML) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir %q: %w", dir, err)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal daemon config: %w", err)
	}

	// Atomic write: write to a sibling temp file then rename.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write temp config %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename temp config: %w", err)
	}
	return nil
}

// ── project-list helpers ──────────────────────────────────────────────────────

// FindProject returns the index of the first ProjectEntry whose RepoURL
// matches repoURL, or -1 if not found.
func (d *DaemonYAML) FindProject(repoURL string) int {
	for i, p := range d.Projects {
		if p.RepoURL == repoURL {
			return i
		}
	}
	return -1
}

// AddOrUpdateProject upserts a ProjectEntry by RepoURL.
// If a matching entry exists it is replaced; otherwise the entry is appended.
func (d *DaemonYAML) AddOrUpdateProject(entry ProjectEntry) {
	if i := d.FindProject(entry.RepoURL); i >= 0 {
		d.Projects[i] = entry
		return
	}
	d.Projects = append(d.Projects, entry)
}

// RemoveProject removes the entry matching repoURL.
// Returns true if an entry was removed, false if none matched.
func (d *DaemonYAML) RemoveProject(repoURL string) bool {
	i := d.FindProject(repoURL)
	if i < 0 {
		return false
	}
	d.Projects = append(d.Projects[:i], d.Projects[i+1:]...)
	return true
}
