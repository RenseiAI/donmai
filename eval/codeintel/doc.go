// Package codeintel implements the code-intelligence A/B eval harness: a
// benchmark-dataset loader, three grader classes, a two-arm (WITH / WITHOUT)
// execution driver, and a thin platform reporting bridge.
//
// The harness measures the agent-success delta of the in-box
// code-intelligence engine (shipped in donmai v0.50.0) by driving an agent
// through a real code task twice on two fresh, identical workareas:
//
//   - the WITHOUT arm gets the baseline tool surface only, with the donmai
//     binary explicitly stripped from PATH (the mandatory contamination guard
//     — the binary is baked into the sandbox images, so prompt-omission alone
//     would leak the tool into the control group);
//   - the WITH arm gets the same baseline plus the REAL MCP surface — server
//     "af-code-intelligence", spawned per the frozen wire contract as
//     `donmai mcp code-intel --root <workarea>` — exactly as a live session
//     would (or, when the advertisement mode is switched, the prompt text
//     generated from live `donmai code --help` output).
//
// This package is a CONSUMER of the existing execution substrate. It reuses
// runtime/worktree.Manager for provisioning, the frozen af-code-intelligence
// MCP entry (runner/codeintel.go), and provider/harness/clijsonl.WriteMCPConfig
// for authoring the exact --mcp-config a real claude session consumes. It does
// NOT modify the afclient/codeintel engine or the runtime/mcp/server — those
// shipped in v0.50.0 and are consumed as-is.
//
// Design references: runs/2026-07-04-code-intel-capability/discovery/06-eval-harness.md
// (§1.1 ADR-017 substrate, §4.1 task families, §4.2 EvalDatasetCase shape,
// §4.3 driver design, §4.4 grader classes, §4.5 metrics).
package codeintel
