// Package afclient daemon_config.go — read/write ~/.donmai/daemon.yaml.
//
// The file is the source-of-truth for the daemon's project allowlist and
// credential configuration. The running daemon reloads on SIGHUP or restart;
// af project commands write atomically (tmp file + rename) to avoid corrupting
// the file while the daemon is live.
package afclient

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

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
// for this project until credentials are added via `donmai project credentials`.
type CredentialHelper struct {
	Kind       CredentialHelperKind `yaml:"kind,omitempty"       json:"kind,omitempty"`
	SSHKeyPath string               `yaml:"sshKeyPath,omitempty" json:"sshKeyPath,omitempty"`
	EnvVarName string               `yaml:"envVarName,omitempty" json:"envVarName,omitempty"`
}

// ProjectEntry is one entry in the daemon.yaml `projects` list.
//
// The yaml key for the repo URL is `repository`, matching the daemon-side
// reader (daemon.ProjectConfig). A past release renamed this from `repoUrl` to
// align writer + reader after a schema-drift bug where the writer emitted
// `repoUrl` but the reader looked for `repository`, causing
// `rensei daemon stats` to report `Projects: 0 allowed` after a successful
// `rensei project allow`. The Go field is still RepoURL for source-compat.
//
// On read, ProjectEntry tolerates the legacy `repoUrl` key for one cycle
// (see UnmarshalYAML below) so pre-fix files in the wild still load.
type ProjectEntry struct {
	// ID is the daemon-side project identifier. The daemon reader requires
	// projects[i].id (see daemon/config.go validateConfig); DeriveProjectID
	// derives it automatically from the repo URL when the caller does not
	// set one (DeriveProjectID).
	ID string `yaml:"id,omitempty" json:"id,omitempty"`
	// RepoURL is the canonical remote URL, e.g. "github.com/foo/bar".
	RepoURL string `yaml:"repository" json:"repository"`
	// CloneStrategy controls how the daemon clones the repo. Default: shallow.
	CloneStrategy CloneStrategy `yaml:"cloneStrategy,omitempty" json:"cloneStrategy,omitempty"`
	// CredentialHelper is the credential source for this project.
	// A nil pointer means no credentials are configured (--no-credentials).
	CredentialHelper *CredentialHelper `yaml:"credentialHelper,omitempty" json:"credentialHelper,omitempty"`
}

// RepositoryEntry is one normalized repository resource. Its ProjectID link
// does not grant project admission.
type RepositoryEntry struct {
	// ID is the durable repository-row identifier. It remains distinct from
	// PathID so consumers retain database metadata without confusing it for the
	// provider-opaque repository identity used by bound calls.
	ID string `yaml:"id" json:"id"`
	// PathID is the provider-opaque repository identity used to bind a resource
	// to calls. For example, a GitHub repository may use "github:owner/repo".
	PathID           string            `yaml:"pathId,omitempty"           json:"pathId,omitempty"`
	ProjectID        string            `yaml:"projectId"                  json:"projectId"`
	Source           string            `yaml:"source"                     json:"source"`
	Primary          bool              `yaml:"primary,omitempty"          json:"primary,omitempty"`
	CloneStrategy    CloneStrategy     `yaml:"cloneStrategy,omitempty"    json:"cloneStrategy,omitempty"`
	CredentialHelper *CredentialHelper `yaml:"credentialHelper,omitempty" json:"credentialHelper,omitempty"`
}

