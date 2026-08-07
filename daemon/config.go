package daemon

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/internal/statepath"
	"github.com/RenseiAI/donmai/runner/access"

	"gopkg.in/yaml.v3"
)

// Config is the in-memory representation of ~/.donmai/daemon.yaml. The wire
// schema mirrors the TS DaemonConfig (donmai-architecture/004 §Configuration
// shape).
type Config struct {
	APIVersion              string         `yaml:"apiVersion"                       json:"apiVersion"`
	Kind                    string         `yaml:"kind"                             json:"kind"`
	ProjectAdmissionVersion int            `yaml:"projectAdmissionVersion,omitempty" json:"projectAdmissionVersion,omitempty"`
	Machine                 MachineConfig  `yaml:"machine"                  json:"machine"`
	Capacity                CapacityConfig `yaml:"capacity"                 json:"capacity"`
	// EnabledProjectIDs is the authoritative project-admission set. A
	// project may be admitted before it has any repository resources.
	// Legacy projects[] entries are projected only when
	// ProjectAdmissionVersion is absent. Version 2 makes this set authoritative,
	// including when it is empty.
	EnabledProjectIDs []string           `yaml:"enabledProjectIds,omitempty" json:"enabledProjectIds,omitempty"`
	Repositories      []RepositoryConfig `yaml:"repositories,omitempty"      json:"repositories,omitempty"`
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
	// (mutation_apply.go); read by the rensei-tui fail-closed gate one step
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
	// resolveDefaultTrustMode — TrustModeSignedByAllowlist unless the
	// operator opts out through DONMAI_KIT_TRUST_MODE. Per
	// 002-provider-base-contract.md § "Signing and trust". Lives on
	// Config (not on KitConfig) because the trust mode applies across
	// all plugin families per 015-plugin-spec.md § "Auth + trust".
	Trust TrustConfig `yaml:"trust,omitempty"        json:"trust,omitempty"`
}

// ProjectAdmissionVersionV2 marks enabledProjectIds as the sole project
// admission authority. Zero is the legacy repository-derived contract.
const ProjectAdmissionVersionV2 = 2

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
}

// ReservedSystemSpec describes resources reserved for the host OS.
type ReservedSystemSpec struct {
	VCpu     int `yaml:"vCpu"     json:"vCpu"`
	MemoryMb int `yaml:"memoryMb" json:"memoryMb"`
}

// ProjectConfig describes one repository resource bound to a project. More
// than one entry may share an ID; project admission is controlled separately
// by Config.EnabledProjectIDs.
type ProjectConfig struct {
	RepositoryID  string        `yaml:"-"                        json:"repositoryId,omitempty"`
	Primary       bool          `yaml:"-"                        json:"primary,omitempty"`
	ID            string        `yaml:"id"                       json:"id"`
	Repository    string        `yaml:"repository"               json:"repository"`
	CloneStrategy CloneStrategy `yaml:"cloneStrategy,omitempty"  json:"cloneStrategy,omitempty"`
	Git           *ProjectGit   `yaml:"git,omitempty"            json:"git,omitempty"`
}

// RepositoryConfig is one repository resource linked to a project. Repository
// identity and project admission are independent.
type RepositoryConfig struct {
	ID            string        `yaml:"id"                      json:"id"`
	ProjectID     string        `yaml:"projectId"               json:"projectId"`
	Source        string        `yaml:"source"                  json:"source"`
	Primary       bool          `yaml:"primary,omitempty"       json:"primary,omitempty"`
	CloneStrategy CloneStrategy `yaml:"cloneStrategy,omitempty" json:"cloneStrategy,omitempty"`
	Git           *ProjectGit   `yaml:"git,omitempty"           json:"git,omitempty"`
}

