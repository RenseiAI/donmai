package shell

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/ptycli"
)

// DefaultShell is the fallback interactive shell when $SHELL is unset.
const DefaultShell = "/bin/sh"

// Provider is the bare interactive-only PTY harness. See doc.go.
type Provider struct {
	deliverSeed func(context.Context, agent.Handle, agent.InteractiveSession, string) error
}

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
	spec, err = agent.PrepareHarness(spec, p.Manifest())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	// Minimal per the wave scope: Spec carries no command-override slot
	// today (agent/types.go), so shell always spawns $SHELL — never a
	// Spec-provided command.
	handle, err := ptycli.Spawn(ctx, shellBinary(), nil, spec, p.Manifest())
	if err != nil {
		return nil, err
	}
	deliverSeed := p.deliverSeed
	if deliverSeed == nil {
		deliverSeed = ptycli.DeliverSeed
	}
	if err := deliverSeed(ctx, handle, handle.InteractiveSession(), spec.Prompt); err != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = handle.Stop(stopCtx)
		stopCancel()
		// PreparePrompt already persisted a ready adaptation decision. Replace
		// it with an application-failed denial so a failed PTY write never
		// leaves a success receipt or usable session behind.
		if receiptErr := emitShellSeedFailure(spec); receiptErr != nil {
			return nil, fmt.Errorf("%w: shell PTY seed delivery: %w; persist denial receipt: %w", agent.ErrSpawnFailed, err, receiptErr)
		}
		return nil, fmt.Errorf("%w: shell PTY seed delivery: %w", agent.ErrSpawnFailed, err)
	}
	return handle, nil
}

// emitShellSeedFailure retracts the ready prompt receipt after a failed PTY
// write. The correction itself is the shared one — every PTY-seed harness owes
// the same retraction, so it lives in the shared driver rather than being
// re-derived here.
func emitShellSeedFailure(spec agent.Spec) error {
	return ptycli.DenyPromptReceiptAfterSeedFailure(spec)
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
