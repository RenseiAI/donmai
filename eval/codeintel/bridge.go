package codeintel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBridgePath is the platform endpoint the harness POSTs each completed
// (case, arm, trial) to. It is the real route the platform GRADERS + DATASET
// REGISTRATION lane shipped (platform/src/app/api/evals/ingest/route.ts): a thin
// per-trial ingestion bridge that writes eval_traces + eval_runs and runs the
// registered graders inline. It is intentionally under /api/evals/ so it inherits
// the getCliOrSessionAuth bearer surface the rest of the eval API uses.
// Overridable via --platform-path.
const DefaultBridgePath = "/api/evals/ingest"

// IngestRequest is the per-trial body the platform /api/evals/ingest route
// accepts (its zod ingestSchema, route.ts). One completed (case, arm, trial):
// the driver hands the platform the full dataset case inline (the canonical
// JSONL lives in donmai, not platform — the route accepts it per-request rather
// than looking it up from eval_datasets.cases), the agent's final answer, and
// the captured trace signals. The platform then writes eval_traces + eval_runs
// and runs the REGISTERED graders inline (structural/codeintel-task-v1,
// model-grader/codeintel-refactor-v1, and — WITH arm only —
// tool-use-correctness/codeintel-v1), so the dashboard's gradeResults come from
// the platform graders. The driver's own Go graders still drive its local A/B
// console rollup + efficiency-threshold verdict; the two are independent by design.
type IngestRequest struct {
	// Arm is the bounded opaque experiment arm. Legacy code-intel callers retain
	// "with" | "without"; the platform only auto-adds tool-use-correctness on
	// the exact legacy "with" value.
	Arm Arm `json:"arm"`
	// DatasetID is the evd_ id of the registered codeintel-benchmark dataset
	// (validated by the platform for FK integrity + org visibility). Supplied via
	// --dataset-id.
	DatasetID string `json:"datasetId"`
	// DatasetCase is the full benchmark case, inline. Case's JSON shape
	// ({id, input, expectedOutput?, rubric?, tags?}) is byte-compatible with the
	// platform caseSchema (datasets/_shared.ts).
	DatasetCase Case `json:"datasetCase"`
	// DispatchID is the driver-side run identifier (the provisioned session id).
	DispatchID string `json:"dispatchId,omitempty"`
	// Repo/Ref/TrialIndex are descriptive grouping metadata stored on the run's
	// graderConfig by the platform.
	Repo       string `json:"repo,omitempty"`
	Ref        string `json:"ref,omitempty"`
	TrialIndex int    `json:"trialIndex"`
	// InputPayload is optional; when omitted the platform falls back to
	// datasetCase.input for the grade context, which is what we want.
	InputPayload any `json:"inputPayload,omitempty"`
	// OutputPayload is the agent's final-response string the structural graders
	// refine over (mirrors implement-result.ts). Always sent.
	OutputPayload any `json:"outputPayload"`
	// ToolCalls is the captured tool-call log — required for tool-use-correctness
	// grading on the WITH arm. The ToolCall JSON (name/arguments/resultText) is
	// serialized+substring-scanned by the platform grader, so both the MCP
	// tool-name form (mcp__af-code-intelligence__af_code_*) and the CLI form
	// (donmai code <subcommand>) are recognized.
	ToolCalls   []ToolCall  `json:"toolCalls"`
	TurnCount   int         `json:"turnCount"`
	TokenCounts TokenCounts `json:"tokenCounts"`
	// ProjectID is optional reporting context for the eval_runs row.
	ProjectID string `json:"projectId,omitempty"`
	// GraderIDs is required for generic experiment families without registered
	// defaults. Omitted for legacy code-intel requests to preserve their wire.
	GraderIDs []string `json:"graderIds,omitempty"`
	// Experiment carries immutable prompt-variant identity, never raw prompt text.
	Experiment *ExperimentReceipt `json:"experiment,omitempty"`
}

// ExperimentReceipt is the durable identity attached to a prompt-experiment
// trial. VariantRef must already have passed experiment.Definition validation.
type ExperimentReceipt struct {
	ExperimentID string `json:"experimentId"`
	SubjectRef   string `json:"subjectRef"`
	VariantRef   string `json:"variantRef"`
}

// IngestResponse is the platform route's 201 body. The driver logs the
// platform-assigned runId so an operator can find the row in /admin/evals.
type IngestResponse struct {
	RunID        string              `json:"runId"`
	TraceID      string              `json:"traceId"`
	DatasetID    string              `json:"datasetId"`
	GradersRun   []string            `json:"gradersRun"`
	GradeResults []IngestGradeResult `json:"gradeResults"`
}