// UnmarshalYAML accepts either the canonical `repository` key or the legacy
// `repoUrl` key (legacy daemon.yaml files written by older versions of
// `rensei project allow`). When the legacy key is found a one-line warning
// is logged so operators know to rewrite the file; this back-compat shim is
// scheduled for removal one release after the canonical writer ships.
func (p *ProjectConfig) UnmarshalYAML(node *yaml.Node) error {
	var raw struct {
		ID            string        `yaml:"id"`
		Repository    string        `yaml:"repository"`
		RepoURL       string        `yaml:"repoUrl"`
		CloneStrategy CloneStrategy `yaml:"cloneStrategy,omitempty"`
		Git           *ProjectGit   `yaml:"git,omitempty"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	p.ID = raw.ID
	p.CloneStrategy = raw.CloneStrategy
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
	applyDefaults(&cfg)
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
	data, err := yaml.Marshal(&normalized)
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

// applyDefaults fills in zero-valued fields with their schema defaults.
func applyDefaults(c *Config) {
	normalizeProjectContract(c)
	if c.APIVersion == "" {
		c.APIVersion = "donmai.dev/v1"
	}
	if c.Kind == "" {
		c.Kind = "LocalDaemon"
	}
	if c.Capacity.MaxConcurrentSessions == 0 {
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
		// Secure default: signed-by-allowlist unless the operator opts
		// out via DONMAI_KIT_TRUST_MODE. Kept in lock-step with
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
	for i := range c.Projects {
		if c.Projects[i].CloneStrategy == "" {
			c.Projects[i].CloneStrategy = CloneShallow
		}
	}
}

// EffectiveEnabledProjectIDs returns the normalized project-admission set.
// Explicit v2 entries are authoritative. When that key is absent, legacy
// repository-bearing projects[] entries are projected so old configurations
// retain their complete working behavior.
func (c *Config) EffectiveEnabledProjectIDs() []string {
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
	if c == nil {
		return nil
	}
	repositories := normalizeRepositories(c.Repositories, c.Projects)
	out := make([]ProjectConfig, 0, len(repositories))
	for _, repository := range repositories {
		out = append(out, ProjectConfig{
			RepositoryID:  repository.ID,
			Primary:       repository.Primary,
			ID:            repository.ProjectID,
			Repository:    repository.Source,
			CloneStrategy: repository.CloneStrategy,
			Git:           repository.Git,
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
		if repository.CloneStrategy == "" {
			repository.CloneStrategy = CloneShallow
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
			ID:            legacyRepositoryID(project.ID, project.Repository),
			ProjectID:     project.ID,
			Source:        project.Repository,
			CloneStrategy: project.CloneStrategy,
			Git:           project.Git,
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
			ID:            repository.ProjectID,
			Repository:    repository.Source,
			CloneStrategy: repository.CloneStrategy,
			Git:           repository.Git,
		})
	}
	c.Projects = projects
}

// validateConfig enforces required fields and value ranges.
func validateConfig(c *Config) error {
	if c.Machine.ID == "" {
		return errors.New("machine.id is required")
	}
	if c.Orchestrator.URL == "" {
		return errors.New("orchestrator.url is required")
	}
	if c.Capacity.MaxConcurrentSessions < 0 {
		return errors.New("capacity.maxConcurrentSessions must be >= 0")
	}
	if c.ProjectAdmissionVersion != 0 && c.ProjectAdmissionVersion != ProjectAdmissionVersionV2 {
		return fmt.Errorf("projectAdmissionVersion invalid: %d (want 2)", c.ProjectAdmissionVersion)
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
		switch p.CloneStrategy {
		case "", CloneShallow, CloneFull, CloneReference:
		default:
			return fmt.Errorf("projects[%d].cloneStrategy invalid: %q", i, p.CloneStrategy)
		}
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
		switch repository.CloneStrategy {
		case "", CloneShallow, CloneFull, CloneReference:
		default:
			return fmt.Errorf("repositories[%d].cloneStrategy invalid: %q", i, repository.CloneStrategy)
		}
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
