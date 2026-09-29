package runner

import (
	"fmt"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/statehome"
)

// resolvePlatformMCPServerName captures the process-owned logical name used by
// the implicit per-session MCP gateway. An explicit value is exact: it is
// neither trimmed nor normalized. The empty value preserves the historical
// brand-derived default, captured once by the caller's constructor.
func resolvePlatformMCPServerName(configured string) (string, error) {
	name := configured
	if name == "" {
		name = statehome.Brand() + "-platform"
	}
	if _, err := agent.MCPServerCapabilityEntryID(name); err != nil {
		return "", fmt.Errorf("runner: platform MCP server name is malformed: %w", err)
	}
	return name, nil
}

func defaultPlatformMCPServerName() string {
	name, _ := resolvePlatformMCPServerName("")
	return name
}
