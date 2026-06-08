package codeintel

// arch_assess_full.go — the full assess-against-baseline pipeline (Layer 2).
//
// Port of donmai-libraries/packages/architectural-intelligence/src/drift.ts
// assessChange(). This is the piece that turns the orphaned LaneAdapter
// (arch_drift.go, lane-backed LLM seam) + the SQLite store (arch_store.go) into
// a live `donmai arch assess` capability:
//
//	1. Read the change's diff observations (ReadDiffObservations on REAL content).
//	2. Query the established baseline (patterns/conventions/decisions) for scope.
//	3. If no baseline → clean report (cannot detect deviations).
//	4. Run the deviation-detection prompt through the ModelAdapter (the lane).
//	5. Materialize each Deviation as a 'deviation' node via store.Contribute,
//	   building Deviation + reinforced[] alongside.
//	6. Evaluate the gate against LLM-produced severity.
//	7. Return a FullDriftReport tagged "mode":"native".
//
// THE MANDATE (arch_drift.go): the LLM call is the lane (agent.Complete) — never
// a direct provider SDK. This pipeline consumes a ModelAdapter, so the caller
// (afcli/arch.go) injects a LaneAdapter and the strictness/cost posture stays
// config-resolved.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// FullDriftReport is the JSON-serialisable result of the full assess pipeline.
// Top-level fields mirror the TS DriftReport; Observations + Mode are
// donmai-native extensions (the TS DriftReport omits them).
type FullDriftReport struct {
	Change           ChangeRefJSON     `json:"change"`
	Observations     []DiffObservation `json:"observations"`
	Deviations       []ReportDeviation `json:"deviations"`
	Reinforced       []ReinforcedRef   `json:"reinforced"`
	HasCriticalDrift bool              `json:"hasCriticalDrift"`
	Gated            bool              `json:"gated"`
	Summary          string            `json:"summary"`
	AssessedAt       string            `json:"assessedAt"`
	// Mode is "native" for the full LLM-backed pipeline (vs "native-diff-only"
	// when no baseline / no lane is available).
	Mode string `json:"mode"`
}

// ReportDeviation is a deviation in the report output. Its JSON matches the TS
// Deviation shape (node form), distinct from the lane's verdict Deviation.
type ReportDeviation struct {
	Title        string         `json:"title"`
	Description  string         `json:"description"`
	DeviatesFrom DeviatesFrom   `json:"deviatesFrom"`
	IntroducedBy *ArchChangeRef `json:"introducedBy,omitempty"`
	Status       string         `json:"status"`   // pending
	Severity     string         `json:"severity"` // high | medium | low
}

// ReinforcedRef names a baseline node the change aligns WITH (referenced in the
// change but not flagged as a deviation). Mirrors DriftReport.reinforced.
type ReinforcedRef struct {
	Kind         string `json:"kind"`
	PatternID    string `json:"patternId,omitempty"`
	ConventionID string `json:"conventionId,omitempty"`
}

// assessFullInput bundles the resolved inputs for the full pipeline so the
// Runner method and the testable core share one shape.
type assessFullInput struct {
	diff       PrDiff
	scope      ArchScope
	gatePolicy string
}

// ArchAssessFull runs the full assess-against-baseline pipeline. It opens the
// store at dbPath (DefaultArchDBPath when empty), reads the diff observations,
// queries the baseline, runs the lane, materializes deviations, and returns a
// FullDriftReport. A nil adapter is a programming error.
//
// The store is opened and closed within this call; the caller does not manage
// its lifecycle. Diff observations are NOT contributed to the store here — only
// the LLM-detected deviations are persisted (mirroring assessChange, which
// writes back only new deviation nodes).
func (r *Runner) ArchAssessFull(
	ctx context.Context,
	adapter ModelAdapter,
	diff PrDiff,
	scope ArchScope,
	gatePolicy, dbPath string,
) (FullDriftReport, error) {
	if adapter == nil {
		return FullDriftReport{}, fmt.Errorf("arch assess full: nil ModelAdapter")
	}

	store, err := OpenArchStore(dbPath)
	if err != nil {
		return FullDriftReport{}, fmt.Errorf("arch assess full: open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	return assessFull(ctx, store, adapter, assessFullInput{
		diff:       diff,
		scope:      scope,
		gatePolicy: gatePolicy,
	})
}

