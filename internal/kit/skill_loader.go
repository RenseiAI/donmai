// Package kit provides runner-facing helpers that collect [provide.skills]
// contributions from installed, active Kits.
//
// The Kit manifest parser and registry live in daemon/kit_registry.go.
// This package is the consumption layer: it bridges the daemon's
// KitRegistry into the runner's prompt-builder + tool-surface, wiring
// the materialization rule from 005-kit-manifest-spec.md §"Composition"
// table row for `skills`:
//
//	skills | Concatenated; duplicate id is an error.
//
// Each active kit's [provide.skills] entries are read from disk (relative
// to the kit's scan path) and their content is appended to the agent's
// system prompt in kit-priority order (higher priority value → earlier
// position, matching the registry spec's "priority tiebreaker" concept).
//
// Tool restriction: SKILL.md files may include a YAML frontmatter block
// that declares a "tools" section with a "disallow" list.  Each disallow
// entry's "shell" value is collected and returned as an additional
// DisallowedTools slice to be merged onto the agent.Spec.  This is a
// subtractive operation — Kit skills can only narrow the tool surface,
// never widen it.
//
// Paths: skill file paths in the manifest are relative to the directory
// that contains the kit's .kit.toml.  The KitRegistry scanPaths are used
// as the search roots.
package kit

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// KitSkillSource is the minimal kit descriptor the skill loader needs.
// Callers (typically the daemon's KitRegistry) supply these.
type KitSkillSource struct {
	// ID is the kit's canonical id (e.g. "spring/java").
	ID string
	// Priority is the kit's declared priority used to order skill bodies
	// in the merged system-prompt append.  Higher value → higher priority
	// → earlier position.
	Priority int
	// ManifestPath is the absolute path to the kit's .kit.toml file.
	// Skill file paths declared in the manifest are resolved relative to
	// the directory containing this file.
	ManifestPath string
	// SkillFiles is the slice of skill file paths declared in the kit's
	// [provide.skills] array (relative to ManifestPath's directory).
	SkillFiles []string
}

// LoadedSkills is the output of LoadSkills: the merged system-prompt
// append text (all skill bodies concatenated in priority order) and the
// aggregated DisallowedTools entries scraped from SKILL.md frontmatter.
type LoadedSkills struct {
	// SystemAppend is the concatenation of all skill bodies in priority
	// order, separated by blank lines.  Empty when no active kit has
	// [provide.skills] entries whose files are readable.
	SystemAppend string

	// DisallowedTools is the union of all "tools.disallow[*].shell" values
	// found in SKILL.md frontmatter across all loaded skill files.  Each
	// entry is a shell-glob pattern suitable for agent.Spec.DisallowedTools.
	DisallowedTools []string
}

// LoadSkills walks sources in descending priority order, reads each
// skill file from disk, collects the body and any frontmatter-declared
// tool disallow rules, and returns the merged result.
//
// Unreadable skill files are skipped with a slog.Warn so a single
// broken kit does not abort the session.  Callers should treat the
// returned error as diagnostic-only; a non-nil error is joined from
// any per-file read failures and does not prevent the returned
// LoadedSkills from being used (it contains whatever was successfully
// loaded).
func LoadSkills(sources []KitSkillSource) (LoadedSkills, error) {
	// Sort descending by priority (higher priority → earlier in prompt).
	sorted := make([]KitSkillSource, len(sources))
	copy(sorted, sources)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Priority != sorted[j].Priority {
			return sorted[i].Priority > sorted[j].Priority
		}
		return sorted[i].ID < sorted[j].ID
	})

	var (
		bodies    []string
		disallows []string
		errs      []string
	)

	for _, src := range sorted {
		kitDir := filepath.Dir(src.ManifestPath)
		for _, relFile := range src.SkillFiles {
			absPath := filepath.Join(kitDir, relFile)
			body, fileDisallows, err := readSkillFile(absPath)
			if err != nil {
				msg := fmt.Sprintf("kit %s: skill %q: %v", src.ID, relFile, err)
				slog.Warn("kit skill loader: skip unreadable skill", //nolint:gosec
					"kitId", src.ID,
					"file", absPath,
					"err", err.Error(),
				)
				errs = append(errs, msg)
				continue
			}
			if body != "" {
				bodies = append(bodies, body)
			}
			disallows = append(disallows, fileDisallows...)
		}
	}

	var joinedErr error
	if len(errs) > 0 {
		joinedErr = fmt.Errorf("skill loader: %s", strings.Join(errs, "; "))
	}

	return LoadedSkills{
		SystemAppend:    strings.Join(bodies, "\n\n"),
		DisallowedTools: deduplicateStrings(disallows),
	}, joinedErr
}

