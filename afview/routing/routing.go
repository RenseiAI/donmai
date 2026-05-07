// Package routing hosts the canonical renderers for the af /
// rensei `routing` command tree, sourced from afclient.RoutingConfig
// and afclient.RoutingExplainResponse wire types. Lifted from the
// previous rensei-tui resident copy under
// rensei-tui/internal/views/routing per ADR-2026-05-07-daemon-http-
// control-api.md §D3.
//
// Two output paths:
//
//   - RenderShow / RenderExplain — ANSI rendering for TTY users.
//   - PlainShow / PlainExplain — deterministic plain-text rendering used
//     by rensei-smokes integration tests and by `--plain` mode. No ANSI
//     escapes, no emoji. The explain plain-text emits a numbered trace
//     so smoke pins are stable across renderer churn.
//
// Wave 9 / A4 — read-only this wave; the `tail` subcommand from the
// previous rensei-tui placement is intentionally NOT lifted (the daemon
// does not expose a streaming endpoint per ADR D1; consumers should use
// `rensei observability events tail --filter kind=routing-decision`).
package routing

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// ─── ANSI helpers ────────────────────────────────────────────────────────────
// Minimal palette — matches afview/provider for visual consistency.

const (
	ansiReset   = "\033[0m"
	ansiBold    = "\033[1m"
	ansiDim     = "\033[2m"
	ansiGreen   = "\033[32m"
	ansiYellow  = "\033[33m"
	ansiRed     = "\033[31m"
	ansiCyan    = "\033[36m"
	ansiMagenta = "\033[35m"
)

func bold(s string, noColor bool) string {
	if noColor {
		return s
	}
	return ansiBold + s + ansiReset
}

func muted(s string, noColor bool) string {
	if noColor {
		return s
	}
	return ansiDim + s + ansiReset
}

func colHeader(s string, noColor bool) string {
	if noColor {
		return s
	}
	return ansiDim + ansiBold + s + ansiReset
}

func sectionHeader(s string, noColor bool) string {
	if noColor {
		return "=== " + s + " ==="
	}
	return ansiMagenta + ansiBold + s + ansiReset
}

func highlight(s string, noColor bool) string {
	if noColor {
		return s
	}
	return ansiCyan + s + ansiReset
}

func warnStr(s string, noColor bool) string {
	if noColor {
		return s
	}
	return ansiYellow + s + ansiReset
}

func errStr(s string, noColor bool) string {
	if noColor {
		return s
	}
	return ansiRed + s + ansiReset
}

// NoColorEnv returns true when NO_COLOR is set in the environment. Used
// by callers to derive the noColor flag without each importing os.
func NoColorEnv() bool {
	return os.Getenv("NO_COLOR") != ""
}

// ─── ConfigView / ExplainView types ──────────────────────────────────────────

// ConfigView is the public renderer entry-point for the `show` command.
// It wraps an afclient.RoutingConfig so callers can import a typed
// renderer without dragging individual functions through the import
// boundary. Use RenderShow to produce the ANSI form, PlainShow for the
// smoke-pin form.
type ConfigView struct {
	Config afclient.RoutingConfig
}

// ExplainView wraps an afclient.RoutingExplainResponse for the `explain`
// command. Use RenderExplain for ANSI, PlainExplain for smoke pins.
type ExplainView struct {
	Response afclient.RoutingExplainResponse
}

// ─── RenderShow ──────────────────────────────────────────────────────────────

