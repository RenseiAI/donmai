package codex

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

func countIgnored(state *mapperState, method string) {
	if state.ignoredCounts == nil {
		state.ignoredCounts = make(map[string]uint64)
	}
	state.ignoredCounts[method]++
	slog.Debug("Codex notification ignored", "method", method, "count", state.ignoredCounts[method])
}

type codexTokenUsage struct {
	Input     int64 `json:"inputTokens"`
	Output    int64 `json:"outputTokens"`
	Cached    int64 `json:"cachedInputTokens"`
	Reasoning int64 `json:"reasoningOutputTokens"`
	Total     int64 `json:"totalTokens"`
}

// Missing measurements are unknown, not reported zeros. The schema requires
// these four counters in each breakdown; reject partial or malformed snapshots.
func (u *codexTokenUsage) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Input     *int64 `json:"inputTokens"`
		Output    *int64 `json:"outputTokens"`
		Cached    *int64 `json:"cachedInputTokens"`
		Total     *int64 `json:"totalTokens"`
		Reasoning int64  `json:"reasoningOutputTokens"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return fmt.Errorf("decode Codex usage: %w", err)
	}
	if wire.Input == nil || wire.Output == nil || wire.Cached == nil || wire.Total == nil {
		return fmt.Errorf("codex usage is missing required counters")
	}
	*u = codexTokenUsage{Input: *wire.Input, Output: *wire.Output, Cached: *wire.Cached, Total: *wire.Total, Reasoning: wire.Reasoning}
	return nil
}

// Notifications carry thread lifetime totals. Seed the baseline from total-last
// on attachment so a resumed thread does not bill its historical conversation.
// Later snapshots contribute only their increase; repeated snapshots are silent.
func mapTokenUsageUpdated(params json.RawMessage, state *mapperState, raw any) []agent.Event {
	var p struct {
		TokenUsage *struct {
			Total  *codexTokenUsage `json:"total"`
			Last   *codexTokenUsage `json:"last"`
			Window *int64           `json:"modelContextWindow"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(params, &p) != nil || p.TokenUsage == nil || p.TokenUsage.Total == nil || p.TokenUsage.Last == nil {
		return nil
	}
	u := p.TokenUsage
	for _, v := range []*codexTokenUsage{u.Total, u.Last} {
		if v.Input < 0 || v.Output < 0 || v.Cached < 0 || v.Cached > v.Input || v.Reasoning < 0 || v.Total < 0 {
			return nil
		}
	}
	if state.usageTotal == nil {
		state.usageTotal = &codexTokenUsage{Input: max(0, u.Total.Input-u.Last.Input), Output: max(0, u.Total.Output-u.Last.Output), Cached: max(0, u.Total.Cached-u.Last.Cached), Reasoning: max(0, u.Total.Reasoning-u.Last.Reasoning), Total: max(0, u.Total.Total-u.Last.Total)}
	}
	prev := state.usageTotal
	// An older snapshot must not rewind the baseline or charge again later.
	if u.Total.Input < prev.Input || u.Total.Output < prev.Output || u.Total.Cached < prev.Cached {
		return nil
	}
	in, out, cached := u.Total.Input-prev.Input, u.Total.Output-prev.Output, u.Total.Cached-prev.Cached
	fresh := max(0, in-cached)
	state.usageTotal = u.Total
	state.usageThisTurn = true
	state.totalInputTokens += fresh
	state.totalOutputTokens += out
	state.totalCachedInputTok += cached
	state.totalCostUSD += calculateCostUSD(fresh, cached, out, state.model)
	var events []agent.Event
	if in != 0 || out != 0 || cached != 0 {
		events = append(events, agent.LlmCallEvent{System: "openai", Model: state.model, InputTokens: fresh, OutputTokens: out, CachedInputTokens: cached, ReasoningTokens: max(0, u.Total.Reasoning-prev.Reasoning), UsageSource: agent.LlmUsageProvider})
	}
	notice := agent.ContextUsageEvent{Harness: "codex", Source: "app-server", NoticeKind: agent.ContextUsageThreshold, InputTokens: &u.Total.Input, OutputTokens: &u.Total.Output, Raw: raw}
	if u.Window != nil && *u.Window > 0 {
		notice.ContextWindowSize = u.Window
		pct := min(100, float64(u.Last.Total)/float64(*u.Window)*100)
		notice.UsedPercent = &pct
	}
	return append(events, notice)
}

