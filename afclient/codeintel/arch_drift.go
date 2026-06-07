package codeintel

// arch_drift.go — the LLM seam for arch-intel drift assessment (Phase 5b).
//
// Authority: 02-two-axis-architecture.md §3.5 ("arch-intel drift: same Complete
// call with driftVerdictSchema; runs Auth: host-session → SpawnComplete. This
// stops arch-intel from hard-coding anthropic-sdk-go as an 11th direct-LLM
// bypass."); ADR-2026-06-06.
//
// ── THE MANDATE ──────────────────────────────────────────────────────────────
// arch-intel's full drift pipeline (the SQLite observation graph + assessChange,
// research/18-arch-intel-assessment.md Layer 2) is an unported ~3-week effort.
// When it IS ported, its deviation-detection LLM call MUST go through this
// ModelAdapter — i.e. through the OSS one-shot lane (agent.Complete) — NOT
// through github.com/anthropics/anthropic-sdk-go directly (which research/18
// originally proposed). Routing arch-intel through the lane means it inherits the
// config-resolved cell selection, the ≈$0 host-subscription posture, the
// metered-key fallback, and the cost honesty every other consumer gets, instead
// of becoming a hidden direct-API bypass with its own key handling.
//
// This file is the SEAM, not the pipeline: it ships the adapter contract, a
// working lane-backed implementation, the response schema, and the prompt. The
// Layer-2 port builds its assessChange on top of AssessChange here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// Deviation severities.
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
	SeverityInfo     = "info"
)

// Deviation is one architectural drift the model flagged: an observation that
// conflicts with an established convention/decision. JSON field names match the
// DriftVerdictSchema and are kept compatible with the TS Deviation shape.
type Deviation struct {
	Observation string `json:"observation"` // which observation drifted
	Severity    string `json:"severity"`    // critical | warning | info
	Rationale   string `json:"rationale"`   // why it is a deviation
	Citation    string `json:"citation,omitempty"`
}

// Assessment is the result of a drift assessment. SchemaOK reports whether the
// model produced output matching DriftVerdictSchema; when false the caller gets
// an empty (HasCriticalDrift=false) Assessment and decides whether to retry, fall
// back to the native gate, or drop — the same validate-repair-drop posture the
// one-shot lane and the KG path use.
type Assessment struct {
	Deviations       []Deviation
	HasCriticalDrift bool
	SchemaOK         bool
}

// AssessChangeRequest is the input to a drift assessment: the diff-level
// observations to judge plus the conventions/decisions context to judge them
// against, and a human reference for the change (for the prompt).
type AssessChangeRequest struct {
	Change       string            // e.g. "owner/repo#123" — context only
	Observations []DiffObservation // the signals to assess (from ReadDiffObservations)
	Conventions  []string          // established conventions/decisions (optional context)
}

// ModelAdapter is the arch-intel drift LLM seam. The full pipeline port consumes
// THIS — never a direct provider SDK (see THE MANDATE above).
type ModelAdapter interface {
	AssessChange(ctx context.Context, req AssessChangeRequest) (Assessment, error)
}

// LaneAdapter is the production ModelAdapter: it runs the deviation-detection
// prompt through the OSS one-shot lane (agent.Complete) with DriftVerdictSchema.
// Under a host-session/subscription cell this is a ≈$0 soft-JSON SpawnComplete;
// under a keyed native cell it is strict structured — the cell, not this adapter,
// decides (the whole point of routing through the lane).
type LaneAdapter struct {
	// Harness is the resolved agent harness the assessment runs on.
	Harness agent.HarnessProvider
	// Model is the optional model id; empty falls back to the harness default.
	Model string
}

// DriftVerdictSchema is the JSON Schema the model's deviation output must match.
// Passed as OneShotRequest.ResponseSchema so native harnesses constrain output
// server-side and soft harnesses get it as a prompt instruction + post-validation.
var DriftVerdictSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "deviations": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "observation": {"type": "string"},
          "severity": {"type": "string", "enum": ["critical", "warning", "info"]},
          "rationale": {"type": "string"},
          "citation": {"type": "string"}
        },
        "required": ["observation", "severity", "rationale"]
      }
    }
  },
  "required": ["deviations"]
}`)

const driftSystemPrompt = "You are an architectural drift detector. Given a set of architectural " +
	"observations extracted from a code change and a list of established conventions, identify which " +
	"observations DEVIATE from the conventions. Output ONLY a JSON object matching the provided schema: " +
	"a `deviations` array, each entry naming the offending observation, a severity " +
	"(critical|warning|info), and a one-sentence rationale. If nothing deviates, return an empty " +
	"`deviations` array. Do not invent deviations that the observations do not support."

// AssessChange runs the deviation-detection prompt through the one-shot lane and
// returns the structured Assessment. A nil Harness is a programming error. A
// transport/provider error is returned; a successful-but-unparseable model
// response is NOT an error — it yields SchemaOK=false with no deviations (the
// caller decides how to handle the soft miss).
func (a LaneAdapter) AssessChange(ctx context.Context, req AssessChangeRequest) (Assessment, error) {
	if a.Harness == nil {
		return Assessment{}, errors.New("archdrift: LaneAdapter requires a non-nil Harness")
	}

	res, err := agent.Complete(ctx, a.Harness, agent.OneShotRequest{
		System:         driftSystemPrompt,
		Messages:       []agent.Message{{Role: "user", Content: renderAssessPrompt(req)}},
		Model:          a.Model,
		ResponseSchema: DriftVerdictSchema,
	})
	if err != nil {
		return Assessment{}, fmt.Errorf("archdrift: assess: %w", err)
	}

	out := Assessment{SchemaOK: res.SchemaOK}
	if !res.SchemaOK || len(res.Structured) == 0 {
		// Soft miss: the model did not emit schema-valid JSON. Return empty
		// (no deviations, no critical drift) — the caller falls back to the
		// native gate or retries.
		out.SchemaOK = false
		return out, nil
	}

	var parsed struct {
		Deviations []Deviation `json:"deviations"`
	}
	if err := json.Unmarshal(res.Structured, &parsed); err != nil {
		// Validated against the schema but failed typed decode — treat as a soft
		// miss rather than a hard error (defensive; should not happen post-validate).
		out.SchemaOK = false
		return out, nil
	}

	out.Deviations = parsed.Deviations
	for i := range parsed.Deviations {
		if parsed.Deviations[i].Severity == SeverityCritical {
			out.HasCriticalDrift = true
			break
		}
	}
	return out, nil
}

// renderAssessPrompt serializes the observations + conventions into the user turn.
// Observations are rendered as compact JSON (the model reads structured signals);
// conventions are a bulleted list. The change ref is a header for context.
func renderAssessPrompt(req AssessChangeRequest) string {
	var b strings.Builder
	if req.Change != "" {
		fmt.Fprintf(&b, "Change under review: %s\n\n", req.Change)
	}
	b.WriteString("Observations (JSON):\n")
	if obs, err := json.MarshalIndent(req.Observations, "", "  "); err == nil {
		b.Write(obs)
	} else {
		b.WriteString("[]")
	}
	b.WriteString("\n\n")
	if len(req.Conventions) > 0 {
		b.WriteString("Established conventions:\n")
		for _, c := range req.Conventions {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	} else {
		b.WriteString("Established conventions: (none provided)\n")
	}
	return b.String()
}
