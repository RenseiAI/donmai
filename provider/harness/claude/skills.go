package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// maxCardSkillBodyBytes caps one card skill body materialized into the
// per-session plugin directory. 64 KiB is far past any realistic skill
// (a SKILL.md of that size is already a small manual) and keeps a runaway
// card from being read into memory unbounded. Bodies over the cap are
// rejected closed before spawn — never truncated, so the agent never runs
// on a silently cut skill.
const maxCardSkillBodyBytes = 64 * 1024

// maxCardSkillIDLength caps the skill id. The id becomes a directory name
// under skills/, so it stays short.
const maxCardSkillIDLength = 64

// cardPluginName is the session plugin's manifest name. One plugin per
// session carries every card skill as skills/<id>/SKILL.md.
const cardPluginName = "donmai-card-skills"

// writeCardSkillsPluginDir materializes spec CardSkills into a per-session
// Claude plugin directory outside the worktree and returns its absolute path.
//
// Layout (the shape `claude plugin validate` accepts):
//
//	<dir>/.claude-plugin/plugin.json
//	<dir>/skills/<id>/SKILL.md   (frontmatter name+description, then Body)
//
// The directory is created under os.TempDir with a session-stable prefix.
// Callers pass the returned path as `--plugin-dir <dir>` and remove it when
// the session ends (see removeCardSkillsPluginDir; the headless lane's
// Handle.Stop and the interactive lane's spawn cleanup both do this).
//
// Guarantees:
//   - "" with nil error when skills is empty (no flag emitted — the spawn
//     argv stays byte-identical to a skill-less session).
//   - Entries with a whitespace-only body are skipped (they contribute no
//     SKILL.md, mirroring foldInlineSkills which injects no text for them).
//   - Bodies over maxCardSkillBodyBytes, ids that are empty, too long,
//     duplicated, or unsafe as a path segment (separators, dot segments,
//     absolute, charset outside [A-Za-z0-9_-]), are rejected with an error
//     wrapping agent.ErrSpawnFailed — the spawn fails closed before any
//     subprocess starts.
//   - After writing, every file is read back and its sha256 compared to the
//     bytes just written (readback): a short write or a concurrent
//     modification surfaces here instead of reaching the agent as a
//     truncated skill.
//
// The directory sits outside the worktree (os.TempDir, never spec.Cwd), so
// no skill file can appear in the checkout or in `git status`.
func writeCardSkillsPluginDir(skills []agent.CardSkill) (string, error) {
	// Filter whitespace-only bodies first: they carry no SKILL.md, so a
	// session whose skills are all blank stays flag-less.
	var active []agent.CardSkill
	for _, sk := range skills {
		if strings.TrimSpace(sk.Body) == "" {
			continue
		}
		active = append(active, sk)
	}
	if len(active) == 0 {
		return "", nil
	}
	if err := validateCardSkills(active); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "donmai-claude-skills-*")
	if err != nil {
		return "", fmt.Errorf("%w: create card-skills plugin dir: %v", agent.ErrSpawnFailed, err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("%w: resolve card-skills plugin dir: %v", agent.ErrSpawnFailed, err)
	}
	if err := writeCardSkillsPluginFiles(abs, active); err != nil {
		_ = os.RemoveAll(abs)
		return "", err
	}
	return abs, nil
}

// validateCardSkills fails closed on any skill the plugin writer cannot
// materialize safely: empty or over-long ids, duplicates, path-unsafe ids,
// or bodies over the size cap.
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
// under skills/. Only [A-Za-z0-9_-], starting alphanumerically, length
// 1..maxCardSkillIDLength. This rejects separators, dot segments, absolute
// paths, whitespace, and non-ASCII — anything that could escape the plugin
// directory — without depending on the host's path cleaning.
func validateCardSkillID(id string) error {
	if id == "" {
		return fmt.Errorf("empty skill id")
	}
	if len(id) > maxCardSkillIDLength {
		return fmt.Errorf("skill id exceeds %d characters", maxCardSkillIDLength)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !ok {
			return fmt.Errorf("skill id must match [A-Za-z0-9_-]")
		}
	}
	first := id[0]
	if !(first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z' || first >= '0' && first <= '9') {
		return fmt.Errorf("skill id must start alphanumerically")
	}
	if id == "." || id == ".." {
		return fmt.Errorf("skill id must not be a dot segment")
	}
	if strings.Contains(id, "/") || strings.Contains(id, string(os.PathSeparator)) {
		return fmt.Errorf("skill id must not contain a path separator")
	}
	return nil
}