// UnmarshalYAML accepts either the canonical `repository` key or the
// legacy `repoUrl` key. When the legacy key is found a
// one-line warning is logged via slog so operators know to rewrite the file
// (the next write will use the canonical key automatically).
func (p *ProjectEntry) UnmarshalYAML(node *yaml.Node) error {
	var raw struct {
		ID               string            `yaml:"id"`
		Repository       string            `yaml:"repository"`
		RepoURL          string            `yaml:"repoUrl"`
		CloneStrategy    CloneStrategy     `yaml:"cloneStrategy,omitempty"`
		CredentialHelper *CredentialHelper `yaml:"credentialHelper,omitempty"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	p.ID = raw.ID
	p.CloneStrategy = raw.CloneStrategy
	p.CredentialHelper = raw.CredentialHelper
	switch {
	case raw.Repository != "":
		p.RepoURL = raw.Repository
	case raw.RepoURL != "":
		p.RepoURL = raw.RepoURL
		slog.Warn(
			"daemon.yaml: legacy 'repoUrl' key on project entry; will be rewritten as 'repository' on next write",
			"repoUrl", raw.RepoURL,
		)
	}
	return nil
}

// CapacityConfig holds the configurable capacity limits written into
// daemon.yaml under the `capacity` key. `workareaMaxDiskGb` drives
// automatic LRU eviction of the warm workarea cache once the disk threshold is
// hit.
type CapacityConfig struct {
	// MaxConcurrentSessions is the maximum number of sessions the local
	// daemon accepts concurrently. 0 means do not accept new sessions.
	MaxConcurrentSessions int `yaml:"maxConcurrentSessions,omitempty" json:"maxConcurrentSessions,omitempty"`
	// PoolMaxDiskGb is the maximum total disk usage (in GiB) for the warm
	// workarea cache before the daemon starts LRU-evicting cold members.
	// 0 means no limit.
	//
	// The Go field name is retained deliberately — downstream embedders
	// assign to it — while the serialized key is the renamed
	// `workareaMaxDiskGb`. The previous `poolMaxDiskGb` key is still read;
	// see UnmarshalYAML.
	PoolMaxDiskGb int `yaml:"workareaMaxDiskGb,omitempty" json:"workareaMaxDiskGb,omitempty"`
}

// UnmarshalYAML reads the workarea disk envelope from either the current
// `workareaMaxDiskGb` key or the deprecated `poolMaxDiskGb` alias, removed in
// WorkareaAliasRemovalVersion.
//
// This alias has to live on the struct rather than in a CLI key table: the
// decoder is non-strict, so an unrecognised key is dropped in silence, and the
// dropped field defaults to 0 — which this setting defines as "no limit".
// A CLI-level-only alias would therefore turn LRU eviction off on every machine
// whose daemon.yaml predates the rename, and fill the disk.
func (c *CapacityConfig) UnmarshalYAML(node *yaml.Node) error {
	var raw struct {
		MaxConcurrentSessions int `yaml:"maxConcurrentSessions"`
		WorkareaMaxDiskGb     int `yaml:"workareaMaxDiskGb"`
		PoolMaxDiskGb         int `yaml:"poolMaxDiskGb"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	c.MaxConcurrentSessions = raw.MaxConcurrentSessions
	switch {
	case raw.WorkareaMaxDiskGb != 0:
		c.PoolMaxDiskGb = raw.WorkareaMaxDiskGb
	case raw.PoolMaxDiskGb != 0:
		c.PoolMaxDiskGb = raw.PoolMaxDiskGb
		slog.Warn(
			"daemon.yaml: "+DeprecatedSurfaceNotice(
				legacyWorkareaMaxDiskGbYAMLKey, workareaMaxDiskGbYAMLKey)+
				" It is rewritten on the next write.",
			legacyWorkareaMaxDiskGbYAMLKey, raw.PoolMaxDiskGb,
		)
	default:
		c.PoolMaxDiskGb = 0
	}
	return nil
}

// DaemonYAML is the in-memory representation of ~/.donmai/daemon.yaml.
// Only the fields relevant to the project command tree are modelled here;
// unknown top-level keys are preserved via the yaml decoder's pass-through.
type DaemonYAML struct {
	// ProjectAdmissionVersion distinguishes the legacy repository-derived
	// contract (absent) from explicit v2 admission.
	ProjectAdmissionVersion int `yaml:"projectAdmissionVersion,omitempty"`
	// EnabledProjectIDs is the authoritative project-admission set. It is
	// independent of repository resources so a project can be enabled before
	// any repository is configured. It is authoritative only in v2.
	EnabledProjectIDs []string `yaml:"enabledProjectIds,omitempty"`
	// Repositories is the normalized zero-to-many repository-resource set.
	Repositories []RepositoryEntry `yaml:"repositories,omitempty"`
	// Projects contains repository resources. Multiple entries may share a
	// project ID.
	Projects []ProjectEntry `yaml:"projects,omitempty"`
	// Capacity holds the configurable resource limits for the daemon.
	Capacity CapacityConfig `yaml:"capacity,omitempty"`
}

// ProjectAdmissionVersionV2 marks enabledProjectIds as authoritative.
const ProjectAdmissionVersionV2 = 2

// ── default path ─────────────────────────────────────────────────────────────

// DefaultDaemonYAMLPath returns the canonical path to daemon.yaml, expanding ~.
func DefaultDaemonYAMLPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "~/.donmai/daemon.yaml"
	}
	return filepath.Join(home, ".donmai", "daemon.yaml")
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
	cfg.normalizeProjectContract()
	return &cfg, nil
}