// RenderShow writes the `routing show` output: current Thompson-Sampling
// state across both dimensions, capability filters, weights, and a
// recent decisions table.
func RenderShow(out io.Writer, cfg *afclient.RoutingConfig, noColor bool) error {
	_, _ = fmt.Fprintln(out, sectionHeader("Routing Configuration", noColor))
	_, _ = fmt.Fprintf(out, "  %s %s\n\n",
		muted("Captured:", noColor),
		bold(cfg.CapturedAt.UTC().Format(time.RFC3339), noColor),
	)

	// Scoring weights
	_, _ = fmt.Fprintln(out, bold("Scoring Weights", noColor))
	_, _ = fmt.Fprintf(out, "  cost=%.0f%%  latency=%.0f%%\n\n",
		cfg.Weights.Cost*100,
		cfg.Weights.Latency*100,
	)

	// Capability filters
	if len(cfg.CapabilityFilters) > 0 {
		_, _ = fmt.Fprintln(out, bold("Capability Filters", noColor))
		for _, f := range cfg.CapabilityFilters {
			_, _ = fmt.Fprintf(out, "  %s %s %s\n",
				highlight(f.Field, noColor),
				muted(f.Op, noColor),
				bold(f.Value, noColor),
			)
		}
		_, _ = fmt.Fprintln(out)
	}

	// Sandbox providers — Thompson-Sampling state
	if len(cfg.SandboxProviders) > 0 {
		_, _ = fmt.Fprintln(out, bold("Sandbox Providers (Thompson Sampling)", noColor))
		if err := renderSandboxProviderTable(out, cfg.SandboxProviders, noColor); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out)
	}

	// LLM providers — Thompson-Sampling state
	if len(cfg.LLMProviders) > 0 {
		_, _ = fmt.Fprintln(out, bold("LLM Providers (Thompson Sampling)", noColor))
		if err := renderLLMProviderTable(out, cfg.LLMProviders, noColor); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out)
	}

	// 2D heatmap (LLM rows × sandbox columns)
	if len(cfg.SandboxProviders) > 0 && len(cfg.LLMProviders) > 0 {
		_, _ = fmt.Fprintln(out, bold("2D Routing Heatmap  (cell = mean Thompson score; lower is better)", noColor))
		if err := renderHeatmap(out, cfg, noColor); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out)
	}

	// Recent decisions
	if len(cfg.RecentDecisions) > 0 {
		_, _ = fmt.Fprintln(out, bold("Recent Decisions", noColor))
		if err := renderDecisionsTable(out, cfg.RecentDecisions, noColor); err != nil {
			return err
		}
	}

	return nil
}

func renderSandboxProviderTable(out io.Writer, providers []afclient.SandboxProviderState, noColor bool) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\n",
		colHeader("PROVIDER", noColor),
		colHeader("ALPHA", noColor),
		colHeader("BETA", noColor),
		colHeader("SCORE", noColor),
		colHeader("COST(¢)", noColor),
		colHeader("LATENCY(ms)", noColor),
	)
	for _, p := range providers {
		score := "—"
		if p.RecentScore != nil {
			score = fmt.Sprintf("%.3f", *p.RecentScore)
		}
		cost := "—"
		if p.RecentCostCents != nil {
			cost = fmt.Sprintf("%.2f", *p.RecentCostCents)
		}
		latency := "—"
		if p.RecentLatencyMs != nil {
			latency = fmt.Sprintf("%.0f", *p.RecentLatencyMs)
		}
		_, _ = fmt.Fprintf(w, "  %s\t%.1f\t%.1f\t%s\t%s\t%s\n",
			bold(p.ProviderID, noColor),
			p.Alpha,
			p.Beta,
			highlight(score, noColor),
			cost,
			latency,
		)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush sandbox table: %w", err)
	}
	return nil
}

func renderLLMProviderTable(out io.Writer, providers []afclient.LLMProviderState, noColor bool) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n",
		colHeader("PROVIDER", noColor),
		colHeader("MODEL", noColor),
		colHeader("ALPHA", noColor),
		colHeader("BETA", noColor),
		colHeader("SCORE", noColor),
	)
	for _, p := range providers {
		score := "—"
		if p.RecentScore != nil {
			score = fmt.Sprintf("%.3f", *p.RecentScore)
		}
		model := p.Model
		if model == "" {
			model = muted("—", noColor)
		}
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%.1f\t%.1f\t%s\n",
			bold(p.ProviderID, noColor),
			model,
			p.Alpha,
			p.Beta,
			highlight(score, noColor),
		)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush llm table: %w", err)
	}
	return nil
}

