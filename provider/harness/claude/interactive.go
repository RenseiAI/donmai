package claude

import (
	"context"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/ptycli"
)

// spawnInteractive opens the claude CLI's OWN interactive REPL — bare
// `claude`, with NEITHER `-p`/`--print` NOR `--output-format stream-json`
// (those are the headless one-shot flags buildArgs uses for the default
// loop) — under a PTY via ptycli, seeded with spec.Prompt when set.
//
// This is a distinct SPAWN MODE from the default headless loop, not a
// different Transport: Manifest().Caps.Transport stays
// agent.TransportCLIInjection (the headless loop's transport); TransportPTY
// is not claude's declared Transport, only its interactive spawn mode. See
// manifest.go and agent/harness.go's HarnessCaps.SupportsInteractivePTY doc
// comment for why the two are orthogonal, and provider/harness/shell for the
// contrasting harness whose ONLY transport is PTY.
//
// Event semantics are the coarse ptycli contract (program decision D4 — the
// byte-accurate PTY stream is the product): an InitEvent once the PTY child
// is up (SessionID stays empty — claude's session id is only observable
// through its JSONL headless-mode output, which the interactive REPL never
// emits) and a single terminal ResultEvent when the CLI process exits.
func (p *Provider) spawnInteractive(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	return ptycli.Spawn(ctx, p.binary, interactiveArgs(spec), spec)
}

// interactiveArgs builds the argv for claude's own interactive REPL. The
// claude CLI accepts a positional prompt to seed the first message of an
// interactive session — `claude "fix the bug"` launches the REPL with that
// initial prompt already queued, distinct from `claude -p "..."` (headless,
// one-shot, no session) which buildArgs uses. An empty prompt starts the
// REPL bare, with no seeded message.
func interactiveArgs(spec agent.Spec) []string {
	if spec.Prompt == "" {
		return nil
	}
	return []string{spec.Prompt}
}
