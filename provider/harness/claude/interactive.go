package claude

import (
	"context"
	"fmt"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/clijsonl"
	"github.com/RenseiAI/donmai/provider/harness/ptycli"
)

// spawnInteractive opens the claude CLI's OWN interactive REPL — bare
// `claude`, with NEITHER `-p`/`--print` NOR `--output-format stream-json`
// (those are the headless one-shot flags buildArgs uses for the default
// loop) — under a PTY via ptycli, seeded with spec.Prompt when set and
// carrying the session-level flags interactiveArgs maps from the Spec.
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
	mcpPath, err := clijsonl.WriteMCPConfig(spec.MCPServers)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrSpawnFailed, err)
	}
	return ptycli.SpawnWithCleanup(ctx, p.binary, interactiveArgsWithMCP(spec, mcpPath), spec, p.Manifest(), func() error {
		return clijsonl.RemoveMCPConfig(mcpPath)
	})
}

// interactiveArgs builds the argv for claude's own interactive REPL.
//
// Spec → CLI mapping (interactive spawn mode):
//
//	Autonomous         → --permission-mode bypassPermissions
//	SystemPromptAppend → --append-system-prompt <text>
//	Prompt             → positional argument, ALWAYS LAST
//
// The Autonomous mapping is deliberately byte-identical to buildArgs' (see
// cli_args.go) — one convention, two spawn modes. Before it existed the
// interactive REPL silently DROPPED Spec.Autonomous and fell back to the
// CLI's own default permission mode, so a headless run and an interactive
// run built from the SAME Spec got DIFFERENT permission postures.
// --permission-mode is a session-level flag the REPL honors exactly as the
// headless invocation does, so the divergence was never capability gating
// (the CLI can honor it) — the CLI simply was not being asked.
//
// The claude CLI accepts a positional prompt to seed the first message of
// an interactive session — `claude "fix the bug"` launches the REPL with
// that initial prompt already queued, distinct from `claude -p "..."`
// (headless, one-shot, no session) which buildArgs uses. An empty prompt
// starts the REPL bare, with no seeded message. The positional prompt must
// stay LAST so it is never consumed as the value of a preceding flag.
func interactiveArgs(spec agent.Spec) []string {
	return interactiveArgsWithMCP(spec, "")
}

func interactiveArgsWithMCP(spec agent.Spec, mcpConfigPath string) []string {
	var argv []string

	// Permission mode: autonomous sessions get bypassPermissions so the
	// REPL does not stall on an approval prompt no human is present to
	// answer. Non-autonomous sessions inherit the CLI default. Mirrors
	// buildArgs' identical branch.
	if spec.Autonomous {
		argv = append(argv, "--permission-mode", "bypassPermissions")
	}

	// SystemPromptAppend carries the composed session instructions assembled
	// by the runner. The Claude REPL accepts the same flag as the headless CLI;
	// omit it when empty so a bare interactive session stays bare.
	if spec.SystemPromptAppend != "" {
		argv = append(argv, "--append-system-prompt", spec.SystemPromptAppend)
	}

	// Claude's interactive REPL accepts the same exact per-session MCP file as
	// headless mode. Strict mode prevents ambient user MCP config from silently
	// widening the session tool surface.
	if mcpConfigPath != "" {
		argv = append(argv, "--mcp-config", mcpConfigPath, "--strict-mcp-config")
	}

	// Positional prompt LAST — every flag-shaped argument precedes it.
	if spec.Prompt != "" {
		argv = append(argv, spec.Prompt)
	}
	return argv
}
