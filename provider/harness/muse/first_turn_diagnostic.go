package muse

import (
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/RenseiAI/donmai/agent"
)

// firstTurnDiagnosticMaxBytes caps every first-turn diagnostic payload at
// 2 KiB. The shape carries only allowlisted names, counts, presence
// flags and validated tokens — never raw prompt, path, URL, header,
// environment or free-form host text — so a compliant payload is far
// smaller; the cap is the backstop that keeps an oversize or duplicated
// delivery bounded.
const firstTurnDiagnosticMaxBytes = 2048

// firstTurnRequestShape is the bounded summary of the actual outgoing
// session/first-turn RPC shape kept per Handle. Every field is safe by
// construction: known parameter names, input counts and validated
// types, presence flags and bounded counts. Raw prompt, display text,
// paths, server names, URLs, headers, environment values and arbitrary
// configuration never enter this struct.
type firstTurnRequestShape struct {
	SessionMethod string   `json:"sessionMethod"`
	SessionKeys   []string `json:"sessionKeys"`

	ModelID      string `json:"modelId,omitempty"`
	ApprovalMode string `json:"approvalMode,omitempty"`
	ProviderID   string `json:"providerId,omitempty"`

	HasWorkspaceRoot bool `json:"hasWorkspaceRoot"`
	HasConfig        bool `json:"hasConfig"`
	MCPServerCount   int  `json:"mcpServerCount"`

	TurnKeys            []string `json:"turnKeys"`
	InputPartCount      int      `json:"inputPartCount"`
	InputPartTypes      []string `json:"inputPartTypes"`
	HasDisplayText      bool     `json:"hasDisplayText"`
	WorkspaceRootsCount int      `json:"workspaceRootsCount"`
	ReasoningEffort     string   `json:"reasoningEffort,omitempty"`
	IfBusy              string   `json:"ifBusy,omitempty"`
}

// firstTurnDiagnosticData is the wire-safe diagnostic envelope: the
// request shape plus safely filtered error classification. Unknown free
// text is omitted, never copied wholesale.
type firstTurnDiagnosticData struct {
	Shape firstTurnRequestShape `json:"shape"`

	ErrorKind             string `json:"errorKind,omitempty"`
	ErrorRetryable        *bool  `json:"errorRetryable,omitempty"`
	ErrorHasQuota         bool   `json:"errorHasQuota,omitempty"`
	ErrorHasRequestDenied bool   `json:"errorHasRequestDenied,omitempty"`
	ErrorHasReason        bool   `json:"errorHasReason,omitempty"`
	ErrorMethod           string `json:"errorMethod,omitempty"`
	ErrorCode             *int   `json:"errorCode,omitempty"`
}

// firstTurnShapeState holds the per-handle bounded summary. It lives on
// Handle so Spawn and Resume share one summary per handle and emit at
// most one diagnostic per first-turn outcome.
type firstTurnShapeState struct {
	mu            sync.Mutex
	sessionMethod string
	shape         firstTurnRequestShape
	shapeJSON     string
	firstTurnID   string
	emitted       bool
}

var allowedSessionStartKeys = map[string]bool{
	"approvalMode":  true,
	"commandId":     true,
	"config":        true,
	"modelId":       true,
	"providerId":    true,
	"sessionId":     true,
	"workspaceRoot": true,
}

var allowedSessionResumeKeys = map[string]bool{
	"sessionId":    true,
	"commandId":    true,
	"excludeItems": true,
	"config":       true,
}

var allowedTurnStartKeys = map[string]bool{
	"sessionId":       true,
	"commandId":       true,
	"input":           true,
	"ifBusy":          true,
	"displayText":     true,
	"workspaceRoots":  true,
	"reasoningEffort": true,
}

var allowedInputPartTypes = map[string]bool{
	string(TurnInputText):  true,
	string(TurnInputImage): true,
	string(TurnInputSkill): true,
	string(TurnInputVideo): true,
}

var allowedApprovalModes = map[string]bool{
	string(ApprovalModeAllowAll):        true,
	string(ApprovalModePromptUnmatched): true,
	string(ApprovalModeOnRequest):       true,
	string(ApprovalModeDenyUnmatched):   true,
}

var allowedReasoningEfforts = map[string]bool{
	string(ReasoningEffortMinimal): true,
	string(ReasoningEffortLow):     true,
	string(ReasoningEffortMedium):  true,
	string(ReasoningEffortHigh):    true,
	string(ReasoningEffortXHigh):   true,
	string(ReasoningEffortMax):     true,
}

