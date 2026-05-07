package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the in-memory representation of ~/.rensei/daemon.yaml. The wire
// schema mirrors the TS DaemonConfig (rensei-architecture/004 §Configuration
// shape).
type Config struct {
	APIVersion    string               `yaml:"apiVersion"             json:"apiVersion"`
	Kind          string               `yaml:"kind"                   json:"kind"`
	Machine       MachineConfig        `yaml:"machine"                json:"machine"`
	Capacity      CapacityConfig       `yaml:"capacity"               json:"capacity"`
	Projects      []ProjectConfig      `yaml:"projects,omitempty"     json:"projects,omitempty"`
	Orchestrator  OrchestratorConfig   `yaml:"orchestrator"           json:"orchestrator"`
	AutoUpdate    AutoUpdateConfig     `yaml:"autoUpdate"             json:"autoUpdate"`
	Observability *ObservabilityConfig `yaml:"observability,omitempty" json:"observability,omitempty"`
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
	// actor). Optional; applyDefaults seeds Mode to
	// TrustModePermissive when absent. Per WAVE12_PLAN Q2 and
	// 002-provider-base-contract.md § "Signing and trust". Lives on
	// Config (not on KitConfig) because the trust mode applies across
	// all plugin families per 015-plugin-spec.md § "Auth + trust".
	Trust TrustConfig `yaml:"trust,omitempty"        json:"trust,omitempty"`
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
	// 0 means no limit. (REN-1334.)
	PoolMaxDiskGb int `yaml:"poolMaxDiskGb,omitempty" json:"poolMaxDiskGb,omitempty"`
}

// ReservedSystemSpec describes resources reserved for the host OS.
type ReservedSystemSpec struct {
	VCpu     int `yaml:"vCpu"     json:"vCpu"`
	MemoryMb int `yaml:"memoryMb" json:"memoryMb"`
}

// ProjectConfig describes one entry in the project allowlist.
type ProjectConfig struct {
	ID            string        `yaml:"id"                       json:"id"`
	Repository    string        `yaml:"repository"               json:"repository"`
	CloneStrategy CloneStrategy `yaml:"cloneStrategy,omitempty"  json:"cloneStrategy,omitempty"`
	Git           *ProjectGit   `yaml:"git,omitempty"            json:"git,omitempty"`
}

// UnmarshalYAML accepts either the canonical `repository` key or the legacy
// `repoUrl` key (pre-REN-1419 daemon.yaml files written by older versions of
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
			"daemon.yaml: legacy 'repoUrl' key on project entry; will be rewritten as 'repository' on next write (REN-1419)",
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
	// Default ~/.rensei/workareas (resolved at runtime by the handler if
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

// DefaultConfigPath returns the canonical path to ~/.rensei/daemon.yaml.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/.rensei/daemon.yaml"
	}
	return filepath.Join(home, ".rensei", "daemon.yaml")
}

// DefaultJWTPath returns the canonical path to the cached JWT.
func DefaultJWTPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/.rensei/daemon.jwt"
	}
	return filepath.Join(home, ".rensei", "daemon.jwt")
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
	if envTok := os.Getenv("RENSEI_DAEMON_TOKEN"); envTok != "" {
		cfg.Orchestrator.AuthToken = envTok
	}

	if err := validateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("invalid daemon config %q: %w", path, err)
	}

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
	data, err := yaml.Marshal(cfg)
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
	if c.APIVersion == "" {
		c.APIVersion = "rensei.dev/v1"
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
		c.Trust.Mode = TrustModePermissive
	}
	for i := range c.Projects {
		if c.Projects[i].CloneStrategy == "" {
			c.Projects[i].CloneStrategy = CloneShallow
		}
	}
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

// DeriveDefaultMachineID returns a hostname-derived identifier suitable for
// machine.id when the user has not set one.
func DeriveDefaultMachineID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "local-machine"
	}
	host = strings.ToLower(host)
	// Collapse anything not in [a-z0-9-] to "-" then squash repeats.
	cleanRE := regexp.MustCompile(`[^a-z0-9-]`)
	host = cleanRE.ReplaceAllString(host, "-")
	repeatRE := regexp.MustCompile(`-+`)
	host = repeatRE.ReplaceAllString(host, "-")
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
		APIVersion: "rensei.dev/v1",
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
			URL:       firstNonEmpty(os.Getenv("RENSEI_ORCHESTRATOR_URL"), "https://platform.rensei.dev"),
			AuthToken: os.Getenv("RENSEI_DAEMON_TOKEN"),
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
