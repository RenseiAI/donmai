package stub

import (
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// Test_InterfaceCompliance is a compile-time assertion that the
// concrete *provider and *handle types satisfy the agent.Provider and
// agent.Handle interfaces. If a future change to the agent contract
// adds or renames a method this test fails to build, surfacing the
// drift before runtime.
func Test_InterfaceCompliance(t *testing.T) {
	var _ agent.Provider = (*provider)(nil)
	var _ agent.Handle = (*handle)(nil)

	// Also verify New returns a non-nil Provider — guards against a
	// future constructor refactor that accidentally returns nil on
	// the success path. The static type guarantees the interface; we
	// only need a runtime nil check.
	p, err := New()
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	if p == nil {
		t.Fatalf("New() returned a nil agent.Provider")
	}
}
