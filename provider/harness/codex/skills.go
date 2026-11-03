package codex

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/agent"
)

const (
	// codexSkillsDirName is the user-scope skills root inside a
	// CODEX_HOME. Deprecated-but-still-scanned upstream; the spike in
	// docs/codex-card-skills-spike.md proves the session's isolated
	// home is discovered in an untrusted project.
	codexSkillsDirName = "skills"
	// codexSkillFileName is the per-skill document the CLI parses.
	codexSkillFileName = "SKILL.md"
	// cardSkillDirPrefix namespaces session-delivered skills so they
	// never collide with each other across cards or with the CLI's
	// own `.system` subtree (which starts with a dot).
	cardSkillDirPrefix = "donmai-"
	// cardSkillFallbackID names a skill whose card id sanitizes to
	// nothing. The CLI defaults a missing name to the directory name
	// and requires a non-empty description, so delivery must always
	// mint a usable pair.
	cardSkillFallbackID = "skill"
	// maxSkillNameLen is the CLI's SKILL.md frontmatter `name` limit:
	// longer names are a parse error and the skill is dropped.
	maxSkillNameLen = 64
	// maxSkillDescriptionRunes caps the derived frontmatter
	// `description`. The CLI collapses it to one line and spends it
	// from a context budget shared by every discovered skill, so one
	// card skill must not eat the whole budget with a pathological
	// first line.
	maxSkillDescriptionRunes = 500
)

// deliverCardSkills renders spec Skills as SKILL.md files under the
// session's isolated CODEX_HOME and verifies each file by reading it
// back and comparing sha256 digests. home must be the boundary-owned
// isolated home (never ambient operator state, never the worktree).
//
// An empty skill list is a no-op that touches nothing, so skill-less
// sessions keep the byte-identical fresh-home shape. Any failure —
// unwritable home, symlink where a directory or file should be, short
// write, readback mismatch — fails the whole batch: the caller denies
// the spawn rather than running a session whose card guidance is
// silently partial.
func deliverCardSkills(home string, skills []agent.CardSkill) error {
	if len(skills) == 0 {
		return nil
	}
	if strings.TrimSpace(home) == "" {
		return errors.New("codex deliver card skills: isolated home is empty")
	}
	if err := rejectSymlink(home, true); err != nil {
		return fmt.Errorf("codex deliver card skills: %w", err)
	}
	skillsRoot := filepath.Join(home, codexSkillsDirName)
	if err := os.MkdirAll(skillsRoot, codexHomeMode); err != nil {
		return fmt.Errorf("codex deliver card skills: create skills root: %w", err)
	}
	if err := rejectSymlink(skillsRoot, true); err != nil {
		return fmt.Errorf("codex deliver card skills: %w", err)
	}

	used := make(map[string]struct{}, len(skills))
	for i := range skills {
		dirName := cardSkillDirName(skills[i].ID, used)
		used[dirName] = struct{}{}
		skillDir := filepath.Join(skillsRoot, dirName)
		if err := os.MkdirAll(skillDir, codexHomeMode); err != nil {
			return fmt.Errorf("codex deliver card skills: create skill %q: %w", dirName, err)
		}
		if err := rejectSymlink(skillDir, true); err != nil {
			return fmt.Errorf("codex deliver card skills: skill %q: %w", dirName, err)
		}
		content := renderCardSkillFile(dirName, skills[i].Body)
		path := filepath.Join(skillDir, codexSkillFileName)
		if err := writeSkillFileNoFollow(path, content); err != nil {
			return fmt.Errorf("codex deliver card skills: skill %q: %w", dirName, err)
		}
		if err := verifySkillFileReadback(path, content); err != nil {
			return fmt.Errorf("codex deliver card skills: skill %q: %w", dirName, err)
		}
	}
	return nil
}

// cardSkillDirName maps a card skill id to its delivery directory name:
// `donmai-` plus the sanitized id, deduplicated against used (input
// order wins; later collisions take a numeric suffix). The result is
// always within the CLI's 64-character name limit and never empty,
// never "."/"..", and never the CLI's `.system` subtree.
func cardSkillDirName(id string, used map[string]struct{}) string {
	slug := sanitizeSkillSlug(id)
	if slug == "" {
		slug = cardSkillFallbackID
	}
	maxSlug := maxSkillNameLen - len(cardSkillDirPrefix)
	if len(slug) > maxSlug {
		slug = slug[:maxSlug]
		slug = strings.Trim(slug, "-")
		if slug == "" {
			slug = cardSkillFallbackID
		}
	}
	base := cardSkillDirPrefix + slug
	if _, taken := used[base]; !taken {
		return base
	}
	for n := 2; ; n++ {
		suffix := fmt.Sprintf("-%d", n)
		trimmed := slug
		if len(cardSkillDirPrefix)+len(trimmed)+len(suffix) > maxSkillNameLen {
			trimmed = strings.Trim(trimmed[:maxSlug-len(suffix)], "-")
			if trimmed == "" {
				trimmed = cardSkillFallbackID
			}
		}
		candidate := cardSkillDirPrefix + trimmed + suffix
		if _, taken := used[candidate]; !taken {
			return candidate
		}
	}
}