// assessFull is the testable core. It takes an already-open store + an adapter
// so tests inject an in-memory store and a fake adapter.
func assessFull(
	ctx context.Context,
	store *ArchStore,
	adapter ModelAdapter,
	in assessFullInput,
) (FullDriftReport, error) {
	scope := in.scope
	if scope.Level == "" {
		scope.Level = "project"
	}
	assessedAt := time.Now().UTC()

	// Step 1: extract change observations from the (real) PR diff.
	changeObs := ReadDiffObservations(in.diff, scope.Level)

	changeRef := changeRefFromDiff(in.diff)

	// Step 2: query the established baseline.
	view, err := store.Query(ArchQuerySpec{WorkType: "qa", Scope: scope})
	if err != nil {
		return FullDriftReport{}, fmt.Errorf("arch assess full: query baseline: %w", err)
	}

	baselineCount := len(view.Patterns) + len(view.Conventions) + len(view.Decisions)

	// Step 3: no baseline → cannot detect deviations. Return a clean report.
	// Mode stays "native" (full pipeline was selected) but no LLM call is made:
	// there is nothing to compare against.
	if baselineCount == 0 {
		return FullDriftReport{
			Change:       changeRef,
			Observations: changeObs,
			Deviations:   []ReportDeviation{},
			Reinforced:   []ReinforcedRef{},
			Gated:        false,
			Summary: "No established architectural baseline found for this scope. " +
				"Contribute patterns, conventions, and decisions to enable drift detection.",
			AssessedAt: assessedAt.Format(time.RFC3339),
			Mode:       "native",
		}, nil
	}

	// Step 4: run the deviation-detection prompt through the lane.
	conventions := baselineSummary(view)
	assessment, err := adapter.AssessChange(ctx, AssessChangeRequest{
		Change:       describeChange(in.diff),
		Observations: changeObs,
		Conventions:  conventions,
	})
	if err != nil {
		return FullDriftReport{}, fmt.Errorf("arch assess full: model adapter: %w", err)
	}

	// A soft miss (SchemaOK=false) yields no deviations — degrade to a clean
	// report rather than erroring (the validate-repair-drop posture).
	deviations := make([]ReportDeviation, 0, len(assessment.Deviations))
	deviatedTitles := make(map[string]struct{})

	// Step 5: materialize each deviation as a 'deviation' node + collect report.
	for _, d := range assessment.Deviations {
		severity := laneSeverityToNode(d.Severity)
		df := resolveDeviatesFrom(d, view)

		rep := ReportDeviation{
			Title:        firstNonEmpty(d.Observation, "Untitled deviation"),
			Description:  d.Rationale,
			DeviatesFrom: df,
			IntroducedBy: changeRefToArch(in.diff),
			Status:       "pending",
			Severity:     severity,
		}
		deviations = append(deviations, rep)
		deviatedTitles[d.Observation] = struct{}{}

		// Persist via Contribute — materializes a deviation NODE in the store.
		if cErr := store.Contribute(ArchObservation{
			Kind:    "deviation",
			Payload: deviationPayload(rep),
			Source: ObservationSource{
				ChangeRef:   changeRefToArch(in.diff),
				ExtractorID: "arch-drift-lane",
			},
			Confidence: 0.95, // capped, mirrors assessChange Math.min(confidence, 0.95)
			Scope:      scope,
		}); cErr != nil {
			return FullDriftReport{}, fmt.Errorf("arch assess full: contribute deviation: %w", cErr)
		}
	}

	// Step 6: reinforced — baseline patterns/conventions NOT flagged as a
	// deviation are reinforced (the change aligns with them). We key on the
	// baseline node TITLE since the lane references observations by title, not id.
	var reinforced []ReinforcedRef
	for _, p := range view.Patterns {
		if _, hit := deviatedTitles[p.Title]; !hit {
			reinforced = append(reinforced, ReinforcedRef{Kind: "pattern", PatternID: p.ID})
		}
	}
	for _, c := range view.Conventions {
		if _, hit := deviatedTitles[c.Title]; !hit {
			reinforced = append(reinforced, ReinforcedRef{Kind: "convention", ConventionID: c.ID})
		}
	}
	if reinforced == nil {
		reinforced = []ReinforcedRef{}
	}

	// Step 7: gate + summary against LLM-produced severity.
	hasCritical := assessment.HasCriticalDrift
	gated := evaluateGateNodes(deviations, in.gatePolicy)
	summary := buildFullSummary(deviations, hasCritical, gated, in.gatePolicy)

	return FullDriftReport{
		Change:           changeRef,
		Observations:     changeObs,
		Deviations:       deviations,
		Reinforced:       reinforced,
		HasCriticalDrift: hasCritical,
		Gated:            gated,
		Summary:          summary,
		AssessedAt:       assessedAt.Format(time.RFC3339),
		Mode:             "native",
	}, nil
}

