package span

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// Sender accepts a completed span for asynchronous delivery. Implementations
// must be non-blocking; Poster satisfies this contract.
type Sender interface {
	Send(agent.Span) bool
}

// SendFunc adapts a function to Sender. It is useful for tests and embedders
// that already own an observability pipeline.
type SendFunc func(agent.Span) bool

// Send implements Sender.
func (f SendFunc) Send(s agent.Span) bool { return f(s) }

// IDGenerator returns a lower-case hex identifier containing byteLen random
// bytes. Tests inject a deterministic implementation.
type IDGenerator func(byteLen int) (string, error)

// ProcessorConfig carries session-scoped correlation context. SessionID,
// OrgID, and WorkspaceID are required because the accepted span contract makes
// all three mandatory on every variant.
type ProcessorConfig struct {
	SessionID   string
	OrgID       string
	WorkspaceID string
	WorkType    string
	PoolID      string
	System      string
	Model       string

	// REN-2649 incoming trace correlation (optional). When present the
	// processor reuses the platform-minted trace ID and parents the session
	// root to the dispatch parent ID; absent preserves locally minted trace.
	Traceparent      string
	Tracestate       string
	SessionStorageID string
	SessionPublicID  string
	TrackerSessionID string

	Sender      Sender
	Now         func() time.Time
	IDGenerator IDGenerator
}

type pendingTool struct {
	traceID      string
	spanID       string
	parentSpanID string
	toolName     string
	toolUseID    string
	start        time.Time
}

// Processor stamps one trace-id spine across a session, enriches LLM/tool
// events with span correlation, and emits completed span variants. Process and
// Finish are safe for concurrent callers, although the runner normally invokes
// Process from its single event-consumer goroutine.
type Processor struct {
	mu sync.Mutex

	cfg ProcessorConfig

	traceID          string
	rootSpanID       string
	dispatchParentID string
	rootStart        time.Time
	turnStart        time.Time

	activeLlmSpanID  string
	activeLlmStart   time.Time
	activeLlmEmitted bool
	llmSinceResult   bool

	pendingTools   map[string]pendingTool
	pendingOrder   []string
	completedTools map[string]pendingTool
	finished       bool
}

// NewProcessor validates cfg and allocates the session trace/root ids.
func NewProcessor(cfg ProcessorConfig) (*Processor, error) {
	if cfg.SessionID == "" {
		return nil, errors.New("runtime/span: SessionID required")
	}
	if cfg.OrgID == "" {
		return nil, errors.New("runtime/span: OrgID required")
	}
	if cfg.WorkspaceID == "" {
		return nil, errors.New("runtime/span: WorkspaceID required")
	}
	if cfg.Sender == nil {
		return nil, errors.New("runtime/span: Sender required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.IDGenerator == nil {
		cfg.IDGenerator = randomHexID
	}
	// REN-2649: reuse incoming W3C traceparent when valid; otherwise mint.
	var traceID, dispatchParentID string
	if cfg.Traceparent != "" {
		if tid, pid, ok := parseTraceparent(cfg.Traceparent); ok {
			traceID = tid
			dispatchParentID = pid
		}
	}
	if traceID == "" {
		var err error
		traceID, err = cfg.IDGenerator(16)
		if err != nil {
			return nil, fmt.Errorf("runtime/span: generate trace id: %w", err)
		}
		if !validHexID(traceID, 16) {
			return nil, errors.New("runtime/span: IDGenerator returned invalid hex id")
		}
	}
	rootID, err := cfg.IDGenerator(8)
	if err != nil {
		return nil, fmt.Errorf("runtime/span: generate root span id: %w", err)
	}
	if !validHexID(rootID, 8) {
		return nil, errors.New("runtime/span: IDGenerator returned invalid hex id")
	}
	now := cfg.Now()
	return &Processor{
		cfg:                cfg,
		traceID:            traceID,
		rootSpanID:         rootID,
		dispatchParentID:   dispatchParentID,
		rootStart:          now,
		turnStart:          now,
		pendingTools:       make(map[string]pendingTool),
		completedTools:     make(map[string]pendingTool),
	}, nil
}

// Process enriches ev and returns the event sequence the runner should persist
// and observe. Usually the sequence has one element. When a terminal
// ResultEvent arrives without a provider LlmCallEvent since the prior result,
// Process prepends one explicit synthetic/aggregate LlmCallEvent. Aggregate
// counts are copied as reported and are never divided across guessed calls.
func (p *Processor) Process(ev agent.Event) []agent.Event {
	if ev == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return []agent.Event{ev}
	}

	now := p.cfg.Now()
	switch e := ev.(type) {
	case agent.InitEvent:
		p.turnStart = now
		return []agent.Event{e}
	case agent.SystemEvent:
		if e.Subtype == "turn_started" {
			p.turnStart = now
			p.startProvisionalLlm(now)
		}
		return []agent.Event{e}
	case agent.LlmCallEvent:
		return []agent.Event{p.processLlm(e, now)}
	case agent.ToolUseEvent:
		return []agent.Event{p.processToolUse(e, now)}
	case agent.ToolResultEvent:
		return []agent.Event{p.processToolResult(e, now)}
	case agent.ResultEvent:
		out := make([]agent.Event, 0, 2)
		if !p.llmSinceResult {
			synthetic := aggregateLlmEvent(e, p.cfg.System, p.cfg.Model)
			out = append(out, p.processLlm(synthetic, now))
		}
		p.flushPendingTools(now, statusFromResult(e))
		p.activeLlmSpanID = ""
		p.activeLlmStart = time.Time{}
		p.activeLlmEmitted = false
		p.llmSinceResult = false
		p.turnStart = now
		out = append(out, e)
		return out
	default:
		return []agent.Event{ev}
	}
}