// sanitizeSkillSlug lowercases id and maps every run of characters
// outside [a-z0-9] to a single '-', trimmed of leading/trailing
// dashes. The output is path-safe (no separators, no dots) and
// YAML-safe unquoted.
func sanitizeSkillSlug(id string) string {
	lower := strings.ToLower(id)
	var b strings.Builder
	b.Grow(len(lower))
	dash := false
	for _, r := range lower {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// renderCardSkillFile builds the SKILL.md bytes: YAML frontmatter with
// the directory name and a derived single-line description, then the
// card body verbatim. Tool restrictions are deliberately NOT
// restated here — they stay enforced runner-side (Spec → approval
// bridge), so the file carries the minimum the CLI parser needs and a
// quoting bug cannot drop the skill.
func renderCardSkillFile(dirName, body string) []byte {
	var b strings.Builder
	b.WriteString("---\nname: ")
	b.WriteString(dirName)
	b.WriteString("\ndescription: \"")
	b.WriteString(yamlDoubleQuoted(cardSkillDescription(dirName, body)))
	b.WriteString("\"\n---\n")
	if trimmed := strings.TrimSpace(body); trimmed != "" {
		b.WriteString(trimmed)
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// cardSkillDescription derives the required frontmatter description:
// the first non-empty body line, whitespace-collapsed and capped.
// The CLI requires a non-empty description and drops the skill without
// one, so an empty body still yields a stable fallback.
func cardSkillDescription(dirName, body string) string {
	for _, line := range strings.Split(body, "\n") {
		if collapsed := collapseWhitespace(line); collapsed != "" {
			return truncateRunes(collapsed, maxSkillDescriptionRunes)
		}
	}
	return "Card skill " + dirName + "."
}

// collapseWhitespace joins the whitespace-separated fields with single
// spaces and drops non-printable runes, mirroring the CLI's own
// single-line sanitization so what we write is what it indexes.
func collapseWhitespace(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	joined := strings.Join(fields, " ")
	return strings.Map(func(r rune) rune {
		if r == ' ' || unicode.IsPrint(r) {
			return r
		}
		return -1
	}, joined)
}

// truncateRunes cuts s to at most n runes (never splitting UTF-8).
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	var b strings.Builder
	count := 0
	for _, r := range s {
		if count >= n {
			break
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

// yamlDoubleQuoted escapes s for a double-quoted YAML scalar. The input
// is already single-line printable text; only backslash and quote need
// escaping.
func yamlDoubleQuoted(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString("\\\\")
		case '"':
			b.WriteString("\\\"")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// writeSkillFileNoFollow writes content to path, refusing to follow a
// symlink: a resumed session re-delivers into a home its previous agent
// could have learned, so a planted link must deny the spawn rather than
// redirect the write.
func writeSkillFileNoFollow(path string, content []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("skill file path is a symlink")
		}
		if !info.Mode().IsRegular() {
			return errors.New("skill file path is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect skill file: %w", err)
	}
	if err := os.WriteFile(path, content, codexConfigMode); err != nil {
		return fmt.Errorf("write skill file: %w", err)
	}
	return nil
}

// deliverSpecCardSkills renders the session's card skills (Spec.Skills,
// threaded by the runner for headless codex only) into the Provider's
// isolated home. No skills is a no-op; a missing boundary or any
// delivery failure denies the spawn — a session must never run with
// silently partial card guidance.
func (p *Provider) deliverSpecCardSkills(spec agent.Spec) error {
	if len(spec.Skills) == 0 {
		return nil
	}
	if p == nil || p.config == nil || strings.TrimSpace(p.config.home) == "" {
		return errors.New("codex deliver card skills: isolated config boundary is missing")
	}
	return deliverCardSkills(p.config.home, spec.Skills)
}

// verifySkillFileReadback reads path back and compares its sha256
// against the digest of what delivery wrote, proving the bytes on disk
// are the bytes rendered — the same readback shape the MCP config
// proof uses.
func verifySkillFileReadback(path string, want []byte) error {
	got, err := os.ReadFile(path) //nolint:gosec // path is boundary-owned, validated above
	if err != nil {
		return fmt.Errorf("read back skill file: %w", err)
	}
	if sha256.Sum256(got) != sha256.Sum256(want) {
		return errors.New("skill file readback digest mismatch")
	}
	return nil
}
