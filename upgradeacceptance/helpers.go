package upgradeacceptance

import (
	"context"
	"os"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/pi"
)

// headlessShimLaunchEnabled reports whether this build can launch a headless
// seat under shim ownership. It is false on current main: the daemon's
// selection rule owns interactive sessions only, and the local runtime
// holds rather than adopts a recovered session. The red acceptance record
// (TestUpgradeAcceptanceRed) pins this false; the green slices flip the
// production rule, and this helper must follow it — never the other way
// around — so the red cannot be edited green without the behaviour.
func headlessShimLaunchEnabled() bool {
	return false
}

// piSpec builds a minimal headless spec for the fake harness: a bare prompt
// in a work dir, with no model pin (the fake answers regardless). The
// FAKE_HARNESS_* knobs ride Spec.Env so they survive the harness's
// allowlist partition into the session credential file, which the fake
// restores at startup the way the real boundary extension does.
func piSpec(dir, prompt string) agent.Spec {
	return agent.Spec{
		Cwd:    dir,
		Prompt: prompt,
		Env: map[string]string{
			"FAKE_HARNESS_TRIGGER":   os.Getenv("FAKE_HARNESS_TRIGGER"),
			"FAKE_HARNESS_STATE_DIR": os.Getenv("FAKE_HARNESS_STATE_DIR"),
			"FAKE_HARNESS_REPLY":     os.Getenv("FAKE_HARNESS_REPLY"),
		},
	}
}

// collectAssistantText drains the handle's event stream until the terminal
// result event or the timeout, returning the concatenated assistant text.
func collectAssistantText(ctx context.Context, handle agent.Handle, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var text string
	for {
		select {
		case <-ctx.Done():
			return text, ctx.Err()
		case ev, ok := <-handle.Events():
			if !ok {
				return text, nil
			}
			switch e := ev.(type) {
			case agent.AssistantTextEvent:
				text += e.Text
			case agent.ResultEvent:
				return text, nil
			case agent.ErrorEvent:
				return text, nil
			}
		}
	}
}

// resumeFakeHarness re-execs the fake harness binary against a persisted
// session the way the pi provider's Resume does: argv carries the session
// flag so the harness loads its transcript history, and the caller then
// injects the resume directive as a follow-up turn — the same two-step the
// runner's stop-and-resume fallback uses (stop the turn, Resume, then the
// queued steer content rides an Inject). The first resumed turn reports the
// loaded history.
func resumeFakeHarness(ctx context.Context, provider *pi.Provider, bin, dir, sessionID, prompt string) (agent.Handle, error) {
	_ = bin
	// Drive the production Resume entry point: the fake answers get_entries
	// and reports its loaded history on the next turn.
	handle, err := provider.Resume(ctx, sessionID, piSpec(dir, prompt))
	if err != nil {
		return nil, err
	}
	if err := handle.Inject(ctx, prompt); err != nil {
		_ = handle.Stop(context.Background())
		return nil, err
	}
	return handle, nil
}
