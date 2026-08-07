package ptyhost

import "github.com/RenseiAI/donmai/agent"

// Compile-time conformance: *Session is the host implementation of the
// agent.InteractiveSession seam (agent/interactive.go), and *subscription is one
// agent.InteractiveSubscription. Importing agent here is acyclic — agent imports
// only attachwire, never ptyhost.
var (
	_ agent.InteractiveSession      = (*Session)(nil)
	_ agent.InteractiveNotifier     = (*Session)(nil)
	_ agent.InteractiveSubscription = (*subscription)(nil)
)
