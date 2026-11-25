package tempdoctor

import (
	"strings"

	providercodex "github.com/RenseiAI/donmai/provider/harness/codex"
	"github.com/RenseiAI/donmai/tempown"
)

// Class groups temp-folder entries by who produced them.
type Class string

const (
	// ClassDaemon is a temp artefact a donmai probe, harness session or the
	// runner creates (a tempown producer), recorded or legacy.
	ClassDaemon Class = "daemon"
	// ClassCodex is a codex harness home or app-server socket directory,
	// reclaimed by the codex orphan sweep.
	ClassCodex Class = "codex"
	// ClassDaemonCache is a donmai cache shared across sessions. Never
	// removed by the janitor or by --fix.
	ClassDaemonCache Class = "daemon-cache"
	// ClassToolCache is a third-party tool cache shared by every process that
	// inherits this temp folder. Removed only on explicit request.
	ClassToolCache Class = "tool-cache"
	// ClassOther is everything else.
	ClassOther Class = "other"
)

// namedPattern matches a top-level entry by exact name or by prefix.
type namedPattern struct {
	Label   string
	Pattern string // display form: "tsx-*" or "jiti"
	prefix  string
	exact   string
}

func (p namedPattern) matches(name string) bool {
	if p.exact != "" {
		return name == p.exact
	}
	return strings.HasPrefix(name, p.prefix)
}

// toolCaches are the shared third-party caches seen in agent hosts' temp
// folders. Each is one tool's own store; deleting one costs every process
// that uses it a cold start, so only an explicit, confirmed request removes
// them.
var toolCaches = []namedPattern{
	{Label: "tsx cache", Pattern: "tsx-*", prefix: "tsx-"},
	{Label: "Node compile cache", Pattern: "node-compile-cache", exact: "node-compile-cache"},
	{Label: "Go build work dirs", Pattern: "go-build*", prefix: "go-build"},
	{Label: "Codex CLI temp files", Pattern: ".com.openai.codex.*", prefix: ".com.openai.codex."},
	{Label: "jiti cache", Pattern: "jiti", exact: "jiti"},
}

// daemonCaches are donmai's own caches under the temp folder. They are
// shared across sessions by design (content-addressed, linked into live
// sessions), so nothing here removes them.
var daemonCaches = []namedPattern{
	{Label: "pi extension cache", Pattern: "donmai-pi-ext-cache", exact: "donmai-pi-ext-cache"},
	{Label: "codex plugin cache", Pattern: "donmai-codex-plugin-cache", exact: "donmai-codex-plugin-cache"},
}

// ToolCachePatterns returns the display patterns of the shared tool caches
// `--include-tool-caches` may remove.
func ToolCachePatterns() []string {
	out := make([]string, 0, len(toolCaches))
	for _, c := range toolCaches {
		out = append(out, c.Pattern)
	}
	return out
}

// classify returns the class and group key of a top-level entry name.
func classify(name string) (Class, string) {
	if p, _, ok := tempown.Parse(name); ok {
		return ClassDaemon, p.Label
	}
	if p, ok := tempown.Legacy(name); ok {
		return ClassDaemon, p.Label
	}
	if providercodex.IsArtifactName(name) {
		if providercodex.IsHomeName(name) {
			return ClassCodex, "codex homes"
		}
		return ClassCodex, "codex app-server sockets"
	}
	for _, c := range daemonCaches {
		if c.matches(name) {
			return ClassDaemonCache, c.Label
		}
	}
	for _, c := range toolCaches {
		if c.matches(name) {
			return ClassToolCache, c.Label
		}
	}
	return ClassOther, "other"
}

// isToolCache reports whether name is a shared tool cache entry.
func isToolCache(name string) bool {
	class, _ := classify(name)
	return class == ClassToolCache
}

// patternFor returns the display pattern of a group, for reports.
func patternFor(class Class, label string) string {
	switch class {
	case ClassDaemon:
		for _, p := range tempown.Producers() {
			if p.Label == label {
				return p.Prefix + "*" + p.Suffix
			}
		}
	case ClassCodex:
		if label == "codex homes" {
			return "donmai-codex-home-*"
		}
		return "donmai-codex-app-*"
	case ClassDaemonCache:
		for _, c := range daemonCaches {
			if c.Label == label {
				return c.Pattern
			}
		}
	case ClassToolCache:
		for _, c := range toolCaches {
			if c.Label == label {
				return c.Pattern
			}
		}
	}
	return "*"
}
