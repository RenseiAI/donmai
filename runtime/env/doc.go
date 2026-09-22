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
// # Two layers, and the declaration that tells them apart
//
// The distinction the blocklist enforces is between layers, not between
// secrets:
//
//   - INHERITED — the host process env passed through. Filtered against
//     AGENT_ENV_BLOCKLIST, because its usual author is an operator's shell.
//   - EXPLICIT — Spec.Env and the ComposeChildEnv override maps. Runner-set
//     and trusted, so the blocklist does not apply: this is how a provider
//     delivers the credential it resolved for the session.
//
// A supervising daemon that resolves per-session credentials out of band and
// merges them into the worker process environment writes into the INHERITED
// layer, where the filter cannot tell its deliberate injection from a shell
// leak. InjectedEnvKeysVar (DONMAI_INJECTED_ENV_KEYS) is how that supervisor
// declares, by NAME only, which inherited names are its own; a declared name
// crosses the blocklist, and everything else behaves exactly as before.
//
// The declaration re-admits AGENT_ENV_BLOCKLIST names ONLY. IsRunnerOnly names
// (the attach controls, the session-shim launch contract, and the declaration
// variable itself) are stripped from every layer regardless — they address the
// supervisor of the process that would receive them, so no supervisor hands
// them down.
//
// Source: ../donmai-libraries/packages/core/src/orchestrator/orchestrator.ts
// (AGENT_ENV_BLOCKLIST) — port verbatim per F.1.1 §1.
package env