// renderHeatmap renders a 2D grid with LLM rows and sandbox columns.
// Each cell shows the mean Thompson score for that (LLM × sandbox)
// combination derived from recent decisions. Cells with no data show "—".
func renderHeatmap(out io.Writer, cfg *afclient.RoutingConfig, noColor bool) error {
	scoreMap := make(map[string][]float64)
	for _, d := range cfg.RecentDecisions {
		key := d.ChosenLLM + "|" + d.ChosenSandbox
		scoreMap[key] = append(scoreMap[key], d.Score)
	}

	sandboxIDs := make([]string, 0, len(cfg.SandboxProviders))
	for _, sp := range cfg.SandboxProviders {
		sandboxIDs = append(sandboxIDs, sp.ProviderID)
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)

	headerParts := []string{"  " + colHeader("LLM \\ SANDBOX", noColor)}
	for _, sid := range sandboxIDs {
		headerParts = append(headerParts, colHeader(ShortID(sid), noColor))
	}
	_, _ = fmt.Fprintln(w, strings.Join(headerParts, "\t"))

	for _, llm := range cfg.LLMProviders {
		rowParts := []string{"  " + bold(ShortID(llm.ProviderID), noColor)}
		for _, sid := range sandboxIDs {
			key := llm.ProviderID + "|" + sid
			scores := scoreMap[key]
			if len(scores) == 0 {
				rowParts = append(rowParts, muted("—", noColor))
			} else {
				mean := MeanFloat(scores)
				cell := fmt.Sprintf("%.3f", mean)
				rowParts = append(rowParts, scoreColorize(cell, mean, noColor))
			}
		}
		_, _ = fmt.Fprintln(w, strings.Join(rowParts, "\t"))
	}

	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush heatmap: %w", err)
	}
	return nil
}

func scoreColorize(s string, score float64, noColor bool) string {
	if noColor {
		return s
	}
	switch {
	case score < ScoreColorThresholds.Green:
		return ansiGreen + s + ansiReset
	case score < ScoreColorThresholds.Yellow:
		return ansiYellow + s + ansiReset
	default:
		return ansiRed + s + ansiReset
	}
}

// MeanFloat returns the arithmetic mean of vals, or 0 for an empty slice.
func MeanFloat(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

// ShortID truncates a provider ID to at most 16 characters for compact
// display in tables and the heatmap header row.
func ShortID(id string) string {
	const maxLen = 16
	if len(id) <= maxLen {
		return id
	}
	return id[:maxLen-1] + "…"
}

// ShortSession returns a short identifier for a session ID: up to 12 chars.
func ShortSession(id string) string {
	const maxLen = 12
	if len(id) <= maxLen {
		return id
	}
	return id[:maxLen]
}

func renderDecisionsTable(out io.Writer, decisions []afclient.RoutingDecision, noColor bool) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\t%s\n",
		colHeader("SESSION", noColor),
		colHeader("DECIDED", noColor),
		colHeader("SANDBOX", noColor),
		colHeader("LLM", noColor),
		colHeader("SCORE", noColor),
		colHeader("COST(¢)", noColor),
	)
	for _, d := range decisions {
		cost := "—"
		if d.EstimatedCostCents != nil {
			cost = fmt.Sprintf("%.2f", *d.EstimatedCostCents)
		}
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%.3f\t%s\n",
			muted(ShortSession(d.SessionID), noColor),
			d.DecidedAt.UTC().Format("15:04:05"),
			bold(ShortID(d.ChosenSandbox), noColor),
			bold(ShortID(d.ChosenLLM), noColor),
			d.Score,
			cost,
		)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush decisions table: %w", err)
	}
	return nil
}

// ─── RenderExplain ───────────────────────────────────────────────────────────

