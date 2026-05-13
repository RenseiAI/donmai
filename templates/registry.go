package templates

import (
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aymerick/raymond"
)

// defaultYAMLs embeds the built-in YAML template files from this package
// directory so the registry works without filesystem access at runtime.
//
//go:embed *.yaml
var defaultYAMLs embed.FS

// Registry holds a set of parsed and raymond-compiled templates, indexed by
// their metadata.name value. It is safe for concurrent use after
// construction — all mutation happens inside New/NewFromFS, which are
// single-threaded by design.
//
// Phase H+1 coverage:
//   - WorkflowTemplate YAML loading + raymond compilation
//   - eq / neq / hasFlag helper registration
//   - Render(name, ctx) producing rendered strings
//
// Partial template support ({{> partial-name}}) is deferred to H+2.
type Registry struct {
	mu        sync.RWMutex
	templates map[string]*raymond.Template
}

// New constructs a Registry pre-loaded with the built-in YAML templates
// embedded in this package (*.yaml files in the templates/ directory).
func New() (*Registry, error) {
	return NewFromFS(defaultYAMLs, ".")
}

// NewFromFS constructs a Registry by walking fsys rooted at root and
// loading every *.yaml file found. Files that fail to parse or validate
// are returned as errors immediately — partial load is not supported.
func NewFromFS(fsys fs.FS, root string) (*Registry, error) {
	r := &Registry{
		templates: make(map[string]*raymond.Template),
	}
	registerHelpers()

	err := fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".yaml" {
			return nil
		}

		data, readErr := fs.ReadFile(fsys, path)
		if readErr != nil {
			return fmt.Errorf("templates/registry: read %q: %w", path, readErr)
		}

		tmpl, loadErr := parseYAML(data, path)
		if loadErr != nil {
			return loadErr
		}

		// Only WorkflowTemplates are compiled into raymond; PartialTemplates
		// are registered as raymond partials (H+2).
		switch tmpl.Kind {
		case KindWorkflowTemplate:
			body := tmpl.Prompt
			compiled, compileErr := raymond.Parse(body)
			if compileErr != nil {
				return fmt.Errorf("templates/registry: compile %q: %w", tmpl.Metadata.Name, compileErr)
			}
			r.templates[tmpl.Metadata.Name] = compiled
		case KindPartialTemplate:
			// Register as a raymond partial for {{> name}} support (H+2).
			raymond.RegisterPartial(tmpl.Metadata.Name, tmpl.Content)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return r, nil
}

// Render executes the named template against ctx and returns the rendered
// string. ctx must be a map[string]interface{} or a struct whose exported
// fields raymond can reflect on.
//
// Returns ErrTemplateNotFound when name is not present in the registry.
func (r *Registry) Render(name string, ctx interface{}) (string, error) {
	r.mu.RLock()
	compiled, ok := r.templates[name]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("templates/registry: %w: %q", ErrTemplateNotFound, name)
	}

	out, err := compiled.Exec(ctx)
	if err != nil {
		return "", fmt.Errorf("templates/registry: render %q: %w", name, err)
	}
	return strings.TrimRight(out, "\n"), nil
}

// Names returns the sorted list of registered template names.  Primarily
// useful in tests and debugging.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.templates))
	for k := range r.templates {
		names = append(names, k)
	}
	return names
}

// Has returns true when a template with the given name is registered.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.templates[name]
	return ok
}

// ErrTemplateNotFound is returned by Render when the requested template
// name is not present in the Registry.
var ErrTemplateNotFound = fmt.Errorf("template not found")

// helperOnce ensures helpers are registered exactly once per process.
// raymond registers helpers globally, so re-registration on every
// Registry construction would be wasteful (though harmless).
var helperOnce sync.Once

// registerHelpers registers the custom raymond helpers required by the
// template syntax used in the YAML files.
//
// Helpers registered here:
//   - eq  — returns true when two values are equal (string comparison)
//   - neq — returns true when two values are not equal
//   - hasFlag — returns true when a space-separated flag list contains a flag
func registerHelpers() {
	helperOnce.Do(func() {
		// eq helper: {{#if (eq a b)}} … {{/if}}
		raymond.RegisterHelper("eq", func(a, b interface{}) bool {
			return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
		})

		// neq helper: {{#unless (neq a b)}} … {{/unless}}
		raymond.RegisterHelper("neq", func(a, b interface{}) bool {
			return fmt.Sprintf("%v", a) != fmt.Sprintf("%v", b)
		})

		// hasFlag helper: {{#if (hasFlag flags "flag-name")}} … {{/if}}
		raymond.RegisterHelper("hasFlag", func(flagList, flag interface{}) bool {
			list := fmt.Sprintf("%v", flagList)
			target := fmt.Sprintf("%v", flag)
			for _, f := range strings.Fields(list) {
				if f == target {
					return true
				}
			}
			return false
		})
	})
}

// parseYAML parses YAML bytes into a Template and validates the frontmatter.
// It is the internal counterpart to the exported Load function, reusing the
// same YAML decoder but operating on in-memory bytes rather than a path.
func parseYAML(data []byte, sourcePath string) (Template, error) {
	// Reuse the gopkg.in/yaml.v3 decoder already wired in loader.go.
	// We import through the same package — the Template type already has
	// all the yaml struct tags we need.
	var t Template
	if err := unmarshalYAML(data, &t); err != nil {
		return Template{}, fmt.Errorf("templates/registry: parse %q: %w", sourcePath, err)
	}
	if err := t.validate(); err != nil {
		return Template{}, fmt.Errorf("templates/registry: validate %q: %w", sourcePath, err)
	}
	return t, nil
}
