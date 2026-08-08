package claude

import (
	"context"
	"errors"
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

	// The Stop-hook notice channel is BEST EFFORT at spawn time and load-
	// bearing at delivery time. A drop directory that cannot be created is a
	// session with no live-turn delivery, which is a degraded session — it is
	// not a failed one, because the durable mailbox is the floor and the agent
	// can still pull. So a setup failure spawns the session WITHOUT the hook
	// and lets the runner report the absence per message (it dead-letters
	// against a declared-but-absent channel, so the producer learns), rather
	// than killing a session over a latency optimisation.
	hook, hookErr := newStopHookChannel()
	settings := ""
	if hookErr == nil {
		if settings, err = hook.settingsJSON(); err != nil {
			_ = hook.close()
			hook, settings = nil, ""
		}
	} else {
		hook = nil
	}

	cleanup := func() error {
		errs := []error{clijsonl.RemoveMCPConfig(mcpPath)}
		if hook != nil {
			errs = append(errs, hook.close())
		}
		return errors.Join(errs...)
	}

	h, err := ptycli.SpawnWithCleanup(ctx, p.binary,
		interactiveArgsWith(spec, mcpPath, settings), spec, p.Manifest(), cleanup)
	if err != nil {
		return nil, err
	}
	if hook == nil {
		return h, nil
	}
	return &interactiveHandle{Handle: h, notices: hook}, nil
}

// interactiveHandle adds the Stop-hook pull channel to the shared PTY handle.
//
// Embedding *ptycli.Handle rather than reimplementing it keeps agent.Handle and
// agent.InteractiveCapable behaviour byte-identical to every other interactive
// harness; the only thing this type adds is the door the runner delivers
// through.
type interactiveHandle struct {
	*ptycli.Handle
	notices *stopHookChannel
}

var (
	_ agent.InteractiveCapable   = (*interactiveHandle)(nil)
	_ agent.NoticeChannelCapable = (*interactiveHandle)(nil)
)

// NoticeChannel returns this session's Stop-hook channel.
func (h *interactiveHandle) NoticeChannel() agent.NoticeChannel { return h.notices }

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
	return interactiveArgsWith(spec, "", "")
}

func interactiveArgsWithMCP(spec agent.Spec, mcpConfigPath string) []string {
	return interactiveArgsWith(spec, mcpConfigPath, "")
}

// interactiveArgsWith adds the notice-channel settings to the interactive argv.
//
// settingsJSON is passed as `--settings <json string>`, not as a file. The CLI
// accepts either, and the string form is what keeps the hook definition out of
// the session's worktree entirely — a settings FILE there is a file the agent
// can read, edit, or commit by accident.
//
// --settings is ADDITIVE: it layers onto whichever base layer the CLI's own
// --setting-sources selects, so declaring `hooks` here adds this Stop hook
// without replacing the user's configuration. That is why no --setting-sources
// is passed: suppressing the base layer would silently strip the operator's own
// settings from every interactive session, which is a far larger change than
// the one this flag is here to make.
func interactiveArgsWith(spec agent.Spec, mcpConfigPath, settingsJSON string) []string {
	var argv []string

	// Notice channel first: it is a session-level flag like the rest, and every
	// flag-shaped argument must precede the positional prompt.
	if settingsJSON != "" {
		argv = append(argv, "--settings", settingsJSON)
	}

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