// WriteDaemonYAML atomically writes cfg to path (tmp file + rename).
// The parent directory is created with 0o700 if it does not exist.
//
// The writer preserves any unknown top-level keys present in the existing
// file (e.g. apiVersion, kind, machine, orchestrator, autoUpdate,
// observability) by parsing the on-disk file as a yaml.Node tree, replacing
// only the `projects` and `capacity` mappings, and re-marshalling. This is
// the v0.4.1 follow-up: the previous writer marshalled the
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

	// Auto-derive ID for any project entry that does not have one. The
	// daemon reader treats projects[i].id as required; CLI callers (e.g.
	// `rensei project allow <repo>`) historically left it unset, so the
	// daemon rejected the resulting file at next read.
	for i := range cfg.Projects {
		if cfg.Projects[i].ID == "" {
			cfg.Projects[i].ID = DeriveProjectID(cfg.Projects[i].RepoURL)
		}
	}
	cfg.normalizeProjectContract()

	writeCfg := *cfg
	if writeCfg.ProjectAdmissionVersion == 0 {
		writeCfg.EnabledProjectIDs = nil
		writeCfg.Repositories = nil
	} else {
		writeCfg.syncLegacyProjectProjection()
	}
	data, err := mergeDaemonYAML(path, &writeCfg)
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
// yaml.Node tree, replaces the project-admission, projects, and capacity keys
// with the values from cfg, and returns the marshalled result. When the file does
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
	repositoriesNode, err := encodeYAMLNode(cfg.Repositories)
	if err != nil {
		return nil, fmt.Errorf("encode repositories: %w", err)
	}
	enabledProjectIDsNode, err := encodeYAMLNode(cfg.EnabledProjectIDs)
	if err != nil {
		return nil, fmt.Errorf("encode enabled project ids: %w", err)
	}
	capacityNode, err := encodeYAMLNode(cfg.Capacity)
	if err != nil {
		return nil, fmt.Errorf("encode capacity: %w", err)
	}

	if cfg.ProjectAdmissionVersion != 0 {
		projectAdmissionVersionNode, versionErr := encodeYAMLNode(cfg.ProjectAdmissionVersion)
		if versionErr != nil {
			return nil, fmt.Errorf("encode project admission version: %w", versionErr)
		}
		upsertMappingKey(doc, "projectAdmissionVersion", projectAdmissionVersionNode)
	}
	if cfg.ProjectAdmissionVersion == ProjectAdmissionVersionV2 {
		upsertMappingKey(doc, "enabledProjectIds", enabledProjectIDsNode)
		upsertMappingKey(doc, "repositories", repositoriesNode)
	}
	upsertMappingKey(doc, "projects", projectsNode)
	// Capacity is preserved as a partial overlay — only the cfg-modelled
	// fields (e.g. workareaMaxDiskGb) are merged into the existing capacity
	// mapping. If no capacity key exists yet a new one is added.
	mergeMappingKey(doc, "capacity", capacityNode)
	// Migrate the deprecated `poolMaxDiskGb` spelling off disk, but only once
	// its value has actually been re-emitted under the current name. Deleting
	// it unconditionally would discard the operator's disk cap whenever the
	// value happens to be absent from cfg.
	if mappingHasKey(findMappingValue(doc, "capacity"), workareaMaxDiskGbYAMLKey) {
		deleteMappingKey(findMappingValue(doc, "capacity"), legacyWorkareaMaxDiskGbYAMLKey)
	}

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
	mapping.Content = append(
		mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"},
		value,
	)
}

