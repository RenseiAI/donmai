package matrix

import "github.com/RenseiAI/donmai/agent"

// LegacyCell returns the canonical (harness, endpoint, host) cell that a
// back-compat ProviderName resolves to, reading the generated
// LegacyAliasMap. The bool is false when the name has no legacy alias
// (an unknown / future ProviderName).
//
// This is the consumer P1 deferred: LegacyAliasMap was generated but
// unread. LegacyCell makes it authoritative for the ProviderName→harness
// half of resolution. It is a pure lookup — it does not change which
// concrete provider answers a name (the runner.Registry stays
// ProviderName-keyed); it lets callers assert that the provider a name
// resolves to is the one the matrix says it should be (see the agent-run
// registry's alias-consistency check + the 8-provider resolution test).
func LegacyCell(p agent.ProviderName) (CellKey, bool) {
	cell, ok := LegacyAliasMap[p]
	return cell, ok
}