// writeCardSkillsPluginFiles writes the manifest, every SKILL.md, and reads
// each file back comparing sha256. The first error aborts; the caller
// removes the directory.
func writeCardSkillsPluginFiles(dir string, skills []agent.CardSkill) error {
	manifestDir := filepath.Join(dir, ".claude-plugin")
	if err := os.MkdirAll(manifestDir, 0o700); err != nil {
		return fmt.Errorf("%w: create plugin manifest dir: %v", agent.ErrSpawnFailed, err)
	}
	manifest := map[string]string{
		"name":        cardPluginName,
		"version":     "0.0.1",
		"description": "Per-session agent-card skills for this run only",
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: marshal plugin manifest: %v", agent.ErrSpawnFailed, err)
	}
	manifestPath := filepath.Join(manifestDir, "plugin.json")
	if err := writeFileWithReadback(manifestPath, append(manifestBytes, '\n'), 0o600); err != nil {
		return err
	}
	for _, sk := range skills {
		skillDir := filepath.Join(dir, "skills", sk.ID)
		if err := os.MkdirAll(skillDir, 0o700); err != nil {
			return fmt.Errorf("%w: create skill dir %q: %v", agent.ErrSpawnFailed, sk.ID, err)
		}
		content := renderCardSkillFile(sk)
		if err := writeFileWithReadback(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o600); err != nil {
			return fmt.Errorf("%w: skill %q: %v", agent.ErrSpawnFailed, sk.ID, err)
		}
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

// writeFileWithReadback writes data to path (0600, O_EXCL) then reads the
// file back and compares sha256. Any mismatch, short write, or read error
// fails closed: the caller removes the plugin directory and the spawn never
// starts on a truncated skill.
func writeFileWithReadback(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // G304: path derives from our own MkdirTemp name + validated skill id
	if err != nil {
		return fmt.Errorf("%w: create %s: %v", agent.ErrSpawnFailed, filepath.Base(path), err)
	}
	want := sha256.Sum256(data)
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("%w: write %s: %v", agent.ErrSpawnFailed, filepath.Base(path), err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%w: close %s: %v", agent.ErrSpawnFailed, filepath.Base(path), err)
	}
	back, err := os.ReadFile(path) //nolint:gosec // G304: path just created above in our own temp dir
	if err != nil {
		return fmt.Errorf("%w: read back %s: %v", agent.ErrSpawnFailed, filepath.Base(path), err)
	}
	if got := sha256.Sum256(back); got != want {
		return fmt.Errorf("%w: readback mismatch for %s (want sha256 %s, got %s)",
			agent.ErrSpawnFailed, filepath.Base(path), hex.EncodeToString(want[:]), hex.EncodeToString(got[:]))
	}
	return nil
}

// removeCardSkillsPluginDir deletes the per-session plugin directory.
// Idempotent: missing or empty paths return nil.
func removeCardSkillsPluginDir(path string) error {
	if path == "" {
		return nil
	}
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("provider/claude: remove card-skills plugin dir: %w", err)
	}
	return nil
}

// pluginDirArgs renders the --plugin-dir argv fragment for a materialized
// plugin directory. Empty when dir is "" so skill-less spawns keep their
// exact argv. The path is validated syntactically (absolute, clean):
// anything else is a caller bug and fails closed with nil — the parent
// spawn refuses earlier, so reaching here with a bad path must not widen
// the CLI's plugin surface.
func pluginDirArgs(dir string) []string {
	if dir == "" {
		return nil
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return nil
	}
	return []string{"--plugin-dir", dir}
}
