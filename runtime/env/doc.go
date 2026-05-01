// Package env composes process environments for agent provider
// subprocesses.
//
// The runner hands every provider a base environment merged from
// (a) the host process env, (b) provider-resolved secrets (e.g.
// ANTHROPIC_API_KEY from the daemon credential store), and (c) the
// per-session Spec.Env map. Before merging, sensitive variables in
// AGENT_ENV_BLOCKLIST are stripped from the host pass-through so an
// operator's interactive shell credential cannot leak into the agent's
// subprocess.
//
// Source: ../agentfactory/packages/core/src/orchestrator/orchestrator.ts
// (AGENT_ENV_BLOCKLIST) — port verbatim per F.1.1 §1.
package env
