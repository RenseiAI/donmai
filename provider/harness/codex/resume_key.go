package codex

import (
	"path/filepath"
	"strings"

	"github.com/RenseiAI/donmai/sessionshim"
)

// recordResumeKey publishes a named interactive thread after Codex has
// confirmed the rollout flush. The dedicated environment values are supplied
// only by a shim-owned launch; ordinary and standalone sessions retain their
// existing behavior.
func recordResumeKey(home, threadID string, env map[string]string) {
	if home == "" || threadID == "" || env == nil || !hasCodexRollout(home) {
		return
	}
	registryDir := strings.TrimSpace(env[sessionshim.EnvCodexResumeRegistry])
	id := sessionshim.Identity{
		OrgID:     strings.TrimSpace(env[sessionshim.EnvCodexResumeOrg]),
		SessionID: strings.TrimSpace(env[sessionshim.EnvCodexResumeSession]),
	}
	if registryDir == "" || id.Validate() != nil {
		return
	}
	registry, err := sessionshim.NewRegistry(registryDir)
	if err != nil {
		return
	}
	_ = registry.PutResumeKey(id, sessionshim.ResumeKey{CodexHome: filepath.Clean(home), ThreadID: threadID})
}