// readSkillFile reads the SKILL.md (or any skill body file) at path.
// It returns the content of the file with YAML frontmatter stripped
// (the frontmatter is parsed for tool disallow rules and not included
// in the body), and the slice of disallowed tool shell patterns
// declared in the frontmatter.
//
// SKILL.md frontmatter convention (Anthropic agentskills.io spec):
//
//	---
//	tools:
//	  disallow:
//	    - shell: "pattern"
//	---
//	... markdown body ...
//
// Files without a frontmatter block are returned as-is (body is the
// full file content, disallows is empty).
func readSkillFile(path string) (body string, disallows []string, err error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-installed kit file
	if err != nil {
		return "", nil, err
	}
	content := string(data)
	frontmatter, rest := splitFrontmatter(content)
	if frontmatter != "" {
		disallows = parseFrontmatterDisallows(frontmatter)
	}
	body = strings.TrimSpace(rest)
	return body, disallows, nil
}

// splitFrontmatter splits a Markdown document into its YAML frontmatter
// block (between leading "---" delimiters) and the remaining body.
// Returns ("", full content) when no frontmatter is present.
func splitFrontmatter(content string) (frontmatter, body string) {
	if !strings.HasPrefix(strings.TrimLeft(content, "\r\n"), "---") {
		return "", content
	}
	// Find the opening "---" line.
	scanner := bufio.NewScanner(strings.NewReader(content))
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if len(lines) == 0 {
		return "", content
	}
	// First line must be exactly "---".
	if strings.TrimSpace(lines[0]) != "---" {
		return "", content
	}
	// Find closing "---".
	closeIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			closeIdx = i
			break
		}
	}
	if closeIdx < 0 {
		return "", content
	}
	frontmatter = strings.Join(lines[1:closeIdx], "\n")
	body = strings.Join(lines[closeIdx+1:], "\n")
	return frontmatter, body
}

// skillFrontmatter is the minimal TOML-free YAML-ish shape we parse for
// tool disallow rules.  We avoid a full YAML parser dependency by using
// a simple hand-rolled parser for the narrow shape we care about:
//
//	tools:
//	  disallow:
//	    - shell: "Bash(rm -rf *)"
//
// A TOML-based kit frontmatter is also accepted when the frontmatter
// block does not use "---" but a [[skills.frontmatter]] section — that
// variant is not emitted by agentskills.io–conforming kits today.
//
// parseFrontmatterDisallows returns the shell patterns found under
// tools.disallow[*].shell. Malformed entries are skipped silently.
func parseFrontmatterDisallows(frontmatter string) []string {
	// Try TOML first (the manifest ecosystem is TOML-first).
	type tomlDisallowEntry struct {
		Shell string `toml:"shell"`
	}
	type tomlToolsBlock struct {
		Disallow []tomlDisallowEntry `toml:"disallow"`
	}
	type tomlFrontmatter struct {
		Tools tomlToolsBlock `toml:"tools"`
	}
	var tf tomlFrontmatter
	if err := toml.Unmarshal([]byte(frontmatter), &tf); err == nil && len(tf.Tools.Disallow) > 0 {
		var out []string
		for _, d := range tf.Tools.Disallow {
			if d.Shell != "" {
				out = append(out, d.Shell)
			}
		}
		return out
	}

	// Fall back to simple line-by-line YAML scanning.
	return parseYAMLDisallows(frontmatter)
}

// parseYAMLDisallows is a narrow line-scanning parser for the
// agentskills.io SKILL.md frontmatter shape:
//
//	tools:
//	  disallow:
//	    - shell: "pattern"
//
// It does not handle anchors, multi-doc, or complex YAML — only the
// specific structure the agentskills.io spec mandates for tool
// restrictions.
func parseYAMLDisallows(frontmatter string) []string {
	const (
		stateRoot     = 0
		stateTools    = 1
		stateDisallow = 2
	)
	state := stateRoot
	var out []string
	for _, raw := range strings.Split(frontmatter, "\n") {
		line := strings.TrimRight(raw, "\r")
		stripped := strings.TrimLeft(line, " \t")
		indent := len(line) - len(stripped)
		key, val, _ := strings.Cut(stripped, ":")

		switch state {
		case stateRoot:
			if strings.TrimSpace(key) == "tools" && indent == 0 {
				state = stateTools
			}
		case stateTools:
			if indent == 0 && stripped != "" {
				state = stateRoot
				continue
			}
			if strings.TrimSpace(key) == "disallow" {
				state = stateDisallow
			}
		case stateDisallow:
			// Back to a lower-indent key means we left the disallow block.
			if indent <= 2 && stripped != "" && !strings.HasPrefix(stripped, "-") {
				state = stateRoot
				continue
			}
			// List entry: "- shell: pattern"
			if strings.HasPrefix(stripped, "-") {
				entry := strings.TrimPrefix(stripped, "-")
				entry = strings.TrimSpace(entry)
				k2, v2, ok := strings.Cut(entry, ":")
				if ok && strings.TrimSpace(k2) == "shell" {
					pattern := strings.TrimSpace(v2)
					pattern = strings.Trim(pattern, `"'`)
					_ = val // suppress unused warning
					if pattern != "" {
						out = append(out, pattern)
					}
				}
			}
		}
	}
	return out
}

// deduplicateStrings returns s with duplicates removed, preserving order.
func deduplicateStrings(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(s))
	out := make([]string, 0, len(s))
	for _, v := range s {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}
