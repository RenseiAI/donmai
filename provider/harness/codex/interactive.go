package codex

import (
	"context"
	"fmt"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/ptycli"
)

// SpawnInteractive opens the codex CLI's OWN interactive TUI — bare
// `codex`, NOT `codex exec` (the non-interactive/headless subcommand this
// package's default Spawn drives via the app-server) — under a PTY via
// ptycli, seeded with spec.Prompt when set.
//
// It is completely independent of the app-server JSON-RPC subprocess this
// package otherwise drives: it never touches Provider.client/cmd, resolving
// the codex binary itself via resolveCodexBinary. That is why it is a
// package-level function taking Options rather than a (*Provider) method
// bound to a live app-server — the interactive spawn mode needs no live
// Provider at all, only the same binary-resolution rule New uses. Provider's
// Spawn (codex.go) is the production call site; it is exported so a caller
// (or a test) that only wants the interactive path can reach it directly
// without paying for an app-server handshake.
//
// This is a distinct SPAWN MODE from the default headless loop, not a
// different Transport: Manifest().Caps.Transport stays
// agent.TransportSubprocessRPC (the app-server's transport); TransportPTY is
// not codex's declared Transport, only its interactive spawn mode. See
// manifest.go and agent/harness.go's HarnessCaps.SupportsInteractivePTY doc
// comment for why the two are orthogonal, and provider/harness/shell for the
// contrasting harness whose ONLY transport is PTY.
//
// Event semantics are the coarse ptycli contract (program decision D4 — the
// byte-accurate PTY stream is the product): an InitEvent once the PTY child
// is up (SessionID stays empty — codex's thread id is only observable
// through the app-server's JSON-RPC notifications, which the interactive TUI
// never emits) and a single terminal ResultEvent when the CLI process exits.
func SpawnInteractive(ctx context.Context, opts Options, spec agent.Spec) (agent.Handle, error) {
	bin, err := resolveCodexBinary(opts.CodexBin)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	return ptycli.Spawn(ctx, bin, interactiveArgs(spec), spec)
}

// interactiveArgs builds the argv for codex's own interactive TUI. The codex
// CLI accepts a positional prompt to seed the first message of an
// interactive session — `codex "fix the failing tests"` launches the TUI
// with that initial prompt already queued, distinct from
// `codex exec "..."` (headless, one-shot, prints the final message to
// stdout and exits) which this package never uses for the interactive
// spawn mode. An empty prompt starts the TUI bare, with no seeded message.
func interactiveArgs(spec agent.Spec) []string {
	if spec.Prompt == "" {
		return nil
	}
	return []string{spec.Prompt}
}
