package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/statepath"
	"github.com/RenseiAI/donmai/runner/access"

	"gopkg.in/yaml.v3"
)

// Config is the in-memory representation of ~/.donmai/daemon.yaml. The wire
// schema mirrors the TS DaemonConfig (donmai-architecture/004 §Configuration
// shape).
type Config struct {
	// LocalRuntime selects a GitHub-only, exact-host file queue profile.
	LocalRuntime            *LocalRuntimeConfig `yaml:"localRuntime,omitempty" json:"localRuntime,omitempty"`
	APIVersion              string              `yaml:"apiVersion"                       json:"apiVersion"`
	Kind                    string              `yaml:"kind"                             json:"kind"`
	ProjectAdmissionVersion int                 `yaml:"projectAdmissionVersion,omitempty" json:"projectAdmissionVersion,omitempty"`
	Machine                 MachineConfig       `yaml:"machine"                  json:"machine"`
	Capacity                CapacityConfig      `yaml:"capacity"                 json:"capacity"`
	// EnabledProjectIDs is the authoritative project-admission set. A
	// project may be admitted before it has any repository resources.
	// Legacy projects[] entries are projected only when
	// ProjectAdmissionVersion is absent. Version 2 makes this set authoritative,
	// including when it is empty.
	EnabledProjectIDs []string `yaml:"enabledProjectIds,omitempty" json:"enabledProjectIds,omitempty"`
	// ProjectAdmissionMode selects HOW EnabledProjectIDs is interpreted, and is
	// the machine owner's standing consent decision rather than a per-project
	// one.
	//
	//	"enumerated" (default, and the semantics every pre-existing config keeps)
	//	    admit exactly the projects in EnabledProjectIDs and nothing else. The
	//	    owner re-consents once per project.
	//	"all-routed"
	//	    admit any project the orchestrator dispatches to this machine. The
	//	    owner consents ONCE — "I trust my organization's routing to decide
	//	    what runs here" — and never edits this list again.
	//
	// The org boundary still holds under "all-routed": the daemon's
	// registration token is org-scoped, so the only work that can ever reach
	// AcceptWork is work the operator's own control plane routed to a pool this
	// machine belongs to. What the mode drops is the SECOND enumeration of an
	// intent the operator already declared upstream — not the consent itself.
	//
	// Empty string means "enumerated"; EffectiveProjectAdmissionMode() is the
	// only correct reader.
	ProjectAdmissionMode string             `yaml:"projectAdmissionMode,omitempty" json:"projectAdmissionMode,omitempty"`
	Repositories         []RepositoryConfig `yaml:"repositories,omitempty"      json:"repositories,omitempty"`
	// Projects is the legacy compatibility projection. Version 2 readers use
	// Repositories; writers retain enabled repository-bearing entries here for
	// one mixed-version window.
	Projects      []ProjectConfig      `yaml:"projects,omitempty"     json:"projects,omitempty"`
	Orchestrator  OrchestratorConfig   `yaml:"orchestrator"           json:"orchestrator"`
	AutoUpdate    AutoUpdateConfig     `yaml:"autoUpdate"             json:"autoUpdate"`
	Observability *ObservabilityConfig `yaml:"observability,omitempty" json:"observability,omitempty"`
	// ModelAccess is the platform-synced per-machine + per-workload
	// model-access narrowing block (P3 / ADR-2026-06-06 D5). nil = no
	// machine narrowing => the platform ceiling holds unchanged (identity).
	// Written by the modelAccess.set / modelAccess.clear daemon mutations
	// (mutation_apply.go); read by the downstream UI fail-closed gate one step
	// before the credential hop. Policy/routing only — NEVER credentials.
	// The type lives in runner/access so the enforcement mirror and the
	// daemon read the same struct (daemon -> runner/access, one-way; no
	// cycle). Mirrors the Observability optional-block slot above.
	ModelAccess *access.ModelAccessConfig `yaml:"modelAccess,omitempty" json:"modelAccess,omitempty"`
	// Workarea holds Layer-3 workarea-surface tunables (archive root,
	// diff streaming threshold). Optional; populated with defaults if
	// absent.
	Workarea WorkareaConfig `yaml:"workarea,omitempty"     json:"workarea,omitempty"`
	// Kit holds Layer-4 kit-surface tunables (scan paths). Optional;
	// applyDefaults seeds ScanPaths to [DefaultKitScanPath()] when
	// absent. Per ADR-2026-05-07 § D4.
	Kit KitConfig `yaml:"kit,omitempty"          json:"kit,omitempty"`
	// Trust holds the daemon-wide signature-verification policy
	// (sigstore bundle-mode verifier mode + issuer allowlist + audit
	// actor). Optional; applyDefaults seeds Mode via
	// resolveDefaultTrustMode — TrustModeSignedByAllowlist unless another
	// non-permissive environment mode is selected. Per
	// 002-provider-base-contract.md § "Signing and trust". Lives on
	// Config (not on KitConfig) because the trust mode applies across
	// all plugin families per 015-plugin-spec.md § "Auth + trust".
	Trust TrustConfig `yaml:"trust,omitempty"        json:"trust,omitempty"`
	// unknownFields holds top-level daemon.yaml keys the Config struct
	// does not declare, captured verbatim at load time and merged back
	// at write time so embedding binaries can share the file without
	// losing their own settings on the next write. Declared fields
	// always win: keys also present in the marshaled Config are skipped.
	unknownFields map[string]*yaml.Node
}

// LocalRuntimeConfig is non-secret operator policy. Credentials remain in
// standalone credential sources and protected local files, never this block.
type LocalRuntimeConfig struct {
	ExecutionSecurity    *LocalExecutionSecurity `yaml:"executionSecurity" json:"executionSecurity"`
	Harness              string                  `yaml:"harness" json:"harness"`
	Model                string                  `yaml:"model" json:"model"`
	ModelAuthor          string                  `yaml:"modelAuthor" json:"modelAuthor"`
	ModelCatalogRevision string                  `yaml:"modelCatalogRevision,omitempty" json:"modelCatalogRevision,omitempty"`
	Repositories         []LocalGitHubRepository `yaml:"repositories" json:"repositories"`
}

