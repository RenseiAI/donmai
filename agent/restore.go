package agent

// restore.go — the restore state machine: re-entering a prior harness
// conversation without ever silently starting fresh.
//
// A seat that comes back — daemon restart, runner retry, operator re-attach —
// holds at most a resume token (a Claude session id the harness generated at
// launch, a Codex thread id captured from thread/start) and a directive. The
// rule this file enforces:
//
//	fresh is taken on an explicit flag or a no-conversation-found error,
//	and the receipt records which. Every other failure asks or escalates.
//
// The four outcomes:
//
//	resumed            the harness accepted the token; the prior
//	                   conversation continues.
//	fresh_primed       a new conversation starts, and the receipt says why:
//	                   the caller passed AllowFresh explicitly, or the
//	                   resume attempt proved the conversation is gone
//	                   (agent.ErrSessionNotFound or a harness
//	                   no-conversation-found rejection).
//	awaiting_decision  no token and no explicit flag: starting fresh would
//	                   be a guess, so the caller must ask (operator pick,
//	                   policy default) instead of spawning.
//	attention_required the resume attempt failed for any other reason
//	                   (auth, network, crashed backend): retrying fresh
//	                   would orphan the prior conversation's work, so the
//	                   caller escalates instead of spawning.
//

import (
	"errors"
	"strings"
)

// RestoreOutcome is the restore state machine's terminal state. Stable
// wire strings; the receipt carries one verbatim.
type RestoreOutcome string

const (
	// RestoreResumed means the harness accepted the resume token and the
	// prior conversation continues under it.
	RestoreResumed RestoreOutcome = "resumed"
	// RestoreFreshPrimed means a new conversation starts. Taken ONLY on
	// an explicit AllowFresh flag or a proven no-conversation-found
	// resume failure; FreshReason on the receipt names which.
	RestoreFreshPrimed RestoreOutcome = "fresh_primed"
	// RestoreAwaitingDecision means there is no token and no explicit
	// fresh flag: the caller must ask rather than guess. Never spawns.
	RestoreAwaitingDecision RestoreOutcome = "awaiting_decision"
	// RestoreAttentionRequired means the resume attempt failed for a
	// reason other than not-found. Never spawns; the caller escalates.
	RestoreAttentionRequired RestoreOutcome = "attention_required"
)

// Fresh reasons recorded on a fresh_primed receipt: which of the two
// admitted fresh paths was taken.
const (
	// FreshReasonExplicitFlag means the caller passed AllowFresh: the
	// operator (or policy) chose a new conversation up front.
	FreshReasonExplicitFlag = "explicit_flag"
	// FreshReasonNoConversation means the resume attempt proved the prior
	// conversation is gone (ErrSessionNotFound or a harness
	// no-conversation-found rejection), so fresh is the only
	// continuation available.
	FreshReasonNoConversation = "no_conversation_found"
)

// RestoreRequest is the restore state machine's input.
type RestoreRequest struct {
	// ResumeToken is the harness-native resume token: a Claude session id
	// owned at launch, or a Codex thread id captured after launch.
	// Empty means no prior conversation is addressable.
	ResumeToken string `json:"resumeToken,omitempty"`

	// AllowFresh is the explicit fresh flag: the caller has decided a new
	// conversation is acceptable (operator pick, policy default). It is
	// the ONLY way to take fresh without first proving the prior
	// conversation gone.
	AllowFresh bool `json:"allowFresh,omitempty"`

	// Harness names the harness the token belongs to, for receipt
	// attribution. Empty leaves the receipt unattributed.
	Harness HarnessName `json:"harness,omitempty"`
}

// RestoreReceipt records which restore path was taken and why. It is the
// machine's durable output: a later restore reads the prior receipt's
// outcome instead of re-deriving it.
type RestoreReceipt struct {
	// Outcome is the terminal state. Always set.
	Outcome RestoreOutcome `json:"outcome"`

	// ResumeToken echoes the request's token: the conversation this
	// receipt decided about.
	ResumeToken string `json:"resumeToken,omitempty"`

	// FreshReason is set only on fresh_primed: explicit_flag or
	// no_conversation_found.
	FreshReason string `json:"freshReason,omitempty"`

	// Detail is a human-readable one-liner naming the deciding evidence
	// (never a bearer or token value beyond the echoed id).
	Detail string `json:"detail,omitempty"`

	// Harness echoes the request's harness attribution.
	Harness HarnessName `json:"harness,omitempty"`
}

