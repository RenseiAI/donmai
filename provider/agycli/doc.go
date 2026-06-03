// Package agycli implements an agent.Provider that shells out to Google
// Antigravity's `agy` CLI — the closed-source Go successor to the EOL
// `gemini` CLI.
//
// This is the LOCAL / HOST-SESSION / USER-SUBSCRIPTION provider: it launches
// the user's own, already-logged-in, first-party `agy` binary on the user's
// own machine to do the user's work. The CLI owns its OAuth credentials
// (`~/.gemini/oauth_creds.json`, `security.auth.selectedType=oauth-personal`);
// this provider injects NO API key and discovers NO credentials — it is
// host-session fail-open. It is NOT a third-party OAuth wrapper: it automates
// the official tool the user authenticated, not a re-implementation that
// borrows the user's token.
//
// It is DISTINCT from two sibling providers:
//
//   - "gemini"     — API-direct Go provider (net/http to generativelanguage),
//     key-based, structured.
//   - "gemini-cli" — subprocess wrap of the EOL `gemini` CLI, API-KEY-only,
//     stdin + `--output-format stream-json`. Retired after 2026-06-18.
//   - "agy-cli"    — THIS package: subprocess wrap of `agy`, OAuth/local,
//     pty + plain-text, no key.
//
// # Why this is materially different from the gemini-cli wrap
//
// Empirical probe of `agy` v1.0.4 (runs/2026-06-03-antigravity-migration/CONTRACT.md):
//
//   - `agy -p "<prompt>"` MUST run under a pty — plain stdin/stdout pipes hang
//     with zero output. We allocate a pseudo-terminal via github.com/creack/pty;
//     the geminicli stdin-pipe path does NOT transfer.
//   - There is NO structured output mode. `--output-format json` hard-errors in
//     v1.0.4. stdout is plain-text agentic prose (terse narration + final
//     answer). There is NO `--model` and NO `--skip-trust` flag.
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
//     transcript to ~/.gemini/antigravity-cli/brain/<conv-id>/.system_generated/
//     logs/transcript.jsonl. We discover the conv-id (by diffing the brain/
//     directory across spawn) and replay tool_calls → agent.ToolUseEvent and
//     tool-result steps → agent.ToolResultEvent. This is an INTERNAL,
//     version-fragile path: any discovery/parse failure degrades silently to
//     the stdout spine. Toggle: Options.DisableTranscriptEnrichment.
//
// # Capability matrix (see CONTRACT.md §5)
//
//   - SupportsMessageInjection=false / SupportsSessionResume=false: single-shot
//     `-p`. (`--conversation <id>` resume exists; deferred.)
//   - AcceptsMcpServerSpec=false / SupportsToolPlugins=false (v1): `agy` reads
//     MCP from the GLOBAL ~/.gemini/settings.json mcpServers, not a CLI flag or
//     a per-worktree file we can verify; MCP wiring is deferred.
//   - SupportsReasoningEffort=false: no flag.
//   - Model selection is a persisted CLI setting with NO per-run flag, so
//     spec.Model is informational only; the run uses the user's configured agy
//     model.
//
// # Known limitations
//
//   - Prompt exposure: agy takes the prompt as the `-p` argv VALUE (it has no
//     stdin-prompt mode), so the prompt text is visible in the host process
//     listing (e.g. `ps`). The geminicli provider hides the prompt via stdin;
//     agy cannot. Acceptable for the single-user host this provider targets,
//     but callers should not embed secrets in the prompt.
//   - Workspace trust is opt-in (Options.TrustWorkspace, default off): untrusted
//     cwds run fine under --dangerously-skip-permissions, so the provider does
//     not mutate agy's shared settings.json by default.
//
// # Auth / token usage
//
// No key is injected; the `agy` binary authenticates via its own host OAuth.
// Token/cost accounting is NOT available (the transcript carries no token
// counts) — agent.ResultEvent.Cost is left nil. This is a documented loss
// versus the gemini stream-json provider.
package agycli
