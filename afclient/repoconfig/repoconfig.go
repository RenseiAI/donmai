// Package repoconfig loads and validates the .agentfactory/config.yaml file
// (RepositoryConfig kind).  It is the Go port of
// packages/core/src/config/repository-config.ts.
package repoconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ProjectConfig holds per-project overrides for a monorepo setup.
// Mirrors the TS ProjectConfig type.
type ProjectConfig struct {
	// Path is the root directory of this project within the repository.
	Path string `yaml:"path"`
	// PackageManager overrides the repo-level package manager for this project.
	PackageManager string `yaml:"packageManager,omitempty"`
	// BuildCommand overrides the repo-level build command.
	BuildCommand string `yaml:"buildCommand,omitempty"`
	// TestCommand overrides the repo-level test command.
	TestCommand string `yaml:"testCommand,omitempty"`
	// ValidateCommand overrides the repo-level validate command.
	ValidateCommand string `yaml:"validateCommand,omitempty"`
}

// projectPathValue can be unmarshaled from either a string shorthand
// ("path/to/project") or a full ProjectConfig object.
type projectPathValue struct {
	cfg ProjectConfig
}

func (p *projectPathValue) UnmarshalYAML(value *yaml.Node) error {
	// String shorthand: "apps/family"
	if value.Kind == yaml.ScalarNode {
		p.cfg = ProjectConfig{Path: value.Value}
		return nil
	}
	// Object form
	var cfg ProjectConfig
	if err := value.Decode(&cfg); err != nil {
		return err
	}
	p.cfg = cfg
	return nil
}

// RepositoryConfig is the top-level schema for .agentfactory/config.yaml.
// Mirrors the TS RepositoryConfigSchema (minus complex sections not needed by
// the Go orchestrator: mergeQueue, routing, quality, etc. — those are parsed
// as raw YAML so existing config files continue to load without error).
type RepositoryConfig struct {
	// APIVersion should be "v1".
	APIVersion string `yaml:"apiVersion"`
	// Kind must be "RepositoryConfig".
	Kind string `yaml:"kind"`
	// Repository is the git remote URL pattern validated at startup.
	Repository string `yaml:"repository,omitempty"`
	// AllowedProjects lists the Linear project names this repo handles.
	// Mutually exclusive with ProjectPaths.
	AllowedProjects []string `yaml:"allowedProjects,omitempty"`
	// ProjectPaths maps project names to their normalized ProjectConfig.
	// Populated after Load() — use GetEffectiveAllowedProjects for the list.
	ProjectPaths map[string]ProjectConfig
	// SharedPaths lists directories any project's agent may modify.
	SharedPaths []string `yaml:"sharedPaths,omitempty"`
	// PackageManager is the default package manager ("pnpm", "npm", etc.).
	PackageManager string `yaml:"packageManager,omitempty"`
	// BuildCommand is the default build command.
	BuildCommand string `yaml:"buildCommand,omitempty"`
	// TestCommand is the default test command.
	TestCommand string `yaml:"testCommand,omitempty"`
	// ValidateCommand is the default validate command.
	ValidateCommand string `yaml:"validateCommand,omitempty"`
	// LinearCli is the command to invoke the Linear CLI.
	LinearCli string `yaml:"linearCli,omitempty"`
}

// rawRepositoryConfig is used to decode the YAML with flexible projectPaths.
type rawRepositoryConfig struct {
	APIVersion      string                       `yaml:"apiVersion"`
	Kind            string                       `yaml:"kind"`
	Repository      string                       `yaml:"repository,omitempty"`
	AllowedProjects []string                     `yaml:"allowedProjects,omitempty"`
	ProjectPaths    map[string]*projectPathValue `yaml:"projectPaths,omitempty"`
	SharedPaths     []string                     `yaml:"sharedPaths,omitempty"`
	PackageManager  string                       `yaml:"packageManager,omitempty"`
	BuildCommand    string                       `yaml:"buildCommand,omitempty"`
	TestCommand     string                       `yaml:"testCommand,omitempty"`
	ValidateCommand string                       `yaml:"validateCommand,omitempty"`
	LinearCli       string                       `yaml:"linearCli,omitempty"`
}