// Finish emits the whole-session root span and closes any orphaned tool spans.
// It is idempotent. status is the runner result status ("completed" is OK;
// every other non-empty terminal value is ERROR).
func (p *Processor) Finish(status, message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return
	}
	p.finished = true
	now := p.cfg.Now()
	rootStatus := agent.SpanStatus{Code: agent.StatusUnset}
	switch status {
	case "completed", "success", "passed":
		rootStatus.Code = agent.StatusOK
	case "":
		rootStatus.Code = agent.StatusUnset
	default:
		rootStatus.Code = agent.StatusError
		rootStatus.Message = message
	}
	p.flushPendingTools(now, rootStatus)
	p.cfg.Sender.Send(agent.SessionSpan{
		SpanCore: agent.SpanCore{
			TraceID:           p.traceID,
			SpanID:            p.rootSpanID,
			ParentSpanID:      p.dispatchParentID,
			Kind:              agent.SpanKindAgent,
			Name:              "agent session",
			StartTimeUnixNano: unixNanoString(p.rootStart),
			EndTimeUnixNano:   unixNanoString(now),
			Status:            rootStatus,
			Donmai:            p.extensions("", "", ""),
		},
		AgentProvider: p.cfg.System,
	})
}

func (p *Processor) processLlm(e agent.LlmCallEvent, now time.Time) agent.LlmCallEvent {
	if e.UsageSource == "" {
		e.UsageSource = agent.LlmUsageProvider
	}
	if e.Synthetic {
		// A synthetic event is aggregate by definition. Correct malformed
		// adapter input toward the conservative interpretation.
		e.UsageSource = agent.LlmUsageAggregate
	}

	if p.activeLlmSpanID == "" || p.activeLlmEmitted {
		p.startProvisionalLlm(now)
	}
	if validHexID(e.SpanID, 8) && !p.activeLlmEmitted {
		p.activeLlmSpanID = e.SpanID
	}
	e.TraceID = p.traceID
	e.SpanID = p.activeLlmSpanID
	e.ParentSpanID = p.rootSpanID
	if e.System == "" {
		e.System = p.cfg.System
	}
	if e.Model == "" {
		e.Model = p.cfg.Model
	}

	start := parseUnixNano(e.StartTimeUnixNano, p.activeLlmStart)
	end := parseUnixNano(e.EndTimeUnixNano, now)
	if end.Before(start) {
		end = start
	}
	e.StartTimeUnixNano = unixNanoString(start)
	e.EndTimeUnixNano = unixNanoString(end)

	status := statusFromFinishReason(e.FinishReason)
	name := "chat"
	if e.System != "" || e.Model != "" {
		name += " " + strings.Trim(strings.TrimSpace(e.System+"/"+e.Model), "/")
	}
	p.cfg.Sender.Send(agent.LlmCallSpan{
		SpanCore: agent.SpanCore{
			TraceID:           e.TraceID,
			SpanID:            e.SpanID,
			ParentSpanID:      e.ParentSpanID,
			Kind:              agent.SpanKindLLM,
			Name:              name,
			StartTimeUnixNano: e.StartTimeUnixNano,
			EndTimeUnixNano:   e.EndTimeUnixNano,
			Status:            status,
			Donmai: p.extensions(
				e.PromptHash,
				e.ContextHash,
				e.ModelSnapshotID,
			),
		},
		GenAI: agent.GenAIAttributes{
			System:                    e.System,
			RequestModel:              e.Model,
			UsageInputTokens:          e.InputTokens,
			UsageOutputTokens:         e.OutputTokens,
			UsageCacheReadInputTokens: e.CachedInputTokens,
			ResponseFinishReason:      e.FinishReason,
		},
	})
	p.activeLlmEmitted = true
	p.llmSinceResult = true
	return e
}

