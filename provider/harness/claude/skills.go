package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/skillfs"
)

// Native skill delivery for Claude Code: a per-session plugin directory.
//
//	<tmp>/donmai-skills-XXXX/
//	  .claude-plugin/plugin.json      {"name":"donmai-skills",...}
//	  skills/<id>/SKILL.md            one directory per native skill
//	  skills/<id>/...                 the rest of the package
//
// The directory lives OUTSIDE the working directory, so nothing the agent
// does in the worktree can reach it and nothing in it can enter a commit. It
// is passed with --plugin-dir on the spawn and re-passed on every --resume
// (Provider.Resume and the handle's Inject turns), and removed when the
// handle stops. Plugin skills are namespaced by the plugin name, so an
// attached skill never shadows, and is never shadowed by, a repository
// skill of the same name.
//
// This adapter passes no flag that suppresses discovery of the repository's
// own skills while keeping its project settings and instructions, so the
// manifest declares no repository-skill control: the plan records the
// achieved policy as all whatever was requested (best-effort, D8.4).

// skillPluginName is the plugin name; skills appear as
// "donmai-skills:<id>".
const skillPluginName = "donmai-skills"

// skillPluginDirPrefix names the per-session plugin directory. The clijsonl
// driver admits a --plugin-dir resume argument only when its last element
// carries this prefix (clijsonl.skillPluginDirPrefix mirrors it).
const skillPluginDirPrefix = "donmai-skills-"

// claudeSkillProfile is the manifest's skill declaration.
var claudeSkillProfile = agent.SkillDeliveryProfile{Layout: agent.SkillLayoutPluginDir}

// skillMaterializer writes Spec.Skills' native entries into a fresh plugin
// directory under tempDir ("" is the OS temporary directory).
type skillMaterializer struct {
	tempDir string
}

var _ agent.SkillMaterializer = skillMaterializer{}

// MaterializeSkills implements agent.SkillMaterializer.
func (m skillMaterializer) MaterializeSkills(spec agent.Spec) (agent.MaterializedSkills, error) {
	native, err := spec.Skills.Native()
	if err != nil || len(native) == 0 {
		return agent.MaterializedSkills{}, err
	}
	if layout := spec.Skills.Plan.Layout; layout != agent.SkillLayoutPluginDir {
		return agent.MaterializedSkills{}, fmt.Errorf("provider/claude: skill plan layout %q is not this harness's", layout)
	}
	dir, err := os.MkdirTemp(m.tempDir, skillPluginDirPrefix+"*")
	if err != nil {
		return agent.MaterializedSkills{}, fmt.Errorf("provider/claude: create skill plugin dir: %w", err)
	}
	// MkdirTemp may answer through a symlinked temporary root (macOS
	// /var -> /private/var); hand the harness the resolved path.
	if resolved, rerr := filepath.EvalSymlinks(dir); rerr == nil {
		dir = resolved
	}
	cleanup := func() error { return os.RemoveAll(dir) }
	if err := writeSkillPlugin(dir, native); err != nil {
		return agent.MaterializedSkills{}, errors.Join(err, cleanup())
	}
	return agent.MaterializedSkills{Args: []string{"--plugin-dir", dir}, Cleanup: cleanup}, nil
}

func writeSkillPlugin(dir string, native []agent.NativeSkill) error {
	manifest, err := json.Marshal(map[string]string{
		"name":        skillPluginName,
		"description": "Skills delivered for this session",
		"version":     "1.0.0",
	})
	if err != nil {
		return fmt.Errorf("provider/claude: render plugin manifest: %w", err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".claude-plugin"), 0o700); err != nil {
		return fmt.Errorf("provider/claude: create plugin manifest dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), manifest, 0o600); err != nil {
		return fmt.Errorf("provider/claude: write plugin manifest: %w", err)
	}
	for _, skill := range native {
		if err := skillfs.Write(dir, "skills/"+skill.Entry.Dir, skill, skillfs.Options{}); err != nil {
			return fmt.Errorf("provider/claude: skill %q: %w", skill.Entry.ID, err)
		}
	}
	return nil
}