var allowedProviderIDs = map[string]bool{
	ProviderID: true,
}

var allowedIfBusy = map[string]bool{
	string(IfBusyQueue):   true,
	string(IfBusySteer):   true,
	string(IfBusyReplace): true,
}

var allowedSessionMethods = map[string]bool{
	"session/start":  true,
	"session/resume": true,
}

var allowedFirstTurnRPCMethods = map[string]bool{
	"session/start":  true,
	"session/resume": true,
	"turn/start":     true,
}

// allowedErrorKinds is the closed vocabulary a diagnostic may repeat.
// Every ErrorKind wire value plus the adapter's own turn_failed
// default. Anything else — including a sentinel planted in host error
// text — maps to "unknown" and the raw text is never logged.
var allowedErrorKinds = func() map[string]bool {
	kinds := []string{
		string(ErrorKindSessionNotFound),
		string(ErrorKindNotFound),
		string(ErrorKindParseError),
		string(ErrorKindInvalidRequest),
		string(ErrorKindNotInitialized),
		string(ErrorKindAlreadyInitialized),
		string(ErrorKindMethodNotFound),
		string(ErrorKindInvalidParams),
		string(ErrorKindExperimentalRequired),
		string(ErrorKindInternal),
		string(ErrorKindPageEventTooLarge),
		string(ErrorKindOutputResultTooLarge),
		string(ErrorKindOverloaded),
		string(ErrorKindInputTooLarge),
		string(ErrorKindCapabilityRequired),
		string(ErrorKindInterrupted),
		string(ErrorKindCancelled),
		string(ErrorKindSessionInUse),
		string(ErrorKindSessionAmbiguous),
		string(ErrorKindForkBoundaryInvalid),
		string(ErrorKindSessionNotLoaded),
		string(ErrorKindSessionStreamMismatch),
		string(ErrorKindCommandRejected),
		string(ErrorKindBackpressured),
		string(ErrorKindSkillNotFound),
		string(ErrorKindComputerUseUnavailable),
		string(ErrorKindMarketplaceNotFound),
		string(ErrorKindPluginNotFound),
		string(ErrorKindMarketplaceAlreadyExists),
		string(ErrorKindPluginMutationsDisabled),
		string(ErrorKindUploadNotFound),
		string(ErrorKindViewTruncated),
		string(ErrorKindOutputUnavailable),
		string(ErrorKindBoundaryPruned),
		string(ErrorKindBoundaryUnusable),
		string(ErrorKindNoBoundary),
		string(ErrorKindApprovalNotFound),
		string(ErrorKindApprovalAlreadyResolved),
		string(ErrorKindApprovalChoiceInvalid),
		string(ErrorKindApprovalRequirementStale),
		string(ErrorKindApprovalReviewerUnavailable),
		string(ErrorKindUserInputNotFound),
		string(ErrorKindUserInputAlreadySettled),
		string(ErrorKindUserInputAnswerInvalid),
		"turn_failed",
	}
	out := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		out[k] = true
	}
	return out
}()

func validatedModelID(v any) (string, bool) {
	s, ok := v.(string)
	if !ok || s == "" || len(s) > 128 {
		return "", false
	}
	for _, r := range s {
		isLower := r >= 'a' && r <= 'z'
		isUpper := r >= 'A' && r <= 'Z'
		isDigit := r >= '0' && r <= '9'
		if !(isLower || isUpper || isDigit || r == '.' || r == '_' || r == '-') {
			return "", false
		}
	}
	hasAlnum := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			hasAlnum = true
			break
		}
	}
	if !hasAlnum {
		return "", false
	}
	return s, true
}

func validatedToken(v any, allow map[string]bool) (string, bool) {
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	if !allow[s] {
		return "", false
	}
	return s, true
}

func validatedErrorKind(kind string) string {
	if kind == "" {
		return "unknown"
	}
	if allowedErrorKinds[kind] {
		return kind
	}
	return "unknown"
}

func validatedSessionMethod(method string) string {
	if allowedSessionMethods[method] {
		return method
	}
	return "unknown"
}

func validatedRPCMethod(method string) string {
	if allowedFirstTurnRPCMethods[method] {
		return method
	}
	return "unknown"
}

func validatedInputPartType(t string) string {
	if allowedInputPartTypes[t] {
		return t
	}
	return "unknown"
}

