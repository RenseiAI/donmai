// Package codesurvival implements the donmai worker side of the code-survival
// scan work-type (RW3 + RW4). It is a NON-AGENT batch executor: the worker poll
// loop routes batchWork[] items here, fully separate from the agent-session path
// (runner.Run / AgentRuntimeProvider). This package NEVER spawns an agent.
//
// Scope (RW3 — survival): clone a merged PR's repo deep enough to reach the
// merge SHA, blame each modified file at the merge SHA and at HEAD, count how
// many merge-authored lines survive, and POST a CodeSurvivalScanResult back to
// the platform ingestion endpoint.
//
// Scope (RW4 — reachability / hot-path weighting): AFTER survival succeeds, run
// a per-language reachability pass over the SAME clone (Go via x/tools
// callgraph; TS/JS via a baked ts-morph subprocess) to classify each surviving
// line hot/cold/unknown and populate hotWeighted + perSymbol. Reachability is
// best-effort and NEVER hard-fails survival: a degrade collapses to
// status:partial + hotWeighted=null with survival preserved intact. See
// reachability.go / reachability_go.go / reachability_ts.go.
//
// Wire contract:
//   - input  (BatchWorkItem):  runs/2026-06-01-code-survival-runtime-research/03-SEAM-DESIGN.md
//   - output (CodeSurvivalScanResult): platform/src/lib/factory/code-survival-scan-contract.ts
//
// Both sides keep CodeSurvivalContractVersion in lockstep. Any change to the
// payload bumps this constant and the platform CODE_SURVIVAL_CONTRACT_VERSION.
package codesurvival

// CodeSurvivalContractVersion mirrors CODE_SURVIVAL_CONTRACT_VERSION in
// platform/src/lib/factory/code-survival-scan-contract.ts. The executor echoes
// the version it was built against; the platform ingestion rejects unknown
// majors so a stale worker image cannot silently write malformed rows.
const CodeSurvivalContractVersion = 1

// WorkTypeCodeSurvivalScan is the batchWork[].workType this package handles.
const WorkTypeCodeSurvivalScan = "code-survival-scan"

// ScanStatus is the terminal disposition of a single scan attempt. Mirrors the
// ScanStatus union in the platform contract.
type ScanStatus string

const (
	// StatusOK — survival + (if applicable) reachability both computed.
	StatusOK ScanStatus = "ok"
	// StatusPartial — survival computed; reachability skipped/degraded
	// (hotWeighted=null). In RW3 a partial blame run (a file vanished at HEAD)
	// reports partial.
	StatusPartial ScanStatus = "partial"
	// StatusSkipped — could not scan (history can't reach mergeSha, repo gone).
	StatusSkipped ScanStatus = "skipped"
	// StatusFailed — survival itself failed; retry-eligible.
	StatusFailed ScanStatus = "failed"
)

// ScanSkipReason explains a skipped/degraded scan. Mirrors the platform union.
type ScanSkipReason string

const (
	// SkipRepoGone — clone failed: repo deleted / access revoked.
	SkipRepoGone ScanSkipReason = "repo_gone"
	// SkipShallowHistory — history cannot reach mergeSha (force-push / rewrite /
	// too-shallow clone).
	SkipShallowHistory ScanSkipReason = "shallow_history"
)

// SymbolReachability mirrors the platform union. RW4 emits these per surviving
// symbol; `unknown` is weighted as hot (no down-weight).
type SymbolReachability string

// Reachability classifications. `unknown` is weighted as hot (no down-weight).
const (
	ReachableHot     SymbolReachability = "hot"
	ReachableCold    SymbolReachability = "cold"
	ReachableUnknown SymbolReachability = "unknown"
)

// ScanSurvival mirrors the platform ScanSurvival interface.
type ScanSurvival struct {
	LinesTotalAtMerge int `json:"linesTotalAtMerge"`
	LinesSurviving    int `json:"linesSurviving"`
	// SurvivalRatePct is computeSurvivalRate(surviving, total); null when
	// total == 0. *float64 so the zero-vs-null distinction round-trips as the
	// contract requires (consumers must filter survival_rate_pct IS NOT NULL).
	SurvivalRatePct *float64 `json:"survivalRatePct"`
}

// ScanHotWeighted mirrors the platform ScanHotWeighted interface. Populated by
// RW4 when reachability ran cleanly; null on a degrade (status:partial).
type ScanHotWeighted struct {
	HotLinesSurviving  int      `json:"hotLinesSurviving"`
	ColdLinesSurviving int      `json:"coldLinesSurviving"`
	WCold              float64  `json:"wCold"`
	HotWeightedRatePct *float64 `json:"hotWeightedRatePct"`
}

// ScanSymbolBreakdown mirrors the platform ScanSymbolBreakdown interface. RW4
// emits one row per surviving symbol (bounded by maxPerSymbol); empty on a
// reachability degrade.
type ScanSymbolBreakdown struct {
	File           string             `json:"file"`
	Symbol         string             `json:"symbol"`
	StartLine      int                `json:"startLine"`
	EndLine        *int               `json:"endLine"`
	LinesSurviving int                `json:"linesSurviving"`
	Reachable      SymbolReachability `json:"reachable"`
}

