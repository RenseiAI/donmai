package pi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// Card-skill delivery for the pi harness.
//
// Agent-card inline skills are materialized into a per-session directory
// outside the worktree diff and passed with pi's native `--skill` flag.
// Layout:
//
//	<state-root>/skills-injected/<id>/SKILL.md
//
// Each skill gets its own directory containing a single SKILL.md with YAML
// frontmatter (name + description) followed by the card body verbatim. The
// caller passes each directory with `--skill <dir>` (repeatable) and
// `--no-skills` (so a repo skill with the same name cannot shadow the card
// skill — pi keeps the first skill found on a name collision, and CLI paths
// sort last without it).
//
// Guarantees:
//   - ""/nil with no error when skills are empty (no flag emitted — the spawn
//     argv stays byte-identical to a skill-less session).
//   - Entries with a whitespace-only body are skipped (they contribute no
//     SKILL.md, mirroring foldInlineSkills which injects no text for them).
//   - Bodies over maxCardSkillBodyBytes, ids that are empty, too long,
//     duplicated, or unsafe as a path segment or as a pi skill name, are
//     rejected with an error wrapping agent.ErrSpawnFailed — the spawn fails
//     closed before any subprocess starts.
//   - After writing, every file is read back and its sha256 compared to the
//     bytes just written (readback): a short write or a concurrent
//     modification surfaces here instead of reaching the agent as a
//     truncated skill.
//
// The directory sits inside the session state root (confinement.go
// sessionStateRoot), which the parent records in the checkout's exclude file
// at materialize time — so no skill file can appear in `git status` or the
// user's worktree diff.

// maxCardSkillBodyBytes caps one card skill body materialized into the
// per-session skills directory. 64 KiB is far past any realistic skill
// (a SKILL.md of that size is already a small manual) and keeps a runaway
// card from being read into memory unbounded. Bodies over the cap are
// rejected closed before spawn — never truncated, so the agent never runs
// on a silently cut skill.
const maxCardSkillBodyBytes = 64 * 1024

// maxCardSkillIDLength caps the skill id. The id becomes a directory name
// under skills-injected/ and the pi skill name, so it stays short.
const maxCardSkillIDLength = 64

// cardSkillsDirName is the per-session card-skill container under the session
// state root. Each skill lives at <root>/skills-injected/<id>/SKILL.md.
const cardSkillsDirName = "skills-injected"

// cardSkillFileName is the skill file pi discovers inside each skill directory.
const cardSkillFileName = "SKILL.md"

// writeCardSkills materializes spec CardSkills into the session state root
// and returns the absolute skill directories to pass with `--skill`.
// Empty (nil) with no error when no skill carries a body — the caller emits
// no flag. Fail-closed (wrapping agent.ErrSpawnFailed) on any unsafe entry
// or I/O/readback failure; the caller must not spawn.
func writeCardSkills(layout sessionLayout, skills []agent.CardSkill) ([]string, error) {
	var active []agent.CardSkill
	for _, sk := range skills {
		if strings.TrimSpace(sk.Body) == "" {
			continue
		}
		active = append(active, sk)
	}
	if len(active) == 0 {
		return nil, nil
	}
	if err := validateCardSkills(active); err != nil {
		return nil, err
	}
	base := filepath.Join(layout.root, cardSkillsDirName)
	var dirs []string
	for _, sk := range active {
		dir := filepath.Join(base, sk.ID)
		content := renderCardSkillFile(sk)
		if layout.strict {
			if err := writeCardSkillStrict(layout, dir, content); err != nil {
				_ = removeCardSkillsDir(layout)
				return nil, err
			}
		} else {
			if err := writeCardSkillPlain(dir, content); err != nil {
				_ = os.RemoveAll(base)
				return nil, err
			}
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			_ = removeCardSkillsDir(layout)
			return nil, fmt.Errorf("%w: resolve card skill dir: %v", agent.ErrSpawnFailed, err)
		}
		dirs = append(dirs, abs)
	}
	return dirs, nil
}

// validateCardSkills fails closed on any skill the writer cannot
// materialize safely: empty or over-long ids, duplicates, path-unsafe ids,
// pi-name violations, or bodies over the size cap.
func validateCardSkills(skills []agent.CardSkill) error {
	seen := make(map[string]struct{}, len(skills))
	for _, sk := range skills {
		if err := validateCardSkillID(sk.ID); err != nil {
			return fmt.Errorf("%w: invalid card skill id %q: %v", agent.ErrSpawnFailed, sk.ID, err)
		}
		if _, dup := seen[sk.ID]; dup {
			return fmt.Errorf("%w: duplicate card skill id %q", agent.ErrSpawnFailed, sk.ID)
		}
		seen[sk.ID] = struct{}{}
		if len(sk.Body) > maxCardSkillBodyBytes {
			return fmt.Errorf("%w: card skill %q body is %d bytes (limit %d)", agent.ErrSpawnFailed, sk.ID, len(sk.Body), maxCardSkillBodyBytes)
		}
	}
	return nil
}

