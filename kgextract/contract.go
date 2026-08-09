// Package kgextract implements the donmai worker side of the kg-extraction
// work-type. It is a NON-AGENT, non-interactive batch executor: the worker poll
// loop routes kgExtractWork[] items here, fully separate from the agent-session
// path (runner.Run / AgentRuntimeProvider). This package NEVER spawns a
// long-lived agent session.
//
// Scope: each item is a constrained, single-shot LLM emit. For every observation
// the executor runs the model once with the platform-supplied
// extractionSystemPrompt, parses the emitted {nodes,edges} JSON into graph
// triples, validates each node/edge against the ExtractedNode/ExtractedEdge
// shape (dropping invalid entries), assembles a per-observation result, and
// POSTs a KGExtractionResult back to the platform ingestion endpoint.
//
// This package mirrors codesurvival/ in structure, error-handling, and idioms:
//   - contract.go : KgExtractWorkItem + KGExtractionResult + the
//     Observation/ExtractedNode/ExtractedEdge structs + WorkType /
//     ContractVersion consts (this file).
//   - executor.go : Executor + Handle(ctx,item) entry: per-observation
//     provider-emit → parse → validate → assemble → postResult.
//   - router.go   : BatchHandler(exec) returning worker.BatchHandler, routing on
//     WorkType (kg-extraction); unknown work-types are logged + skipped.
//
// Wire contract (both sides keep KGExtractionContractVersion in lockstep with
// the platform's KG_EXTRACTION_CONTRACT_VERSION; any change to the payload bumps
// this constant and the platform constant together):
//   - input  (KgExtractWorkItem):   sibling to batchWork[] in the worker poll
//     response, emitted by the platform poll route as a top-level
//     `kgExtractWork` array.
//   - output (KGExtractionResult):  POSTed to KgExtractWorkItem.ResultEndpoint
//     ("/api/factory/kg-extraction/results"), Zod-validated by the platform.
//
// The platform re-validates every node/edge on ingest and is the source of
// truth; the worker's validation here is defense-in-depth (drop malformed
// triples rather than poison the graph).
package kgextract

// KGExtractionContractVersion mirrors KG_EXTRACTION_CONTRACT_VERSION on the
// platform. The executor echoes the version it was built against; the platform
// ingestion rejects unknown majors so a stale worker image cannot silently write
// malformed rows.
//
// v2: edges carry an optional confidence label + discrete confidenceScore, and
// the emit prompt asks for a `semantically_similar_to` relation. Both edge
// fields are OPTIONAL on the platform schema, so the addition is purely
// additive; the relation name needs no constant here because RelationshipName is
// a free string and the prompt + JSON-Schema ride on the wire from the platform.
// The canonical fixture of a real v2 work item is
// testdata/platform_v2_work_item.json (generated from the platform's own
// dispatcher) — the decode test reads it, so a future contract move that the Go
// side does not follow fails here rather than in a daemon log.
const KGExtractionContractVersion = 2

// WorkTypeKGExtraction is the kgExtractWork[].workType this package handles.
const WorkTypeKGExtraction = "kg-extraction"

// AuthMode is the credential mode the platform dispatched the item under.
// Mirrors the platform `authMode` union. host-session runs the agentic claude
// CLI (provider emit); local runs a raw completion. Both flow through the same
// provider-emit seam — the provider implementation decides the transport.
type AuthMode string

// AuthMode values mirror the platform union.
const (
	// AuthModeHostSession runs the constrained turn under the agentic claude CLI
	// (host-session credentials).
	AuthModeHostSession AuthMode = "host-session"
	// AuthModeLocal runs the constrained turn as a raw local completion.
	AuthModeLocal AuthMode = "local"
)

// ExtractionStatus is the terminal disposition of a kg-extraction item. Mirrors
// the platform status union on KGExtractionResult.
type ExtractionStatus string

// ExtractionStatus values mirror the platform union.
const (
	// StatusOK — every observation produced a (possibly empty) validated graph.
	StatusOK ExtractionStatus = "ok"
	// StatusPartial — at least one observation succeeded and at least one failed
	// (provider error / unparseable emit). The succeeded ones are reported.
	StatusPartial ExtractionStatus = "partial"
	// StatusError — every observation failed (no graph could be produced).
	StatusError ExtractionStatus = "error"
)