// IngestGradeResult mirrors the platform's status-bearing grader response.
// Pointer verdict fields preserve null for pending/skipped/error results so the
// bridge cannot silently coerce an ungraded receipt into score=0/pass=false.
type IngestGradeResult struct {
	GraderID string   `json:"graderId"`
	Status   string   `json:"status"`
	Score    *float64 `json:"score"`
	Pass     *bool    `json:"pass"`
}

// BuildIngestRequest assembles the per-trial ingest body from a graded arm
// execution. The platform computes its own row ids + input/output hashes and runs
// its own graders, so this payload carries only the raw inputs the route needs:
// the case, the arm, the captured transcript, and reporting context.
func BuildIngestRequest(c Case, tr Transcript, trial int, dispatchID, datasetID, projectID string) IngestRequest {
	return IngestRequest{
		Arm:           tr.Arm,
		DatasetID:     datasetID,
		DatasetCase:   c,
		DispatchID:    dispatchID,
		Repo:          c.Input.Repo,
		Ref:           c.Input.Ref,
		TrialIndex:    trial,
		OutputPayload: tr.FinalAnswer,
		ToolCalls:     tr.ToolCalls,
		TurnCount:     tr.TurnCount,
		TokenCounts:   tr.TokenCounts,
		ProjectID:     projectID,
	}
}

// Bridge posts per-trial ingest bodies to the platform. A zero BaseURL makes
// every post a no-op (the --dry / offline path) so the harness runs fully
// without a live platform — results are still captured locally.
type Bridge struct {
	BaseURL string
	Token   string
	Path    string
	Client  *http.Client
}

// NewBridge builds a Bridge. baseURL "" disables posting (offline/dry).
func NewBridge(baseURL, token, path string) *Bridge {
	if path == "" {
		path = DefaultBridgePath
	}
	return &Bridge{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		Path:    path,
		Client:  &http.Client{Timeout: 15 * time.Second},
	}
}

// Enabled reports whether posts will actually be sent.
func (b *Bridge) Enabled() bool { return b != nil && b.BaseURL != "" }

// Post sends one per-trial ingest body. When the bridge is disabled it returns
// (nil, nil): "not posted, not an error". On a successful 2xx it returns the
// parsed IngestResponse (with the platform-assigned runId); on a non-2xx or
// transport error it returns (nil, err).
func (b *Bridge) Post(ctx context.Context, req IngestRequest) (*IngestResponse, error) {
	if !b.Enabled() {
		return nil, nil
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal ingest request: %w", err)
	}
	url := b.BaseURL + b.Path
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if b.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.Token)
	}
	client := b.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("post ingest request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, fmt.Errorf("read ingest response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("bridge POST %s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	out := &IngestResponse{}
	if err := json.Unmarshal(respBody, out); err != nil {
		return nil, fmt.Errorf("decode ingest response: %w", err)
	}
	if strings.TrimSpace(out.RunID) == "" || strings.TrimSpace(out.TraceID) == "" || strings.TrimSpace(out.DatasetID) == "" || len(out.GradersRun) == 0 {
		return nil, fmt.Errorf("decode ingest response: missing runId, traceId, datasetId, or gradersRun")
	}
	resultGraders := make(map[string]struct{}, len(out.GradeResults))
	for _, result := range out.GradeResults {
		if strings.TrimSpace(result.GraderID) == "" {
			return nil, fmt.Errorf("decode ingest response: grade result missing graderId")
		}
		if result.Status != "scored" || result.Score == nil || result.Pass == nil {
			return nil, fmt.Errorf("decode ingest response: grader %q returned non-scored status %q", result.GraderID, result.Status)
		}
		resultGraders[result.GraderID] = struct{}{}
	}
	for _, graderID := range out.GradersRun {
		if _, ok := resultGraders[graderID]; !ok {
			return nil, fmt.Errorf("decode ingest response: missing grade result for %q", graderID)
		}
	}
	if len(req.GraderIDs) > 0 && !sameStrings(req.GraderIDs, out.GradersRun) {
		return nil, fmt.Errorf("decode ingest response: gradersRun does not match requested graderIds")
	}
	return out, nil
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, value := range left {
		counts[value]++
	}
	for _, value := range right {
		counts[value]--
		if counts[value] < 0 {
			return false
		}
	}
	return true
}
