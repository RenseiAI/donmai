package afcli

import (
	"testing"
)

// TestMuseCtorOptions_TrustsWorkspace proves the production muse
// provider starts trusted: card skills materialize into the session
// workspace as .agents/skills/donmai-* files, and only a trusted host
// start (--trust-workspace) loads workspace skills. Dropping the flag
// here would silently park every muse session on skills the host never
// loads, so this pins the value the registry ctor threads without
// exercising the real binary/version probe.
func TestMuseCtorOptions_TrustsWorkspace(t *testing.T) {
	t.Parallel()

	if got := museCtorOptions(agentRunCtorHints{}); !got.TrustWorkspace {
		t.Errorf("museCtorOptions(zero hints).TrustWorkspace = false; want true")
	}
}
