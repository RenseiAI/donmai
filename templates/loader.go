// Package templates provides Go scaffolding for loading the YAML-based
// workflow template system that mirrors the TS implementation in
// packages/core/src/templates/ of the legacy agentfactory repo.
//
// Current state (Phase H): stub loader only.
// Full Handlebars rendering, partial support, TemplateContext, and
// ToolPermissionAdapter are deferred to H+1 … H+4 follow-up lanes.
// See docs/templates-port-audit.md for the porting plan.
package templates

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Kind discriminates the two top-level YAML document types in the
// template system. The TS side uses string literals; Go uses a typed
// constant for exhaustive switch safety.
type Kind string

const (
	KindWorkflowTemplate Kind = "WorkflowTemplate"
	KindPartialTemplate  Kind = "PartialTemplate"
)

// ToolPermission is the provider-agnostic permission value that lives
// inside a template's tools.allow / tools.disallow list.
//
// The TS union type is:
//
//	type ToolPermission = { shell: string } | "user-input" | string
//
// In YAML the value is either a plain string (e.g. "user-input") or a
// mapping with a single "shell" key. We decode both into this struct:
// when Shell is empty the raw string is stored in Raw.
type ToolPermission struct {
	// Shell is set when the YAML entry is { shell: "pnpm *" }.
	Shell string `yaml:"shell,omitempty"`
	// Raw is set when the YAML entry is a plain string, e.g. "user-input".
	Raw string `yaml:"-"`
}

// UnmarshalYAML decodes a ToolPermission from either a plain scalar
// string or a { shell: "…" } mapping.
func (t *ToolPermission) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		t.Raw = value.Value
		return nil
	case yaml.MappingNode:
		// Decode the mapping normally; yaml.v3 will fill Shell via the tag.
		type raw ToolPermission
		return value.Decode((*raw)(t))
	default:
		return fmt.Errorf("templates: ToolPermission must be a string or mapping, got tag %q", value.Tag)
	}
}

// ToolsBlock is the optional tools section of a WorkflowTemplate.
type ToolsBlock struct {
	Allow    []ToolPermission `yaml:"allow,omitempty"`
	Disallow []ToolPermission `yaml:"disallow,omitempty"`
}

// Metadata is the metadata block shared by both template kinds.
type Metadata struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	// WorkType is present on WorkflowTemplate metadata.
	WorkType string `yaml:"workType,omitempty"`
	// Frontend is present on PartialTemplate metadata; when set the
	// partial is frontend-specific (e.g., "linear").
	Frontend string `yaml:"frontend,omitempty"`
}

// Template is the unified Go representation of a parsed YAML template
// document.  Both WorkflowTemplate and PartialTemplate decode into this
// struct; the Kind field discriminates them.
//
// Phase H only populates the frontmatter fields (apiVersion, kind,
// metadata, tools). The prompt/content string is kept as a raw string
// pending the raymond Handlebars integration in H+1.
type Template struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       Kind   `yaml:"kind"`
	Metadata   Metadata

	// Prompt is populated for WorkflowTemplate documents.
	Prompt string `yaml:"prompt,omitempty"`
	// Content is populated for PartialTemplate documents.
	Content string `yaml:"content,omitempty"`
	// Tools is populated for WorkflowTemplate documents.
	Tools *ToolsBlock `yaml:"tools,omitempty"`
}

// validate returns a non-nil error when the template is structurally
// invalid.  Intentionally minimal for the H stub; full Zod-equivalent
// validation is deferred to H+1.
func (t *Template) validate() error {
	if t.APIVersion != "v1" {
		return fmt.Errorf("templates: unsupported apiVersion %q (want \"v1\")", t.APIVersion)
	}
	switch t.Kind {
	case KindWorkflowTemplate:
		if t.Metadata.Name == "" {
			return fmt.Errorf("templates: WorkflowTemplate missing metadata.name")
		}
		if t.Metadata.WorkType == "" {
			return fmt.Errorf("templates: WorkflowTemplate %q missing metadata.workType", t.Metadata.Name)
		}
		if t.Prompt == "" {
			return fmt.Errorf("templates: WorkflowTemplate %q has empty prompt", t.Metadata.Name)
		}
	case KindPartialTemplate:
		if t.Metadata.Name == "" {
			return fmt.Errorf("templates: PartialTemplate missing metadata.name")
		}
		if t.Content == "" {
			return fmt.Errorf("templates: PartialTemplate %q has empty content", t.Metadata.Name)
		}
	case "":
		return fmt.Errorf("templates: missing kind field")
	default:
		return fmt.Errorf("templates: unknown kind %q (want WorkflowTemplate or PartialTemplate)", t.Kind)
	}
	return nil
}

// Load reads the YAML file at path, parses it into a Template, and
// validates the frontmatter.  It returns a non-nil error when the file
// cannot be read, is malformed YAML, or fails basic schema validation.
//
// Load is the single entry point for the templates package in Phase H.
// Rendering (Handlebars) and registry management are deferred to H+1.
func Load(path string) (Template, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Template{}, fmt.Errorf("templates: read %q: %w", path, err)
	}

	var t Template
	if err := yaml.Unmarshal(data, &t); err != nil {
		return Template{}, fmt.Errorf("templates: parse %q: %w", path, err)
	}

	if err := t.validate(); err != nil {
		return Template{}, fmt.Errorf("templates: validate %q: %w", path, err)
	}

	return t, nil
}

// unmarshalYAML is an internal helper shared with registry.go.
// It decodes raw YAML bytes into a Template without reading from disk.
func unmarshalYAML(data []byte, t *Template) error {
	return yaml.Unmarshal(data, t)
}
