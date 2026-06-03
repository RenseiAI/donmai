package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/claude"
)

// shouldSteer reports whether stage 1 of tail recovery (steering)
// should fire. Two preconditions must hold:
//
//  1. The session ended with terminalSuccess but did not produce a PR
//     URL (or did not produce a comment / issue update for non-code
//     work types).
//  2. The provider supports message injection
//     (Capabilities.SupportsMessageInjection).
//
// NOTE: steering deliberately does NOT gate on
// Capabilities.SupportsSessionResume. attemptSteering only ever calls
// handle.Inject (see [Runner.injectDirective]); the documented
// resume-based steering fallback ("stop the handle and resume with a
// fresh Spec") is unimplemented. Greenlighting steering for a
// resume-only provider — e.g. codex, whose Inject is a hard
// agent.ErrUnsupported — logged "steering: injecting follow-up prompt"
// then silently no-opped on every session, masking the real outcome.
// Until resume-steering lands, message injection is the only capability
// that makes steering productive.
//
// When the precondition doesn't hold the runner skips steering and goes
// straight to the deterministic backstop. The decision is encoded
// here so backstop.go and the loop don't have to re-derive it.
func shouldSteer(obs streamObservation, caps agent.Capabilities) bool {
	// Provider must support mid-session message injection — the only
	// post-completion steering mechanism the runner implements today.
	if !caps.SupportsMessageInjection {
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
// asking the agent to commit, push, and open a PR. The ONLY mechanism
// is message injection via [agent.Handle.Inject]; shouldSteer gates on
// Capabilities.SupportsMessageInjection so this path is never reached
// for a provider that cannot accept an inject. The resume-based
// steering fallback ("stop the handle and resume with a fresh Spec")
// described in F.1.1 §3 is NOT implemented — a resume-only provider is
// handled by the deterministic backstop instead, not by steering.
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
//   - [claude.ErrSessionNotReady] — no InitEvent observed yet; injecting
//     before the session id is captured is a transient race, not an error.
//   - [claude.ErrInjectInFlight]  — a previous inject's --resume subprocess
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
		case errors.Is(err, claude.ErrSessionNotReady):
			r.logger.Debug("inject skipped: session not ready (no InitEvent yet)")
			return nil
		case errors.Is(err, claude.ErrInjectInFlight):
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
		b.WriteString("via `rensei linear create-comment`.\n\n")
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
