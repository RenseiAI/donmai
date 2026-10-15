package claude

import (
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/RenseiAI/donmai/agent"
)

// interactiveUsageConfigDir resolves the transcript store from the child's
// session-over-process environment, including an explicitly cleared config
// override. The default store is ~/.claude under that child's home.
func interactiveUsageConfigDir(env map[string]string) string {
	dir, overridden := env[claudeConfigDirEnv]
	if !overridden {
		dir = os.Getenv(claudeConfigDirEnv)
	}
	if dir != "" {
		if !filepath.IsAbs(dir) {
			return ""
		}
		return dir
	}
	home := env["HOME"]
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return ""
		}
	}
	if !filepath.IsAbs(home) {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// usageTranscriptPath looks only for this spawn's pinned conversation id.
// Enumerating the project directories avoids depending on Claude's cwd
// encoding (including symlinks) and never chooses a different conversation
// based on file timestamps. Multiple matches are ambiguous and stay unknown.
func (c *stopHookChannel) usageTranscriptPath() (string, bool) {
	if c.usageSessionID == "" || c.usageConfigDir == "" {
		return "", false
	}
	root := filepath.Join(c.usageConfigDir, "projects")
	projects, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	found := ""
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		path := filepath.Join(root, project.Name(), c.usageSessionID+".jsonl")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if found != "" {
			return "", false
		}
		found = path
	}
	return found, found != ""
}

// readSidecarUsage snapshots the documented cumulative USD cost. The
// context_window token counts describe the last response, not session spend. A
// percentage or window size is context occupancy, not token spend. Missing,
// malformed, impossible, and all-zero readings preserve unknown usage. The
// sidecar is a last resort, never added to transcript totals or live events.
func (c *stopHookChannel) readSidecarUsage() agent.CostData {
	f, err := openTranscript(c.sidecarPath())
	if err != nil {
		return agent.CostData{}
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 64*1024))
	if err != nil {
		return agent.CostData{}
	}
	var doc struct {
		TotalCostUSD float64 `json:"total_cost_usd"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil ||
		doc.TotalCostUSD <= 0 || math.IsNaN(doc.TotalCostUSD) || math.IsInf(doc.TotalCostUSD, 0) {
		return agent.CostData{}
	}
	return agent.CostData{TotalCostUsd: doc.TotalCostUSD}
}
