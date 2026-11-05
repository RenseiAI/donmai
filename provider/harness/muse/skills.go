package muse

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/harnessstate"
	"github.com/RenseiAI/donmai/runtime/skillfs"
)

// Native skill delivery for Muse: reserved skill directories inside the
// workspace, discovered once the workspace is trusted.
//
//	<workspace>/.agents/skills/donmai-<id>/SKILL.md (+ the rest of the package)
//	muse serve --trust-workspace ...
//
// The serve host has no flag that loads a skill directory by path; it
// discovers workspace skills only under --trust-workspace. Trust also loads
// the repository's own rules, hooks and skills, and hooks execute code. That
// exposure was accepted for this harness only (ADR-2026-11-02 Risks,
// "Workspace trust"), so the adapter trusts the workspace exactly when the
// plan delivers a skill natively, and never otherwise. The plan records the
// consequence for the repository-skill policy: all while trusted, none
// while not, whatever was requested.
//
// The directories carry the reserved donmai- prefix, which the checkout's
// exclude file and the backstop both key on (runtime/harnessstate), so they
// never show as untracked and never enter a commit. A directory that already
// exists is repository content and is never overwritten, unless it carries
// the runner's marker (a stale copy from an earlier run of this session).
// Every directory the adapter wrote is removed when the session stops.

// museSkillProfile returns the manifest's skill declaration for an instance
// whose host already trusts every workspace (Options.TrustWorkspace) or not.
func museSkillProfile(workspaceTrusted bool) agent.SkillDeliveryProfile {
	return agent.SkillDeliveryProfile{
		Layout:           agent.SkillLayoutWorkspace,
		RepoSkills:       agent.RepoSkillsByTrust,
		WorkspaceTrusted: workspaceTrusted,
	}
}

// inTreeSkillsDir is where the workspace layout writes, relative to the
// workspace root.
var inTreeSkillsDir = []string{".agents", "skills"}

// skillMaterializer writes Spec.Skills into the workspace at spec.Cwd.
type skillMaterializer struct{}

var _ agent.SkillMaterializer = skillMaterializer{}

// MaterializeSkills implements agent.SkillMaterializer.
func (skillMaterializer) MaterializeSkills(spec agent.Spec) (agent.MaterializedSkills, error) {
	native, err := spec.Skills.Native()
	if err != nil || len(native) == 0 {
		return agent.MaterializedSkills{}, err
	}
	if layout := spec.Skills.Plan.Layout; layout != agent.SkillLayoutWorkspace {
		return agent.MaterializedSkills{}, fmt.Errorf("muse: skill plan layout %q is not this harness's", layout)
	}
	base := spec.Cwd
	if base == "" {
		return agent.MaterializedSkills{}, errors.New("muse: native skills need a workspace")
	}
	// Keep the directories out of `git status` before writing them; a
	// non-git workspace is a silent no-op.
	_ = harnessstate.EnsureGitExcluded(base, harnessstate.InTreeSkillExcludeEntry)
	createdParents := missingParents(base)
	var written []string
	cleanup := func() error {
		var errs []error
		for _, dir := range written {
			if err := skillfs.Remove(base, dir); err != nil {
				errs = append(errs, err)
			}
		}
		// Remove the parents this materialization created, deepest
		// first, only when nothing else lives in them.
		for i := len(createdParents) - 1; i >= 0; i-- {
			if entries, err := os.ReadDir(createdParents[i]); err == nil && len(entries) == 0 {
				if err := os.Remove(createdParents[i]); err != nil && !errors.Is(err, fs.ErrNotExist) {
					errs = append(errs, err)
				}
			}
		}
		return errors.Join(errs...)
	}
	for _, skill := range native {
		dir := filepath.ToSlash(filepath.Join(append(append([]string(nil), inTreeSkillsDir...), skill.Entry.Dir)...))
		if err := skillfs.Write(base, dir, skill, skillfs.Options{Marker: true}); err != nil {
			return agent.MaterializedSkills{}, errors.Join(fmt.Errorf("muse: skill %q: %w", skill.Entry.ID, err), cleanup())
		}
		written = append(written, dir)
	}
	return agent.MaterializedSkills{TrustWorkspace: spec.Skills.Plan.TrustWorkspace, Cleanup: cleanup}, nil
}

// missingParents returns the absolute .agents and .agents/skills paths under
// base that do not exist yet, shallowest first.
func missingParents(base string) []string {
	var out []string
	current := base
	for _, part := range inTreeSkillsDir {
		current = filepath.Join(current, part)
		if _, err := os.Lstat(current); errors.Is(err, fs.ErrNotExist) {
			out = append(out, current)
		}
	}
	return out
}
