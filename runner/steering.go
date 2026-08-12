package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
//
// workType gates the whole chain on the completion contract: a work
// type whose completion is NOT result-sensitive (isResultSensitive,
// sdlc.go — the contract requires no PR/branch artifact, e.g.
// WorkTypeBacklogGroomer / research / refinement which produce only a
// comment or issue update) NEVER enters the commit/PR steering chain.
// This exempts non-version-controlled PM work from PR pressure WITHOUT
// changing behaviour for development / qa / acceptance, all of which
// are result-sensitive and keep their existing steering flow.
func shouldSteer(obs streamObservation, caps agent.Capabilities, workType string) bool {
	// Contract gate: non-result-sensitive work types are never steered
	// toward a commit/PR — they produce comments/issue updates, not code.
	if !isResultSensitive(workType) {
		return false
	}
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
// asking the agent to commit, push, and open a PR. The injection path
// is preferred (no subprocess overhead). When the live handle rejects
// the inject as unsupported — either because the harness never
// supports message injection (e.g. Codex) or because THIS handle
// doesn't even though the provider's capability matrix allows it on
// another lane (e.g. OpenCode's one-shot handle) — and the harness
// declares session resume (Capabilities.SupportsSessionResume),
// attemptSteering falls back to stop-and-resume: it stops the current
// turn, then calls Provider.Resume on the SAME provider-native session
// with the steering prompt riding Spec.Prompt as the resumed turn's
// directive. This is the fallback this file has documented since
// F.1.1 §3 but never wired for any provider until now.
//
// Returns the Handle the caller should keep draining events from: the
// SAME handle on the inject-success and soft-fail paths (no behavior
// change), or the NEW Handle Provider.Resume returned on the fallback
// path. A non-nil error means steering could not be delivered through
// either rail; the returned Handle is still safe to use (Stop is
// idempotent) and the caller falls through to the deterministic
// backstop instead of re-consuming events.
func (r *Runner) attemptSteering(
	ctx context.Context,
	provider agent.Provider,
	handle agent.Handle,
	spec agent.Spec,
	caps agent.Capabilities,
	qw QueuedWork,
	obs streamObservation,
	res *Result,
) (agent.Handle, error) {
	if obs.terminalSuccess && obs.pullRequestURL != "" {
		// Sanity guard — shouldSteer already returned false in this
		// case but keep the post-condition explicit so future calls
		// don't accidentally double-steer.
		return handle, nil
	}
	steerText := buildSteeringPrompt(qw, obs)
	r.logger.Info("steering: injecting follow-up prompt",
		"sessionId", qw.SessionID,
		"len", len(steerText),
	)

	// Deliberately NOT routed through the shared injectDirective helper:
	// that helper's contract (used by the Wave 3 memory drain too) swallows
	// ErrUnsupported into a silent nil so a soft-failed inject looks
	// identical to a delivered one. Steering needs the raw classification
	// to decide whether an ErrUnsupported is a dead end or a resume
	// opportunity, so it inspects handle.Inject's error directly.
	switch err := handle.Inject(ctx, steerText); {
	case err == nil:
		return handle, nil
	case errors.Is(err, clijsonl.ErrSessionNotReady):
		// Transient — no InitEvent observed yet. There is nothing to
		// resume from either; same soft no-op contract as injectDirective.
		r.logger.Debug("steering: inject skipped: session not ready (no InitEvent yet)")
		return handle, nil
	case errors.Is(err, clijsonl.ErrInjectInFlight):
		// Transient — a previous inject subprocess is still running.
		// Falling back to resume here would race that subprocess; same
		// soft no-op contract as injectDirective.
		r.logger.Debug("steering: inject skipped: a previous inject is still in flight")
		return handle, nil
	case errors.Is(err, agent.ErrUnsupported):
		if !caps.SupportsSessionResume {
			r.logger.Debug("steering: inject unsupported and harness declares no resume fallback")
			return handle, nil
		}
		return r.attemptSteeringResume(ctx, provider, handle, spec, qw, steerText, res)
	default:
		return handle, fmt.Errorf("steering: inject failed: %w", err)
	}
}

// attemptSteeringResume performs the stop-and-resume fallback documented
// in attemptSteering's doc comment: stop the current turn, then resume
// the SAME provider-native session with the steering directive as the
// resumed turn's Spec.Prompt.
//
// Routing through the SAME provider reference the caller used for Spawn
// preserves session continuity end to end: an embedder's DecorateProvider
// wrapper (agent/spec_decorator.go) and any harness's additional-extension
// materialize+digest-verify seam (e.g. pi's launch()) both re-run for the
// resumed session exactly as they would for any other Resume call — this
// fallback adds no bypass of either, it just gives Resume a real caller.
//
// Returns the new Handle on success — the caller re-consumes its events
// exactly like a successful inject. On any other failure it returns the
// ORIGINAL (now-stopped) handle plus an error, except when Provider.Resume
// itself rejects with agent.ErrUnsupported: that is the same soft no-op
// contract as the inject path above (nil error, caller falls through to
// backstop untouched).
func (r *Runner) attemptSteeringResume(
	ctx context.Context,
	provider agent.Provider,
	handle agent.Handle,
	spec agent.Spec,
	qw QueuedWork,
	steerText string,
	res *Result,
) (agent.Handle, error) {
	sessionID := handle.SessionID()
	if sessionID == "" {
		// No InitEvent was ever observed on this handle, so there is no
		// provider-native session id to resume. Same soft no-op shape as
		// the transient inject failures in attemptSteering.
		r.logger.Debug("steering: resume fallback skipped: no session id captured")
		return handle, nil
	}
	r.logger.Info("steering: inject unsupported, falling back to stop-and-resume",
		"sessionId", qw.SessionID,
		"providerSessionId", sessionID,
	)

	// Stop the turn safely before resuming. attemptSteering only runs
	// after the runner has already drained the handle to its terminal
	// event (F.1.1 §4 step 11 fires after step 10's terminal wait), so
	// this Stop releases provider-side resources deterministically
	// rather than aborting in-flight work.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	stopErr := handle.Stop(stopCtx)
	stopCancel()
	if stopErr != nil {
		return handle, fmt.Errorf("steering: resume fallback: stop failed: %w", stopErr)
	}

	// The resumed turn's directive is the same steering prompt Inject
	// would have delivered — only the transport changes. Copying spec
	// (a value type) preserves every other field the session was spawned
	// with (Cwd, Env, MCPServers, tool permissions, AdditionalExtensions, …).
	resumeSpec := spec
	resumeSpec.Prompt = steerText

	newHandle, err := provider.Resume(ctx, sessionID, resumeSpec)
	if err != nil {
		if errors.Is(err, agent.ErrUnsupported) {
			r.logger.Debug("steering: resume fallback rejected as unsupported",
				"sessionId", qw.SessionID,
			)
			return handle, nil
		}
		return handle, fmt.Errorf("steering: resume fallback: resume failed: %w", err)
	}
	if res != nil {
		res.SteeringResumeFallback = true
	}
	return newHandle, nil
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