func mapWarning(method string, params json.RawMessage, state *mapperState, raw any) []agent.Event {
	var p struct {
		From     string          `json:"fromModel"`
		To       string          `json:"toModel"`
		Reason   string          `json:"reason"`
		Model    string          `json:"model"`
		Reasons  []string        `json:"reasons"`
		Metadata json.RawMessage `json:"metadata"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil
	}
	var subtype, message string
	switch method {
	case "model/rerouted":
		subtype, message = "codex_model_rerouted", fmt.Sprintf("Codex model rerouted from %s to %s: %s", p.From, p.To, p.Reason)
		if p.To != "" {
			state.model = p.To
		}
	case "turn/moderationMetadata":
		subtype, message = "codex_moderation", "Codex turn moderation: "+string(p.Metadata)
	case "model/safetyBuffering/updated":
		subtype, message = "codex_safety_buffering", "Codex safety buffering for "+p.Model+": "+strings.Join(p.Reasons, ", ")
	}
	return []agent.Event{agent.SystemEvent{Subtype: subtype, Message: message, Raw: raw}}
}

type reasoningBuffer struct{ summary, text strings.Builder }

// Preserve the full thought while delta notifications retain their content-free
// liveness semantics. Completion prefers the native summary; the buffer is a
// fallback for versions that omit it. Summary and private text are alternatives.
func bufferReasoning(method string, params json.RawMessage, state *mapperState) {
	var p struct {
		ID    string `json:"itemId"`
		Delta string `json:"delta"`
	}
	if json.Unmarshal(params, &p) != nil || p.ID == "" {
		return
	}
	if state.reasoning == nil {
		state.reasoning = make(map[string]*reasoningBuffer)
	}
	b := state.reasoning[p.ID]
	if b == nil {
		b = &reasoningBuffer{}
		state.reasoning[p.ID] = b
	}
	switch method {
	case "item/reasoning/textDelta":
		b.text.WriteString(p.Delta)
	case "item/reasoning/summaryTextDelta":
		b.summary.WriteString(p.Delta)
	case "item/reasoning/summaryPartAdded":
		if b.summary.Len() > 0 {
			b.summary.WriteString("\n")
		}
	}
}

func finishReasoning(state *mapperState, id string, raw any) []agent.Event {
	b := state.reasoning[id]
	delete(state.reasoning, id)
	if b == nil {
		return nil
	}
	text := b.summary.String()
	if strings.TrimSpace(text) == "" {
		text = b.text.String()
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return []agent.Event{agent.SystemEvent{Subtype: "reasoning", Message: text, Raw: raw}}
}

func flushReasoning(state *mapperState, raw any) []agent.Event {
	ids := make([]string, 0, len(state.reasoning))
	for id := range state.reasoning {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var events []agent.Event
	for _, id := range ids {
		events = append(events, finishReasoning(state, id, raw)...)
	}
	return events
}

func reasoningText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []string
	if json.Unmarshal(raw, &parts) == nil {
		return strings.Join(parts, "\n")
	}
	return ""
}

// Diagnostics are observations, not terminal ErrorEvents or provider-error
// signals. Only turn/completed decides the turn outcome, even without a retry.
func mapDiagnostic(method string, params json.RawMessage) []agent.Event {
	var p struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil
	}
	message := p.Message
	if method == "error" {
		message = p.Error.Message
	}
	message = strings.Map(func(r rune) rune {
		if (r < 32 && r != '\n' && r != '\t') || (r >= 127 && r <= 159) {
			return -1
		}
		return r
	}, stripANSI(message))
	message = strings.TrimSpace(redactAppServerStderr(message))
	if message == "" {
		message = "Codex " + method
	}
	// Do not retain raw params: they may contain unredacted diagnostic text.
	return []agent.Event{agent.SystemEvent{Subtype: "codex_" + method, Message: message}}
}
