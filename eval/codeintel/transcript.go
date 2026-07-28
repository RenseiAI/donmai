package codeintel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/RenseiAI/donmai/eval/experiment"
)

// Arm identifies an experiment arm. It aliases the provider-neutral arm type
// so the code-intel A/B remains a concrete consumer of the generic matrix.
type Arm = experiment.ArmID

const (
	// ArmWith is the treatment arm: baseline tools + the real code-intel MCP
	// surface (or the prompt-help advertisement).
	ArmWith Arm = "with"
	// ArmWithout is the control arm: baseline tools only, donmai stripped from
	// PATH (the mandatory contamination guard).
	ArmWithout Arm = "without"
)

// ToolCall is one captured tool invocation from a run's transcript. It maps into
// EvalTraceInsert.toolCalls[] (platform/src/lib/evals/types.ts:170). The shape is
// deliberately provider-agnostic: name + arguments + the (possibly truncated)
// result text + an error flag are enough for the tool-use-correctness grader
// (brief 06 §4.4) to score adoption and correct-tool-choice.
type ToolCall struct {
	Name       string          `json:"name"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	ResultText string          `json:"resultText,omitempty"`
	IsError    bool            `json:"isError"`
	// Phase identifies the fresh-session phase for context-reset transcripts.
	// Ordinary executions leave it zero, so the field is omitted from JSON.
	Phase int `json:"phase,omitempty"`
}

// TokenCounts mirrors the platform tokenCounts shape
// ({input, output, cache_read}). JSON tags match exactly so the bridge payload
// round-trips into eval_traces without a translation layer.
type TokenCounts struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	CacheRead int64 `json:"cache_read"`
}

// Total returns the "tokens-to-solution" numerator for the efficiency metric
// (brief 06 §4.5): input + output + cache-read. Cache reads are included
// deliberately — the WITH arm attaches the af-code-intelligence MCP surface
// (tool schemas + tool-result payloads) and a larger orienting context, much of
// which lands in cached-input / cache-read accounting in a real agent harness.
// Excluding cache-read would let the WITH arm breach the true <=+10% token
// budget while the harness reported a ratio under the gate — a token cost the
// WITH arm is most likely to grow must not be counted as free.
func (t TokenCounts) Total() int64 { return t.Input + t.Output + t.CacheRead }

// Retain tags for a snapshot / trace (EvalTraceRetain).
const (
	RetainDefault       = "default"
	RetainEvalPermanent = "eval-permanent"
)

// SnapshotRef is the Go mirror of WorkareaSnapshotRef (donmai-architecture/006
// Seam 5; platform/src/lib/evals/types.ts:120). The harness tags fixture
// snapshots 'eval-permanent' so they survive normal GC.
type SnapshotRef struct {
	Provider   string `json:"provider"`
	SnapshotID string `json:"snapshotId"`
	Retain     string `json:"retain"`
	CapturedAt string `json:"capturedAt,omitempty"`
}

// Transcript is the harness's in-memory capture of one arm's execution — the
// EvalTraceInsert signal set (brief 06 §4.3.5): toolCalls[], turnCount,
// tokenCounts, snapshotRef, plus the agent's final answer (the string the
// task-success graders refine over, mirroring implement-result.ts).
type Transcript struct {
	Arm         Arm
	FinalAnswer string
	ToolCalls   []ToolCall
	TurnCount   int
	TokenCounts TokenCounts
	// CostUSD is the provider-reported actual execution cost for this trial.
	// Context-reset trials sum both fresh sessions so spend ledgers can enforce
	// their authorization cap from the same durable report as the receipts.
	CostUSD float64
	// CostReported distinguishes known provider cost, including a known partial
	// amount, from entirely absent billing data.
	CostReported bool
	// CostComplete is true only when CostUSD covers every provider invocation in
	// this execution. Prompt experiments may post only complete provider cost.
	CostComplete bool
	SnapshotRef  *SnapshotRef
	// AdvertisedTools is the set of code-intel tool names advertised to this arm
	// (empty for the WITHOUT arm). Retained so the tool-use grader knows what
	// adoption was even possible.
	AdvertisedTools []string
}

// ---------------------------------------------------------------------------
// Platform-shaped insert payloads (brief 06 §1.1 schema).
// ---------------------------------------------------------------------------

// GradeResult mirrors platform GradeResult (types.ts:48). Stored in
// eval_runs.gradeResults[].
type GradeResult struct {
	GraderID  string         `json:"graderId"`
	Score     float64        `json:"score"`
	Pass      bool           `json:"pass"`
	Reasoning string         `json:"reasoning,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// TraceInsert mirrors platform EvalTraceInsert (types.ts:164).
type TraceInsert struct {
	ID            string       `json:"id"`
	EvalRunID     string       `json:"evalRunId,omitempty"`
	DispatchID    string       `json:"dispatchId,omitempty"`
	InputPayload  any          `json:"inputPayload,omitempty"`
	OutputPayload any          `json:"outputPayload,omitempty"`
	ToolCalls     []ToolCall   `json:"toolCalls,omitempty"`
	TurnCount     int          `json:"turnCount"`
	TokenCounts   TokenCounts  `json:"tokenCounts"`
	SnapshotRef   *SnapshotRef `json:"snapshotRef,omitempty"`
	Retain        string       `json:"retain,omitempty"`
}

// RunInsert mirrors platform EvalRunInsert (types.ts:183).
type RunInsert struct {
	ID               string         `json:"id"`
	DispatchID       string         `json:"dispatchId,omitempty"`
	InputHash        string         `json:"inputHash"`
	OutputHash       string         `json:"outputHash,omitempty"`
	InputSnapshotRef *SnapshotRef   `json:"inputSnapshotRef,omitempty"`
	TraceRef         string         `json:"traceRef,omitempty"`
	DatasetID        string         `json:"datasetId,omitempty"`
	DatasetCaseID    string         `json:"datasetCaseId,omitempty"`
	GradeResults     []GradeResult  `json:"gradeResults,omitempty"`
	OrgID            string         `json:"orgId"`
	ProjectID        string         `json:"projectId,omitempty"`
	GraderConfig     map[string]any `json:"graderConfig,omitempty"`
	EvalMode         string         `json:"evalMode,omitempty"`
}

// ReportEnvelope is the single JSON body the bridge POSTs per (case, arm, trial):
// one eval_runs row + its eval_traces row, in the shape brief 06 §1.1 defines.
// The platform lane is confirming the exact endpoint; until it lands this is the
// agreed payload contract (see integrationNotes).
type ReportEnvelope struct {
	Run   RunInsert   `json:"run"`
	Trace TraceInsert `json:"trace"`
	// Meta carries harness context the dashboard can group by but that is not
	// part of the core schema (arm, family, repo, trial, advertisement mode).
	Meta ReportMeta `json:"meta"`
}

// ReportMeta is harness-side grouping context for one reported run.
type ReportMeta struct {
	CaseID        string `json:"caseId"`
	Arm           Arm    `json:"arm"`
	Family        string `json:"family"`
	Repo          string `json:"repo"`
	Ref           string `json:"ref"`
	Trial         int    `json:"trial"`
	Advertisement string `json:"advertisement"`
	DatasetName   string `json:"datasetName"`
}

// canonicalJSON returns a stable JSON encoding of v (sorted keys via the stdlib's
// map ordering) for hashing. It never HTML-escapes.
func canonicalJSON(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonicalize: %w", err)
	}
	return b, nil
}

