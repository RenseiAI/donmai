// Package codeintelhost implements a repository-bearing warm host for the
// donmai native code-intelligence engine (afclient/codeintel). Where
// runtime/mcp/server exposes ONE warm codeintel.NativeRunner rooted at a
// single --root over stdio JSON-RPC, this package fans that same engine out
// across many exact repository/revision bindings behind a single long-lived
// HTTP process: `donmai code host` (wired in afcli/code_host.go).
//
// A resident workarea is keyed by the complete immutable request binding
// (orgId, projectId, repositoryPathId, revisionKind, revision) — see
// Binding. The Pool warms each distinct binding at most once (single-flight),
// reference-counts concurrent leases against it, and evicts idle entries
// under a bounded-LRU policy once resident-workarea capacity is reached.
// Repository content is resolved through an operator-owned Catalog (the
// same repositories[] shape the daemon's ~/.donmai/daemon.yaml uses) via a
// Factory; the shipped Factory (GitFactory) maintains one persistent bare
// mirror per repository plus one detached worktree per binding.
//
// Every inbound call is authenticated by a hand-rolled, stdlib-only HS256
// bearer-JWT Verifier before a lease is acquired, and the authenticated
// claims are re-checked against both the request body binding and the
// binding actually held by the leased workarea before any tool dispatches.
// Handler exposes the fixed wire contract: POST /v1/tools/call (request/
// response shapes frozen — see Binding and the reused
// runtime/mcp/server.ToolResult/ContentItem types), plus /healthz and
// /readyz for process and admission-state liveness.
package codeintelhost
