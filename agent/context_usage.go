package agent

import (
	"encoding/json"
	"fmt"
	"math"
)

// This file declares the typed context/compaction notice: the harness-level
// observation a session emits when its harness compacts the conversation or
// crosses a context-window threshold. It is the runner-facing half of the
// usage-series feed — the platform appends the delivered session event to
// its usage series — and it is deliberately SEPARATE from LlmCallEvent
// (per-call model spend) and UsageEvent (subscription quota windows).
//
// # Why pointers, not zeros
//
// Every measurement rides a pointer so "unknown" is ABSENT, never zero. A
// harness that reports only a percentage must not fabricate token counts,
// and a harness that reports only token counts must not fabricate a window
// size: a zero on the wire reads as "measured zero" downstream, which the
// usage series would store as a real data point. Omit what the harness did
// not report; the parsers below enforce this at construction.

// ContextUsageKind names what the notice observes: a compaction the harness
// performed, or a context-window threshold the session crossed.
type ContextUsageKind string

const (
	// ContextUsageCompaction marks a notice for a compaction the harness
	// performed (history summarized or truncated to fit the window).
	ContextUsageCompaction ContextUsageKind = "compaction"
	// ContextUsageThreshold marks a notice for a context-window threshold
	// the session crossed (a usage percentage passing a reporting bound).
	ContextUsageThreshold ContextUsageKind = "threshold"
)

// ContextUsageSource names the harness-native source a notice was read
// from. One value per harness with a native context signal; a harness with
// no such source reports none and emits no notice.
const (
	// ContextUsageSourceStatuslineSidecar is the Claude statusline sidecar
	// JSON (used_percentage, total_input_tokens, total_output_tokens,
	// context_window_size).
	ContextUsageSourceStatuslineSidecar = "statusline-sidecar"
	// ContextUsageSourceRolloutTokenCount is a token_count event in the
	// Codex rollout JSONL.
	//nolint:gosec // G101: source identifier containing "token", not a credential.
	ContextUsageSourceRolloutTokenCount = "rollout-token-count"
	// ContextUsageSourceRPCUsage is the pi RPC usage object, when the
	// session's RPC stream carries one.
	ContextUsageSourceRPCUsage = "rpc-usage"
	// ContextUsageSourceMuseTokenUsage is the muse session/tokenUsage
	// cumulative block plus session/contextUsage window pressure.
	ContextUsageSourceMuseTokenUsage = "muse-token-usage"
)

// ClaudeLaunchContextFileName is the session file the runner stages beside
// the sidecar and the compact hook re-injects. Shared so both sides agree.
const ClaudeLaunchContextFileName = "launch-context.md"

// ClaudeCompactionLogFileName is the session file the compact hook appends
// one line per compaction. The watcher counts new lines, so a later
// statusline rewrite cannot erase an unread compaction. Shared so the hook
// (provider) and the watcher (runner) agree on the sibling path.
const ClaudeCompactionLogFileName = "compactions.log"