// validateCardSkillID reports whether id is safe as a single path segment
// under skills-injected/ AND as a pi skill name. Pi names are lowercase
// a-z, 0-9 and hyphens, 1-64 chars, no leading/trailing hyphen, no
// consecutive hyphens (docs/skills.md). The charset also guarantees the id
// cannot escape the skills directory (no separators, dot segments,
// whitespace, or non-ASCII) without depending on path cleaning.
func validateCardSkillID(id string) error {
	if id == "" {
		return fmt.Errorf("empty skill id")
	}
	if len(id) > maxCardSkillIDLength {
		return fmt.Errorf("skill id exceeds %d characters", maxCardSkillIDLength)
	}
	if strings.HasPrefix(id, "-") || strings.HasSuffix(id, "-") {
		return fmt.Errorf("skill id must not start or end with a hyphen")
	}
	if strings.Contains(id, "--") {
		return fmt.Errorf("skill id must not contain consecutive hyphens")
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-'
		if !ok {
			return fmt.Errorf("skill id must match [a-z0-9-]")
		}
	}
	if id == "." || id == ".." {
		return fmt.Errorf("skill id must not be a dot segment")
	}
	if strings.Contains(id, "/") || strings.Contains(id, string(os.PathSeparator)) {
		return fmt.Errorf("skill id must not contain a path separator")
	}
	return nil
}

// renderCardSkillFile renders one SKILL.md: YAML frontmatter with the skill
// name and a session-scoped description, then the body verbatim.
func renderCardSkillFile(sk agent.CardSkill) string {
	var b strings.Builder
	b.WriteString("---\nname: ")
	b.WriteString(sk.ID)
	b.WriteString("\ndescription: Session card skill ")
	b.WriteString(sk.ID)
	b.WriteString("\n---\n\n")
	b.WriteString(sk.Body)
	if !strings.HasSuffix(sk.Body, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// writeCardSkillPlain writes one skill directory's SKILL.md via the plain
// filesystem (unconfined sessions) with sha256 readback.
func writeCardSkillPlain(dir, content string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%w: create skill dir: %v", agent.ErrSpawnFailed, err)
	}
	path := filepath.Join(dir, cardSkillFileName)
	data := []byte(content)
	want := sha256.Sum256(data)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("%w: write %s: %v", agent.ErrSpawnFailed, cardSkillFileName, err)
	}
	back, err := os.ReadFile(path) //nolint:gosec // G304: path just written under our own state root
	if err != nil {
		return fmt.Errorf("%w: read back %s: %v", agent.ErrSpawnFailed, cardSkillFileName, err)
	}
	if got := sha256.Sum256(back); got != want {
		return fmt.Errorf("%w: readback mismatch for %s (want sha256 %s, got %s)",
			agent.ErrSpawnFailed, cardSkillFileName, hex.EncodeToString(want[:]), hex.EncodeToString(got[:]))
	}
	return nil
}

// writeCardSkillStrict writes one skill directory's SKILL.md through the
// strict session state writer (confined sessions): every component is
// Lstat-checked, so a symlink the seat planted in the state refuses the
// spawn before anything is written. Readback compares sha256 like the plain
// path.
func writeCardSkillStrict(layout sessionLayout, dir, content string) error {
	state, err := openSessionStateFS(layout)
	if err != nil {
		return fmt.Errorf("%w: open state dir: %v", agent.ErrSpawnFailed, err)
	}
	defer func() { _ = state.Close() }()
	if err := state.mkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%w: create skill dir: %v", agent.ErrSpawnFailed, err)
	}
	path := filepath.Join(dir, cardSkillFileName)
	data := []byte(content)
	want := sha256.Sum256(data)
	if err := state.writeFile(path, data, 0o600); err != nil {
		return fmt.Errorf("%w: write %s: %v", agent.ErrSpawnFailed, cardSkillFileName, err)
	}
	back, err := os.ReadFile(path) //nolint:gosec // G304: path just written under our own state root
	if err != nil {
		return fmt.Errorf("%w: read back %s: %v", agent.ErrSpawnFailed, cardSkillFileName, err)
	}
	if got := sha256.Sum256(back); got != want {
		return fmt.Errorf("%w: readback mismatch for %s (want sha256 %s, got %s)",
			agent.ErrSpawnFailed, cardSkillFileName, hex.EncodeToString(want[:]), hex.EncodeToString(got[:]))
	}
	return nil
}

// removeCardSkillsDir deletes the per-session skills-injected directory.
// Idempotent: missing or empty roots return nil.
func removeCardSkillsDir(layout sessionLayout) error {
	base := filepath.Join(layout.root, cardSkillsDirName)
	if err := os.RemoveAll(base); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("provider/pi: remove card skills dir: %w", err)
	}
	return nil
}

// skillArgs renders the `--no-skills` + `--skill` argv fragment for
// materialized skill directories. Empty when dirs is empty so skill-less
// spawns keep their exact argv. `--no-skills` rides exactly when at least
// one skill directory is present: pi keeps the first skill found on a name
// collision and CLI paths sort last, so without it a repo skill with the
// same name would shadow the card skill. Paths are validated syntactically
// (absolute, clean): anything else is a caller bug and is skipped rather
// than widening the skill surface.
func skillArgs(dirs []string) []string {
	var valid []string
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
			continue
		}
		valid = append(valid, dir)
	}
	if len(valid) == 0 {
		return nil
	}
	out := []string{"--no-skills"}
	for _, dir := range valid {
		out = append(out, "--skill", dir)
	}
	return out
}