// RenderExplain writes the full decision trace for `routing explain`.
func RenderExplain(out io.Writer, resp *afclient.RoutingExplainResponse, noColor bool) error {
	_, _ = fmt.Fprintln(out, sectionHeader("Routing Decision Trace", noColor))
	_, _ = fmt.Fprintf(out, "  %s %s\n\n",
		muted("Session:", noColor),
		bold(resp.SessionID, noColor),
	)

	_, _ = fmt.Fprintln(out, bold("Chosen", noColor))
	_, _ = fmt.Fprintf(out, "  %s %s\n",
		muted("Sandbox:", noColor),
		highlight(resp.Decision.ChosenSandbox, noColor),
	)
	_, _ = fmt.Fprintf(out, "  %s %s\n",
		muted("LLM:    ", noColor),
		highlight(resp.Decision.ChosenLLM, noColor),
	)
	_, _ = fmt.Fprintf(out, "  %s %.3f\n",
		muted("Score:  ", noColor),
		resp.Decision.Score,
	)
	if resp.Decision.EstimatedCostCents != nil {
		_, _ = fmt.Fprintf(out, "  %s %.2f¢\n",
			muted("Est. cost:", noColor),
			*resp.Decision.EstimatedCostCents,
		)
	}
	if resp.Decision.EstimatedLatencyMs != nil {
		_, _ = fmt.Fprintf(out, "  %s %.0fms\n",
			muted("Est. latency:", noColor),
			*resp.Decision.EstimatedLatencyMs,
		)
	}
	_, _ = fmt.Fprintln(out)

	// Rejected candidates
	if len(resp.Decision.RejectedCandidates) > 0 {
		_, _ = fmt.Fprintln(out, bold("Rejected Candidates", noColor))
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n",
			colHeader("PROVIDER", noColor),
			colHeader("DIM", noColor),
			colHeader("REASON", noColor),
			colHeader("DETAIL", noColor),
		)
		for _, rc := range resp.Decision.RejectedCandidates {
			detail := rc.Detail
			if detail == "" {
				detail = muted("—", noColor)
			}
			_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n",
				bold(rc.ProviderID, noColor),
				muted(rc.Dimension, noColor),
				warnStr(rc.Reason, noColor),
				detail,
			)
		}
		if err := w.Flush(); err != nil {
			return fmt.Errorf("flush rejected candidates: %w", err)
		}
		_, _ = fmt.Fprintln(out)
	}

	if len(resp.Trace) == 0 {
		_, _ = fmt.Fprintln(out, muted("(no trace steps available)", noColor))
		return nil
	}

	_, _ = fmt.Fprintln(out, bold("Decision Trace", noColor))
	for _, step := range resp.Trace {
		stepHeader := fmt.Sprintf("Step %d — %s [%s]",
			step.Step,
			step.Phase,
			step.Dimension,
		)
		_, _ = fmt.Fprintf(out, "\n  %s\n", bold(stepHeader, noColor))

		if step.Note != "" {
			_, _ = fmt.Fprintf(out, "  %s %s\n",
				muted("Note:", noColor),
				step.Note,
			)
		}

		if len(step.Remaining) > 0 {
			_, _ = fmt.Fprintf(out, "  %s %s\n",
				muted("Remaining:", noColor),
				highlight(strings.Join(step.Remaining, ", "), noColor),
			)
		}

		for _, e := range step.Eliminated {
			detail := ""
			if e.Detail != "" {
				detail = " (" + e.Detail + ")"
			}
			_, _ = fmt.Fprintf(out, "  %s %s — %s%s\n",
				muted("Eliminated:", noColor),
				errStr(e.ProviderID, noColor),
				e.Reason,
				detail,
			)
		}
	}

	return nil
}

// ─── Plain-text fallback (rensei-smokes pin point) ───────────────────────────

