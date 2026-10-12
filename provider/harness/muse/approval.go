package muse

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// Approval modes (MSP closed vocabulary on session/start):
// allowAll approves everything; promptUnmatched asks for what no rule
// covers; denyUnmatched refuses it. The read-only lane pins
// denyUnmatched via resolveApprovalMode in spec_translation.go.
// Approval modes (MSP closed vocabulary on session/start):
// allowAll, promptUnmatched, onRequest, denyUnmatched. Autonomous
// sessions run onRequest (the bridge answers); the read-only lane pins
// denyUnmatched via resolveApprovalMode in spec_translation.go.

// ApprovalBridge answers host approval requests from
// Spec.PermissionConfig. Autonomous sessions run unattended: allow and
// disallow patterns decide, the default decision breaks ties, and
// anything without a decided allow is denied (fail-closed — an
// unattended session must never auto-approve what policy did not name).
type ApprovalBridge struct {
	allow    []string
	disallow []string
	def      string
}

// NewApprovalBridge builds the bridge from the Spec's permission
// config. A nil config means "no policy": the bridge denies everything
// except the read-only-safe classes the host itself gates (fail-closed
// for an autonomous session with no policy), and denyUnmatched
// sessions always deny.
func NewApprovalBridge(cfg *agent.PermissionConfig, readOnly bool) *ApprovalBridge {
	b := &ApprovalBridge{def: "deny"}
	if readOnly {
		// The read-only lane denies every approval: the serve flags
		// already remove shell + write, and denyUnmatched is pinned,
		// so the bridge is a second denial, never an allow.
		return b
	}
	if cfg == nil {
		return b
	}
	b.allow = append([]string(nil), cfg.AllowPatterns...)
	b.disallow = append([]string(nil), cfg.DisallowPatterns...)
	if cfg.DefaultDecision != "" {
		b.def = strings.ToLower(cfg.DefaultDecision)
	}
	return b
}

// Decision is one bridge verdict.
type Decision struct {
	// Allow selects the choice: an allow choice when true, the abort
	// choice when false (decline maps to abort — shell has no deny
	// choice).
	Allow bool
	// ChoiceLabel names the chosen arm for diagnostics.
	ChoiceLabel string
	// Reason explains the verdict for the activity stream.
	Reason string
}

// Evaluate decides an approval request. Disallow patterns win over
// allow patterns; the default decides the rest. Unknown subjects with
// no matching pattern fall to the default (deny unless the Spec set
// "allow" — an explicit, reviewable opt-in).
func (b *ApprovalBridge) Evaluate(req ApprovalRequestParams) Decision {
	subject := approvalSubjectText(req)
	if matchesAny(subject, b.disallow) {
		return Decision{Allow: false, ChoiceLabel: "abort", Reason: "matched disallow pattern: " + subject}
	}
	if matchesAny(subject, b.allow) {
		return Decision{Allow: true, ChoiceLabel: "allow", Reason: "matched allow pattern: " + subject}
	}
	if b.def == "allow" {
		return Decision{Allow: true, ChoiceLabel: "allow", Reason: "default allow: " + subject}
	}
	return Decision{Allow: false, ChoiceLabel: "abort", Reason: "no allow pattern matched (default deny): " + subject}
}

// PickChoice selects the choiceId to send on approval/decide: the
// first available choice whose decision matches the verdict (approved*
// for allow, abort for deny). Shell approvals carry no deny choice —
// decline maps to abort — so deny always resolves to the abort arm and
// fails closed (error) when no abort arm is offered.
func (b *ApprovalBridge) PickChoice(req ApprovalRequestParams, verdict Decision) (string, error) {
	if len(req.AvailableChoices) == 0 {
		return "", fmt.Errorf("muse approval: no available choices on %s", req.ApprovalID)
	}
	if verdict.Allow {
		for _, ch := range req.AvailableChoices {
			if strings.HasPrefix(ch.Decision, "approved") {
				return ch.ChoiceID, nil
			}
		}
		return "", fmt.Errorf("muse approval: no approved choice on %s", req.ApprovalID)
	}
	for _, ch := range req.AvailableChoices {
		if ch.Decision == "abort" {
			return ch.ChoiceID, nil
		}
	}
	// No abort arm: fall back to any denied* arm so the decision is
	// still a refusal, never an accidental allow.
	for _, ch := range req.AvailableChoices {
		if strings.HasPrefix(ch.Decision, "denied") {
			return ch.ChoiceID, nil
		}
	}
	return "", fmt.Errorf("muse approval: no abort/denied choice on %s", req.ApprovalID)
}

func approvalSubjectText(req ApprovalRequestParams) string {
	subject := req.Subject
	parts := []string{}
	if req.ToolName != "" {
		parts = append(parts, req.ToolName)
	}
	switch {
	case subject.Command != "":
		parts = append(parts, subject.Command)
	case subject.Path != "":
		parts = append(parts, subject.Path)
	case subject.Target != "":
		parts = append(parts, subject.Target)
	case subject.Kind != "":
		parts = append(parts, subject.Kind)
	}
	if len(parts) == 0 {
		return req.ApprovalID
	}
	return strings.Join(parts, " ")
}

func matchesAny(subject string, patterns []string) bool {
	for _, pat := range patterns {
		if pat == "" {
			continue
		}
		if ok, err := matchGlob(pat, subject); err == nil && ok {
			return true
		}
		// A literal substring match covers unanchored policy
		// fragments ("pnpm:*" style prefixes without a full glob).
		if strings.Contains(subject, strings.Trim(pat, "*")) && strings.Trim(pat, "*") != "" {
			return true
		}
	}
	return false
}

// matchGlob matches a small glob dialect (* and ? wildcards, anchored)
// against the subject. It is the same minimal dialect the codex
// approval bridge documents; full doublestar is out of scope.
func matchGlob(pattern, subject string) (bool, error) {
	px, sx := []rune(pattern), []rune(subject)
	var pxI, sxI, star, mark int
	star = -1
	for sxI < len(sx) {
		switch {
		case pxI < len(px) && (px[pxI] == '?' || px[pxI] == sx[sxI]):
			pxI++
			sxI++
		case pxI < len(px) && px[pxI] == '*':
			star = pxI
			mark = sxI
			pxI++
		case star != -1:
			pxI = star + 1
			mark++
			sxI = mark
		default:
			return false, nil
		}
	}
	for pxI < len(px) && px[pxI] == '*' {
		pxI++
	}
	return pxI == len(px), nil
}

// parseApprovalRequest decodes the params of an approval/request
// server-request or approval/requested notification.
func parseApprovalRequest(raw json.RawMessage) (ApprovalRequestParams, error) {
	var req ApprovalRequestParams
	if err := json.Unmarshal(raw, &req); err != nil {
		return ApprovalRequestParams{}, fmt.Errorf("muse approval: decode request: %w", err)
	}
	return req, nil
}