// findMappingValue returns the value node stored at key in mapping, or nil.
func findMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// mappingHasKey reports whether mapping carries key.
func mappingHasKey(mapping *yaml.Node, key string) bool {
	return findMappingValue(mapping, key) != nil
}

// deleteMappingKey removes the {key, value} pair from mapping if present.
func deleteMappingKey(mapping *yaml.Node, key string) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
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
	mapping.Content = append(
		mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"},
		value,
	)
}

// ── project-list helpers ──────────────────────────────────────────────────────

// FindProject returns the index of the first ProjectEntry whose RepoURL
// matches repoURL, or -1 if not found.
func (d *DaemonYAML) FindProject(repoURL string) int {
	d.normalizeProjectContract()
	d.syncLegacyProjectProjection()
	for i, p := range d.Projects {
		if p.RepoURL == repoURL {
			return i
		}
	}
	return -1
}

// FindRepository returns the normalized repository-resource index by source.
func (d *DaemonYAML) FindRepository(source string) int {
	d.normalizeProjectContract()
	key := normalizeRepositoryEntrySource(source)
	for i, repository := range d.Repositories {
		if normalizeRepositoryEntrySource(repository.Source) == key {
			return i
		}
	}
	return -1
}

// RepositoryProjectEntries returns repository resources in the legacy table
// shape used by compatibility CLI renderers.
func (d *DaemonYAML) RepositoryProjectEntries() []ProjectEntry {
	d.syncLegacyProjectProjection()
	return append([]ProjectEntry(nil), d.Projects...)
}

// AddOrUpdateProject upserts a ProjectEntry by RepoURL.
// If a matching entry exists it is replaced; otherwise the entry is appended.
func (d *DaemonYAML) AddOrUpdateProject(entry ProjectEntry) {
	explicitProjectID := strings.TrimSpace(entry.ID) != ""
	if entry.ID == "" {
		entry.ID = DeriveProjectID(entry.RepoURL)
	}
	d.migrateProjectAdmissionV2()
	existingIndex := d.FindRepository(entry.RepoURL)
	if existingIndex >= 0 && !explicitProjectID {
		entry.ID = d.Repositories[existingIndex].ProjectID
	}
	repository := RepositoryEntry{
		ID:               deriveRepositoryID(entry.ID, entry.RepoURL),
		ProjectID:        entry.ID,
		Source:           entry.RepoURL,
		CloneStrategy:    entry.CloneStrategy,
		CredentialHelper: entry.CredentialHelper,
	}
	if existingIndex >= 0 {
		repository.ID = d.Repositories[existingIndex].ID
		d.Repositories[existingIndex] = repository
	} else {
		d.Repositories = append(d.Repositories, repository)
	}
	d.EnableProject(entry.ID)
	d.syncLegacyProjectProjection()
}

// SetRepositoryCredentialHelper updates a repository resource by source. A
// successful update migrates legacy project configuration to the v2 contract
// and refreshes the compatibility projection.
func (d *DaemonYAML) SetRepositoryCredentialHelper(source string, helper *CredentialHelper) bool {
	d.migrateProjectAdmissionV2()
	i := d.FindRepository(source)
	if i < 0 {
		return false
	}
	d.Repositories[i].CredentialHelper = helper
	d.syncLegacyProjectProjection()
	return true
}