// LocalRuntimeConfigAPIVersion identifies an own-policy local file controller.
// Its security policy is required; absence is never a compatibility default.
const LocalRuntimeConfigAPIVersion = "donmai.dev/v2"

// LocalExecutionSecurity is the explicit outermost local policy. The schema
// version is Config.APIVersion; this object contains only six authored levels.
type LocalExecutionSecurity struct {
	ToolApproval agent.ExecutionSecurityLevel `yaml:"toolApproval" json:"toolApproval"`
	FileRead     agent.ExecutionSecurityLevel `yaml:"fileRead" json:"fileRead"`
	FileWrite    agent.ExecutionSecurityLevel `yaml:"fileWrite" json:"fileWrite"`
	Network      agent.ExecutionSecurityLevel `yaml:"network" json:"network"`
	Credentials  agent.ExecutionSecurityLevel `yaml:"credentials" json:"credentials"`
	Isolation    agent.ExecutionSecurityLevel `yaml:"isolation" json:"isolation"`
}

// InitialLocalExecutionSecurity is the visible first-install/known-version
// upgrade seed prescribed by the execution-security contract. Only installers
// and the explicit startup migration may persist it. Readers/issuers must NOT
// call it to fill a missing policy or dimension.
func InitialLocalExecutionSecurity() *LocalExecutionSecurity {
	return &LocalExecutionSecurity{ToolApproval: agent.ToolApprovalBypass, FileRead: agent.FileReadHost, FileWrite: agent.FileWriteHost, Network: agent.NetworkOpen, Credentials: agent.CredentialsAmbientHostLogin, Isolation: agent.IsolationHostUser}
}

// Levels returns the authored values without filling any missing dimension.
func (s LocalExecutionSecurity) Levels() agent.ExecutionSecurityLevels {
	return agent.ExecutionSecurityLevels{ToolApproval: s.ToolApproval, FileRead: s.FileRead, FileWrite: s.FileWrite, Network: s.Network, Credentials: s.Credentials, Isolation: s.Isolation}
}

// Validate refuses an absent own value before checking the closed vocabulary.
func (s *LocalExecutionSecurity) Validate() error {
	if s == nil {
		return &agent.ExecutionSecurityError{Code: agent.ExecutionSecurityUnconfigured, Detail: "local outermost executionSecurity is required"}
	}
	levels := s.Levels()
	for _, dimension := range agent.ExecutionSecurityDimensions() {
		if levels.Level(dimension) == "" {
			return &agent.ExecutionSecurityError{Code: agent.ExecutionSecurityUnconfigured, Dimension: dimension, Detail: "local outermost dimension is missing"}
		}
	}
	return levels.Validate()
}

// UnmarshalJSON is closed: own configuration does not ignore unknown levels,
// unknown dimensions or duplicates, and never obtains values by omission.
func (s *LocalExecutionSecurity) UnmarshalJSON(raw []byte) error {
	if _, err := executioncell.NormalizeOperationalPayload(raw); err != nil {
		return &agent.ExecutionSecurityError{Code: agent.ExecutionSecurityUnresolvable, Detail: "local executionSecurity must be a unique-key object"}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, dimension := range agent.ExecutionSecurityDimensions() {
		value, exists := fields[string(dimension)]
		if !exists || string(value) == "null" {
			return &agent.ExecutionSecurityError{Code: agent.ExecutionSecurityUnconfigured, Dimension: dimension, Detail: "local outermost dimension is missing"}
		}
	}
	wrapper := append([]byte(`{"version":1,"levels":`), raw...)
	wrapper = append(wrapper, '}')
	parsed, err := agent.ParseExecutionSecurity(wrapper)
	if err != nil {
		return err
	}
	value := parsed.Levels
	*s = LocalExecutionSecurity{ToolApproval: value.ToolApproval, FileRead: value.FileRead, FileWrite: value.FileWrite, Network: value.Network, Credentials: value.Credentials, Isolation: value.Isolation}
	return s.Validate()
}

// UnmarshalYAML shares the closed JSON vocabulary rather than YAML's default
// unknown-field dropping behavior.
func (s *LocalExecutionSecurity) UnmarshalYAML(node *yaml.Node) error {
	var fields map[string]any
	if err := node.Decode(&fields); err != nil {
		return err
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	return s.UnmarshalJSON(raw)
}

// ValidateLocalExecutionSecurity reads the local controller's own scope. The
// legacy version is handled only by the explicit startup migration, not here.
func ValidateLocalExecutionSecurity(c *Config) error {
	if c == nil || c.APIVersion != LocalRuntimeConfigAPIVersion {
		return &agent.ExecutionSecurityError{Code: agent.ExecutionSecurityUnconfigured, Detail: "local file controller requires explicit donmai.dev/v2 policy"}
	}
	if c.LocalRuntime == nil {
		return &agent.ExecutionSecurityError{Code: agent.ExecutionSecurityUnconfigured, Detail: "local runtime and its outermost policy are required"}
	}
	return c.LocalRuntime.ExecutionSecurity.Validate()
}

// ProjectAdmissionVersionV2 marks enabledProjectIds as the sole project
// admission authority. Zero is the legacy repository-derived contract.
const ProjectAdmissionVersionV2 = 2

// Project admission modes. See Config.ProjectAdmissionMode.
const (
	// ProjectAdmissionModeEnumerated admits only the projects listed in
	// enabledProjectIds. This is the default and the behaviour every config
	// written before this field existed keeps.
	ProjectAdmissionModeEnumerated = "enumerated"
	// ProjectAdmissionModeAllRouted admits any project the orchestrator routes
	// to this machine, without a per-project entry.
	ProjectAdmissionModeAllRouted = "all-routed"
)

// EffectiveProjectAdmissionMode returns the normalized admission mode. An
// absent, blank, or unrecognized value reads as "enumerated" — admission never
// widens by accident, only by an explicit, spelled-out opt-in.
func (c *Config) EffectiveProjectAdmissionMode() string {
	if localRuntimeRequested(c) && c.LocalRuntime != nil {
		return ProjectAdmissionModeEnumerated
	}
	if c == nil {
		return ProjectAdmissionModeEnumerated
	}
	return normalizeProjectAdmissionMode(c.ProjectAdmissionMode)
}

// AdmitsAnyRoutedProject reports whether this config consents to every project
// the orchestrator dispatches, rather than an enumerated set.
func (c *Config) AdmitsAnyRoutedProject() bool {
	return c.EffectiveProjectAdmissionMode() == ProjectAdmissionModeAllRouted
}

func normalizeProjectAdmissionMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), ProjectAdmissionModeAllRouted) {
		return ProjectAdmissionModeAllRouted
	}
	return ProjectAdmissionModeEnumerated
}

