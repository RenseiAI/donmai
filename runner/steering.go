package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/provider/harness/clijsonl"
)

// shouldSteer reports whether stage 1 of tail recovery (steering)
// should fire. Two preconditions must hold:
//
//  1. The session ended with terminalSuccess but did not produce a PR
//     URL (or did not produce a comment / issue update for non-code
//     work types).
//  2. The provider supports either message injection
//     (Capabilities.SupportsMessageInjection — preferred, no
//     subprocess overhead) or session resume
//     (Capabilities.SupportsSessionResume — falls back to
//     stop-and-resume).
//
// When neither precondition holds the runner skips steering and goes
// straight to the deterministic backstop. The decision is encoded
// here so backstop.go and the loop don't have to re-derive it.
func shouldSteer(obs streamObservation, caps agent.Capabilities) bool {
	// Provider must support some form of post-completion steering.
	if !caps.SupportsMessageInjection && !caps.SupportsSessionResume {
		return false
	}
	// If the session didn't finish successfully, steering can't help —
	// the agent itself reported failure.
	if !obs.terminalSuccess {
		return false
	}
	// Already produced a PR — nothing to steer.
	if obs.pullRequestURL != "" {
		return false
	}
	return true
}

// attemptSteering injects a per-provider templated steering prompt
// asking the agent to commit, push, and open a PR. The injection
// path is preferred (no subprocess overhead); when only Resume is
// available the runner stops the current handle and resumes (today
// the runner does not exercise that path — providers must implement
// Resume separately, and v0.5.0 ships with all three providers
// returning ErrUnsupported on Resume by design — see F.1.1 §3).
//
// Returns nil when the steering inject was accepted by the provider;
// the caller is responsible for re-consuming events to capture any
// new tool calls. Returns an error when the inject path is
// unsupported or the provider rejected it — the caller falls through
// to backstop.
func (r *Runner) attemptSteering(ctx context.Context, handle agent.Handle, qw QueuedWork, obs streamObservation) error {
	if obs.terminalSuccess && obs.pullRequestURL != "" {
		// Sanity guard — shouldSteer already returned false in this
		// case but keep the post-condition explicit so future calls
		// don't accidentally double-inject.
		return nil
	}
	prompt := buildSteeringPrompt(qw, obs)
	r.logger.Info("steering: injecting follow-up prompt",
		"sessionId", qw.SessionID,
		"len", len(prompt),
	)
	if err := r.injectDirective(ctx, handle, prompt); err != nil {
		return fmt.Errorf("steering: inject failed: %w", err)
	}
	return nil
}

// injectDirective delivers text into a live session as a follow-up user
// message via [agent.Handle.Inject], framing it the same way steering
// frames its directive (the agent treats it as a prompt). It is shared by
// [Runner.attemptSteering] and the Wave 3 runtime memory drain so both
// honour the same "soft-fail" contract.
//
// Returns nil (NON-fatal) when the provider cannot accept the inject right
// now for a benign reason:
//
//   - [agent.ErrUnsupported]      — provider has no injection capability;
//     the caller relies on the dispatch-time fold / backstop instead.
//   - [clijsonl.ErrSessionNotReady] — no InitEvent observed yet; injecting
//     before the session id is captured is a transient race, not an error.
//   - [clijsonl.ErrInjectInFlight]  — a previous inject's --resume subprocess
//     is still running; claude is single-in-flight, so the caller skips
//     this one rather than failing the run.
//
// Any other error is returned to the caller (fatal-ish; the caller decides
// whether to log-and-continue or abort). All Inject calls flow through here
// on the single runner goroutine — never call Inject concurrently.
func (r *Runner) injectDirective(ctx context.Context, handle agent.Handle, text string) error {
	if err := handle.Inject(ctx, text); err != nil {
		switch {
		case errors.Is(err, agent.ErrUnsupported):
			r.logger.Debug("inject skipped: provider does not support injection")
			return nil
		case errors.Is(err, clijsonl.ErrSessionNotReady):
			r.logger.Debug("inject skipped: session not ready (no InitEvent yet)")
			return nil
		case errors.Is(err, clijsonl.ErrInjectInFlight):
			r.logger.Debug("inject skipped: a previous inject is still in flight")
			return nil
		default:
			return fmt.Errorf("inject: %w", err)
		}
	}
	return nil
}

// buildSteeringPrompt renders the per-provider steering prompt asking
// the agent to commit, push, and open a PR. Plain text — providers
// (Claude/Codex/stub) accept it as a follow-up user message.
//
// The prompt is intentionally short and directive: it lists the
// missing fields and the exact CLI commands the agent should run.
// Long prose makes the agent more likely to "explore" instead of
// finishing the work.
func buildSteeringPrompt(qw QueuedWork, obs streamObservation) string {
	var b strings.Builder
	b.WriteString("Your previous turn finished without opening a pull request. ")
	b.WriteString("Please commit your work and open a PR before stopping.\n\n")
	b.WriteString("Run these commands now:\n")
	b.WriteString("  git status\n")
	b.WriteString("  git add -A\n")
	b.WriteString(fmt.Sprintf("  git commit -m \"feat: %s\"\n", commitSubject(qw)))
	b.WriteString("  git push -u origin HEAD\n")
	b.WriteString("  gh pr create --fill\n\n")
	if !obs.commentPosted {
		b.WriteString("Also post a brief progress comment on the Linear issue ")
		b.WriteString(fmt.Sprintf("via `%s linear create-comment`.\n\n", prompt.ResolveBrand().BrandCLI))
	}
	b.WriteString("After the PR is open, output the PR URL on a single line ")
	b.WriteString("and stop.\n")
	return b.String()
}

// commitSubject returns a sensible default commit subject derived
// from the QueuedWork.
func commitSubject(qw QueuedWork) string {
	switch {
	case qw.IssueIdentifier != "" && qw.Title != "":
		return qw.IssueIdentifier + ": " + qw.Title
	case qw.IssueIdentifier != "":
		return qw.IssueIdentifier
	case qw.Title != "":
		return qw.Title
	default:
		return "agent session " + qw.SessionID
	}
}
