package muse

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/harnessstate"
)

// Card skills ride the workspace the trusted host loads.
//
// A Muse session started with --trust-workspace loads workspace skills
// from the host's project discovery path, <workspace>/.agents/skills/.
// Card skills arrive inline on the spawn spec (agent.Spec.CardSkills),
// so the provider materializes each one as
// <workspace>/.agents/skills/donmai-<skill>/SKILL.md before the serve
// child starts. The donmai- prefix namespaces harness-written skills
// away from the user's own, and the matching git exclude glob keeps
// them out of the user's diff.

// cardSkillsRoot is the host's project skill discovery directory,
// relative to the session workspace root.
const cardSkillsRoot = ".agents/skills"

// cardSkillNamePrefix namespaces every materialized card skill so a
// session never collides with (or overwrites) a user-owned skill.
const cardSkillNamePrefix = "donmai-"

// cardSkillsExcludeGlob is the git exclude entry that keeps
// materialized card skills out of `git status` for the checkout that
// runs the session. A trailing slash matches each skill directory and
// everything under it, and can never match a FILE that happens to
// share the name.
const cardSkillsExcludeGlob = ".agents/skills/donmai-*/"

// cardSkillDescriptionLimit bounds the synthesized frontmatter
// description to one short line: it names the skill for the host's
// loader, it is not the skill content.
const cardSkillDescriptionLimit = 160

// ensureCardSkillsExcluded is the seam through which materialization
// keeps card skills out of the user's diff. It defaults to the real
// git-exclude write; tests replace it to capture the entry without
// needing a git checkout.
var ensureCardSkillsExcluded = func(workspaceRoot string) error {
	return harnessstate.EnsureGitExcluded(workspaceRoot, cardSkillsExcludeGlob)
}

// materializeCardSkills writes every card skill on the spec into the
// workspace's project skill discovery path so a trusted host start
// loads them as native workspace skills. It is a no-op (no files, no
// exclude write) when the spec carries no card skills.
//
// Fail-closed: an empty skill id, a name collision after
// normalization, or any filesystem error is returned and the caller
// must NOT start the session — a session that runs without its card
// skills would silently run under a weaker policy than admitted. The
// git-exclude write is deliberately best-effort instead: a session
// whose exclude entry could not be written is noisier (skills show in
// `git status`), not broken, and refusing to spawn over git hygiene
// would turn a cosmetic problem into a hard failure (mirrors the pi
// harness's materializeExtension posture).
func materializeCardSkills(workspaceRoot string, skills []agent.CardSkill) error {
	if len(skills) == 0 {
		return nil
	}
	if strings.TrimSpace(workspaceRoot) == "" {
		return fmt.Errorf("muse: card skills materialization requires a workspace root")
	}
	seen := make(map[string]string, len(skills))
	for _, skill := range skills {
		name, err := cardSkillName(skill.ID)
		if err != nil {
			return err
		}
		if prev, dup := seen[name]; dup {
			return fmt.Errorf("muse: duplicate card skill %q (collides with %q after normalization)", skill.ID, prev)
		}
		seen[name] = skill.ID
		dir := filepath.Join(workspaceRoot, cardSkillsRoot, name)
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: workspace content directory, world-readable exactly like the checkout around it.
			return fmt.Errorf("muse: create card skill dir %q: %w", name, err)
		}
		content := renderCardSkillFile(name, skill)
		path := filepath.Join(dir, "SKILL.md")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return fmt.Errorf("muse: write card skill %q: %w", name, err)
		}
	}
	// Best-effort (see above): a failed exclude write never fails the spawn.
	_ = ensureCardSkillsExcluded(workspaceRoot)
	return nil
}

// cardSkillName normalizes a card skill id into the materialized
// directory (and frontmatter) name: the donmai- prefix plus the id
// lowercased to its [a-z0-9-] skeleton, with runs of any other byte
// folding to a single dash. An id that normalizes to nothing (or
// nothing but the prefix) is malformed input and fails closed.
func cardSkillName(id string) (string, error) {
	var b strings.Builder
	b.WriteString(cardSkillNamePrefix)
	prevDash := true // the prefix ends in a dash; fold leading separators into it
	for _, r := range strings.ToLower(strings.TrimSpace(id)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	name := strings.TrimRight(b.String(), "-")
	// The trim above strips the prefix's own trailing dash when the id
	// contributes nothing, so both spellings mean "no usable name".
	if name == cardSkillNamePrefix || name == strings.TrimSuffix(cardSkillNamePrefix, "-") {
		return "", fmt.Errorf("muse: card skill id %q has no usable name after normalization", id)
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(id)), cardSkillNamePrefix) {
		// The id already carries the namespace: folding the prefix twice
		// (donmai-donmai-*) would drift from the card's own naming, so
		// strip the doubled prefix back to one.
		name = cardSkillNamePrefix + strings.TrimPrefix(name, cardSkillNamePrefix+cardSkillNamePrefix)
	}
	return name, nil
}

// renderCardSkillFile renders one loadable SKILL.md: the frontmatter
// the host requires (name + description — a skill missing either is
// rejected by the host's own validator) followed by the card body
// verbatim. The body is never parsed for frontmatter of its own: card
// bodies are prompt markdown, not skill packages, and synthesizing
// exactly one frontmatter block keeps what the host loads identical
// to what the spec carried.
func renderCardSkillFile(name string, skill agent.CardSkill) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + yamlSingleLine(name) + "\n")
	b.WriteString("description: " + yamlSingleLine(cardSkillDescription(name, skill)) + "\n")
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(skill.Body) + "\n")
	return b.String()
}

// cardSkillDescription derives the required one-line skill
// description from the body's first substantive line (headings,
// quotes, and list markers stripped) bounded to
// cardSkillDescriptionLimit. A body with no substantive line falls
// back to an explicit card-origin description so the file still
// validates and the skill's provenance stays visible to the host.
func cardSkillDescription(name string, skill agent.CardSkill) string {
	for line := range strings.Lines(strings.TrimSpace(skill.Body)) {
		trimmed := strings.TrimSpace(strings.TrimLeft(line, "#>*- \t"))
		if trimmed == "" {
			continue
		}
		if len(trimmed) > cardSkillDescriptionLimit {
			trimmed = strings.TrimSpace(trimmed[:cardSkillDescriptionLimit]) + "…"
		}
		return trimmed
	}
	return "Session card skill " + name + "."
}

// yamlSingleLine renders one double-quoted YAML scalar on a single
// line: backslashes and double quotes escaped, control bytes and
// newlines folded to spaces. Frontmatter values here are
// harness-synthesized (never raw multi-line markdown), so folding is
// lossless for what the host reads.
func yamlSingleLine(s string) string {
	folded := strings.Join(strings.Fields(s), " ")
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range folded {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