// hashPayload returns the sha256 hex of v's canonical JSON — the eval_runs
// inputHash/outputHash contract (hashes-only is PII-safe by construction,
// ADR-017 Open Q4c).
func hashPayload(v any) (string, error) {
	b, err := canonicalJSON(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// BuildEnvelope assembles the eval_runs + eval_traces payload for one graded arm
// execution. runID/traceID are caller-minted UUID-ish ids; input is the case
// input object; grades are the grader outputs. It computes inputHash/outputHash
// and cross-links the two rows (run.traceRef → trace.id, trace.evalRunId →
// run.id).
func BuildEnvelope(
	runID, traceID, dispatchID, orgID, projectID, datasetID string,
	c Case, tr Transcript, grades []GradeResult, meta ReportMeta,
) (ReportEnvelope, error) {
	inHash, err := hashPayload(c.Input)
	if err != nil {
		return ReportEnvelope{}, err
	}
	outHash, err := hashPayload(tr.FinalAnswer)
	if err != nil {
		return ReportEnvelope{}, err
	}
	retain := RetainEvalPermanent // benchmark snapshots must survive GC.
	trace := TraceInsert{
		ID:            traceID,
		EvalRunID:     runID,
		DispatchID:    dispatchID,
		InputPayload:  c.Input,
		OutputPayload: tr.FinalAnswer,
		ToolCalls:     tr.ToolCalls,
		TurnCount:     tr.TurnCount,
		TokenCounts:   tr.TokenCounts,
		SnapshotRef:   tr.SnapshotRef,
		Retain:        retain,
	}
	run := RunInsert{
		ID:               runID,
		DispatchID:       dispatchID,
		InputHash:        inHash,
		OutputHash:       outHash,
		InputSnapshotRef: tr.SnapshotRef,
		TraceRef:         traceID,
		DatasetID:        datasetID,
		DatasetCaseID:    c.ID,
		GradeResults:     grades,
		OrgID:            orgID,
		ProjectID:        projectID,
		EvalMode:         "sync",
	}
	return ReportEnvelope{Run: run, Trace: trace, Meta: meta}, nil
}

// nowISO returns the current UTC time in RFC3339 — used for SnapshotRef.capturedAt.
func nowISO() string { return time.Now().UTC().Format(time.RFC3339) }