// ErrConfigNotFound is returned when .agentfactory/config.yaml does not exist.
var ErrConfigNotFound = errors.New("repoconfig: .agentfactory/config.yaml not found")

// Load reads and validates .agentfactory/config.yaml from gitRoot.
// Returns ErrConfigNotFound when the file does not exist.
func Load(gitRoot string) (*RepositoryConfig, error) {
	path := filepath.Join(gitRoot, ".agentfactory", "config.yaml")
	data, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrConfigNotFound
		}
		return nil, fmt.Errorf("repoconfig: read %s: %w", path, err)
	}
	return parse(data)
}

// parse decodes raw YAML bytes into a validated RepositoryConfig.
func parse(data []byte) (*RepositoryConfig, error) {
	var raw rawRepositoryConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("repoconfig: parse yaml: %w", err)
	}

	if raw.Kind != "RepositoryConfig" {
		return nil, fmt.Errorf("repoconfig: expected kind RepositoryConfig, got %q", raw.Kind)
	}
	if len(raw.AllowedProjects) > 0 && len(raw.ProjectPaths) > 0 {
		return nil, errors.New("repoconfig: allowedProjects and projectPaths are mutually exclusive")
	}

	cfg := &RepositoryConfig{
		APIVersion:      raw.APIVersion,
		Kind:            raw.Kind,
		Repository:      raw.Repository,
		AllowedProjects: raw.AllowedProjects,
		SharedPaths:     raw.SharedPaths,
		PackageManager:  raw.PackageManager,
		BuildCommand:    raw.BuildCommand,
		TestCommand:     raw.TestCommand,
		ValidateCommand: raw.ValidateCommand,
		LinearCli:       raw.LinearCli,
	}

	// Normalize projectPaths
	if len(raw.ProjectPaths) > 0 {
		cfg.ProjectPaths = make(map[string]ProjectConfig, len(raw.ProjectPaths))
		for name, v := range raw.ProjectPaths {
			pc := v.cfg
			// Inherit repo-level defaults where the project doesn't override
			if pc.PackageManager == "" {
				pc.PackageManager = raw.PackageManager
			}
			if pc.BuildCommand == "" {
				pc.BuildCommand = raw.BuildCommand
			}
			if pc.TestCommand == "" {
				pc.TestCommand = raw.TestCommand
			}
			if pc.ValidateCommand == "" {
				pc.ValidateCommand = raw.ValidateCommand
			}
			cfg.ProjectPaths[name] = pc
		}
	}

	return cfg, nil
}

// GetEffectiveAllowedProjects returns the list of project names this config
// authorises.  When ProjectPaths is set the keys are the allowed projects;
// otherwise AllowedProjects is returned.  Returns nil when neither is set
// (meaning all projects are allowed).
func (c *RepositoryConfig) GetEffectiveAllowedProjects() []string {
	if len(c.ProjectPaths) > 0 {
		names := make([]string, 0, len(c.ProjectPaths))
		for k := range c.ProjectPaths {
			names = append(names, k)
		}
		return names
	}
	if len(c.AllowedProjects) > 0 {
		return c.AllowedProjects
	}
	return nil
}

// IsProjectAllowed reports whether the given project name is allowed by this
// config.  When no allowlist is configured (neither AllowedProjects nor
// ProjectPaths), all projects are allowed.
func (c *RepositoryConfig) IsProjectAllowed(project string) bool {
	allowed := c.GetEffectiveAllowedProjects()
	if len(allowed) == 0 {
		return true
	}
	for _, p := range allowed {
		if p == project {
			return true
		}
	}
	return false
}

// GetProjectConfig returns the ProjectConfig for the named project, or nil
// when it is not found.
func (c *RepositoryConfig) GetProjectConfig(project string) *ProjectConfig {
	if c.ProjectPaths == nil {
		return nil
	}
	if pc, ok := c.ProjectPaths[project]; ok {
		return &pc
	}
	return nil
}
