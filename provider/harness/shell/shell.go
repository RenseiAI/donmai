package shell

import (
	"context"
	"fmt"
	"os"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/ptycli"
)

// DefaultShell is the fallback interactive shell when $SHELL is unset.
const DefaultShell = "/bin/sh"

// Provider is the bare interactive-only PTY harness. See doc.go.
type Provider struct{}

// New constructs the shell Provider. Construction never fails: unlike
// claude/codex there is no CLI to probe at construction time — the shell
// binary is resolved lazily at Spawn time from $SHELL (mirroring how a real
// login shell is chosen), the same way a fresh terminal picks its shell.
func New() (*Provider, error) { return &Provider{}, nil }

// Name returns agent.ProviderShell.
func (*Provider) Name() agent.ProviderName { return agent.ProviderShell }

// Capabilities returns the all-off agent-loop matrix: shell has no agent
// loop at all — it is a bare terminal, not a headless CLI-JSONL/JSON-RPC
// driver — only the interactive spawn mode declared on Manifest().
func (*Provider) Capabilities() agent.Capabilities {
	return agent.Capabilities{HumanLabel: "Shell"}
}

// Spawn starts the interactive shell under a PTY. shell is interactive-only:
// a Spec without Interactive set has no headless fallback to run, so Spawn
// returns a clear agent.ErrUnsupported instead of silently no-op'ing.
func (p *Provider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	if spec.Interactive == nil {
		return nil, fmt.Errorf(
			"provider/harness/shell: Spawn: %w (shell is interactive-only; set Spec.Interactive)",
			agent.ErrUnsupported,
		)
	}
	var err error
	spec, err = agent.PreparePrompt(spec, p.Manifest())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	// Minimal per the wave scope: Spec carries no command-override slot
	// today (agent/types.go), so shell always spawns $SHELL — never a
	// Spec-provided command.
	return ptycli.Spawn(ctx, shellBinary(), nil, spec)
}

// Resume returns agent.ErrUnsupported: a bare shell has no session identity
// to resume (SupportsSessionResume=false).
func (*Provider) Resume(_ context.Context, _ string, _ agent.Spec) (agent.Handle, error) {
	return nil, fmt.Errorf(
		"provider/harness/shell: Resume: %w (SupportsSessionResume=false; shell has no resumable session state)",
		agent.ErrUnsupported,
	)
}

// Shutdown is a no-op: each session is its own pty-attached subprocess that
// terminates with its Handle; the provider holds no long-lived resources.
func (*Provider) Shutdown(_ context.Context) error { return nil }

// shellBinary resolves the interactive shell to spawn: $SHELL, falling back
// to DefaultShell.
func shellBinary() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return DefaultShell
}
