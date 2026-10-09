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
	"strconv"
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

// CloneStrategy is the retired per-repository clone override. The key is
// still tolerated on read (see the deprecation notice in ReadDaemonYAML) but
// is never written and never influences behaviour. The constants stay so
// previously written values keep decoding to the same strings.
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
//
// The retired per-repository clone override is not modelled here. Files that
// still carry the key keep loading; ReadDaemonYAML logs one deprecation
// warning per file and the value is ignored. Writers never emit the key.
type ProjectEntry struct {
	// ID is the daemon-side project identifier. The daemon reader requires
	// projects[i].id (see daemon/config.go validateConfig); DeriveProjectID
	// derives it automatically from the repo URL when the caller does not
	// set one (DeriveProjectID).
	ID string `yaml:"id,omitempty" json:"id,omitempty"`
	// RepoURL is the canonical remote URL, e.g. "github.com/foo/bar".
	RepoURL string `yaml:"repository" json:"repository"`
	// CredentialHelper is the credential source for this project.
	// A nil pointer means no credentials are configured (--no-credentials).
	CredentialHelper *CredentialHelper `yaml:"credentialHelper,omitempty" json:"credentialHelper,omitempty"`
}

// RepositoryEntry is one normalized repository resource. Its ProjectID link
// does not grant project admission. The retired clone override is not
// modelled here; see ProjectEntry.
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
	CredentialHelper *CredentialHelper `yaml:"credentialHelper,omitempty" json:"credentialHelper,omitempty"`
}