// PlainShow writes a deterministic plain-text rendering of the routing
// configuration. No ANSI escapes, no emoji, fixed column ordering. This
// is what rensei-smokes pins against — keep the format stable across
// patches; if the columns must change, bump a version label and fix the
// pin atomically.
func PlainShow(out io.Writer, cfg *afclient.RoutingConfig) error {
	_, _ = fmt.Fprintf(out, "captured: %s\n", cfg.CapturedAt.UTC().Format(time.RFC3339))
	_, _ = fmt.Fprintf(out, "weights: cost=%.2f latency=%.2f\n", cfg.Weights.Cost, cfg.Weights.Latency)

	if len(cfg.CapabilityFilters) > 0 {
		_, _ = fmt.Fprintln(out, "capability-filters:")
		for _, f := range cfg.CapabilityFilters {
			_, _ = fmt.Fprintf(out, "  - field=%s op=%s value=%s\n", f.Field, f.Op, f.Value)
		}
	}

	_, _ = fmt.Fprintln(out, "sandbox-providers:")
	for _, p := range cfg.SandboxProviders {
		_, _ = fmt.Fprintf(out, "  - id=%s alpha=%.2f beta=%.2f selections=%d\n",
			p.ProviderID, p.Alpha, p.Beta, p.SelectionCount)
	}

	_, _ = fmt.Fprintln(out, "llm-providers:")
	for _, p := range cfg.LLMProviders {
		_, _ = fmt.Fprintf(out, "  - id=%s alpha=%.2f beta=%.2f selections=%d\n",
			p.ProviderID, p.Alpha, p.Beta, p.SelectionCount)
	}

	_, _ = fmt.Fprintln(out, "recent-decisions:")
	for _, d := range cfg.RecentDecisions {
		_, _ = fmt.Fprintf(out, "  - session=%s sandbox=%s llm=%s score=%.3f decided=%s\n",
			d.SessionID,
			d.ChosenSandbox,
			d.ChosenLLM,
			d.Score,
			d.DecidedAt.UTC().Format(time.RFC3339),
		)
	}

	return nil
}

// PlainExplain writes a deterministic plain-text rendering of a routing
// explain response. The trace is emitted in numbered order so smoke
// pins can read step N's phase/dimension/remaining/eliminated by line
// position. No ANSI, no emoji.
func PlainExplain(out io.Writer, resp *afclient.RoutingExplainResponse) error {
	_, _ = fmt.Fprintf(out, "session: %s\n", resp.SessionID)
	_, _ = fmt.Fprintf(out, "chosen-sandbox: %s\n", resp.Decision.ChosenSandbox)
	_, _ = fmt.Fprintf(out, "chosen-llm: %s\n", resp.Decision.ChosenLLM)
	_, _ = fmt.Fprintf(out, "score: %.3f\n", resp.Decision.Score)
	if resp.Decision.EstimatedCostCents != nil {
		_, _ = fmt.Fprintf(out, "est-cost-cents: %.2f\n", *resp.Decision.EstimatedCostCents)
	}
	if resp.Decision.EstimatedLatencyMs != nil {
		_, _ = fmt.Fprintf(out, "est-latency-ms: %.0f\n", *resp.Decision.EstimatedLatencyMs)
	}

	if len(resp.Decision.RejectedCandidates) > 0 {
		_, _ = fmt.Fprintln(out, "rejected:")
		for _, rc := range resp.Decision.RejectedCandidates {
			detail := rc.Detail
			if detail == "" {
				detail = "-"
			}
			_, _ = fmt.Fprintf(out, "  - id=%s dim=%s reason=%s detail=%s\n",
				rc.ProviderID, rc.Dimension, rc.Reason, detail,
			)
		}
	}

	if len(resp.Trace) == 0 {
		_, _ = fmt.Fprintln(out, "trace: (empty)")
		return nil
	}
	_, _ = fmt.Fprintln(out, "trace:")
	for _, step := range resp.Trace {
		_, _ = fmt.Fprintf(out, "  step=%d phase=%s dim=%s remaining=[%s]",
			step.Step,
			step.Phase,
			step.Dimension,
			strings.Join(step.Remaining, ","),
		)
		if step.Note != "" {
			_, _ = fmt.Fprintf(out, " note=%q", step.Note)
		}
		_, _ = fmt.Fprintln(out)
		for _, e := range step.Eliminated {
			detail := e.Detail
			if detail == "" {
				detail = "-"
			}
			_, _ = fmt.Fprintf(out, "    eliminated=%s reason=%s detail=%s\n",
				e.ProviderID, e.Reason, detail,
			)
		}
	}
	return nil
}

// ─── Thresholds exported for tests ───────────────────────────────────────────

// ScoreColorThresholds are the score thresholds used for color-coding
// cells in the 2D heatmap. Exported so tests can assert on the boundary
// values; lower bounds are exclusive (score < Green => green; score <
// Yellow but ≥ Green => yellow; score ≥ Yellow => red).
var ScoreColorThresholds = struct {
	Green  float64
	Yellow float64
}{
	Green:  0.33,
	Yellow: 0.67,
}
