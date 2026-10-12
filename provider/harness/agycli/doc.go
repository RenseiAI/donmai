// Package agycli implements an agent.Provider that shells out to Google
// Antigravity's `agy` CLI.
//
// Two auth modes, selected per spawn and never mixed:
//
//   - KEYED (API-key mode): when spec.Env carries GEMINI_API_KEY, the child
//     runs under a per-session HOME holding settings.json
//     {"modelProvider":"gemini"} (keymode.go), so `agy` bills the session
//     key instead of the CLI's own subscription login. Nothing is written
//     to the operator's real ~/.gemini on this path, and the isolated HOME
//     is removed when the session ends.
//   - HOST-SESSION: otherwise the provider launches the user's own,
//     already-logged-in `agy` binary to do the user's work. The CLI owns
//     its login state; this provider injects NO API key and discovers NO
//     credentials — it is host-session fail-open. It is NOT a third-party
//     OAuth wrapper: it automates the official tool the user authenticated,
//     not a re-implementation that borrows the user's token. NOT suitable
//     for cloud sandboxes.
//
// It is DISTINCT from the sibling "gemini" provider:
//
//   - "gemini"  — API-direct Go provider (net/http to generativelanguage),
//     key-based, structured, cloud-capable.
//   - "agy-cli" — THIS package: subprocess wrap of `agy`, keyed or
//     host-session, pty + plain-text.
//
// # Why agy requires a pty
//
//   - `agy -p "<prompt>"` MUST run under a pty — plain stdin/stdout pipes hang
//     with zero output. We allocate a pseudo-terminal via github.com/creack/pty.
//   - `agy` advertises `--add-dir <worktree>`. We require that flag at
//     construction and pass the canonical Spec.Cwd as its sole value on every
//     workarea-bound spawn. `cmd.Dir` alone is not sufficient: agy may create a
//     scratch project and use that as its tool workspace. An older or otherwise
//     incompatible CLI fails unavailable instead of silently widening or
//     changing the requested worktree authority.
//   - There is NO structured output mode consumed here. v1.1.10 advertised
//     `--model`, but this provider intentionally does not forward Spec.Model
//     until that behavior has its own compatibility contract. There is no
//     `--skip-trust` flag.
//
// # How this package recovers structure anyway
//
//  1. stdout (the authoritative spine): read line-by-line off the pty master,
//     emitted as agent.AssistantTextEvent. The runner's WORK_RESULT marker scan
//     works off this text unchanged.
//  2. Result-envelope injection: the provider appends an instruction to the
//     prompt asking `agy` to print its final result inside a delimited JSON
//     envelope (<<<DONMAI_RESULT>>> … <<<END_DONMAI_RESULT>>>). `agy` reproduces
//     it reliably (probe-confirmed), giving a clean machine-parseable final
//     result without any structured-output mode. Toggle: Options.InjectResultEnvelope.
//  3. Transcript enrichment (best-effort): `agy` writes a structured JSONL
//     transcript to <stateHome>/antigravity-cli/brain/<conv-id>/.system_generated/
//     logs/transcript.jsonl — the operator's real state home on the
//     host-session path, the session's isolated HOME on the keyed path. We
//     discover the conv-id (by diffing the brain/ directory across spawn)
//     and TAIL the transcript live during the run, streaming tool_calls →
//     agent.ToolUseEvent and tool-result steps → agent.ToolResultEvent as agy
//     appends them (a final catch-up drain after exit covers anything still
//     buffered — there is no after-the-fact EOF replay). This is an INTERNAL,
//     version-fragile path: any discovery/parse failure degrades silently to
//     the stdout spine. Toggle: Options.DisableTranscriptEnrichment.
//
// # Capability matrix
//
//   - SupportsMessageInjection=false / SupportsSessionResume=false: single-shot
//     `-p`. (`--conversation <id>` resume exists; deferred.)
//   - AcceptsMcpServerSpec=false / SupportsToolPlugins=false (v1): `agy` reads
//     MCP from the GLOBAL settings.json mcpServers, not a CLI flag or a
//     per-worktree file we can verify; MCP wiring is deferred.
//   - SupportsReasoningEffort=false: no flag.
//   - Model selection is a persisted CLI setting with NO per-run flag, so
//     spec.Model is informational only; the run uses the configured agy model.
//
// # Known limitations
//
//   - Prompt exposure: agy takes the prompt as the `-p` argv VALUE (it has no
//     stdin-prompt mode), so the prompt text is visible in the host process
//     listing (e.g. `ps`). Acceptable for the single-user host this provider
//     targets, but callers should not embed secrets in the prompt. The
//     session KEY never rides argv — it travels in the child environment
//     (argvSecret test).
//   - Workspace trust is opt-in (Options.TrustWorkspace, default off) and
//     host-session-only: untrusted cwds run fine under
//     --dangerously-skip-permissions, so the provider does not mutate shared
//     settings by default. Keyed spawns seed trust into the session's own
//     isolated settings file instead, never into operator state.
//
// # Auth / token usage
//
// On a keyed spawn the session key rides the child environment
// (GEMINI_API_KEY from Spec.Env, plus the optional GOOGLE_GEMINI_BASE_URL
// endpoint override); the injected-env-keys declaration (keyEnvNames in
// keymode.go) names every variable the keyed path sets. On a host-session
// spawn no key is injected; the `agy` binary authenticates via its own
// host login. Token/cost accounting is NOT available (the transcript
// carries no token counts) — agent.ResultEvent.Cost is left nil. This is a
// documented limitation versus API-direct providers (e.g. "gemini") that
// receive structured token usage.
package agycli