// UnmarshalYAML accepts either the canonical `repository` key or the
// legacy `repoUrl` key. When the legacy key is found a
// one-line warning is logged via slog so operators know to rewrite the file
// (the next write will use the canonical key automatically).
//
// The retired `cloneStrategy` key is not decoded: files that still carry it
// keep loading and the value is ignored. ReadDaemonYAML logs one deprecation
// warning per file, so this decoder stays silent.
func (p *ProjectEntry) UnmarshalYAML(node *yaml.Node) error {
	var raw struct {
		ID               string            `yaml:"id"`
		Repository       string            `yaml:"repository"`
		RepoURL          string            `yaml:"repoUrl"`
		CredentialHelper *CredentialHelper `yaml:"credentialHelper,omitempty"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	p.ID = raw.ID
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
// daemon.yaml under the `capacity` key. `poolMaxDiskGb` drives
// automatic LRU eviction of the workarea pool once the disk threshold is hit.
type CapacityConfig struct {
	// MaxConcurrentSessions is the maximum number of sessions the local
	// daemon accepts concurrently. 0 means do not accept new sessions.
	MaxConcurrentSessions int `yaml:"maxConcurrentSessions,omitempty" json:"maxConcurrentSessions,omitempty"`
	// PoolMaxDiskGb is the maximum total disk usage (in GiB) for the workarea
	// pool before the daemon starts LRU-evicting cold members.  0 means no limit.
	PoolMaxDiskGb int `yaml:"poolMaxDiskGb,omitempty" json:"poolMaxDiskGb,omitempty"`
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
	// ProjectAdmissionMode is the machine owner's standing consent decision:
	// ProjectAdmissionModeEnumerated (default — admit only EnabledProjectIDs)
	// or ProjectAdmissionModeAllRouted (admit any project the orchestrator
	// routes to this machine). Empty reads as enumerated.
	ProjectAdmissionMode string `yaml:"projectAdmissionMode,omitempty"`
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

// Project admission modes. Mirrors daemon.ProjectAdmissionMode* — this package
// cannot import daemon (daemon imports afclient), so the two constant pairs are
// pinned equal by TestProjectAdmissionModeConstantsMatchDaemon in the daemon
// package.
const (
	// ProjectAdmissionModeEnumerated admits only the enabled project ids.
	ProjectAdmissionModeEnumerated = "enumerated"
	// ProjectAdmissionModeAllRouted admits any project the orchestrator routes
	// to this machine, with no per-project entry.
	ProjectAdmissionModeAllRouted = "all-routed"
)

// NormalizeProjectAdmissionMode canonicalizes a mode string. Anything that is
// not recognizably "all-routed" reads as "enumerated": admission widens only on
// an explicit, correctly spelled opt-in.
func NormalizeProjectAdmissionMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), ProjectAdmissionModeAllRouted) {
		return ProjectAdmissionModeAllRouted
	}
	return ProjectAdmissionModeEnumerated
}

// retiredCloneStrategyWarning is the single deprecation notice logged when
// a read encounters the retired per-repository clone override. The key is
// ignored; the warning tells operators to drop it.
const retiredCloneStrategyWarning = "daemon.yaml: 'cloneStrategy' is retired and ignored; remove the key"

// warnIfRetiredCloneStrategyPresent logs exactly one deprecation warning
// when the raw file still carries the retired key on any entry. The typed
// decoder ignores the key, so detection runs on the raw mapping tree.
func warnIfRetiredCloneStrategyPresent(data []byte) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i]
		if key.Kind != yaml.ScalarNode {
			continue
		}
		if key.Value != "projects" && key.Value != "repositories" {
			continue
		}
		entries := root.Content[i+1]
		if entries.Kind == yaml.AliasNode && entries.Alias != nil {
			entries = entries.Alias
		}
		if entries.Kind != yaml.SequenceNode {
			continue
		}
		for _, entry := range entries.Content {
			item := entry
			if item.Kind == yaml.AliasNode && item.Alias != nil {
				item = item.Alias
			}
			if item.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(item.Content); j += 2 {
				if item.Content[j].Kind == yaml.ScalarNode && item.Content[j].Value == "cloneStrategy" {
					slog.Warn(retiredCloneStrategyWarning)
					return
				}
			}
		}
	}
}

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
	// The retired clone override is not part of the typed schema, so its
	// presence is detected on the raw document: one warning per read no
	// matter how many entries still carry the key.
	warnIfRetiredCloneStrategyPresent(data)
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
	return writeDaemonYAML(path, cfg, nil)
}

type capacityWriteIntent struct {
	key   string
	value int
}

// WriteDaemonYAMLWithCapacity writes the usual config overlay and explicitly
// authors one supported capacity value, including zero. Unlike WriteDaemonYAML,
// this records the operator's intent even when the value would be omitted by
// the exported config's YAML tags.
func WriteDaemonYAMLWithCapacity(path string, cfg *DaemonYAML, key string, value int) error {
	if cfg == nil {
		return fmt.Errorf("daemon config is required")
	}
	var field string
	switch key {
	case "capacity.maxConcurrentSessions":
		field = "maxConcurrentSessions"
	case "capacity.poolMaxDiskGb":
		field = "poolMaxDiskGb"
	default:
		return fmt.Errorf("unsupported capacity key %q", key)
	}
	if value < 0 {
		return fmt.Errorf("%s must be >= 0", key)
	}
	return writeDaemonYAML(path, cfg, &capacityWriteIntent{key: field, value: value})
}

func writeDaemonYAML(path string, cfg *DaemonYAML, intent *capacityWriteIntent) error {
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
	data, err := mergeDaemonYAML(path, &writeCfg, intent)
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
// not exist the cfg struct is marshalled with any explicit capacity intent.
func mergeDaemonYAML(path string, cfg *DaemonYAML, intent *capacityWriteIntent) ([]byte, error) {
	existing, readErr := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if readErr != nil {
		if !errors.Is(readErr, os.ErrNotExist) {
			return nil, fmt.Errorf("read existing config %q: %w", path, readErr)
		}
		// Fresh file — emit the cfg struct directly. The daemon reader
		// will reject this if it lacks machine.id / orchestrator.url; the
		// CLI does not own those fields, so we leave the wizard /
		// installer to populate them.
		return marshalDaemonYAML(cfg, intent)
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
		return marshalDaemonYAML(cfg, intent)
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
		// Only write the mode key when it is the non-default choice, so a file
		// that never opted in stays byte-identical to what it was.
		if cfg.AdmitsAnyRoutedProject() {
			modeNode, modeErr := encodeYAMLNode(ProjectAdmissionModeAllRouted)
			if modeErr != nil {
				return nil, fmt.Errorf("encode project admission mode: %w", modeErr)
			}
			upsertMappingKey(doc, "projectAdmissionMode", modeNode)
		} else {
			deleteMappingKey(doc, "projectAdmissionMode")
		}
	}
	upsertMappingKey(doc, "projects", projectsNode)
	// The retired clone override must not survive a write: a file that
	// still carries it loses the key on the next write, entry by entry.
	// The merge splices cfg-owned keys wholesale, so only unowned leftovers
	// can still hold the key — drop it wherever it remains.
	stripRetiredCloneStrategyKey(doc)
	// Capacity is preserved as a partial overlay — only the cfg-modelled
	// fields (e.g. poolMaxDiskGb) are merged into the existing capacity
	// mapping. If no capacity key exists yet a new one is added.
	if intent != nil {
		upsertMappingKey(capacityNode, intent.key, capacityIntentNode(intent))
		// Preserve alias inheritance without mutating the shared anchor. The
		// new local mapping overrides only the explicitly authored capacity.
		for i := 0; i+1 < len(doc.Content); i += 2 {
			if doc.Content[i].Value == "capacity" && doc.Content[i+1].Kind == yaml.AliasNode {
				doc.Content[i+1] = &yaml.Node{
					Kind: yaml.MappingNode, Tag: "!!map",
					Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!merge", Value: "<<"}, doc.Content[i+1]},
				}
				break
			}
		}
	}
	mergeMappingKey(doc, "capacity", capacityNode)

	out, err := yaml.Marshal(&root)
	if err != nil {
		return nil, fmt.Errorf("marshal merged config: %w", err)
	}
	return out, nil
}

// stripRetiredCloneStrategyKey removes the retired per-repository clone
// override from every entry under the projects and repositories sequences.
// Writers never emit the key; this scrubs copies the merge preserved from
// the on-disk file (e.g. a repositories block the project command tree does
// not own in legacy files).
func stripRetiredCloneStrategyKey(doc *yaml.Node) {
	if doc == nil || doc.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(doc.Content); i += 2 {
		key := doc.Content[i]
		if key.Kind != yaml.ScalarNode {
			continue
		}
		if key.Value != "projects" && key.Value != "repositories" {
			continue
		}
		entries := doc.Content[i+1]
		if entries.Kind == yaml.AliasNode && entries.Alias != nil {
			entries = entries.Alias
		}
		if entries.Kind != yaml.SequenceNode {
			continue
		}
		for _, entry := range entries.Content {
			item := entry
			if item.Kind == yaml.AliasNode && item.Alias != nil {
				item = item.Alias
			}
			if item.Kind != yaml.MappingNode {
				continue
			}
			deleteMappingKey(item, "cloneStrategy")
		}
	}
}

func capacityIntentNode(intent *capacityWriteIntent) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(intent.value)}
}

func marshalDaemonYAML(cfg *DaemonYAML, intent *capacityWriteIntent) ([]byte, error) {
	if intent == nil {
		return yaml.Marshal(cfg)
	}
	root, err := encodeYAMLNode(cfg)
	if err != nil {
		return nil, err
	}
	capacity, err := encodeYAMLNode(cfg.Capacity)
	if err != nil {
		return nil, err
	}
	upsertMappingKey(capacity, intent.key, capacityIntentNode(intent))
	upsertMappingKey(root, "capacity", capacity)
	return yaml.Marshal(root)
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

// deleteMappingKey removes the given key (and its value) from the mapping node
// if present. Used to clear an opt-in key when the operator opts back out —
// leaving a stale `projectAdmissionMode: all-routed` behind would keep granting
// consent the operator just withdrew.
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
				// The capacity leaf owns its anchor definition. Retain it on
				// replacement so aliases remain valid and follow the new value.
				// A borrowed alias has no local definition to transfer.
				for k := 0; k+1 < len(existing.Content); k += 2 {
					if existing.Content[k].Value == value.Content[j].Value && existing.Content[k+1].Kind == yaml.ScalarNode {
						value.Content[j+1].Anchor = existing.Content[k+1].Anchor
						break
					}
				}
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

// SetProjectAdmissionMode records the standing consent mode. Setting it also
// migrates the file to admission v2, because the mode is only meaningful once
// enabledProjectIds is authoritative.
func (d *DaemonYAML) SetProjectAdmissionMode(mode string) {
	d.migrateProjectAdmissionV2()
	d.ProjectAdmissionMode = NormalizeProjectAdmissionMode(mode)
}

// EffectiveProjectAdmissionMode returns the normalized standing consent mode.
func (d *DaemonYAML) EffectiveProjectAdmissionMode() string {
	return NormalizeProjectAdmissionMode(d.ProjectAdmissionMode)
}

// AdmitsAnyRoutedProject reports whether every routed project is admitted
// without a per-project entry.
func (d *DaemonYAML) AdmitsAnyRoutedProject() bool {
	return d.EffectiveProjectAdmissionMode() == ProjectAdmissionModeAllRouted
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
