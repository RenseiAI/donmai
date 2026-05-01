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
	"log/slog"
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
//
// The yaml key for the repo URL is `repository`, matching the daemon-side
// reader (daemon.ProjectConfig). REN-1419 renamed this from `repoUrl` to
// align writer + reader after a schema-drift bug where the writer emitted
// `repoUrl` but the reader looked for `repository`, causing
// `rensei daemon stats` to report `Projects: 0 allowed` after a successful
// `rensei project allow`. The Go field is still RepoURL for source-compat.
//
// On read, ProjectEntry tolerates the legacy `repoUrl` key for one cycle
// (see UnmarshalYAML below) so pre-fix files in the wild still load.
type ProjectEntry struct {
	// RepoURL is the canonical remote URL, e.g. "github.com/foo/bar".
	RepoURL string `yaml:"repository" json:"repository"`
	// CloneStrategy controls how the daemon clones the repo. Default: shallow.
	CloneStrategy CloneStrategy `yaml:"cloneStrategy,omitempty" json:"cloneStrategy,omitempty"`
	// CredentialHelper is the credential source for this project.
	// A nil pointer means no credentials are configured (--no-credentials).
	CredentialHelper *CredentialHelper `yaml:"credentialHelper,omitempty" json:"credentialHelper,omitempty"`
}