func (p *Processor) processToolUse(e agent.ToolUseEvent, now time.Time) agent.ToolUseEvent {
	p.ensureLlm(now)
	key := e.ToolUseID
	if key != "" {
		if pending, ok := p.pendingTools[key]; ok {
			e.TraceID = pending.traceID
			e.SpanID = pending.spanID
			e.ParentSpanID = pending.parentSpanID
			if pending.toolName == "" && e.ToolName != "" {
				pending.toolName = e.ToolName
				p.pendingTools[key] = pending
			}
			return e
		}
		if completed, ok := p.completedTools[key]; ok {
			e.TraceID = completed.traceID
			e.SpanID = completed.spanID
			e.ParentSpanID = completed.parentSpanID
			return e
		}
	}
	spanID := e.SpanID
	if !validHexID(spanID, 8) {
		spanID = p.mustID(8)
	}
	e.TraceID = p.traceID
	e.SpanID = spanID
	e.ParentSpanID = p.activeLlmSpanID
	if key == "" {
		key = spanID
	}
	p.pendingTools[key] = pendingTool{
		traceID:      e.TraceID,
		spanID:       e.SpanID,
		parentSpanID: e.ParentSpanID,
		toolName:     e.ToolName,
		toolUseID:    e.ToolUseID,
		start:        now,
	}
	p.pendingOrder = append(p.pendingOrder, key)
	return e
}

func (p *Processor) processToolResult(e agent.ToolResultEvent, now time.Time) agent.ToolResultEvent {
	key := e.ToolUseID
	if key != "" {
		if completed, ok := p.completedTools[key]; ok {
			e.TraceID = completed.traceID
			e.SpanID = completed.spanID
			e.ParentSpanID = completed.parentSpanID
			if e.ToolName == "" {
				e.ToolName = completed.toolName
			}
			return e
		}
	}
	pending, ok := p.pendingTools[key]
	if !ok && validHexID(e.SpanID, 8) {
		pending, ok = p.pendingTools[e.SpanID]
		key = e.SpanID
	}
	if !ok && e.ToolUseID == "" {
		key, pending, ok = p.firstPendingTool(e.ToolName)
	}
	if !ok {
		p.ensureLlm(now)
		spanID := e.SpanID
		if !validHexID(spanID, 8) {
			spanID = p.mustID(8)
		}
		pending = pendingTool{
			traceID:      p.traceID,
			spanID:       spanID,
			parentSpanID: p.activeLlmSpanID,
			toolName:     e.ToolName,
			toolUseID:    e.ToolUseID,
			start:        now,
		}
		key = e.ToolUseID
		if key == "" {
			key = spanID
		}
	}
	if e.ToolName == "" {
		e.ToolName = pending.toolName
	}
	e.TraceID = pending.traceID
	e.SpanID = pending.spanID
	e.ParentSpanID = pending.parentSpanID
	status := agent.SpanStatus{Code: agent.StatusOK}
	if e.IsError {
		status.Code = agent.StatusError
		status.Message = "tool execution failed"
	}
	p.emitTool(pending, e.ToolName, e.IsError, now, status)
	p.removePendingTool(key)
	if e.ToolUseID != "" {
		p.completedTools[e.ToolUseID] = pending
	}
	p.turnStart = now
	return e
}