// NodeType is the closed set of node types the platform's ExtractedNode schema
// accepts. A node whose type is not one of these is dropped (defense-in-depth);
// the platform re-validates against the same set.
type NodeType string

// NodeType values mirror the platform ExtractedNode `type` enum EXACTLY.
const (
	NodeTypeService    NodeType = "Service"
	NodeTypeModule     NodeType = "Module"
	NodeTypeAPI        NodeType = "API"
	NodeTypeDatabase   NodeType = "Database"
	NodeTypeDecision   NodeType = "Decision"
	NodeTypePattern    NodeType = "Pattern"
	NodeTypeConvention NodeType = "Convention"
	NodeTypeDeviation  NodeType = "Deviation"
	NodeTypePerson     NodeType = "Person"
	NodeTypeConfig     NodeType = "Config"
	NodeTypeDependency NodeType = "Dependency"
)

// validNodeTypes is the membership set used by validation. Kept in lockstep with
// the NodeType consts above and the platform ExtractedNode enum.
var validNodeTypes = map[NodeType]struct{}{
	NodeTypeService:    {},
	NodeTypeModule:     {},
	NodeTypeAPI:        {},
	NodeTypeDatabase:   {},
	NodeTypeDecision:   {},
	NodeTypePattern:    {},
	NodeTypeConvention: {},
	NodeTypeDeviation:  {},
	NodeTypePerson:     {},
	NodeTypeConfig:     {},
	NodeTypeDependency: {},
}

// isValidNodeType reports whether t is one of the closed-set node types.
func isValidNodeType(t NodeType) bool {
	_, ok := validNodeTypes[t]
	return ok
}

// Observation is one input the model extracts a graph from. Mirrors the platform
// observation shape on KgExtractWorkItem.observations[].
type Observation struct {
	// ID is the platform-owned observation identifier; echoed back on the result
	// as KGExtractionResultEntry.ObservationID so the platform can join the graph
	// to its source observation.
	ID string `json:"id"`
	// Type is the observation type/category (advisory; passed through unused).
	Type string `json:"type"`
	// Content is the text the model extracts triples from.
	Content string `json:"content"`
}

// ExtractedNode mirrors the platform ExtractedNode shape field-for-field. The
// Type is constrained to the NodeType closed set; an out-of-set value is dropped
// during validation.
type ExtractedNode struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Type        NodeType `json:"type"`
	Description string   `json:"description"`
}

// EdgeConfidence is the v2 confidence label carried on an extracted edge.
// Mirrors the platform EDGE_CONFIDENCE_LEVELS union. It is OPTIONAL on the
// platform schema, so an emit that omits it is still valid — the platform
// defaults a missing label when it persists the edge.
type EdgeConfidence string

// EdgeConfidence values mirror the platform union EXACTLY.
const (
	// EdgeConfidenceExtracted — the relationship is explicit in the source.
	EdgeConfidenceExtracted EdgeConfidence = "EXTRACTED"
	// EdgeConfidenceInferred — a reasonable inference from the source.
	EdgeConfidenceInferred EdgeConfidence = "INFERRED"
	// EdgeConfidenceAmbiguous — uncertain; surfaced for review, never omitted.
	EdgeConfidenceAmbiguous EdgeConfidence = "AMBIGUOUS"
)

// ExtractedEdge mirrors the platform ExtractedEdge shape field-for-field.
//
// v2 added Confidence + ConfidenceScore. Both are PASSTHROUGH: the worker does
// NOT validate or default them (the platform's schema marks both optional and
// owns the defaulting), it only carries the model's labels through so they are
// not silently dropped on the way back. omitempty on both keeps a v1-shaped emit
// serializing exactly as it did before. ConfidenceScore is a pointer so a
// legitimate 0 score is distinguishable from "absent".
type ExtractedEdge struct {
	SourceNodeID     string         `json:"sourceNodeId"`
	TargetNodeID     string         `json:"targetNodeId"`
	RelationshipName string         `json:"relationshipName"`
	Confidence       EdgeConfidence `json:"confidence,omitempty"`
	ConfidenceScore  *float64       `json:"confidenceScore,omitempty"`
}