// RemoveProject removes the entry matching repoURL.
// Returns true if an entry was removed, false if none matched.
func (d *DaemonYAML) RemoveProject(repoURL string) bool {
	d.migrateProjectAdmissionV2()
	i := d.FindRepository(repoURL)
	if i < 0 {
		return false
	}
	removedID := d.Repositories[i].ProjectID
	d.Repositories = append(d.Repositories[:i], d.Repositories[i+1:]...)
	for _, repository := range d.Repositories {
		if repository.ProjectID == removedID {
			d.syncLegacyProjectProjection()
			return true
		}
	}
	d.DisableProject(removedID)
	d.syncLegacyProjectProjection()
	return true
}

// EnableProject adds id to the project-admission set idempotently.
func (d *DaemonYAML) EnableProject(id string) {
	d.migrateProjectAdmissionV2()
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	for _, existing := range d.EnabledProjectIDs {
		if existing == id {
			d.syncLegacyProjectProjection()
			return
		}
	}
	d.EnabledProjectIDs = append(d.EnabledProjectIDs, id)
	d.normalizeProjectContract()
	d.syncLegacyProjectProjection()
}

// DisableProject removes id from the project-admission set without deleting
// its repository resources.
func (d *DaemonYAML) DisableProject(id string) {
	d.migrateProjectAdmissionV2()
	filtered := make([]string, 0, len(d.EnabledProjectIDs))
	for _, existing := range d.EnabledProjectIDs {
		if existing != id {
			filtered = append(filtered, existing)
		}
	}
	d.EnabledProjectIDs = filtered
	d.syncLegacyProjectProjection()
}

// IsProjectEnabled reports whether id is in the project-admission set.
func (d *DaemonYAML) IsProjectEnabled(id string) bool {
	d.normalizeProjectContract()
	for _, existing := range d.EnabledProjectIDs {
		if existing == id {
			return true
		}
	}
	return false
}

func (d *DaemonYAML) normalizeProjectContract() {
	legacy := d.ProjectAdmissionVersion == 0
	seen := make(map[string]struct{}, len(d.EnabledProjectIDs)+len(d.Projects))
	ids := make([]string, 0, len(d.EnabledProjectIDs)+len(d.Projects))
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, id := range d.EnabledProjectIDs {
		add(id)
	}
	if legacy {
		for _, project := range d.Projects {
			projectID := project.ID
			if strings.TrimSpace(projectID) == "" {
				projectID = DeriveProjectID(project.RepoURL)
			}
			add(projectID)
		}
	}
	slices.Sort(ids)
	if ids == nil {
		ids = []string{}
	}
	d.EnabledProjectIDs = ids
	d.Repositories = normalizeRepositoryEntries(d.Repositories, d.Projects)
}

func (d *DaemonYAML) migrateProjectAdmissionV2() {
	d.normalizeProjectContract()
	d.ProjectAdmissionVersion = ProjectAdmissionVersionV2
}

func normalizeRepositoryEntries(v2 []RepositoryEntry, legacy []ProjectEntry) []RepositoryEntry {
	byKey := make(map[string]RepositoryEntry, len(v2)+len(legacy))
	order := make([]string, 0, len(v2)+len(legacy))
	add := func(repository RepositoryEntry, wins bool) {
		repository.ProjectID = strings.TrimSpace(repository.ProjectID)
		repository.Source = strings.TrimSpace(repository.Source)
		if repository.ProjectID == "" || repository.Source == "" {
			return
		}
		if repository.ID == "" {
			repository.ID = deriveRepositoryID(repository.ProjectID, repository.Source)
		}
		if repository.CloneStrategy == "" {
			repository.CloneStrategy = CloneShallow
		}
		key := repository.ProjectID + "\x00" + normalizeRepositoryEntrySource(repository.Source)
		if _, exists := byKey[key]; exists && !wins {
			return
		}
		if _, exists := byKey[key]; !exists {
			order = append(order, key)
		}
		byKey[key] = repository
	}
	for _, project := range legacy {
		projectID := project.ID
		if strings.TrimSpace(projectID) == "" {
			projectID = DeriveProjectID(project.RepoURL)
		}
		add(RepositoryEntry{
			ID:               deriveRepositoryID(projectID, project.RepoURL),
			ProjectID:        projectID,
			Source:           project.RepoURL,
			CloneStrategy:    project.CloneStrategy,
			CredentialHelper: project.CredentialHelper,
		}, false)
	}
	for _, repository := range v2 {
		add(repository, true)
	}
	slices.Sort(order)
	out := make([]RepositoryEntry, 0, len(order))
	for _, key := range order {
		out = append(out, byKey[key])
	}
	return out
}

