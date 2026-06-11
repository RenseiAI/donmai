package codeintel

// arch_drift.go — the LLM seam CONTRACT for arch-intel drift assessment.
//
// Authority: 02-two-axis-architecture.md §3.5;
// ADR-2026-06-07-intelligence-implementation-is-platform.md.
//
// ── THE MANDATE ──────────────────────────────────────────────────────────────
// Architectural Intelligence (the Layer-2 learned-baseline + LLM deviation
// pipeline) is implemented by the platform, not by OSS. This file ships ONLY
// the contract a drift implementation must satisfy: the ModelAdapter seam, the
// request/response types, and DriftVerdictSchema. There is intentionally no
// implementation here — per ADR-2026-06-07, OSS carries contracts + extension
// points, never reference implementations of intelligence features.
//
// Whatever implements ModelAdapter MUST route its LLM call through the OSS
// one-shot lane (agent.Complete) — never a direct provider SDK — so it inherits
// the config-resolved cell selection, the host-subscription posture, the
// metered-key fallback, and the cost honesty every other consumer gets, instead
// of becoming a hidden direct-API bypass with its own key handling.

import (
	"context"
	"encoding/json"
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

// ModelAdapter is the arch-intel drift LLM seam. A drift pipeline consumes
// THIS — never a direct provider SDK (see THE MANDATE above). OSS ships no
// implementation; the platform provides one.
type ModelAdapter interface {
	AssessChange(ctx context.Context, req AssessChangeRequest) (Assessment, error)
}

// DriftVerdictSchema is the JSON Schema a ModelAdapter implementation's
// deviation output must match. Passed as OneShotRequest.ResponseSchema so
// native harnesses constrain output server-side and soft harnesses get it as a
// prompt instruction + post-validation.
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