// ExtractedGraph is the {nodes,edges} object the model is instructed to emit and
// that rides on each KGExtractionResultEntry. Both slices serialize as [] (never
// null) so the platform sees a stable shape.
type ExtractedGraph struct {
	Nodes []ExtractedNode `json:"nodes"`
	Edges []ExtractedEdge `json:"edges"`
}

// KGExtractionResultEntry is one observation's validated graph. Mirrors the
// platform results[] element field-for-field.
type KGExtractionResultEntry struct {
	ObservationID string         `json:"observationId"`
	Graph         ExtractedGraph `json:"graph"`
}

// KGExtractionResult is the payload POSTed to the platform ingestion endpoint
// (KgExtractWorkItem.resultEndpoint) with KgExtractWorkItem.resultAuth as bearer.
// Mirrors the platform KGExtractionResult Zod schema field-for-field.
//
// The name deliberately mirrors the platform contract type so the lockstep is
// obvious at the call sites; the revive stutter lint is suppressed for that
// reason.
//
//nolint:revive // name mirrors the platform contract type for lockstep clarity
type KGExtractionResult struct {
	BatchJobID      string `json:"batchJobId"`
	ContractVersion int    `json:"contractVersion"`
	// Results is never nil in the wire payload — an empty slice serializes as []
	// (matching the contract) rather than null.
	Results []KGExtractionResultEntry `json:"results"`
	Status  ExtractionStatus          `json:"status"`
	// Error is an optional human-readable error summary (set on partial/error).
	// omitempty so an "ok" result omits it (matching the platform optional field).
	Error string `json:"error,omitempty"`
}

// KgExtractWorkItem is one item of the poll response's kgExtractWork[] array
// (platform → worker). It lives in the kgextract package (not worker/) so the
// worker poll types stay agnostic of any single batch work-type; worker/types.go
// only carries the json.RawMessage envelope and the workType discriminant.
//
// Field names mirror the platform KgExtractWorkItem EXACTLY — do not rename.
// The type name intentionally mirrors the platform contract type, so the
// revive "stutter" lint is suppressed rather than renamed away.
type KgExtractWorkItem struct { //nolint:revive // name mirrors the platform KgExtractWorkItem contract type
	// BatchJobID is the claim key (e.g. "batch:kg_extract:<uuid>"); namespaced,
	// never a session UUID.
	BatchJobID string `json:"batchJobId"`
	// WorkType discriminates the batch handler ("kg-extraction").
	WorkType string `json:"workType"`
	// ContractVersion is the platform's KG_EXTRACTION_CONTRACT_VERSION at dispatch
	// time. The executor rejects an unknown major.
	ContractVersion int    `json:"contractVersion"`
	OrgID           string `json:"orgId"`
	ProjectID       string `json:"projectId"`
	// AuthMode is the credential/transport mode (host-session | local).
	AuthMode AuthMode `json:"authMode"`
	// Provider is the provider family to run the emit on (e.g. "claude").
	Provider string `json:"provider"`
	// Model is the optional model identifier; empty falls back to the provider
	// default.
	Model string `json:"model,omitempty"`
	// Observations are the inputs to extract graphs from (one emit each).
	Observations []Observation `json:"observations"`
	// ExtractionSystemPrompt instructs the model to emit ONLY {nodes,edges} JSON.
	ExtractionSystemPrompt string `json:"extractionSystemPrompt"`
	// TripleJSONSchema is the JSON-Schema the emitted object must satisfy. Carried
	// as a raw object; passed to the provider as a constraint hint. The worker
	// does not run a full JSON-Schema validator (the platform is the source of
	// truth); structural validation against ExtractedNode/ExtractedEdge is the
	// defense-in-depth check.
	TripleJSONSchema map[string]any `json:"tripleJsonSchema"`
	// ResultEndpoint is the platform ingestion PATH the result is POSTed to (e.g.
	// "/api/factory/kg-extraction/results"). It is prefixed with the platform base
	// URL the runner already knows (see Executor.PlatformBaseURL).
	ResultEndpoint string `json:"resultEndpoint"`
	// ResultAuth is the runtime-JWT envelope. The worker re-verifies the org claim
	// and uses it as the bearer on the result POST.
	ResultAuth string `json:"resultAuth"`
}
