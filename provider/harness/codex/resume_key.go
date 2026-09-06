package codex

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/RenseiAI/donmai/sessionshim"
)

// recordResumeKey publishes a named interactive thread after Codex has
// confirmed the rollout flush. The dedicated environment values are supplied
// only by a shim-owned launch; ordinary and standalone sessions retain their
// existing behavior.
func recordResumeKey(home, threadID string) {
	if home == "" || threadID == "" || !hasCodexRollout(home) {
		return
	}
	registryDir := strings.TrimSpace(os.Getenv(sessionshim.EnvCodexResumeRegistry))
	id := sessionshim.Identity{
		OrgID:     strings.TrimSpace(os.Getenv(sessionshim.EnvCodexResumeOrg)),
		SessionID: strings.TrimSpace(os.Getenv(sessionshim.EnvCodexResumeSession)),
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