func (p *Processor) emitTool(pending pendingTool, toolName string, isError bool, end time.Time, status agent.SpanStatus) {
	name := "execute_tool"
	if toolName != "" {
		name += " " + toolName
	}
	p.cfg.Sender.Send(agent.ToolSpan{
		SpanCore: agent.SpanCore{
			TraceID:           pending.traceID,
			SpanID:            pending.spanID,
			ParentSpanID:      pending.parentSpanID,
			Kind:              agent.SpanKindTool,
			Name:              name,
			StartTimeUnixNano: unixNanoString(pending.start),
			EndTimeUnixNano:   unixNanoString(end),
			Status:            status,
			Donmai:            p.extensions("", "", ""),
		},
		ToolName:  toolName,
		ToolUseID: pending.toolUseID,
		IsError:   isError,
	})
}

func (p *Processor) flushPendingTools(now time.Time, terminalStatus agent.SpanStatus) {
	for key, pending := range p.pendingTools {
		status := agent.SpanStatus{Code: agent.StatusUnset, Message: "tool result not observed"}
		isError := false
		if terminalStatus.Code == agent.StatusError {
			status = terminalStatus
			isError = true
		}
		p.emitTool(pending, pending.toolName, isError, now, status)
		if pending.toolUseID != "" {
			p.completedTools[pending.toolUseID] = pending
		}
		delete(p.pendingTools, key)
	}
	p.pendingOrder = p.pendingOrder[:0]
}

func (p *Processor) startProvisionalLlm(now time.Time) {
	p.activeLlmSpanID = p.mustID(8)
	p.activeLlmStart = p.turnStart
	if p.activeLlmStart.IsZero() {
		p.activeLlmStart = now
	}
	p.activeLlmEmitted = false
}

func (p *Processor) ensureLlm(now time.Time) {
	if p.activeLlmSpanID == "" {
		p.startProvisionalLlm(now)
	}
}

func (p *Processor) firstPendingTool(toolName string) (string, pendingTool, bool) {
	for _, key := range p.pendingOrder {
		pending, ok := p.pendingTools[key]
		if !ok {
			continue
		}
		if toolName == "" || pending.toolName == "" || pending.toolName == toolName {
			return key, pending, true
		}
	}
	return "", pendingTool{}, false
}

func (p *Processor) removePendingTool(key string) {
	delete(p.pendingTools, key)
	for i, candidate := range p.pendingOrder {
		if candidate == key {
			p.pendingOrder = append(p.pendingOrder[:i], p.pendingOrder[i+1:]...)
			return
		}
	}
}

func (p *Processor) mustID(byteLen int) string {
	id, err := p.cfg.IDGenerator(byteLen)
	if err != nil || !validHexID(id, byteLen) {
		// NewProcessor validates the generator. A later injected-generator
		// failure cannot be returned through Process, so derive a valid,
		// process-local fallback from the current clock instead of emitting an
		// invalid wire id.
		n := p.cfg.Now().UnixNano()
		return fmt.Sprintf("%0*x", byteLen*2, uint64(n))[:byteLen*2]
	}
	return id
}