// ContextUsageEvent is the typed harness notice for a compaction or a
// context-window threshold crossing. It rides the normal agent.Event stream
// so the runner's activity sink, execution-event journal, and JSONL audit
// trail all observe it through the existing seams — no new transport.
//
// All measurements are pointers: nil means the harness did not report that
// measurement, and consumers must treat it as unknown (omitted on every
// wire shape), never as zero.
type ContextUsageEvent struct {
	// Harness names the harness that observed the signal (for example
	// "claude-code", "codex", "pi"). It is the manifest HarnessName, not
	// the legacy ProviderName.
	Harness string `json:"harness,omitempty"`

	// Source names the harness-native source the notice was read from
	// (one of the ContextUsageSource* constants). Empty when the producer
	// synthesized the notice without a native source.
	Source string `json:"source,omitempty"`

	// NoticeKind is compaction or threshold. It serializes as "noticeKind"
	// because the event envelope reserves "kind" for the EventKind
	// discriminant (see MarshalEvent).
	NoticeKind ContextUsageKind `json:"noticeKind"`

	// UsedPercent is the consumed share of the context window (0-100),
	// when the harness reports one. Nil when unknown — never a
	// fabricated zero.
	UsedPercent *float64 `json:"usedPercent,omitempty"`

	// InputTokens is the total input tokens the harness reports for the
	// session, when it reports one. Nil when unknown.
	InputTokens *int64 `json:"inputTokens,omitempty"`

	// OutputTokens is the total output tokens the harness reports for
	// the session, when it reports one. Nil when unknown.
	OutputTokens *int64 `json:"outputTokens,omitempty"`

	// ContextWindowSize is the context window size in tokens, when the
	// harness reports one. Nil when unknown.
	ContextWindowSize *int64 `json:"contextWindowSize,omitempty"`

	// Raw is the provider-native payload the notice was read from, opaque
	// to consumers.
	Raw any `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (ContextUsageEvent) Kind() EventKind { return EventContextUsage }
func (ContextUsageEvent) isAgentEvent()   {}

// HasMeasurements reports whether the notice carries at least one
// measurement. A notice with none is still a valid compaction marker (the
// harness compacted but reported no counts); callers that need a series
// data point filter on this.
func (e ContextUsageEvent) HasMeasurements() bool {
	return e.UsedPercent != nil || e.InputTokens != nil ||
		e.OutputTokens != nil || e.ContextWindowSize != nil
}

// ParseClaudeSidecarNotice reads one statusline sidecar JSON document and
// returns the typed context notice it carries. Harness and source are
// stamped by the caller (the harness identity is runner-owned, not parsed
// out of the file). Kind is compaction when the sidecar marks a compaction,
// else threshold.
//
// A document that carries no measurement at all still yields a notice when
// it marks a compaction (the marker itself is the signal); otherwise it
// returns (nil, nil) — nothing to report, not an error. Malformed JSON is
// an error; a well-formed document with impossible counts (negative tokens,
// a non-positive window) is an error rather than a fabricated reading.
//
// Numbers decode tolerantly: whole-number floats (1e5) are accepted for
// integer fields, matching what a shell-rendered sidecar emits.
func ParseClaudeSidecarNotice(raw []byte, harness string) (*ContextUsageEvent, error) {
	var doc struct {
		UsedPercentage    *json.Number `json:"used_percentage"`
		TotalInputTokens  *json.Number `json:"total_input_tokens"`
		TotalOutputTokens *json.Number `json:"total_output_tokens"`
		ContextWindowSize *json.Number `json:"context_window_size"`
		Compacted         *bool        `json:"compacted"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("agent: decode statusline sidecar: %w", err)
	}
	out := &ContextUsageEvent{
		Harness:    harness,
		Source:     ContextUsageSourceStatuslineSidecar,
		NoticeKind: ContextUsageThreshold,
		Raw:        string(raw),
	}
	if doc.UsedPercentage != nil {
		f, err := doc.UsedPercentage.Float64()
		if err != nil {
			return nil, fmt.Errorf("agent: sidecar used_percentage: %w", err)
		}
		if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 100 {
			return nil, fmt.Errorf("agent: sidecar used_percentage %v is not a 0-100 percent", f)
		}
		out.UsedPercent = &f
	}
	if doc.TotalInputTokens != nil {
		n, err := sidecarInt64(*doc.TotalInputTokens, "total_input_tokens")
		if err != nil {
			return nil, err
		}
		out.InputTokens = &n
	}
	if doc.TotalOutputTokens != nil {
		n, err := sidecarInt64(*doc.TotalOutputTokens, "total_output_tokens")
		if err != nil {
			return nil, err
		}
		out.OutputTokens = &n
	}
	if doc.ContextWindowSize != nil {
		n, err := sidecarInt64(*doc.ContextWindowSize, "context_window_size")
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			return nil, fmt.Errorf("agent: sidecar context_window_size %d is not positive", n)
		}
		out.ContextWindowSize = &n
	}
	if doc.Compacted != nil && *doc.Compacted {
		out.NoticeKind = ContextUsageCompaction
	}
	if !out.HasMeasurements() && out.NoticeKind != ContextUsageCompaction {
		return nil, nil
	}
	return out, nil
}