// ── Gate against node-severity (high|medium|low) ──────────────────────────────

// evaluateGateNodes mirrors evaluateGate in drift.ts: it gates against the
// LLM-produced node severity, NOT the confidence-proxy EvaluateGate uses for the
// diff-only path.
//
//	none              → never gate
//	zero-deviations   → gate on any deviation
//	no-severity-high  → gate on any high-severity deviation (default)
//	max:N             → gate when deviation count > N
func evaluateGateNodes(deviations []ReportDeviation, policy string) bool {
	switch policy {
	case "none":
		return false
	case "zero-deviations":
		return len(deviations) > 0
	case "no-severity-high", "":
		for _, d := range deviations {
			if d.Severity == "high" {
				return true
			}
		}
		return false
	default:
		if strings.HasPrefix(policy, "max:") {
			var n int
			if _, err := fmt.Sscanf(policy[4:], "%d", &n); err == nil && n >= 0 {
				return len(deviations) > n
			}
		}
		return false
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// baselineSummary renders the baseline patterns/conventions/decisions into the
// flat convention strings the lane prompt consumes (renderAssessPrompt bullets
// them). Mirrors the BaselineEntry[] construction in drift.ts step 3.
func baselineSummary(view ArchView) []string {
	var out []string
	for _, p := range view.Patterns {
		out = append(out, fmt.Sprintf("[pattern:%s] %s — %s", p.ID, p.Title, p.Description))
	}
	for _, c := range view.Conventions {
		out = append(out, fmt.Sprintf("[convention:%s] %s — %s", c.ID, c.Title, c.Description))
	}
	for _, d := range view.Decisions {
		out = append(out, fmt.Sprintf("[decision:%s] %s — %s (chosen: %s)", d.ID, d.Title, d.Rationale, d.Chosen))
	}
	return out
}

// resolveDeviatesFrom maps a lane Deviation back to a baseline node. The lane
// references the offending OBSERVATION by name and may include a citation that
// embeds a "[kind:id]" tag (from baselineSummary). We try to match the citation
// or observation text to a baseline id; on no match we fall back to a pattern
// reference with an empty id (mirroring _buildDeviatesFrom's fallback).
func resolveDeviatesFrom(d Deviation, view ArchView) DeviatesFrom {
	hay := d.Citation + " " + d.Observation + " " + d.Rationale
	for _, p := range view.Patterns {
		if p.ID != "" && (strings.Contains(hay, p.ID) || strings.EqualFold(p.Title, d.Observation)) {
			return DeviatesFrom{Kind: "pattern", PatternID: p.ID}
		}
	}
	for _, c := range view.Conventions {
		if c.ID != "" && (strings.Contains(hay, c.ID) || strings.EqualFold(c.Title, d.Observation)) {
			return DeviatesFrom{Kind: "convention", ConventionID: c.ID}
		}
	}
	for _, dec := range view.Decisions {
		if dec.ID != "" && (strings.Contains(hay, dec.ID) || strings.EqualFold(dec.Title, d.Observation)) {
			return DeviatesFrom{Kind: "decision", DecisionID: dec.ID}
		}
	}
	return DeviatesFrom{Kind: "pattern"}
}

// laneSeverityToNode maps the lane verdict severity (critical|warning|info) onto
// the stored-node / gate severity (high|medium|low). assessChange's gate and the
// store both speak high|medium|low; the lane's DriftVerdictSchema speaks
// critical|warning|info — this is the single conversion point.
func laneSeverityToNode(s string) string {
	switch s {
	case SeverityCritical, "high":
		return "high"
	case SeverityWarning, "medium":
		return "medium"
	case SeverityInfo, "low":
		return "low"
	default:
		return "medium"
	}
}

// deviationPayload builds the observation payload for a materialized deviation
// node, matching the keys materializeDeviation reads in arch_store.go.
func deviationPayload(rep ReportDeviation) json.RawMessage {
	m := map[string]any{
		"title":        rep.Title,
		"description":  rep.Description,
		"deviatesFrom": rep.DeviatesFrom,
		"status":       rep.Status,
		"severity":     rep.Severity,
	}
	// The map holds only strings + a flat struct — Marshal cannot fail; on the
	// impossible error path fall back to a null payload (never panic).
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// changeRefFromDiff builds the JSON change ref for the report top level.
func changeRefFromDiff(diff PrDiff) ChangeRefJSON {
	return ChangeRefJSON{
		Repository:  diff.Repository,
		Kind:        "pr",
		PrNumber:    diff.PrNumber,
		Description: diff.Title,
	}
}

// changeRefToArch builds the persisted ArchChangeRef (introducedBy) for a
// deviation node.
func changeRefToArch(diff PrDiff) *ArchChangeRef {
	return &ArchChangeRef{
		Repository:  diff.Repository,
		Kind:        "pr",
		PrNumber:    diff.PrNumber,
		Description: diff.Title,
	}
}

// describeChange renders a human reference for the lane prompt header.
func describeChange(diff PrDiff) string {
	if diff.Repository != "" && diff.PrNumber > 0 {
		return fmt.Sprintf("%s#%d", diff.Repository, diff.PrNumber)
	}
	if diff.PrNumber > 0 {
		return fmt.Sprintf("PR #%d", diff.PrNumber)
	}
	return diff.Repository
}

// buildFullSummary mirrors _buildSummary in drift.ts for the node-severity path.
func buildFullSummary(deviations []ReportDeviation, hasCritical, gated bool, policy string) string {
	if len(deviations) == 0 {
		return "No architectural deviations detected. Change aligns with established patterns."
	}

	var high, med, low int
	for _, d := range deviations {
		switch d.Severity {
		case "high":
			high++
		case "medium":
			med++
		case "low":
			low++
		}
	}

	head := fmt.Sprintf("%d deviation%s detected", len(deviations), plural(len(deviations)))
	if high > 0 {
		head += fmt.Sprintf(" (%d high, %d medium, %d low)", high, med, low)
	}
	parts := []string{head + "."}

	if hasCritical {
		parts = append(parts, "Critical: high-severity deviations require architectural review.")
	}
	if gated {
		policyDesc := policy
		if policy == "" {
			policyDesc = "no-severity-high"
		}
		parts = append(parts, fmt.Sprintf("Change is BLOCKED by gate policy: %s.", policyDesc))
	}
	return strings.Join(parts, " ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func firstNonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