// MachineConfig captures the machine identity block from daemon.yaml.
type MachineConfig struct {
	ID     string `yaml:"id"               json:"id"`
	Region string `yaml:"region,omitempty" json:"region,omitempty"`
}

// CapacityConfig is the resource envelope declared in daemon.yaml.
type CapacityConfig struct {
	MaxConcurrentSessions int                `yaml:"maxConcurrentSessions"     json:"maxConcurrentSessions"`
	MaxVCpuPerSession     int                `yaml:"maxVCpuPerSession"         json:"maxVCpuPerSession"`
	MaxMemoryMbPerSession int                `yaml:"maxMemoryMbPerSession"     json:"maxMemoryMbPerSession"`
	ReservedForSystem     ReservedSystemSpec `yaml:"reservedForSystem"         json:"reservedForSystem"`
	// PoolMaxDiskGb is the LRU-eviction trigger for the workarea pool.
	// 0 means no limit.
	PoolMaxDiskGb int `yaml:"poolMaxDiskGb,omitempty" json:"poolMaxDiskGb,omitempty"`
	// SeatBudget is the per-seat resource budget: the CPU/memory share
	// one session's process tree may use. Optional; ResolveSeatBudget
	// derives the per-seat share from host cores and memory divided by
	// MaxConcurrentSessions when fields are omitted. Zero value means
	// budgeting is off (report mode "none").
	SeatBudget SeatBudgetConfig `yaml:"seatBudget,omitempty" json:"seatBudget,omitempty"`
}

// SeatBudgetConfig is the authored per-seat resource budget in daemon.yaml.
// Every field is optional: omitted numerics fall back to host cores/memory
// divided by the max concurrent seat count, and an omitted mode resolves
// per OS (enforced on Linux, best-effort elsewhere). The type lives here
// rather than in the enforcement package so the config layer keeps its
// one-way dependency (daemon -> seatbudget, never the reverse).
type SeatBudgetConfig struct {
	// CPUs is the whole-core seat share. Zero derives from host cores.
	CPUs int `yaml:"cpus,omitempty" json:"cpus,omitempty"`
	// MemoryMB is the seat memory ceiling in mebibytes. Zero derives
	// from host memory on multi-seat hosts, and means no cap on a
	// single-seat host.
	MemoryMB int `yaml:"memoryMb,omitempty" json:"memoryMb,omitempty"`
	// IOWeight is the cgroup v2 IO weight (1-10000). Zero means the
	// backend default (no explicit weight). Linux only.
	IOWeight int `yaml:"ioWeight,omitempty" json:"ioWeight,omitempty"`
	// Mode is auto (default), enforced, best-effort or none. "enforced"
	// on an OS with no enforcement backend degrades to best-effort.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
}

// ReservedSystemSpec describes resources reserved for the host OS.
type ReservedSystemSpec struct {
	VCpu     int `yaml:"vCpu"     json:"vCpu"`
	MemoryMb int `yaml:"memoryMb" json:"memoryMb"`
}

// ProjectConfig describes one repository resource bound to a project. More
// than one entry may share an ID; project admission is controlled separately
// by Config.EnabledProjectIDs.
//
// The retired per-repository clone override is not modelled here. A file
// that still carries the key keeps loading; the loader logs one deprecation
// warning per file (see warnIfRetiredCloneStrategyPresent), and writers
// never emit the key.
type ProjectConfig struct {
	RepositoryID string      `yaml:"-"                        json:"repositoryId,omitempty"`
	Primary      bool        `yaml:"-"                        json:"primary,omitempty"`
	ID           string      `yaml:"id"                       json:"id"`
	Repository   string      `yaml:"repository"               json:"repository"`
	Git          *ProjectGit `yaml:"git,omitempty"            json:"git,omitempty"`
}

// RepositoryConfig is one repository resource linked to a project. Repository
// identity and project admission are independent.
//
// The retired per-repository clone override is not modelled here; see
// ProjectConfig.
type RepositoryConfig struct {
	ID        string      `yaml:"id"                      json:"id"`
	ProjectID string      `yaml:"projectId"               json:"projectId"`
	Source    string      `yaml:"source"                  json:"source"`
	Primary   bool        `yaml:"primary,omitempty"       json:"primary,omitempty"`
	Git       *ProjectGit `yaml:"git,omitempty"           json:"git,omitempty"`
}