func (p *Processor) extensions(promptHash, contextHash, modelSnapshotID string) agent.DonmaiSpanExtensions {
	sessionStorageID := p.cfg.SessionStorageID
	if sessionStorageID == "" {
		sessionStorageID = p.cfg.SessionID
	}
	return agent.DonmaiSpanExtensions{
		OrgID:            p.cfg.OrgID,
		WorkspaceID:      p.cfg.WorkspaceID,
		SessionID:        p.cfg.SessionID,
		WorkType:         p.cfg.WorkType,
		PoolID:           p.cfg.PoolID,
		PromptHash:       promptHash,
		ContextHash:      contextHash,
		ModelSnapshotID:  modelSnapshotID,
		SessionStorageID: sessionStorageID,
		SessionPublicID:  p.cfg.SessionPublicID,
		TrackerSessionID: p.cfg.TrackerSessionID,
	}
}

func aggregateLlmEvent(result agent.ResultEvent, system, model string) agent.LlmCallEvent {
	e := agent.LlmCallEvent{
		System:      system,
		Model:       model,
		UsageSource: agent.LlmUsageAggregate,
		Synthetic:   true,
	}
	if result.Cost != nil {
		e.InputTokens = result.Cost.InputTokens
		e.OutputTokens = result.Cost.OutputTokens
		e.CachedInputTokens = result.Cost.CachedInputTokens
	}
	switch {
	case result.Success:
		e.FinishReason = "stop"
	case result.ErrorSubtype != "":
		e.FinishReason = result.ErrorSubtype
	default:
		e.FinishReason = "error"
	}
	return e
}

func statusFromResult(result agent.ResultEvent) agent.SpanStatus {
	if result.Success {
		return agent.SpanStatus{Code: agent.StatusOK}
	}
	message := result.ErrorSubtype
	if len(result.Errors) > 0 && result.Errors[0] != "" {
		message = result.Errors[0]
	}
	return agent.SpanStatus{Code: agent.StatusError, Message: message}
}

func statusFromFinishReason(reason string) agent.SpanStatus {
	lower := strings.ToLower(reason)
	if strings.Contains(lower, "error") || strings.Contains(lower, "fail") ||
		strings.Contains(lower, "interrupt") || strings.Contains(lower, "safety") ||
		strings.Contains(lower, "max_token") || strings.Contains(lower, "length") {
		return agent.SpanStatus{Code: agent.StatusError, Message: reason}
	}
	return agent.SpanStatus{Code: agent.StatusOK}
}

func parseUnixNano(raw string, fallback time.Time) time.Time {
	if raw == "" {
		return fallback
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return time.Unix(0, n)
}

func unixNanoString(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

func randomHexID(byteLen int) (string, error) {
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func validHexID(id string, byteLen int) bool {
	if len(id) != byteLen*2 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func parseTraceparent(tp string) (traceID, parentID string, ok bool) {
	parts := strings.Split(tp, "-")
	if len(parts) != 4 {
		return "", "", false
	}
	if parts[0] != "00" {
		return "", "", false
	}
	if !validHexID(parts[1], 16) || !validHexID(parts[2], 8) {
		return "", "", false
	}
	if parts[1] == strings.Repeat("0", 32) || parts[2] == strings.Repeat("0", 16) {
		return "", "", false
	}
	if parts[3] != "01" && parts[3] != "00" {
		return "", "", false
	}
	// Lowercase canonical.
	if parts[1] != strings.ToLower(parts[1]) || parts[2] != strings.ToLower(parts[2]) {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// ProviderSystem returns the best provider/system identifier available at the
// runner boundary. Direct adapters override this when their native response
// carries a more precise provider. Values follow the accepted span contract;
// custom harness identifiers remain valid for provider-agnostic wrappers.
func ProviderSystem(provider agent.ProviderName) string {
	switch provider {
	case agent.ProviderClaude:
		return "anthropic"
	case agent.ProviderCodex:
		return "openai"
	case agent.ProviderGemini, agent.ProviderAGYCLI:
		return "gcp.gemini"
	case agent.ProviderOllama:
		return "ollama"
	case agent.ProviderOpenCode:
		return "opencode"
	case agent.ProviderAmp:
		return "amp"
	case agent.ProviderStub:
		return "stub"
	default:
		return string(provider)
	}
}
