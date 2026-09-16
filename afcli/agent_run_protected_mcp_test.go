package afcli

import (
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

func TestAgentRunProtectedRuntimeMCPSelectorReachesChildOptions(t *testing.T) {
	const capability = "example.protected-mcp/v1"
	realizations := receiptCapabilityRealizationsForTest(t, capability, agent.HarnessPi, agent.PromptModeHumanControlled)
	commandOptions := agentRunOptions(Config{
		CapabilityRealizations:        realizations,
		ProtectedRuntimeMCPCapability: capability,
	}, "embedder")
	var child runner.Options
	applyAgentRunCapabilityOptions(&child, commandOptions)
	if child.CapabilityRealizations != realizations || child.ProtectedRuntimeMCPCapability != capability {
		t.Fatalf("child capability options = registry %p selector %q", child.CapabilityRealizations, child.ProtectedRuntimeMCPCapability)
	}
}

func TestAgentRunProtectedRuntimeMCPDefaultStaysLegacyAndMalformedUsesRunnerValidation(t *testing.T) {
	commandOptions := agentRunOptions(Config{}, "embedder")
	child := runner.Options{
		Registry: runner.NewRegistry(), WorktreeManager: &worktree.Manager{}, Poster: &result.Poster{},
	}
	applyAgentRunCapabilityOptions(&child, commandOptions)
	if child.CapabilityRealizations != nil || child.ProtectedRuntimeMCPCapability != "" {
		t.Fatalf("default child capability options = registry %p selector %q", child.CapabilityRealizations, child.ProtectedRuntimeMCPCapability)
	}
	if _, err := runner.New(child); err != nil {
		t.Fatalf("legacy child options rejected: %v", err)
	}

	malformed := agentRunOptions(Config{ProtectedRuntimeMCPCapability: " malformed "}, "embedder")
	applyAgentRunCapabilityOptions(&child, malformed)
	if _, err := runner.New(child); err == nil {
		t.Fatal("runner accepted malformed protected runtime MCP selector")
	}
}