// ScanExecutorInfo mirrors the platform ScanExecutorInfo interface.
type ScanExecutorInfo struct {
	// PoolProviderID is the pool provider class the scan ran in
	// (e2b/local/kubernetes/…).
	PoolProviderID string `json:"poolProviderId"`
	// WorkerVersion is the donmai worker version that produced the result.
	WorkerVersion string `json:"workerVersion"`
	// Toolchains records the toolchain versions present at scan time.
	Toolchains ScanToolchains `json:"toolchains"`
}

// ScanToolchains mirrors the platform `{ go?, node?, git? }` shape. omitempty so
// absent toolchains serialize away rather than as empty strings.
type ScanToolchains struct {
	Go   string `json:"go,omitempty"`
	Node string `json:"node,omitempty"`
	Git  string `json:"git,omitempty"`
}

// CodeSurvivalScanResult is the payload POSTed to the platform ingestion
// endpoint (batchWork.resultEndpoint) with batchWork.resultAuth as bearer.
// Mirrors CodeSurvivalScanResult in the platform contract field-for-field;
// idempotency key is `${attributionId}:${checkpoint}` (platform-owned).
//
// The name deliberately mirrors the platform contract type
// (CodeSurvivalScanResult in code-survival-scan-contract.ts) so the lockstep is
// obvious at the call sites; the revive stutter lint is suppressed for that
// reason.
//
//nolint:revive // name mirrors the platform contract type for lockstep clarity
type CodeSurvivalScanResult struct {
	ContractVersion int              `json:"contractVersion"`
	AttributionID   string           `json:"attributionId"`
	Checkpoint      int              `json:"checkpoint"`
	MergeSha        string           `json:"mergeSha"`
	HeadSha         string           `json:"headSha"`
	Status          ScanStatus       `json:"status"`
	SkipReason      *ScanSkipReason  `json:"skipReason"`
	Survival        ScanSurvival     `json:"survival"`
	HotWeighted     *ScanHotWeighted `json:"hotWeighted"`
	// PerSymbol is never nil in the wire payload — an empty slice serializes as
	// [] (matching the contract) rather than null.
	PerSymbol []ScanSymbolBreakdown `json:"perSymbol"`
	Provider  *string               `json:"provider"`
	WorkType  *string               `json:"workType"`
	Executor  ScanExecutorInfo      `json:"executor"`
}

// BatchWorkItem is one item of the poll response's batchWork[] array
// (platform → worker). The wire shape is frozen in
// runs/2026-06-01-code-survival-runtime-research/03-SEAM-DESIGN.md.
//
// This struct lives in the codesurvival package (not worker/) so the worker
// poll types stay agnostic of any single batch work-type; worker/types.go only
// carries the json.RawMessage envelope and the workType discriminant.
type BatchWorkItem struct {
	// BatchJobID is the claim key (e.g. "batch:due_checkpoint:<id>"); namespaced,
	// never a session UUID.
	BatchJobID string `json:"batchJobId"`
	// WorkType discriminates the batch handler (e.g. "code-survival-scan").
	WorkType string `json:"workType"`
	// ContractVersion is the platform's CODE_SURVIVAL_CONTRACT_VERSION at
	// dispatch time. The executor rejects an unknown major.
	ContractVersion int    `json:"contractVersion"`
	OrgID           string `json:"orgId"`
	ProjectID       string `json:"projectId"`
	AttributionID   string `json:"attributionId"`
	Checkpoint      int    `json:"checkpoint"`
	PRRepo          string `json:"prRepo"`
	PRNumber        int    `json:"prNumber"`
	MergeSha        string `json:"mergeSha"`
	// Needs carries the RW2 language-detection result. Read but unused in RW3
	// (toolchain bake + reachability is RW4); retained so the wire shape is
	// complete and forward-compatible.
	Needs BatchWorkNeeds `json:"needs"`
	// GitCredential carries the short-lived per-repo token + clone URL minted at
	// dispatch (REN-1554). Scrubbed after use; never persisted.
	GitCredential BatchWorkGitCredential `json:"gitCredential"`
	// ResultEndpoint is the platform ingestion URL the result is POSTed to.
	ResultEndpoint string `json:"resultEndpoint"`
	// ResultAuth is the JWT envelope (org/project claim). The worker re-verifies
	// the org claim and uses it as the bearer on the result POST.
	ResultAuth string `json:"resultAuth"`
}

// BatchWorkNeeds mirrors the dispatch-side language-detection hint.
type BatchWorkNeeds struct {
	NeedsGo   bool `json:"needsGo"`
	NeedsNode bool `json:"needsNode"`
}

// BatchWorkGitCredential carries the injected clone credential.
type BatchWorkGitCredential struct {
	Token string `json:"token"`
	// CloneURL is the credential-injected HTTPS URL
	// (https://x-access-token:<token>@github.com/owner/repo.git).
	CloneURL string `json:"cloneUrl"`
	// ExpiresAt is the ISO8601 token expiry; advisory for logging.
	ExpiresAt string `json:"expiresAt"`
}
