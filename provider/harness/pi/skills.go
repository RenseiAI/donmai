package pi

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/skillfs"
	"github.com/RenseiAI/donmai/stagewire"
)

// Native skill delivery for pi: one directory per skill under the session
// state root, each loaded by explicit path.
//
//	<cwd>/.pi-<leaf>/skills-injected/<id>/SKILL.md (+ the rest of the package)
//	pi ... --skill <that dir> [--skill <dir>]... [--no-skills]
//
// pi loads a `--skill <path>` even when discovery is off ("additive even
// with --no-skills"), and keeps the first skill of a name it meets, so an
// attached skill passed by path wins a name clash with a repository skill.
// The state root belongs to the session, so every launch (Spawn and Resume)
// rewrites the directories from the plan and reads them back: the agent
// could write there for the whole previous run.
//
// The repository-skill policy is achievable on this harness:
//
//   - all:       no flag; pi discovers the project's .pi/skills and
//     .agents/skills as a developer's session would (the RPC lane already
//     passes --approve, which trusts project-local files).
//   - none:      --no-skills; only the attached skills load.
//   - allowlist: --no-skills, then --skill for each allowed name found in
//     the working directory's .pi/skills or .agents/skills.

// injectedSkillsDir holds materialized native skills, one level under the
// state root, beside extensions-injected.
const injectedSkillsDir = "skills-injected"

// piSkillProfile is the manifest's skill declaration.
var piSkillProfile = agent.SkillDeliveryProfile{Layout: agent.SkillLayoutStateDir, RepoSkills: agent.RepoSkillsByFlag}

// skillMaterializer writes Spec.Skills for pi's headless lane.
type skillMaterializer struct{}

var _ agent.SkillMaterializer = skillMaterializer{}

// MaterializeSkills implements agent.SkillMaterializer.
func (skillMaterializer) MaterializeSkills(spec agent.Spec) (agent.MaterializedSkills, error) {
	if spec.Skills == nil || spec.Skills.Plan.Layout == "" {
		return agent.MaterializedSkills{}, nil
	}
	plan := spec.Skills.Plan
	if plan.Layout != agent.SkillLayoutStateDir {
		return agent.MaterializedSkills{}, fmt.Errorf("pi: skill plan layout %q is not this harness's", plan.Layout)
	}
	native, err := spec.Skills.Native()
	if err != nil {
		return agent.MaterializedSkills{}, fmt.Errorf("pi: %w", err)
	}
	var args []string
	if plan.RepoSkills.Achieved == stagewire.RepoSkillsNone || plan.RepoSkills.Achieved == stagewire.RepoSkillsAllowlist {
		args = append(args, "--no-skills")
	}
	root := sessionStateRoot(spec)
	base, stateDir := filepath.Dir(root), filepath.Base(root)
	attached := make(map[string]bool, 2*len(native))
	for _, skill := range native {
		dir := stateDir + "/" + injectedSkillsDir + "/" + skill.Entry.Dir
		if err := skillfs.Write(base, dir, skill, skillfs.Options{Replace: true}); err != nil {
			return agent.MaterializedSkills{}, fmt.Errorf("pi: skill %q: %w", skill.Entry.ID, err)
		}
		args = append(args, "--skill", filepath.Join(base, filepath.FromSlash(dir)))
		attached[skill.Entry.ID] = true
		if skill.Entry.Name != "" {
			attached[skill.Entry.Name] = true
		}
	}
	if plan.RepoSkills.Achieved == stagewire.RepoSkillsAllowlist {
		for _, name := range plan.RepoSkills.Allow {
			if attached[name] {
				continue // an attached skill beats a repository skill
			}
			if path := findRepoSkill(spec.Cwd, name); path != "" {
				args = append(args, "--skill", path)
			}
		}
	}
	return agent.MaterializedSkills{Args: args}, nil
}

// findRepoSkill returns the path of the repository skill name in cwd, or ""
// when the repository has none: a directory .pi/skills/<name> or
// .agents/skills/<name> holding a SKILL.md, or a file .pi/skills/<name>.md.
// A symbolic link on the way is not followed: an allowlist names the
// repository's own skills, not whatever a link points at.
func findRepoSkill(cwd, name string) string {
	if cwd == "" {
		return ""
	}
	for _, candidate := range []struct {
		rel    []string
		isFile bool
	}{
		{rel: []string{".pi", "skills", name}},
		{rel: []string{".agents", "skills", name}},
		{rel: []string{".pi", "skills", name + ".md"}, isFile: true},
	} {
		if regularRepoPath(cwd, candidate.rel, candidate.isFile) {
			return filepath.Join(append([]string{cwd}, candidate.rel...)...)
		}
	}
	return ""
}

// regularRepoPath reports whether cwd/rel... exists as a real directory
// holding a regular SKILL.md (or, when isFile, a regular file), with no
// symbolic link anywhere on the way.
func regularRepoPath(cwd string, rel []string, isFile bool) bool {
	current := cwd
	parts := rel
	if !isFile {
		parts = append(append([]string(nil), rel...), "SKILL.md")
	}
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&fs.ModeSymlink != 0 {
			return false
		}
		if last := i == len(parts)-1; (last && !info.Mode().IsRegular()) || (!last && !info.IsDir()) {
			return false
		}
	}
	return true
}