// UnmarshalYAML accepts either the canonical `repository` key or the legacy
// `repoUrl` key (legacy daemon.yaml files written by older versions of
// `rensei project allow`). When the legacy key is found a one-line warning
// is logged so operators know to rewrite the file; this back-compat shim is
// scheduled for removal one release after the canonical writer ships.
//
// The retired per-repository clone override is not decoded: files that still
// carry the key keep loading and the value is ignored. LoadConfig logs one
// deprecation warning per file (see warnIfRetiredCloneStrategyPresent), so
// this decoder stays silent.
func (p *ProjectConfig) UnmarshalYAML(node *yaml.Node) error {
	var raw struct {
		ID         string      `yaml:"id"`
		Repository string      `yaml:"repository"`
		RepoURL    string      `yaml:"repoUrl"`
		Git        *ProjectGit `yaml:"git,omitempty"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	p.ID = raw.ID
	p.Git = raw.Git
	switch {
	case raw.Repository != "":
		p.Repository = raw.Repository
	case raw.RepoURL != "":
		p.Repository = raw.RepoURL
		slog.Warn(
			"daemon.yaml: legacy 'repoUrl' key on project entry; will be rewritten as 'repository' on next write",
			"id", raw.ID,
			"repoUrl", raw.RepoURL,
		)
	}
	return nil
}

// retiredCloneStrategyWarning is the single deprecation notice logged when
// a load encounters the retired per-repository clone override. The key is
// ignored; the warning tells operators to drop it.
const retiredCloneStrategyWarning = "daemon.yaml: 'cloneStrategy' is retired and ignored; remove the key"

// warnIfRetiredCloneStrategyPresent scans the raw file for the retired key
// and logs exactly one deprecation warning when any entry still carries it.
// The typed decoder ignores the key, so detection runs on the raw mapping
// tree: the typed structs no longer name the key at all.
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

// ProjectGit captures per-project credential helper / SSH key hints.
type ProjectGit struct {
	CredentialHelper string `yaml:"credentialHelper,omitempty" json:"credentialHelper,omitempty"`
	SSHKey           string `yaml:"sshKey,omitempty"           json:"sshKey,omitempty"`
}

// OrchestratorConfig is the orchestrator URL + registration token block.
type OrchestratorConfig struct {
	URL       string `yaml:"url"                 json:"url"`
	AuthToken string `yaml:"authToken,omitempty" json:"authToken,omitempty"`
}

// AutoUpdateConfig is the auto-update preferences block.
type AutoUpdateConfig struct {
	Channel             UpdateChannel  `yaml:"channel"             json:"channel"`
	Schedule            UpdateSchedule `yaml:"schedule"            json:"schedule"`
	DrainTimeoutSeconds int            `yaml:"drainTimeoutSeconds" json:"drainTimeoutSeconds"`

	// Signers is the allowlist of identities trusted to sign release
	// binaries. Auto-update is fail-closed: when this list is empty the
	// daemon refuses every binary swap. Each downloaded binary must come
	// with a sibling sigstore bundle (`<binary>.sigstore`, e.g. produced
	// by `cosign sign-blob --bundle … --new-bundle-format`) whose
	// certificate matches one of these identities and chains to the
	// configured trust root. See daemon/README.md § "Auto-update signing".
	Signers []UpdateSigner `yaml:"signers,omitempty" json:"signers,omitempty"`

	// TrustRootPath optionally points at a sigstore trusted-root JSON
	// file used to verify update bundles — for private sigstore
	// deployments. Empty = the embedded public Sigstore production trust
	// root (the same root kit verification uses; see kit_trust.go).
	TrustRootPath string `yaml:"trustRootPath,omitempty" json:"trustRootPath,omitempty"`
}

// UpdateSigner pins one identity trusted to sign release binaries.
// Issuer plus at least one of SAN/SANRegex is required: sigstore identity
// verification is only meaningful when the certificate subject AND the
// OIDC issuer that authenticated it are pinned together.
type UpdateSigner struct {
	// SAN is the exact Fulcio certificate subject
	// (SubjectAlternativeName), e.g. a signer e-mail for
	// key-based/manual signing setups.
	SAN string `yaml:"san,omitempty" json:"san,omitempty"`
	// SANRegex matches the certificate subject by regular expression —
	// needed for CI workflow identities whose SAN embeds the release
	// ref, e.g.
	// "^https://github\\.com/<org>/<repo>/\\.github/workflows/release\\.yml@refs/tags/v.+$".
	SANRegex string `yaml:"sanRegex,omitempty" json:"sanRegex,omitempty"`
	// Issuer is the OIDC issuer that authenticated the signer, e.g.
	// "https://token.actions.githubusercontent.com" for GitHub Actions.
	Issuer string `yaml:"issuer" json:"issuer"`
}

// ObservabilityConfig holds optional log/metrics tuning.
type ObservabilityConfig struct {
	LogFormat   string `yaml:"logFormat,omitempty"   json:"logFormat,omitempty"`
	LogPath     string `yaml:"logPath,omitempty"     json:"logPath,omitempty"`
	MetricsPort int    `yaml:"metricsPort,omitempty" json:"metricsPort,omitempty"`
}

// WorkareaConfig configures the Layer-3 workarea operator surface — archive
// root scan path, diff streaming threshold. Wave 9 / ADR-2026-05-07.
type WorkareaConfig struct {
	// ArchiveRoot is the directory the daemon scans for archived workareas.
	// Default ~/.donmai/workareas (resolved at runtime by the handler if
	// empty).
	ArchiveRoot string `yaml:"archiveRoot,omitempty" json:"archiveRoot,omitempty"`
	// DiffStreamingThreshold is the entry count above which the diff
	// endpoint switches from a single JSON envelope to NDJSON streaming.
	// Default 1000 per ADR D4a.
	DiffStreamingThreshold int `yaml:"diffStreamingThreshold,omitempty" json:"diffStreamingThreshold,omitempty"`
}

// KitConfig configures the Layer-4 kit operator surface — the scan paths
// the daemon walks to discover installed kits. Wave 11 / ADR-2026-05-07
// § D4. ScanPaths are evaluated in declaration order; the first entry is
// also where the .state.json sidecar (enable/disable toggles) lives.
// A leading `~/` is expanded to the user's home directory by
// NewKitRegistry.
type KitConfig struct {
	// ScanPaths is the ordered list of directories the kit registry walks
	// to find installed kits. Empty / absent means [DefaultKitScanPath()]
	// (resolved by applyDefaults).
	ScanPaths []string `yaml:"scanPaths,omitempty" json:"scanPaths,omitempty"`
}

// DefaultConfigPath returns the path to daemon.yaml under ~/.donmai/.
func DefaultConfigPath() string {
	return statepath.Resolve("daemon.yaml", "/tmp/.donmai/daemon.yaml")
}

// DefaultJWTPath returns the path to the cached JWT under ~/.donmai/.
func DefaultJWTPath() string {
	return statepath.Resolve("daemon.jwt", "/tmp/.donmai/daemon.jwt")
}

// LoadConfig reads daemon.yaml from path. Returns (nil, nil) when the file
// does not exist (so callers can branch into the setup wizard / default).
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read daemon config %q: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse daemon config %q: %w", path, err)
	}
	// The retired clone override is not part of the typed schema, so its
	// presence is detected on the raw document: one warning per load no
	// matter how many entries still carry the key.
	warnIfRetiredCloneStrategyPresent(data)

	// Decode authored presence with the same YAML rules as Config, including
	// aliases and merge keys. Null, like omission, requests the default.
	var authored struct {
		Capacity struct {
			MaxConcurrentSessions *int `yaml:"maxConcurrentSessions"`
		} `yaml:"capacity"`
	}
	if err := yaml.Unmarshal(data, &authored); err != nil {
		return nil, fmt.Errorf("parse daemon capacity %q: %w", path, err)
	}

	// Apply env-var substitution on authToken.
	if cfg.Orchestrator.AuthToken != "" {
		cfg.Orchestrator.AuthToken = substituteEnvVars(cfg.Orchestrator.AuthToken)
	}
	if envTok := os.Getenv("DONMAI_DAEMON_TOKEN"); envTok != "" {
		cfg.Orchestrator.AuthToken = envTok
	}
	if err := validateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("invalid daemon config %q: %w", path, err)
	}

	normalizeProjectContract(&cfg)
	applyDefaultsWithCapacityPresence(&cfg, authored.Capacity.MaxConcurrentSessions != nil)
	cfg.unknownFields = captureUnknownFields(data)
	return &cfg, nil
}

// WriteConfig atomically writes cfg to path (tmp file + rename), creating
// parent directories as needed.
func WriteConfig(path string, cfg *Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir %q: %w", dir, err)
	}
	normalized := *cfg
	normalizeProjectContract(&normalized)
	if normalized.ProjectAdmissionVersion == 0 {
		// Keep a legacy write legacy. The enabled set is an in-memory
		// projection until the first successful v2 mutation.
		normalized.EnabledProjectIDs = nil
		normalized.Repositories = nil
	} else {
		syncLegacyProjectProjection(&normalized)
	}
	data, err := marshalConfigPreservingUnknown(&normalized)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename temp config: %w", err)
	}
	return nil
}

// knownConfigKeys is the set of top-level daemon.yaml keys declared by the
// Config struct, derived from its yaml tags so the set cannot drift from
// the struct.
var knownConfigKeys = configYAMLKeys()

func configYAMLKeys() map[string]struct{} {
	keys := make(map[string]struct{})
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		name := strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0]
		name = strings.TrimSpace(name)
		if name == "" || name == "-" {
			continue
		}
		keys[name] = struct{}{}
	}
	return keys
}

// captureUnknownFields returns the top-level mapping entries in data whose
// keys the Config struct does not declare, cloned so they stay valid after
// the source document is discarded. Merge keys ("<<") are skipped: their
// content is already resolved into the declared fields during decode.
func captureUnknownFields(data []byte) map[string]*yaml.Node {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil
	}
	var out map[string]*yaml.Node
	memo := make(map[*yaml.Node]*yaml.Node)
	for i := 0; i+1 < len(root.Content); i += 2 {
		keyNode := root.Content[i]
		if keyNode.Kind != yaml.ScalarNode {
			continue
		}
		key := keyNode.Value
		if key == "<<" {
			continue
		}
		if _, known := knownConfigKeys[key]; known {
			continue
		}
		if out == nil {
			out = make(map[string]*yaml.Node)
		}
		out[key] = cloneYAMLNodeMemo(root.Content[i+1], memo)
	}
	return out
}

// marshalConfigPreservingUnknown marshals cfg and merges back the unknown
// top-level keys captured at load time, verbatim. A key also present in
// the marshaled Config is skipped so the declared field always wins over a
// stale raw copy of that same key.
func marshalConfigPreservingUnknown(cfg *Config) ([]byte, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	if len(cfg.unknownFields) == 0 {
		return data, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return data, nil
	}
	present := make(map[string]struct{}, len(root.Content)/2)
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Kind == yaml.ScalarNode {
			present[root.Content[i].Value] = struct{}{}
		}
	}
	keys := make([]string, 0, len(cfg.unknownFields))
	for k := range cfg.unknownFields {
		if _, dup := present[k]; dup {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	memo := make(map[*yaml.Node]*yaml.Node)
	for _, k := range keys {
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}
		root.Content = append(root.Content, keyNode, cloneYAMLNodeMemo(cfg.unknownFields[k], memo))
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(4)
	if err := enc.Encode(root); err != nil {
		_ = enc.Close()
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// cloneYAMLNodeMemo deep-copies n, preserving alias relationships: nodes
// shared via Alias pointers map to a single clone through memo, so an
// anchor defined under one unknown key and referenced from another
// survives the round-trip as one definition plus a reference instead of
// two duplicate anchor definitions.
func cloneYAMLNodeMemo(n *yaml.Node, memo map[*yaml.Node]*yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	if prev, ok := memo[n]; ok {
		return prev
	}
	out := *n
	memo[n] = &out
	if n.Alias != nil {
		out.Alias = cloneYAMLNodeMemo(n.Alias, memo)
	}
	if len(n.Content) > 0 {
		out.Content = make([]*yaml.Node, len(n.Content))
		for i, c := range n.Content {
			out.Content[i] = cloneYAMLNodeMemo(c, memo)
		}
	}
	return &out
}

// applyDefaults fills in omitted and zero-valued fields with their schema defaults.
func applyDefaults(c *Config) {
	applyDefaultsWithCapacityPresence(c, false)
}

func applyDefaultsWithCapacityPresence(c *Config, sessionLimitAuthored bool) {
	normalizeProjectContract(c)
	if c.APIVersion == "" {
		c.APIVersion = "donmai.dev/v1"
	}
	if c.Kind == "" {
		c.Kind = "LocalDaemon"
	}
	if c.Capacity.MaxConcurrentSessions == 0 && !sessionLimitAuthored {
		c.Capacity.MaxConcurrentSessions = 8
	}
	if c.Capacity.MaxVCpuPerSession == 0 {
		c.Capacity.MaxVCpuPerSession = 4
	}
	if c.Capacity.MaxMemoryMbPerSession == 0 {
		c.Capacity.MaxMemoryMbPerSession = 8192
	}
	if c.Capacity.ReservedForSystem.VCpu == 0 {
		c.Capacity.ReservedForSystem.VCpu = 4
	}
	if c.Capacity.ReservedForSystem.MemoryMb == 0 {
		c.Capacity.ReservedForSystem.MemoryMb = 16384
	}
	if c.AutoUpdate.Channel == "" {
		c.AutoUpdate.Channel = ChannelStable
	}
	if c.AutoUpdate.Schedule == "" {
		c.AutoUpdate.Schedule = ScheduleNightly
	}
	if c.AutoUpdate.DrainTimeoutSeconds == 0 {
		c.AutoUpdate.DrainTimeoutSeconds = 600
	}
	if c.Workarea.DiffStreamingThreshold == 0 {
		c.Workarea.DiffStreamingThreshold = 1000
	}
	if len(c.Kit.ScanPaths) == 0 {
		c.Kit.ScanPaths = []string{DefaultKitScanPath()}
	}
	if c.Trust.Mode == "" {
		// Secure default: signed-by-allowlist; environment cannot
		// lower it to permissive. Kept in lock-step with
		// kitRegistryOrEmpty (handle_kit.go), which applies the same
		// default when no Config is loaded at all.
		c.Trust.Mode = resolveDefaultTrustMode()
	}
	if len(c.Trust.IssuerSet) == 0 {
		// Seed the vendor trust root's default allowlist — the official
		// donmai-kits signing identity — so signed-by-allowlist is usable
		// out of the box for official kits without --allow-unsigned. An
		// operator who configures their own issuerSet replaces this
		// entirely. Kept in lock-step with kitRegistryOrEmpty.
		c.Trust.IssuerSet = defaultVendorIssuerSet()
	}
	// The retired clone override is not defaulted: nothing reads it, and
	// writers never emit it.
}

// EffectiveEnabledProjectIDs returns the normalized project-admission set.
// Explicit v2 entries are authoritative. When that key is absent, legacy
// repository-bearing projects[] entries are projected so old configurations
// retain their complete working behavior.
func (c *Config) EffectiveEnabledProjectIDs() []string {
	if localRuntimeRequested(c) && c.LocalRuntime != nil {
		ids := make([]string, 0, len(c.LocalRuntime.Repositories))
		for _, repository := range c.LocalRuntime.Repositories {
			ids = append(ids, repository.OwnerRepo)
		}
		return normalizeProjectIDs(ids)
	}
	if c == nil {
		return nil
	}
	if c.ProjectAdmissionVersion == ProjectAdmissionVersionV2 {
		return normalizeProjectIDs(c.EnabledProjectIDs)
	}
	return normalizeProjectIDs(append(
		append([]string(nil), c.EnabledProjectIDs...),
		projectIDsFromRepositories(c.Projects)...,
	))
}

func normalizeProjectContract(c *Config) {
	if c == nil {
		return
	}
	c.EnabledProjectIDs = c.EffectiveEnabledProjectIDs()
	// Canonicalize a mode the operator actually wrote (so "All-Routed" is
	// stored as "all-routed"), but never materialize the default into a file
	// that had no opinion — that would rewrite every config on every save.
	if strings.TrimSpace(c.ProjectAdmissionMode) != "" {
		c.ProjectAdmissionMode = c.EffectiveProjectAdmissionMode()
	}
	c.Repositories = normalizeRepositories(c.Repositories, c.Projects)
}

func migrateProjectAdmissionV2(c *Config) bool {
	if c == nil || c.ProjectAdmissionVersion == ProjectAdmissionVersionV2 {
		return false
	}
	normalizeProjectContract(c)
	c.ProjectAdmissionVersion = ProjectAdmissionVersionV2
	return true
}

func normalizeProjectIDs(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	ids := make([]string, 0, len(values))
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
	for _, id := range values {
		add(id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return []string{}
	}
	return ids
}

func projectIDsFromRepositories(projects []ProjectConfig) []string {
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		ids = append(ids, project.ID)
	}
	return normalizeProjectIDs(ids)
}

// EffectiveProjectConfigs projects normalized repository resources into the
// legacy runtime shape consumed by the existing spawner and poll resolver.
func (c *Config) EffectiveProjectConfigs() []ProjectConfig {
	if localRuntimeRequested(c) && c.LocalRuntime != nil {
		projects := make([]ProjectConfig, 0, len(c.LocalRuntime.Repositories))
		for _, repository := range c.LocalRuntime.Repositories {
			projects = append(projects, ProjectConfig{ID: repository.OwnerRepo, RepositoryID: fmt.Sprintf("github:%d", repository.RepositoryID), Repository: "https://github.com/" + repository.OwnerRepo + ".git", Primary: true})
		}
		return projects
	}

	if c == nil {
		return nil
	}
	repositories := normalizeRepositories(c.Repositories, c.Projects)
	out := make([]ProjectConfig, 0, len(repositories))
	for _, repository := range repositories {
		out = append(out, ProjectConfig{
			RepositoryID: repository.ID,
			Primary:      repository.Primary,
			ID:           repository.ProjectID,
			Repository:   repository.Source,
			Git:          repository.Git,
		})
	}
	return out
}

func normalizeRepositories(v2 []RepositoryConfig, legacy []ProjectConfig) []RepositoryConfig {
	byKey := make(map[string]RepositoryConfig, len(v2)+len(legacy))
	order := make([]string, 0, len(v2)+len(legacy))
	add := func(repository RepositoryConfig, wins bool) {
		repository.ProjectID = strings.TrimSpace(repository.ProjectID)
		repository.Source = strings.TrimSpace(repository.Source)
		if repository.ProjectID == "" || repository.Source == "" {
			return
		}
		if repository.ID == "" {
			repository.ID = legacyRepositoryID(repository.ProjectID, repository.Source)
		}
		key := repository.ProjectID + "\x00" + normalizeRepositorySource(repository.Source)
		if _, exists := byKey[key]; exists && !wins {
			return
		}
		if _, exists := byKey[key]; !exists {
			order = append(order, key)
		}
		byKey[key] = repository
	}
	for _, project := range legacy {
		add(RepositoryConfig{
			ID:        legacyRepositoryID(project.ID, project.Repository),
			ProjectID: project.ID,
			Source:    project.Repository,
			Git:       project.Git,
		}, false)
	}
	for _, repository := range v2 {
		add(repository, true)
	}
	sort.Strings(order)
	out := make([]RepositoryConfig, 0, len(order))
	for _, key := range order {
		out = append(out, byKey[key])
	}
	return out
}

func normalizeRepositorySource(source string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(source), "/"), ".git"))
}

func legacyRepositoryID(projectID, source string) string {
	sum := sha256.Sum256([]byte(projectID + "\x00" + normalizeRepositorySource(source)))
	return fmt.Sprintf("repo-%x", sum[:6])
}

func syncLegacyProjectProjection(c *Config) {
	if c == nil || c.ProjectAdmissionVersion != ProjectAdmissionVersionV2 {
		return
	}
	enabled := make(map[string]struct{}, len(c.EnabledProjectIDs))
	for _, id := range c.EnabledProjectIDs {
		enabled[id] = struct{}{}
	}
	projects := make([]ProjectConfig, 0, len(c.Repositories))
	for _, repository := range c.Repositories {
		if _, ok := enabled[repository.ProjectID]; !ok {
			continue
		}
		projects = append(projects, ProjectConfig{
			ID:         repository.ProjectID,
			Repository: repository.Source,
			Git:        repository.Git,
		})
	}
	c.Projects = projects
}

// validateConfig enforces required fields and value ranges.
func validateConfig(c *Config) error {
	if localRuntimeRequested(c) && c.APIVersion == LocalRuntimeConfigAPIVersion {
		if err := ValidateLocalExecutionSecurity(c); err != nil {
			return err
		}
	}
	if c.LocalRuntime != nil {
		if c.LocalRuntime.Harness != "" && c.LocalRuntime.Harness != "codex" && c.LocalRuntime.Harness != "claude-code" {
			return errors.New("localRuntime.harness must be codex or claude-code")
		}
		if (strings.TrimSpace(c.LocalRuntime.Model) == "") != (strings.TrimSpace(c.LocalRuntime.ModelAuthor) == "") {
			return errors.New("localRuntime.model and modelAuthor are required")
		}
		if err := validateLocalRepositories(c.LocalRuntime.Repositories); err != nil {
			return err
		}
	}
	if c.Machine.ID == "" {
		return errors.New("machine.id is required")
	}
	if c.Orchestrator.URL == "" {
		return errors.New("orchestrator.url is required")
	}
	if c.Capacity.MaxConcurrentSessions < 0 {
		return errors.New("capacity.maxConcurrentSessions must be >= 0")
	}
	if err := validateSeatBudget(c.Capacity.SeatBudget); err != nil {
		return err
	}
	if c.ProjectAdmissionVersion != 0 && c.ProjectAdmissionVersion != ProjectAdmissionVersionV2 {
		return fmt.Errorf("projectAdmissionVersion invalid: %d (want 2)", c.ProjectAdmissionVersion)
	}
	// A typo here must be loud, not silent: normalizeProjectAdmissionMode is
	// deliberately fail-closed, so an operator who wrote "all_routed" and was
	// never told would get deny-all and blame the platform.
	if raw := strings.TrimSpace(c.ProjectAdmissionMode); raw != "" &&
		!strings.EqualFold(raw, ProjectAdmissionModeEnumerated) &&
		!strings.EqualFold(raw, ProjectAdmissionModeAllRouted) {
		return fmt.Errorf(
			"projectAdmissionMode invalid: %q (want %q or %q)",
			raw, ProjectAdmissionModeEnumerated, ProjectAdmissionModeAllRouted,
		)
	}
	for i, id := range c.EnabledProjectIDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("enabledProjectIds[%d] is required", i)
		}
	}
	for i, p := range c.Projects {
		if p.ID == "" {
			return fmt.Errorf("projects[%d].id is required", i)
		}
		if p.Repository == "" {
			return fmt.Errorf("projects[%d].repository is required", i)
		}
		// The retired `cloneStrategy` key is not validated: a file that
		// still carries it loads with one deprecation warning (see
		// LoadConfig) and the value is ignored.
	}
	repositoryIDs := make(map[string]struct{}, len(c.Repositories))
	primaryByProject := make(map[string]string)
	for i, repository := range c.Repositories {
		if strings.TrimSpace(repository.ID) == "" {
			return fmt.Errorf("repositories[%d].id is required", i)
		}
		if _, exists := repositoryIDs[repository.ID]; exists {
			return fmt.Errorf("repositories[%d].id is duplicated: %q", i, repository.ID)
		}
		repositoryIDs[repository.ID] = struct{}{}
		if strings.TrimSpace(repository.ProjectID) == "" {
			return fmt.Errorf("repositories[%d].projectId is required", i)
		}
		if strings.TrimSpace(repository.Source) == "" {
			return fmt.Errorf("repositories[%d].source is required", i)
		}
		// The retired `cloneStrategy` key is not validated here either;
		// see the projects[] note above.
		if repository.Primary {
			if prior, exists := primaryByProject[repository.ProjectID]; exists {
				return fmt.Errorf("repositories[%d].primary conflicts with %q for project %q", i, prior, repository.ProjectID)
			}
			primaryByProject[repository.ProjectID] = repository.ID
		}
	}
	switch c.AutoUpdate.Channel {
	case "", ChannelStable, ChannelBeta, ChannelMain:
	default:
		return fmt.Errorf("autoUpdate.channel invalid: %q", c.AutoUpdate.Channel)
	}
	switch c.AutoUpdate.Schedule {
	case "", ScheduleNightly, ScheduleOnRelease, ScheduleManual:
	default:
		return fmt.Errorf("autoUpdate.schedule invalid: %q", c.AutoUpdate.Schedule)
	}
	for i, s := range c.AutoUpdate.Signers {
		if strings.TrimSpace(s.SAN) == "" && strings.TrimSpace(s.SANRegex) == "" {
			return fmt.Errorf("autoUpdate.signers[%d]: san or sanRegex is required", i)
		}
		if strings.TrimSpace(s.Issuer) == "" {
			return fmt.Errorf("autoUpdate.signers[%d].issuer is required", i)
		}
	}
	switch c.Trust.Mode {
	case "", TrustModePermissive, TrustModeSignedByAllowlist, TrustModeAttested:
	default:
		return fmt.Errorf("trust.mode invalid: %q (want permissive | signed-by-allowlist | attested)", c.Trust.Mode)
	}
	return nil
}

var envVarRE = regexp.MustCompile(`\$\{([^}]+)\}`)

// substituteEnvVars expands ${ENV_VAR} patterns using os.Getenv.
// Unmatched patterns are left as-is (matching the TS behavior).
func substituteEnvVars(value string) string {
	return envVarRE.ReplaceAllStringFunc(value, func(match string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(match, "${"), "}")
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return match
	})
}

// DeriveDefaultMachineID returns a hostname-derived LABEL for machine.id when
// the operator has not set one.
//
// This is a display label, not an identity. Host identity is MachineID()
// (machine_id.go) — see RegistrationOptions.MachineID for why a hostname must
// never be keyed on.
//
// Even as a label it is normalized to ONE value per machine: the DNS domain is
// stripped before sanitizing, so a machine whose hostname resolves as
// "<name>.local" on one network and "<name>.localdomain" on another produces
// the same label instead of two.
func DeriveDefaultMachineID() string {
	host, err := os.Hostname()
	if err != nil {
		host = ""
	}
	return normalizeMachineLabel(host)
}

var (
	machineLabelCleanRE  = regexp.MustCompile(`[^a-z0-9-]`)
	machineLabelRepeatRE = regexp.MustCompile(`-+`)
)

// normalizeMachineLabel turns a raw hostname into the canonical machine label.
// Pure, so the normalization can be pinned by tests independently of whatever
// the test host happens to be called.
func normalizeMachineLabel(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	// Keep the leading label only. os.Hostname() returns whatever the
	// resolver currently supplies, which on macOS alternates between the
	// mDNS "<name>.local" form and a DHCP-supplied "<name>.localdomain"
	// form for the same machine — two labels, one box.
	if idx := strings.Index(host, "."); idx > 0 {
		host = host[:idx]
	}
	// Collapse anything not in [a-z0-9-] to "-" then squash repeats.
	host = machineLabelCleanRE.ReplaceAllString(host, "-")
	host = machineLabelRepeatRE.ReplaceAllString(host, "-")
	host = strings.Trim(host, "-")
	if host == "" {
		host = "local-machine"
	}
	return host
}

// DefaultConfig returns a minimal Config suitable as a starting point when
// the wizard is skipped. Capacity defaults are derived from runtime info.
func DefaultConfig() *Config {
	cfg := &Config{
		APIVersion: "donmai.dev/v1",
		Kind:       "LocalDaemon",
		Machine: MachineConfig{
			ID:     DeriveDefaultMachineID(),
			Region: "local",
		},
		Capacity: CapacityConfig{
			MaxConcurrentSessions: defaultMaxSessions(runtime.NumCPU()),
			MaxVCpuPerSession:     4,
			MaxMemoryMbPerSession: 8192,
			ReservedForSystem: ReservedSystemSpec{
				VCpu:     min(4, runtime.NumCPU()/4),
				MemoryMb: 16384,
			},
		},
		Orchestrator: OrchestratorConfig{
			// No vendor default — the OSS binary requires the operator to
			// configure the orchestrator URL explicitly (flag/env/config-file).
			// When unset, registration/poll fail clearly rather than dialing
			// any vendor's platform.
			URL:       os.Getenv("DONMAI_ORCHESTRATOR_URL"),
			AuthToken: os.Getenv("DONMAI_DAEMON_TOKEN"),
		},
		AutoUpdate: AutoUpdateConfig{
			Channel:             ChannelStable,
			Schedule:            ScheduleNightly,
			DrainTimeoutSeconds: 600,
		},
	}
	applyDefaults(cfg)
	return cfg
}

func defaultMaxSessions(cpuCount int) int {
	// Heuristic: ~1 session per 2 CPUs, capped at 8, min 1.
	n := cpuCount / 2
	if n < 1 {
		n = 1
	}
	if n > 8 {
		n = 8
	}
	return n
}
