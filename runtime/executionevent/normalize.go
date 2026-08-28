package executionevent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// NormalizeEvent maps only active, secret-free runtime topics. Events for
// which the hosted contract has no active payload are intentionally omitted;
// callers must not invent a topic or forward provider-native raw payloads.
func NormalizeEvent(sessionID string, seq uint64, observedAt time.Time, event agent.Event) (Record, bool, error) {
	if event == nil {
		return Record{}, false, fmt.Errorf("executionevent: nil agent event")
	}
	var eventType string
	var payload map[string]any
	switch ev := event.(type) {
	case agent.ToolUseEvent:
		eventType = "tool.called"
		payload = map[string]any{"toolName": ev.ToolName}
		if ev.ToolUseID != "" {
			payload["toolUseId"] = ev.ToolUseID
		}
		if ev.ToolCategory != "" {
			payload["toolCategory"] = ev.ToolCategory
		}
		if len(ev.Input) > 0 {
			digest, err := digestInput(ev.Input)
			if err != nil {
				return Record{}, false, err
			}
			payload["inputDigest"] = digest
		}
	case agent.ErrorEvent:
		eventType = "error.raised"
		// Provider error text can contain prompt/tool/credential material.
		// The normalized contract carries only a stable classified summary;
		// raw text remains in the local provider journal, never on this wire.
		payload = map[string]any{"message": "provider emitted an error"}
		if code := safeCode(ev.Code); code != "" {
			payload["code"] = code
		}
	case agent.SystemEvent:
		if strings.EqualFold(ev.Subtype, "blocked") {
			eventType = "session.blocked"
			payload = map[string]any{"reason": "session blocked by provider"}
		}
	default:
		return Record{}, false, nil
	}
	if eventType == "" {
		return Record{}, false, nil
	}
	record, err := NewRecord(sessionID, seq, observedAt, eventType, payload)
	if err != nil {
		return Record{}, false, err
	}
	return record, true, nil
}

// NewSessionEndedRecord creates the terminal source fact from runner-owned
// outcome data. A provider ResultEvent alone is not terminal authority.
func NewSessionEndedRecord(sessionID string, seq uint64, observedAt time.Time, outcome string, resultDigest string) (Record, error) {
	return NewSessionEndedRecordWithEvidence(sessionID, seq, observedAt, outcome, "graceful", resultDigest)
}

// NewSessionEndedRecordWithEvidence creates a terminal source record with
// explicit runner evidence provenance.
func NewSessionEndedRecordWithEvidence(sessionID string, seq uint64, observedAt time.Time, outcome, terminalEvidence, resultDigest string) (Record, error) {
	allowed := map[string]bool{
		"succeeded": true, "failed": true, "cancelled": true, "interrupted": true,
		"expired": true, "terminated": true, "lost": true,
	}
	if !allowed[outcome] {
		return Record{}, fmt.Errorf("executionevent: invalid session outcome %q", outcome)
	}
	validEvidence := map[string]bool{"native": true, "graceful": true, "forced": true, "inferred": true, "model-authored": true}
	if !validEvidence[terminalEvidence] {
		return Record{}, fmt.Errorf("executionevent: invalid terminal evidence %q", terminalEvidence)
	}
	payload := map[string]any{
		"outcome":          outcome,
		"terminalEvidence": terminalEvidence,
	}
	if resultDigest != "" {
		if len(resultDigest) != 64 {
			return Record{}, fmt.Errorf("executionevent: resultDigest must be a sha256 hex digest")
		}
		for _, r := range resultDigest {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				return Record{}, fmt.Errorf("executionevent: resultDigest must be a sha256 hex digest")
			}
		}
		payload["resultDigest"] = resultDigest
	}
	return NewRecord(sessionID, seq, observedAt, "session.ended", payload)
}

// NewSessionBlockedRecord creates the runner-owned fact for a deliberate
// agent refusal. Callers supply a bounded, classified reason only; raw result
// errors may contain provider or prompt material and never belong on this wire.
func NewSessionBlockedRecord(sessionID string, seq uint64, observedAt time.Time, reason string) (Record, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 2048 {
		return Record{}, fmt.Errorf("executionevent: blocked reason must be 1..2048 bytes")
	}
	return NewRecord(sessionID, seq, observedAt, "session.blocked", map[string]any{"reason": reason})
}

// NewPullRequestOpenedRecord creates a complete GitHub PR fact. A URL alone
// is insufficient for this topic because it cannot establish both branches.
func NewPullRequestOpenedRecord(sessionID string, seq uint64, observedAt time.Time, fact agent.PullRequestFact) (Record, error) {
	if err := agent.ValidatePullRequestFact(fact); err != nil {
		return Record{}, fmt.Errorf("executionevent: invalid pull request fact: %w", err)
	}
	payload := map[string]any{
		"provider":   fact.Provider,
		"number":     fact.Number,
		"repository": fact.Repository,
		"url":        fact.URL,
		"baseBranch": fact.BaseBranch,
		"headBranch": fact.HeadBranch,
	}
	if fact.Author != "" {
		payload["author"] = fact.Author
	}
	return NewRecord(sessionID, seq, observedAt, "pr.opened", payload)
}

// DigestResult returns a digest-only result reference for terminal evidence.
func DigestResult(status, summary, failure string) string {
	b, _ := MarshalCompact(map[string]string{"status": status, "summary": summary, "failure": failure})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func digestInput(input map[string]any) (string, error) {
	b, err := MarshalCompact(input)
	if err != nil {
		return "", fmt.Errorf("executionevent: digest tool input: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func safeCode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '.' {
			return ""
		}
	}
	return value
}