func sidecarInt64(n json.Number, field string) (int64, error) {
	f, err := n.Float64()
	if err != nil {
		return 0, fmt.Errorf("agent: sidecar %s: %w", field, err)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f != math.Trunc(f) {
		return 0, fmt.Errorf("agent: sidecar %s %v is not a non-negative integer", field, f)
	}
	if f > float64(math.MaxInt64) {
		return 0, fmt.Errorf("agent: sidecar %s %v overflows int64", field, f)
	}
	return int64(f), nil
}

// codexTokenCountLine is the narrow slice of the rollout JSONL schema the
// context notice reads: an event_msg envelope whose payload carries a
// token_count event with the thread's cumulative token totals and the
// model's context window (verified against live rollout files: 1044
// token_count events, all in this shape). A top-level "compacted" line
// marks a compaction the harness performed. Every other line shape is
// skipped by the caller before decoding reaches here; every count is
// optional so a partial event stays partial, never zero-filled.
type codexTokenCountLine struct {
	Type    string `json:"type"`
	Payload *struct {
		// TokenCountType is the nested event_msg discriminant: only
		// "token_count" carries this signal.
		TokenCountType string `json:"type"`
		Info           *struct {
			TotalTokenUsage *struct {
				InputTokens  *int64 `json:"input_tokens"`
				OutputTokens *int64 `json:"output_tokens"`
				TotalTokens  *int64 `json:"total_tokens"`
			} `json:"total_token_usage"`
			// LastTokenUsage is the most recent model call's usage: its
			// total is what currently occupies the context window.
			// total_token_usage is the thread's running sum across every
			// call (it passes the window within a few turns), so it is
			// never a window share.
			LastTokenUsage *struct {
				TotalTokens *int64 `json:"total_tokens"`
			} `json:"last_token_usage"`
			ModelContextWindow *int64 `json:"model_context_window"`
		} `json:"info"`
	} `json:"payload"`
}

// ParseCodexTokenCountNotice reads one rollout JSONL line and returns the
// typed context notice when the line is an event_msg/token_count event. A
// top-level "compacted" line returns a compaction notice (the marker
// itself is the signal). Every other line returns (nil, nil) — not this
// signal, not an error. Negative counts are an error, never a reading.
//
// The context-window share is DERIVED, not parsed: the rollout carries the
// last call's usage and the model window but no percentage, so UsedPercent
// is 100*last_token_usage.total_tokens/window, the same occupancy the
// harness's own context indicator reads. It is never derived from
// total_token_usage: that is the thread's cumulative sum across every
// call, which passes the window within a few turns and would report a
// full window for a session that is not close to one. Either input
// missing means no notice (a threshold notice with no percentage carries
// only token counts the per-turn usage events already meter). The
// cumulative input/output totals still ride the notice as measurements.
func ParseCodexTokenCountNotice(line []byte, harness string) (*ContextUsageEvent, error) {
	var decoded codexTokenCountLine
	if err := json.Unmarshal(line, &decoded); err != nil {
		return nil, nil
	}
	if decoded.Type == "compacted" {
		return &ContextUsageEvent{
			Harness:    harness,
			Source:     ContextUsageSourceRolloutTokenCount,
			NoticeKind: ContextUsageCompaction,
			Raw:        string(line),
		}, nil
	}
	if decoded.Type != "event_msg" || decoded.Payload == nil ||
		decoded.Payload.TokenCountType != "token_count" || decoded.Payload.Info == nil {
		return nil, nil
	}
	info := decoded.Payload.Info
	out := &ContextUsageEvent{
		Harness:    harness,
		Source:     ContextUsageSourceRolloutTokenCount,
		NoticeKind: ContextUsageThreshold,
		Raw:        string(line),
	}
	if totals := info.TotalTokenUsage; totals != nil {
		if totals.InputTokens != nil {
			if *totals.InputTokens < 0 {
				return nil, fmt.Errorf("agent: rollout token_count input_tokens %d is negative", *totals.InputTokens)
			}
			if *totals.InputTokens > 0 {
				out.InputTokens = totals.InputTokens
			}
		}
		if totals.OutputTokens != nil {
			if *totals.OutputTokens < 0 {
				return nil, fmt.Errorf("agent: rollout token_count output_tokens %d is negative", *totals.OutputTokens)
			}
			if *totals.OutputTokens > 0 {
				out.OutputTokens = totals.OutputTokens
			}
		}
		if totals.TotalTokens != nil && *totals.TotalTokens < 0 {
			return nil, fmt.Errorf("agent: rollout token_count total_tokens %d is negative", *totals.TotalTokens)
		}
	}
	if info.ModelContextWindow != nil {
		if *info.ModelContextWindow <= 0 {
			return nil, fmt.Errorf("agent: rollout token_count model_context_window %d is not positive", *info.ModelContextWindow)
		}
		window := *info.ModelContextWindow
		out.ContextWindowSize = &window
		if last := info.LastTokenUsage; last != nil && last.TotalTokens != nil {
			if *last.TotalTokens < 0 {
				return nil, fmt.Errorf("agent: rollout token_count last total_tokens %d is negative", *last.TotalTokens)
			}
			pct := 100 * float64(*last.TotalTokens) / float64(window)
			if math.IsNaN(pct) || math.IsInf(pct, 0) || pct < 0 {
				return nil, fmt.Errorf("agent: rollout token_count last total %d over window %d is not a share", *last.TotalTokens, window)
			}
			if pct > 100 {
				pct = 100
			}
			out.UsedPercent = &pct
		}
	}
	if !out.HasMeasurements() {
		return nil, nil
	}
	if out.NoticeKind == ContextUsageThreshold && out.UsedPercent == nil {
		// A threshold notice with no percentage carries only token counts
		// the per-turn usage events already meter: nothing new to report.
		// The watcher applies the same rule (classify); the parser holds
		// it too so a direct caller never emits a percent-less threshold.
		return nil, nil
	}
	return out, nil
}

// PiRPCUsage is the narrow slice of the pi RPC usage object the context
// notice reads. Every field is optional: the RPC stream reports what the
// turn exposed, and absent fields stay unknown, never zero.
type PiRPCUsage struct {
	InputTokens       *int64   `json:"inputTokens"`
	Input             *int64   `json:"input"`
	OutputTokens      *int64   `json:"outputTokens"`
	Output            *int64   `json:"output"`
	ContextWindowSize *int64   `json:"contextWindowSize"`
	ContextWindow     *int64   `json:"contextWindow"`
	UsedPercentage    *float64 `json:"usedPercentage"`
	UsedPercent       *float64 `json:"usedPercent"`
	Compacted         *bool    `json:"compacted"`
	Compaction        *bool    `json:"compaction"`
}

// ParsePiRPCUsageNotice converts one pi RPC usage object into the typed
// context notice. A nil usage, or one with no measurement, returns
// (nil, nil) — nothing reported, not an error. Negative counts and a
// non-positive window are errors, never readings.
func ParsePiRPCUsageNotice(usage *PiRPCUsage, harness string) (*ContextUsageEvent, error) {
	if usage == nil {
		return nil, nil
	}
	out := &ContextUsageEvent{
		Harness:    harness,
		Source:     ContextUsageSourceRPCUsage,
		NoticeKind: ContextUsageThreshold,
	}
	if usage.UsedPercentage != nil {
		f := *usage.UsedPercentage
		if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 100 {
			return nil, fmt.Errorf("agent: pi rpc usage usedPercentage %v is not a 0-100 percent", f)
		}
		out.UsedPercent = &f
	} else if usage.UsedPercent != nil {
		f := *usage.UsedPercent
		if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 100 {
			return nil, fmt.Errorf("agent: pi rpc usage usedPercent %v is not a 0-100 percent", f)
		}
		out.UsedPercent = &f
	}
	if usage.InputTokens != nil {
		if *usage.InputTokens < 0 {
			return nil, fmt.Errorf("agent: pi rpc usage inputTokens %d is negative", *usage.InputTokens)
		}
		out.InputTokens = usage.InputTokens
	} else if usage.Input != nil {
		if *usage.Input < 0 {
			return nil, fmt.Errorf("agent: pi rpc usage input %d is negative", *usage.Input)
		}
		out.InputTokens = usage.Input
	}
	if usage.OutputTokens != nil {
		if *usage.OutputTokens < 0 {
			return nil, fmt.Errorf("agent: pi rpc usage outputTokens %d is negative", *usage.OutputTokens)
		}
		out.OutputTokens = usage.OutputTokens
	} else if usage.Output != nil {
		if *usage.Output < 0 {
			return nil, fmt.Errorf("agent: pi rpc usage output %d is negative", *usage.Output)
		}
		out.OutputTokens = usage.Output
	}
	if usage.ContextWindowSize != nil {
		if *usage.ContextWindowSize <= 0 {
			return nil, fmt.Errorf("agent: pi rpc usage contextWindowSize %d is not positive", *usage.ContextWindowSize)
		}
		out.ContextWindowSize = usage.ContextWindowSize
	} else if usage.ContextWindow != nil {
		if *usage.ContextWindow <= 0 {
			return nil, fmt.Errorf("agent: pi rpc usage contextWindow %d is not positive", *usage.ContextWindow)
		}
		out.ContextWindowSize = usage.ContextWindow
	}
	if (usage.Compacted != nil && *usage.Compacted) || (usage.Compaction != nil && *usage.Compaction) {
		out.NoticeKind = ContextUsageCompaction
	}
	if !out.HasMeasurements() && out.NoticeKind != ContextUsageCompaction {
		return nil, nil
	}
	return out, nil
}