func normalizeRepositoryEntrySource(source string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(source), "/"), ".git"))
}

func deriveRepositoryID(projectID, source string) string {
	sum := sha256.Sum256([]byte(projectID + "\x00" + normalizeRepositoryEntrySource(source)))
	return fmt.Sprintf("repo-%x", sum[:6])
}

func (d *DaemonYAML) syncLegacyProjectProjection() {
	if d.ProjectAdmissionVersion != ProjectAdmissionVersionV2 {
		return
	}
	enabled := make(map[string]struct{}, len(d.EnabledProjectIDs))
	for _, id := range d.EnabledProjectIDs {
		enabled[id] = struct{}{}
	}
	projects := make([]ProjectEntry, 0, len(d.Repositories))
	for _, repository := range d.Repositories {
		if _, ok := enabled[repository.ProjectID]; !ok {
			continue
		}
		projects = append(projects, ProjectEntry{
			ID:               repository.ProjectID,
			RepoURL:          repository.Source,
			CloneStrategy:    repository.CloneStrategy,
			CredentialHelper: repository.CredentialHelper,
		})
	}
	d.Projects = projects
}

// derivedIDCleanRE matches any character outside [a-z0-9-] for sanitisation.
var derivedIDCleanRE = regexp.MustCompile(`[^a-z0-9-]`)

// derivedIDRepeatRE collapses runs of "-" produced by the cleanup pass.
var derivedIDRepeatRE = regexp.MustCompile(`-+`)

// DeriveProjectID returns a stable, daemon-acceptable id derived from a
// repo URL. The daemon validates that projects[i].id is non-empty; the CLI
// did not historically write this field, so daemon.yaml files written by
// `rensei project allow` were rejected at next read with
// "projects[0].id is required".
//
// Heuristics:
//   - github.com/foo/bar  → "foo-bar"
//   - https://github.com/foo/bar.git → "foo-bar"
//   - git@github.com:foo/bar.git → "foo-bar"
//   - bare path "bar" → "bar"
//
// Output is lowercased, ASCII-safe (a-z0-9-) and trimmed of leading/trailing
// hyphens. An empty input yields "project".
func DeriveProjectID(repoURL string) string {
	s := strings.ToLower(strings.TrimSpace(repoURL))
	if s == "" {
		return "project"
	}
	// Strip trailing .git
	s = strings.TrimSuffix(s, ".git")
	// Convert SSH-style git@host:owner/repo to host/owner/repo.
	if i := strings.Index(s, "@"); i >= 0 && strings.Contains(s, ":") && !strings.Contains(s, "://") {
		s = strings.Replace(s[i+1:], ":", "/", 1)
	}
	// Strip protocol.
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	// Take the last two path segments (owner/repo) when present.
	parts := strings.Split(s, "/")
	if len(parts) >= 2 {
		s = parts[len(parts)-2] + "-" + parts[len(parts)-1]
	} else {
		s = parts[len(parts)-1]
	}
	// Sanitise to a-z0-9-
	s = derivedIDCleanRE.ReplaceAllString(s, "-")
	s = derivedIDRepeatRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "project"
	}
	return s
}