func filterKeys(params map[string]any, allow map[string]bool) []string {
	keys := make([]string, 0, len(params))
	for k := range params {
		if allow[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func countMcpServers(params map[string]any) (bool, int) {
	raw, ok := params["config"]
	if !ok {
		return false, 0
	}
	cfg, ok := raw.(map[string]any)
	if !ok {
		return true, 0
	}
	servers, ok := cfg["mcpServers"]
	if !ok {
		return true, 0
	}
	switch m := servers.(type) {
	case map[string]SessionMCPServerConfig:
		return true, boundedCount(len(m))
	case map[string]any:
		return true, boundedCount(len(m))
	case []any:
		return true, boundedCount(len(m))
	default:
		return true, 0
	}
}

func summarizeInput(params map[string]any) (int, []string) {
	raw, ok := params["input"]
	if !ok {
		return 0, nil
	}
	var parts []map[string]any
	switch in := raw.(type) {
	case []map[string]any:
		parts = in
	case []any:
		for _, p := range in {
			if m, ok := p.(map[string]any); ok {
				parts = append(parts, m)
			}
		}
	default:
		return 0, nil
	}
	// Bound the type list: the production builder emits at most a
	// handful of parts; anything beyond the cap still counts but only
	// the first entries name their validated type.
	const maxTypes = 16
	types := make([]string, 0, len(parts))
	for i, p := range parts {
		if i >= maxTypes {
			break
		}
		t, _ := p["type"].(string)
		types = append(types, validatedInputPartType(t))
	}
	return boundedCount(len(parts)), types
}

func countWorkspaceRoots(params map[string]any) int {
	raw, ok := params["workspaceRoots"]
	if !ok {
		return 0
	}
	switch v := raw.(type) {
	case []string:
		return boundedCount(len(v))
	case []any:
		return boundedCount(len(v))
	default:
		return 0
	}
}

func boundedCount(n int) int {
	if n < 0 {
		return 0
	}
	if n > 9999 {
		return 9999
	}
	return n
}

// buildFirstTurnShape summarizes the actual outgoing params maps into
// the bounded allowlisted shape. Only validated tokens and counts
// survive; every raw value stays out.
func buildFirstTurnShape(sessionMethod string, sessionParams, turnParams map[string]any) firstTurnRequestShape {
	shape := firstTurnRequestShape{SessionMethod: validatedSessionMethod(sessionMethod)}
	if sessionParams != nil {
		allow := allowedSessionStartKeys
		if sessionMethod == "session/resume" {
			allow = allowedSessionResumeKeys
		}
		shape.SessionKeys = filterKeys(sessionParams, allow)
		if model, ok := validatedModelID(sessionParams["modelId"]); ok {
			shape.ModelID = model
		}
		if mode, ok := validatedToken(sessionParams["approvalMode"], allowedApprovalModes); ok {
			shape.ApprovalMode = mode
		}
		if provider, ok := validatedToken(sessionParams["providerId"], allowedProviderIDs); ok {
			shape.ProviderID = provider
		}
		if _, ok := sessionParams["workspaceRoot"]; ok {
			shape.HasWorkspaceRoot = true
		}
		if hasConfig, count := countMcpServers(sessionParams); hasConfig {
			shape.HasConfig = true
			shape.MCPServerCount = count
		} else if _, ok := sessionParams["config"]; ok {
			shape.HasConfig = true
		}
	}
	if turnParams != nil {
		shape.TurnKeys = filterKeys(turnParams, allowedTurnStartKeys)
		count, types := summarizeInput(turnParams)
		shape.InputPartCount = count
		shape.InputPartTypes = types
		if _, ok := turnParams["displayText"]; ok {
			shape.HasDisplayText = true
		}
		shape.WorkspaceRootsCount = countWorkspaceRoots(turnParams)
		if effort, ok := validatedToken(turnParams["reasoningEffort"], allowedReasoningEfforts); ok {
			shape.ReasoningEffort = effort
		}
		if busy, ok := validatedToken(turnParams["ifBusy"], allowedIfBusy); ok {
			shape.IfBusy = busy
		}
	}
	if shape.SessionKeys == nil {
		shape.SessionKeys = []string{}
	}
	if shape.TurnKeys == nil {
		shape.TurnKeys = []string{}
	}
	if shape.InputPartTypes == nil {
		shape.InputPartTypes = []string{}
	}
	return shape
}

// encodeFirstTurnDiagnostic marshals the diagnostic envelope and caps
// it at 2 KiB. Truncation keeps the emission bounded; the shape is
// small by construction so truncation only fires on misuse.
func encodeFirstTurnDiagnostic(shape firstTurnRequestShape, errData *firstTurnDiagnosticData) string {
	data := firstTurnDiagnosticData{Shape: shape}
	if errData != nil {
		data.ErrorKind = errData.ErrorKind
		data.ErrorRetryable = errData.ErrorRetryable
		data.ErrorHasQuota = errData.ErrorHasQuota
		data.ErrorHasRequestDenied = errData.ErrorHasRequestDenied
		data.ErrorHasReason = errData.ErrorHasReason
		data.ErrorMethod = errData.ErrorMethod
		data.ErrorCode = errData.ErrorCode
	}
	buf, err := json.Marshal(data)
	if err != nil {
		return `{"shape":{"sessionMethod":"unknown","sessionKeys":[],"turnKeys":[],"inputPartTypes":[]}}`
	}
	if len(buf) > firstTurnDiagnosticMaxBytes {
		buf = buf[:firstTurnDiagnosticMaxBytes]
	}
	return string(buf)
}

// firstTurnSyncError classifies a synchronous session/turn RPC failure
// without copying free text. The RPC message, timeout string and any
// host body stay out; only the validated method, code and kind travel.
func firstTurnSyncError(rpcMethod string, err error) *firstTurnDiagnosticData {
	out := &firstTurnDiagnosticData{ErrorMethod: validatedRPCMethod(rpcMethod)}
	if rpc := rpcErrorOf(err); rpc != nil {
		code := rpc.Code
		out.ErrorCode = &code
		out.ErrorKind = validatedErrorKind(ErrorKindOf(rpc))
		return out
	}
	out.ErrorKind = validatedErrorKind("")
	return out
}

func rpcErrorOf(err error) *RPCError {
	if err == nil {
		return nil
	}
	var rpc *RPCError
	if errors.As(err, &rpc) {
		return rpc
	}
	type joinUnwrapper interface{ Unwrap() []error }
	if j, ok := err.(joinUnwrapper); ok {
		for _, inner := range j.Unwrap() {
			if found := rpcErrorOf(inner); found != nil {
				return found
			}
		}
	}
	type causer interface{ Unwrap() error }
	if c, ok := err.(causer); ok {
		if found := rpcErrorOf(c.Unwrap()); found != nil {
			return found
		}
	}
	return nil
}

// firstTurnAsyncError classifies an asynchronous turn/completed failure.
// Message and reason text never travel; only the validated kind,
// retryable flag and presence flags do.
func firstTurnAsyncError(terminal TurnTerminal, turnErr *TurnError, reason *string) *firstTurnDiagnosticData {
	out := &firstTurnDiagnosticData{ErrorMethod: "turn/start"}
	if turnErr != nil {
		out.ErrorKind = validatedErrorKind(turnErr.Kind)
		retryable := turnErr.Retryable
		out.ErrorRetryable = &retryable
		out.ErrorHasQuota = turnErr.Quota != nil
		out.ErrorHasRequestDenied = turnErr.RequestDenied != nil && *turnErr.RequestDenied
		return out
	}
	if reason != nil && strings.TrimSpace(*reason) != "" {
		out.ErrorHasReason = true
	}
	if terminal == TurnTerminalCancelled {
		out.ErrorKind = validatedErrorKind(string(ErrorKindCancelled))
		return out
	}
	out.ErrorKind = validatedErrorKind("turn_failed")
	return out
}

// turnParamsFromPlan renders a turn/start-equivalent params map from the
// handle plan so a session/start failure still carries a comparable
// turn half. Only the summary survives; ids and roots are presence and
// counts downstream.
func turnParamsFromPlan(spec agent.Spec, plan SpawnPlan, workspaceRoot string) map[string]any {
	roots := []string{}
	if workspaceRoot != "" {
		roots = []string{workspaceRoot}
	}
	return turnStartParams("session-id", "command-id", spec, plan.PromptInput, roots)
}

// recordSessionShape stores the session half of the first-turn summary
// from the actual outgoing params. It also pre-fills the turn half
// from the handle plan so a session/start failure still emits a
// comparable shape.
func (h *Handle) recordSessionShape(method string, params map[string]any) {
	if h == nil {
		return
	}
	h.firstTurn.mu.Lock()
	defer h.firstTurn.mu.Unlock()
	turnParams := turnParamsFromPlan(h.spec, h.plan, h.plan.WorkspaceRoot)
	h.firstTurn.sessionMethod = validatedSessionMethod(method)
	shape := buildFirstTurnShape(method, params, turnParams)
	h.firstTurn.shape = shape
	h.firstTurn.shapeJSON = encodeFirstTurnDiagnostic(shape, nil)
}

// recordTurnShape replaces the turn half with the actual outgoing
// turn/start params and records the foreground turn id.
func (h *Handle) recordTurnShape(params map[string]any, turnID string) {
	if h == nil {
		return
	}
	h.firstTurn.mu.Lock()
	defer h.firstTurn.mu.Unlock()
	shape := h.firstTurn.shape
	turnShape := buildFirstTurnShape(h.firstTurn.sessionMethod, nil, params)
	shape.TurnKeys = turnShape.TurnKeys
	shape.InputPartCount = turnShape.InputPartCount
	shape.InputPartTypes = turnShape.InputPartTypes
	shape.HasDisplayText = turnShape.HasDisplayText
	shape.WorkspaceRootsCount = turnShape.WorkspaceRootsCount
	shape.ReasoningEffort = turnShape.ReasoningEffort
	shape.IfBusy = turnShape.IfBusy
	if h.firstTurn.sessionMethod != "" {
		shape.SessionMethod = h.firstTurn.sessionMethod
	}
	if shape.SessionKeys == nil {
		shape.SessionKeys = []string{}
	}
	h.firstTurn.shape = shape
	h.firstTurn.shapeJSON = encodeFirstTurnDiagnostic(shape, nil)
	if turnID != "" && h.firstTurn.firstTurnID == "" {
		h.firstTurn.firstTurnID = turnID
	}
}

func (h *Handle) storedFirstTurnShape() (firstTurnRequestShape, string) {
	if h == nil {
		return firstTurnRequestShape{}, ""
	}
	h.firstTurn.mu.Lock()
	defer h.firstTurn.mu.Unlock()
	return h.firstTurn.shape, h.firstTurn.shapeJSON
}

func (h *Handle) markFirstTurnEmitted() bool {
	if h == nil {
		return true
	}
	h.firstTurn.mu.Lock()
	defer h.firstTurn.mu.Unlock()
	if h.firstTurn.emitted {
		return false
	}
	h.firstTurn.emitted = true
	return true
}

// emitFirstTurnSyncFailure logs one bounded warn diagnostic for a
// synchronous session/start, session/resume or turn/start failure. It
// never changes the returned error: the terminal result contract is
// preserved.
func (h *Handle) emitFirstTurnSyncFailure(rpcMethod string, err error) {
	if h == nil || !h.markFirstTurnEmitted() {
		return
	}
	shape, _ := h.storedFirstTurnShape()
	classified := firstTurnSyncError(rpcMethod, err)
	payload := encodeFirstTurnDiagnostic(shape, classified)
	slog.Warn("muse: first-turn request shape",
		"sessionMethod", shape.SessionMethod,
		"shape", payload,
		"errorKind", classified.ErrorKind,
		"errorMethod", classified.ErrorMethod,
	)
}

// emitFirstTurnAsyncCompletion logs one bounded diagnostic for the
// first asynchronous turn completion: warn on failure, debug on
// success. Duplicate completions stay silent via the emitted flag.
// The turnID gates the emission to the acknowledged first turn: a
// completion for any other turn (for example an older turn finishing
// after a queued first turn was recorded) emits no diagnostic and
// consumes no flag, so the first turn's own completion still reports.
func (h *Handle) emitFirstTurnAsyncCompletion(turnID string, terminal TurnTerminal, turnErr *TurnError, reason *string) {
	if h == nil {
		return
	}
	switch terminal {
	case TurnTerminalCompleted, TurnTerminalFailed, TurnTerminalCancelled:
	default:
		return
	}
	h.firstTurn.mu.Lock()
	firstID := h.firstTurn.firstTurnID
	h.firstTurn.mu.Unlock()
	if firstID != "" && turnID != "" && firstID != turnID {
		return
	}
	if !h.markFirstTurnEmitted() {
		return
	}
	shape, _ := h.storedFirstTurnShape()
	if terminal == TurnTerminalCompleted {
		payload := encodeFirstTurnDiagnostic(shape, nil)
		slog.Debug("muse: first-turn request shape",
			"sessionMethod", shape.SessionMethod,
			"shape", payload,
		)
		return
	}
	classified := firstTurnAsyncError(terminal, turnErr, reason)
	payload := encodeFirstTurnDiagnostic(shape, classified)
	slog.Warn("muse: first-turn request shape",
		"sessionMethod", shape.SessionMethod,
		"shape", payload,
		"errorKind", classified.ErrorKind,
		"errorMethod", classified.ErrorMethod,
	)
}