// UnmarshalYAML accepts either the canonical `repository` key (post-REN-1419)
// or the legacy `repoUrl` key (pre-REN-1419). When the legacy key is found a
// one-line warning is logged via slog so operators know to rewrite the file
// (the next write will use the canonical key automatically).
func (p *ProjectEntry) UnmarshalYAML(node *yaml.Node) error {
	var raw struct {
		Repository       string            `yaml:"repository"`
		RepoURL          string            `yaml:"repoUrl"`
		CloneStrategy    CloneStrategy     `yaml:"cloneStrategy,omitempty"`
		CredentialHelper *CredentialHelper `yaml:"credentialHelper,omitempty"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	p.CloneStrategy = raw.CloneStrategy
	p.CredentialHelper = raw.CredentialHelper
	switch {
	case raw.Repository != "":
		p.RepoURL = raw.Repository
	case raw.RepoURL != "":
		p.RepoURL = raw.RepoURL
		slog.Warn(
			"daemon.yaml: legacy 'repoUrl' key on project entry; will be rewritten as 'repository' on next write (REN-1419)",
			"repoUrl", raw.RepoURL,
		)
	}
	return nil
}

// CapacityConfig holds the configurable capacity limits written into
// daemon.yaml under the `capacity` key. REN-1334 adds `poolMaxDiskGb` for
// automatic LRU eviction of the workarea pool once the disk threshold is hit.
type CapacityConfig struct {
	// PoolMaxDiskGb is the maximum total disk usage (in GiB) for the workarea
	// pool before the daemon starts LRU-evicting cold members.  0 means no limit.
	PoolMaxDiskGb int `yaml:"poolMaxDiskGb,omitempty" json:"poolMaxDiskGb,omitempty"`
}

// DaemonYAML is the in-memory representation of ~/.rensei/daemon.yaml.
// Only the fields relevant to the project command tree are modelled here;
// unknown top-level keys are preserved via the yaml decoder's pass-through.
type DaemonYAML struct {
	// Projects is the allowlist of repos the daemon will accept work for.
	Projects []ProjectEntry `yaml:"projects,omitempty"`
	// Capacity holds the configurable resource limits for the daemon.
	Capacity CapacityConfig `yaml:"capacity,omitempty"`
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
//
// The writer preserves any unknown top-level keys present in the existing
// file (e.g. apiVersion, kind, machine, orchestrator, autoUpdate,
// observability) by parsing the on-disk file as a yaml.Node tree, replacing
// only the `projects` and `capacity` mappings, and re-marshalling. This is
// the v0.4.1 follow-up to REN-1419: the previous writer marshalled the
// minimal DaemonYAML struct directly, which clobbered every key the project
// command tree did not model. After a single `rensei project allow` the
// daemon would refuse to load the resulting file (machine.id missing,
// orchestrator.url missing).
//
// If the file does not exist a fresh document is written from cfg. Callers
// that want a fully-populated daemon.yaml should run the wizard first or
// hand-author the file before calling this writer.
func WriteDaemonYAML(path string, cfg *DaemonYAML) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir %q: %w", dir, err)
	}

	data, err := mergeDaemonYAML(path, cfg)
	if err != nil {
		return fmt.Errorf("merge daemon config: %w", err)
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

// mergeDaemonYAML loads the existing daemon.yaml at path (if present) as a
// yaml.Node tree, replaces the `projects` and `capacity` keys with the
// values from cfg, and returns the marshalled result. When the file does
// not exist the cfg struct is marshalled directly.
func mergeDaemonYAML(path string, cfg *DaemonYAML) ([]byte, error) {
	existing, readErr := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if readErr != nil {
		if !errors.Is(readErr, os.ErrNotExist) {
			return nil, fmt.Errorf("read existing config %q: %w", path, readErr)
		}
		// Fresh file — emit the cfg struct directly. The daemon reader
		// will reject this if it lacks machine.id / orchestrator.url; the
		// CLI does not own those fields, so we leave the wizard /
		// installer to populate them.
		return yaml.Marshal(cfg)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(existing, &root); err != nil {
		return nil, fmt.Errorf("parse existing config: %w", err)
	}

	// A document node wraps the top-level mapping; descend to the mapping.
	doc := &root
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		doc = doc.Content[0]
	}
	if doc.Kind != yaml.MappingNode {
		// File is empty / not a mapping — fall back to fresh emission.
		return yaml.Marshal(cfg)
	}

	// Encode the cfg-side keys we own as nodes for splicing.
	projectsNode, err := encodeYAMLNode(cfg.Projects)
	if err != nil {
		return nil, fmt.Errorf("encode projects: %w", err)
	}
	capacityNode, err := encodeYAMLNode(cfg.Capacity)
	if err != nil {
		return nil, fmt.Errorf("encode capacity: %w", err)
	}

	upsertMappingKey(doc, "projects", projectsNode)
	// Capacity is preserved as a partial overlay — only the cfg-modelled
	// fields (e.g. poolMaxDiskGb) are merged into the existing capacity
	// mapping. If no capacity key exists yet a new one is added.
	mergeMappingKey(doc, "capacity", capacityNode)

	out, err := yaml.Marshal(&root)
	if err != nil {
		return nil, fmt.Errorf("marshal merged config: %w", err)
	}
	return out, nil
}

// encodeYAMLNode marshals v through yaml.v3 and returns the resulting node
// tree for splicing into a parent document.
func encodeYAMLNode(v any) (*yaml.Node, error) {
	data, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	var n yaml.Node
	if err := yaml.Unmarshal(data, &n); err != nil {
		return nil, err
	}
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		return n.Content[0], nil
	}
	return &n, nil
}

// upsertMappingKey replaces (or appends) the given key in the mapping node
// with the given value node.
func upsertMappingKey(mapping *yaml.Node, key string, value *yaml.Node) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"},
		value,
	)
}

// mergeMappingKey merges fields from value into the existing mapping at key.
// If the key does not exist it is added. If value is empty (no scalar
// fields) the existing mapping is preserved as-is.
func mergeMappingKey(mapping *yaml.Node, key string, value *yaml.Node) {
	if mapping == nil || mapping.Kind != yaml.MappingNode || value == nil {
		return
	}
	if value.Kind != yaml.MappingNode || len(value.Content) == 0 {
		return
	}
	// Find existing key.
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			existing := mapping.Content[i+1]
			if existing.Kind != yaml.MappingNode {
				mapping.Content[i+1] = value
				return
			}
			// Splice each {k, v} pair from value into existing, replacing on
			// match.
			for j := 0; j+1 < len(value.Content); j += 2 {
				upsertMappingKey(existing, value.Content[j].Value, value.Content[j+1])
			}
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"},
		value,
	)
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