// DecideRestore runs the restore state machine to its terminal state.
//
// resumeAttempt is the harness resume call against req.ResumeToken; it is
// invoked exactly once, and only when a token is present and no explicit
// fresh flag overrides it. A nil error means the harness accepted the
// token. attemptErr carries the resume failure when the call already ran
// (letting callers that probe before deciding share one attempt); when
// both are absent the machine probes itself.
//
// The machine never returns fresh_primed silently: without AllowFresh it
// probes the token first, and a probe failure that is not
// no-conversation-found escalates to attention_required rather than
// falling back to fresh.
func DecideRestore(req RestoreRequest, resumeAttempt func(token string) error, attemptErr error) RestoreReceipt {
	token := strings.TrimSpace(req.ResumeToken)
	if token == "" {
		if req.AllowFresh {
			return RestoreReceipt{
				Outcome:     RestoreFreshPrimed,
				FreshReason: FreshReasonExplicitFlag,
				Detail:      "no resume token held; explicit fresh flag takes a new conversation",
				Harness:     req.Harness,
			}
		}
		return RestoreReceipt{
			Outcome: RestoreAwaitingDecision,
			Detail:  "no resume token held and no explicit fresh flag: starting fresh would be a guess",
			Harness: req.Harness,
		}
	}
	if req.AllowFresh {
		return RestoreReceipt{
			Outcome:     RestoreFreshPrimed,
			ResumeToken: token,
			FreshReason: FreshReasonExplicitFlag,
			Detail:      "explicit fresh flag overrides the held resume token",
			Harness:     req.Harness,
		}
	}
	err := attemptErr
	if err == nil && resumeAttempt != nil {
		err = resumeAttempt(token)
	}
	if err == nil {
		return RestoreReceipt{
			Outcome:     RestoreResumed,
			ResumeToken: token,
			Detail:      "harness accepted the resume token; prior conversation continues",
			Harness:     req.Harness,
		}
	}
	if IsNoConversationFound(err) {
		return RestoreReceipt{
			Outcome:     RestoreFreshPrimed,
			ResumeToken: token,
			FreshReason: FreshReasonNoConversation,
			Detail:      "resume attempt proved the prior conversation is gone: " + shortCause(err),
			Harness:     req.Harness,
		}
	}
	return RestoreReceipt{
		Outcome:     RestoreAttentionRequired,
		ResumeToken: token,
		Detail:      "resume attempt failed without proving the conversation gone: " + shortCause(err),
		Harness:     req.Harness,
	}
}

// IsNoConversationFound reports whether err proves the resume target does
// not exist (or has no resumable history), which is the only resume
// failure that admits a fresh start without an explicit flag.
//
// The typed form is agent.ErrSessionNotFound, which headless and
// interactive harnesses return for an unknown or history-less id. The
// string forms cover harness backends that reject with plain text: the
// Claude CLI's "No conversation found with session ID" and Codex's "no
// rollout found for thread id" / "No saved session found". Matching is
// case-insensitive and substring-scoped to the known rejection shapes so
// an unrelated error that happens to mention a session is not misread as
// gone.
func IsNoConversationFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrSessionNotFound) {
		return true
	}
	lowered := strings.ToLower(err.Error())
	for _, shape := range []string{
		"no conversation found",
		"no rollout found",
		"no saved session found",
		"conversation not found",
		"session not found",
		"thread not found",
	} {
		if strings.Contains(lowered, shape) {
			return true
		}
	}
	return false
}

// shortCause renders the deciding error for a receipt Detail line. It
// names the failure without quoting bearer material: the known harness
// rejection shapes above carry ids only, and anything else is reduced to
// its first line, bounded.
func shortCause(err error) string {
	msg := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = strings.TrimSpace(msg[:i])
	}
	const limit = 200
	if len(msg) > limit {
		msg = msg[:limit] + "…(truncated)"
	}
	return msg
}
