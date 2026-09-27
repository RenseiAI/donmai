package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// This file carries the execution-security vocabulary of
// ADR-2026-09-27-execution-security-levels.md: six ordered dimensions, the
// closed ladder of each (index 0 the weakest), the queued-work wire section
// that stamps the effective levels on a session, the per-dimension report an
// applied adaptation records, and the closed refusal codes.
//
// The runner never invents a level. A queued work item without the section is
// read as index 0 on every dimension because that is the wire compatibility
// rule (the ADR's D4), not a default the runner chose; a section that is
// present but malformed, or that names an unknown dimension or level, is
// refused rather than guessed.

// ExecutionSecurityDimension names one execution-security dimension.
type ExecutionSecurityDimension string

// The six dimensions, in their canonical order.
const (
	ExecutionSecurityToolApproval ExecutionSecurityDimension = "toolApproval"
	ExecutionSecurityFileRead     ExecutionSecurityDimension = "fileRead"
	ExecutionSecurityFileWrite    ExecutionSecurityDimension = "fileWrite"
	ExecutionSecurityNetwork      ExecutionSecurityDimension = "network"
	ExecutionSecurityCredentials  ExecutionSecurityDimension = "credentials"
	ExecutionSecurityIsolation    ExecutionSecurityDimension = "isolation"
)

// ExecutionSecurityLevel is one rung of a dimension's ladder. Level strings
// are only meaningful together with their dimension ("host" is a rung of both
// fileRead and fileWrite; "allow-list" of both toolApproval and network).
type ExecutionSecurityLevel string

// Ladder rungs, weakest first within each dimension.
const (
	ToolApprovalBypass                 ExecutionSecurityLevel = "bypass"
	ToolApprovalDenyList               ExecutionSecurityLevel = "deny-list"
	ToolApprovalAllowList              ExecutionSecurityLevel = "allow-list"
	ToolApprovalHumanApprovalForWrites ExecutionSecurityLevel = "human-approval-for-writes"

	FileReadHost             ExecutionSecurityLevel = "host"
	FileReadHomeMinusSecrets ExecutionSecurityLevel = "home-minus-secrets" //nolint:gosec // G101: a ladder level name, not a credential.
	FileReadWorkarea         ExecutionSecurityLevel = "workarea"

	FileWriteHost     ExecutionSecurityLevel = "host"
	FileWriteWorkarea ExecutionSecurityLevel = "workarea"

	NetworkOpen      ExecutionSecurityLevel = "open"
	NetworkLogged    ExecutionSecurityLevel = "logged"
	NetworkAllowList ExecutionSecurityLevel = "allow-list"
	NetworkNone      ExecutionSecurityLevel = "none"

	CredentialsAmbientHostLogin ExecutionSecurityLevel = "ambient-host-login" //nolint:gosec // G101: a ladder level name, not a credential.
	CredentialsInjectedOnly     ExecutionSecurityLevel = "injected-only"
	CredentialsShortLivedScoped ExecutionSecurityLevel = "short-lived-scoped"

	IsolationHostUser  ExecutionSecurityLevel = "host-user"
	IsolationOSSandbox ExecutionSecurityLevel = "os-sandbox"
	IsolationContainer ExecutionSecurityLevel = "container"
	IsolationMicroVM   ExecutionSecurityLevel = "microvm"
)

// executionSecurityLadders is the closed vocabulary. Each ladder is ordered
// weakest first; a stronger level permits a subset of what a weaker one does.
var executionSecurityLadders = map[ExecutionSecurityDimension][]ExecutionSecurityLevel{
	ExecutionSecurityToolApproval: {ToolApprovalBypass, ToolApprovalDenyList, ToolApprovalAllowList, ToolApprovalHumanApprovalForWrites},
	ExecutionSecurityFileRead:     {FileReadHost, FileReadHomeMinusSecrets, FileReadWorkarea},
	ExecutionSecurityFileWrite:    {FileWriteHost, FileWriteWorkarea},
	ExecutionSecurityNetwork:      {NetworkOpen, NetworkLogged, NetworkAllowList, NetworkNone},
	ExecutionSecurityCredentials:  {CredentialsAmbientHostLogin, CredentialsInjectedOnly, CredentialsShortLivedScoped},
	ExecutionSecurityIsolation:    {IsolationHostUser, IsolationOSSandbox, IsolationContainer, IsolationMicroVM},
}

// ExecutionSecurityDimensions returns the six dimensions in canonical order.
func ExecutionSecurityDimensions() []ExecutionSecurityDimension {
	return []ExecutionSecurityDimension{
		ExecutionSecurityToolApproval, ExecutionSecurityFileRead, ExecutionSecurityFileWrite,
		ExecutionSecurityNetwork, ExecutionSecurityCredentials, ExecutionSecurityIsolation,
	}
}

// ExecutionSecurityLadder returns a copy of dimension's ladder, weakest first,
// or nil for an unknown dimension.
func ExecutionSecurityLadder(dimension ExecutionSecurityDimension) []ExecutionSecurityLevel {
	ladder, ok := executionSecurityLadders[dimension]
	if !ok {
		return nil
	}
	return append([]ExecutionSecurityLevel(nil), ladder...)
}

// ExecutionSecurityLevelIndex returns level's position on dimension's ladder
// (0 is the weakest). ok is false for an unknown dimension or level.
func ExecutionSecurityLevelIndex(dimension ExecutionSecurityDimension, level ExecutionSecurityLevel) (index int, ok bool) {
	for i, candidate := range executionSecurityLadders[dimension] {
		if candidate == level {
			return i, true
		}
	}
	return 0, false
}

// ExecutionSecurityLevels holds one level per dimension.
type ExecutionSecurityLevels struct {
	ToolApproval ExecutionSecurityLevel `json:"toolApproval"`
	FileRead     ExecutionSecurityLevel `json:"fileRead"`
	FileWrite    ExecutionSecurityLevel `json:"fileWrite"`
	Network      ExecutionSecurityLevel `json:"network"`
	Credentials  ExecutionSecurityLevel `json:"credentials"`
	Isolation    ExecutionSecurityLevel `json:"isolation"`
}

// IndexZeroExecutionSecurityLevels returns the weakest rung of every ladder.
// It exists for exactly one rule: data a peer omitted (a work item with no
// levels section, a receipt with no report) reads as index 0. It is never a
// fallback for a malformed or unreadable value.
func IndexZeroExecutionSecurityLevels() ExecutionSecurityLevels {
	var levels ExecutionSecurityLevels
	for _, dimension := range ExecutionSecurityDimensions() {
		levels.set(dimension, executionSecurityLadders[dimension][0])
	}
	return levels
}

// Level returns the level held for dimension ("" for an unknown dimension).
func (l ExecutionSecurityLevels) Level(dimension ExecutionSecurityDimension) ExecutionSecurityLevel {
	switch dimension {
	case ExecutionSecurityToolApproval:
		return l.ToolApproval
	case ExecutionSecurityFileRead:
		return l.FileRead
	case ExecutionSecurityFileWrite:
		return l.FileWrite
	case ExecutionSecurityNetwork:
		return l.Network
	case ExecutionSecurityCredentials:
		return l.Credentials
	case ExecutionSecurityIsolation:
		return l.Isolation
	default:
		return ""
	}
}

func (l *ExecutionSecurityLevels) set(dimension ExecutionSecurityDimension, level ExecutionSecurityLevel) {
	switch dimension {
	case ExecutionSecurityToolApproval:
		l.ToolApproval = level
	case ExecutionSecurityFileRead:
		l.FileRead = level
	case ExecutionSecurityFileWrite:
		l.FileWrite = level
	case ExecutionSecurityNetwork:
		l.Network = level
	case ExecutionSecurityCredentials:
		l.Credentials = level
	case ExecutionSecurityIsolation:
		l.Isolation = level
	}
}

// Validate reports the first dimension whose level is not on its ladder.
func (l ExecutionSecurityLevels) Validate() error {
	for _, dimension := range ExecutionSecurityDimensions() {
		level := l.Level(dimension)
		if _, ok := ExecutionSecurityLevelIndex(dimension, level); !ok {
			return &ExecutionSecurityError{
				Code: ExecutionSecurityUnresolvable, Dimension: dimension, Level: level,
				Detail: "level is not on the dimension's ladder",
			}
		}
	}
	return nil
}

// ExecutionSecurityWireVersion is the only queued-work section version this
// build reads.
const ExecutionSecurityWireVersion = 1

// ExecutionSecurity is the queued-work section that stamps a session's
// effective levels:
//
//	executionSecurity: { version: 1, levels: {<six dimensions>}, sources: {<dimension>: string} }
//
// Sources are opaque, display-only references to the scope that set each
// level. The decoder is closed: an unknown version, an unknown or missing
// dimension, an unknown level, or any unknown member is refused.
type ExecutionSecurity struct {
	Version int                                   `json:"version"`
	Levels  ExecutionSecurityLevels               `json:"levels"`
	Sources map[ExecutionSecurityDimension]string `json:"sources,omitempty"`
}

// EffectiveExecutionSecurityLevels returns the levels a possibly-absent
// section stamps. A nil section is a work item without the section, which the
// wire compatibility rule reads as index 0 on every dimension.
func EffectiveExecutionSecurityLevels(section *ExecutionSecurity) ExecutionSecurityLevels {
	if section == nil {
		return IndexZeroExecutionSecurityLevels()
	}
	return section.Levels
}

// Clone returns an independent copy.
func (e *ExecutionSecurity) Clone() *ExecutionSecurity {
	if e == nil {
		return nil
	}
	out := *e
	if e.Sources != nil {
		out.Sources = make(map[ExecutionSecurityDimension]string, len(e.Sources))
		for dimension, source := range e.Sources {
			out.Sources[dimension] = source
		}
	}
	return &out
}

// UnmarshalJSON is the closed decoder for the queued-work section. Every
// failure is an *ExecutionSecurityError with code
// execution_security_unresolvable.
func (e *ExecutionSecurity) UnmarshalJSON(raw []byte) error {
	decoded, err := ParseExecutionSecurity(raw)
	if err != nil {
		return err
	}
	if decoded == nil {
		return &ExecutionSecurityError{Code: ExecutionSecurityUnresolvable, Detail: "executionSecurity is null"}
	}
	*e = *decoded
	return nil
}

// ParseExecutionSecurity strictly decodes one queued-work section. Empty
// input (the member was absent) returns nil, nil. A JSON null is returned as
// nil too — encoding/json already maps a null member to an absent pointer, so
// the two cannot be told apart once decoded and are treated the same way.
func ParseExecutionSecurity(raw []byte) (*ExecutionSecurity, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	malformed := func(dimension ExecutionSecurityDimension, level ExecutionSecurityLevel, detail string) error {
		return &ExecutionSecurityError{Code: ExecutionSecurityUnresolvable, Dimension: dimension, Level: level, Detail: detail}
	}
	var wire struct {
		Version *int               `json:"version"`
		Levels  map[string]*string `json:"levels"`
		Sources map[string]string  `json:"sources"`
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return nil, malformed("", "", fmt.Sprintf("executionSecurity is not a closed {version, levels, sources} object: %v", err))
	}
	if decoder.More() {
		return nil, malformed("", "", "executionSecurity has trailing data")
	}
	if wire.Version == nil || *wire.Version != ExecutionSecurityWireVersion {
		return nil, malformed("", "", fmt.Sprintf("executionSecurity version must be %d", ExecutionSecurityWireVersion))
	}
	if wire.Levels == nil {
		return nil, malformed("", "", "executionSecurity levels are missing")
	}
	out := &ExecutionSecurity{Version: ExecutionSecurityWireVersion}
	for name, value := range wire.Levels {
		dimension := ExecutionSecurityDimension(name)
		if _, known := executionSecurityLadders[dimension]; !known {
			return nil, malformed(dimension, "", "unknown dimension")
		}
		if value == nil {
			return nil, malformed(dimension, "", "level is null")
		}
		level := ExecutionSecurityLevel(*value)
		if _, ok := ExecutionSecurityLevelIndex(dimension, level); !ok {
			return nil, malformed(dimension, level, "unknown level")
		}
		out.Levels.set(dimension, level)
	}
	for _, dimension := range ExecutionSecurityDimensions() {
		if out.Levels.Level(dimension) == "" {
			return nil, malformed(dimension, "", "level is missing")
		}
	}
	if len(wire.Sources) > 0 {
		out.Sources = make(map[ExecutionSecurityDimension]string, len(wire.Sources))
		for name, source := range wire.Sources {
			dimension := ExecutionSecurityDimension(name)
			if _, known := executionSecurityLadders[dimension]; !known {
				return nil, malformed(dimension, "", "unknown source dimension")
			}
			out.Sources[dimension] = source
		}
	}
	return out, nil
}

// ExecutionSecurityFromOperationalPayload reads the executionSecurity member
// of a raw queued-work object. An absent member returns nil, nil.
func ExecutionSecurityFromOperationalPayload(raw []byte) (*ExecutionSecurity, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, fmt.Errorf("agent: decode queued work for executionSecurity: %w", err)
	}
	return ParseExecutionSecurity(members["executionSecurity"])
}

// ExecutionSecurityRefusalCode is the closed refusal-code enum of the ADR's D5.
type ExecutionSecurityRefusalCode string

// Refusal codes. The runner raises unresolvable (a malformed stamp),
// unrenderable (no channel for a required level) and receipt_unmet (a report
// below the stamp); the others are carried for wire completeness.
const (
	ExecutionSecurityUnconfigured     ExecutionSecurityRefusalCode = "execution_security_unconfigured"
	ExecutionSecurityUnresolvable     ExecutionSecurityRefusalCode = "execution_security_unresolvable"
	ExecutionSecurityWeakeningRefused ExecutionSecurityRefusalCode = "execution_security_weakening_refused"
	ExecutionSecurityUnmet            ExecutionSecurityRefusalCode = "execution_security_unmet"
	ExecutionSecurityUnrenderable     ExecutionSecurityRefusalCode = "execution_security_unrenderable"
	ExecutionSecurityReceiptUnmet     ExecutionSecurityRefusalCode = "execution_security_receipt_unmet"
)

// ExecutionSecurityError is a typed execution-security refusal. Detail is
// display-only; consumers branch on Code and Dimension.
type ExecutionSecurityError struct {
	Code ExecutionSecurityRefusalCode
	// Dimension is the first dimension (canonical order) that failed; empty
	// when the failure is not about one dimension.
	Dimension ExecutionSecurityDimension
	// Dimensions lists every failing dimension when more than one failed.
	Dimensions []ExecutionSecurityDimension
	// Level is the offending or required level, when there is one.
	Level ExecutionSecurityLevel
	// Harness names the exact harness a rendering refusal is about.
	Harness HarnessName
	Detail  string
}

func (e *ExecutionSecurityError) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Code))
	if len(e.Dimensions) > 1 {
		names := make([]string, len(e.Dimensions))
		for i, dimension := range e.Dimensions {
			names[i] = string(dimension)
		}
		fmt.Fprintf(&b, " (dimensions=%s)", strings.Join(names, ","))
	} else if e.Dimension != "" {
		fmt.Fprintf(&b, " (dimension=%s)", e.Dimension)
	}
	if e.Level != "" {
		fmt.Fprintf(&b, " level=%s", e.Level)
	}
	if e.Harness != "" {
		fmt.Fprintf(&b, " harness=%s", e.Harness)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	return b.String()
}

// ExecutionSecurityErrorCode returns the typed code carried by err, or "".
func ExecutionSecurityErrorCode(err error) ExecutionSecurityRefusalCode {
	var typed *ExecutionSecurityError
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}

// EnforcingLayer names what enforces an achieved level.
type EnforcingLayer string

// The closed enforcing-layer vocabulary.
const (
	LayerHarnessNative     EnforcingLayer = "harness_native"
	LayerInjectedBoundary  EnforcingLayer = "injected_boundary"
	LayerExecutorOSSandbox EnforcingLayer = "executor_os_sandbox"
	LayerProviderSandbox   EnforcingLayer = "provider_sandbox"
	LayerEgressProxy       EnforcingLayer = "egress_proxy"
	LayerCredentialBroker  EnforcingLayer = "credential_broker" //nolint:gosec // G101: an enforcing-layer name, not a credential.
)

// DenyBaselineStatus says how the always-on tool deny entries reached the
// harness. It is reported for toolApproval only.
type DenyBaselineStatus string

// Deny-baseline statuses.
const (
	// DenyBaselineEnforced: the entries are enforced on parsed invocations and
	// proven by the exact version's negative fixture. Required above bypass.
	DenyBaselineEnforced DenyBaselineStatus = "enforced"
	// DenyBaselineBestEffort: the entries are rendered through the harness's
	// deny channel, unattested.
	DenyBaselineBestEffort DenyBaselineStatus = "best_effort"
	// DenyBaselineUnavailable: the harness has no channel for the entries.
	DenyBaselineUnavailable DenyBaselineStatus = "unavailable"
)

// ExecutionSecurityDimensionReport is what one dimension achieved.
type ExecutionSecurityDimensionReport struct {
	// Required echoes the stamped level. Informational only: a verifier
	// compares AchievedLevel with its own stamp, never with this echo.
	Required      ExecutionSecurityLevel `json:"required"`
	AchievedLevel ExecutionSecurityLevel `json:"achievedLevel"`
	// EnforcingLayers is empty only when AchievedLevel is index 0.
	EnforcingLayers []EnforcingLayer   `json:"enforcingLayers"`
	DenyBaseline    DenyBaselineStatus `json:"denyBaseline,omitempty"`
	EvidenceDigest  string             `json:"evidenceDigest,omitempty"`
}

// ExecutionSecurityReport is the per-dimension record an applied adaptation
// carries (the executionSecurity field of the applied receipt).
type ExecutionSecurityReport struct {
	ToolApproval ExecutionSecurityDimensionReport `json:"toolApproval"`
	FileRead     ExecutionSecurityDimensionReport `json:"fileRead"`
	FileWrite    ExecutionSecurityDimensionReport `json:"fileWrite"`
	Network      ExecutionSecurityDimensionReport `json:"network"`
	Credentials  ExecutionSecurityDimensionReport `json:"credentials"`
	Isolation    ExecutionSecurityDimensionReport `json:"isolation"`
}

// Dimension returns the report held for dimension.
func (r ExecutionSecurityReport) Dimension(dimension ExecutionSecurityDimension) ExecutionSecurityDimensionReport {
	switch dimension {
	case ExecutionSecurityToolApproval:
		return r.ToolApproval
	case ExecutionSecurityFileRead:
		return r.FileRead
	case ExecutionSecurityFileWrite:
		return r.FileWrite
	case ExecutionSecurityNetwork:
		return r.Network
	case ExecutionSecurityCredentials:
		return r.Credentials
	case ExecutionSecurityIsolation:
		return r.Isolation
	default:
		return ExecutionSecurityDimensionReport{}
	}
}

func (r *ExecutionSecurityReport) setDimension(dimension ExecutionSecurityDimension, value ExecutionSecurityDimensionReport) {
	switch dimension {
	case ExecutionSecurityToolApproval:
		r.ToolApproval = value
	case ExecutionSecurityFileRead:
		r.FileRead = value
	case ExecutionSecurityFileWrite:
		r.FileWrite = value
	case ExecutionSecurityNetwork:
		r.Network = value
	case ExecutionSecurityCredentials:
		r.Credentials = value
	case ExecutionSecurityIsolation:
		r.Isolation = value
	}
}

// ExecutionSecurityReportMeets checks a report against the stamped levels. A
// nil report is a peer that reported nothing and achieves exactly index 0. A
// report that is malformed, claims a level above index 0 without an enforcing
// layer, claims toolApproval above bypass without an enforced deny baseline,
// or achieves less than a stamped level is execution_security_receipt_unmet.
func ExecutionSecurityReportMeets(report *ExecutionSecurityReport, stamped ExecutionSecurityLevels) error {
	var unmet []ExecutionSecurityDimension
	for _, dimension := range ExecutionSecurityDimensions() {
		required := stamped.Level(dimension)
		requiredIndex, ok := ExecutionSecurityLevelIndex(dimension, required)
		if !ok {
			return &ExecutionSecurityError{Code: ExecutionSecurityUnresolvable, Dimension: dimension, Level: required, Detail: "stamped level is not on the dimension's ladder"}
		}
		achievedIndex := 0
		if report != nil {
			entry := report.Dimension(dimension)
			index, known := ExecutionSecurityLevelIndex(dimension, entry.AchievedLevel)
			switch {
			case !known:
				index = -1
			case index > 0 && len(entry.EnforcingLayers) == 0:
				index = 0
			case dimension == ExecutionSecurityToolApproval && index > 0 && entry.DenyBaseline != DenyBaselineEnforced:
				index = 0
			}
			achievedIndex = index
		}
		if achievedIndex < requiredIndex {
			unmet = append(unmet, dimension)
		}
	}
	if len(unmet) == 0 {
		return nil
	}
	return &ExecutionSecurityError{
		Code: ExecutionSecurityReceiptUnmet, Dimension: unmet[0], Dimensions: unmet,
		Level: stamped.Level(unmet[0]), Detail: "the applied report achieves less than the stamped level",
	}
}

// ExecutionSecurityRendering is an exact harness/version's declaration of what
// it can render (the adaptation-manifest declaration of checklist row 9). The
// zero value renders index 0 only and has no deny channel.
type ExecutionSecurityRendering struct {
	// Levels lists every level above index 0 the harness renders natively,
	// per dimension and session mode, with its enforcing layers. A level not
	// listed has no channel on this harness and is refused.
	Levels []RenderedExecutionSecurityLevel `json:"levels,omitempty"`
	// DenyBaseline is how the always-on deny entries reach the harness at
	// bypass: best_effort when it has a deny channel, unavailable otherwise.
	// A harness only renders toolApproval above bypass through a level entry,
	// which implies an enforced baseline.
	DenyBaseline DenyBaselineStatus `json:"denyBaseline,omitempty"`
}

// RenderedExecutionSecurityLevel declares one renderable level.
type RenderedExecutionSecurityLevel struct {
	Dimension ExecutionSecurityDimension `json:"dimension"`
	Level     ExecutionSecurityLevel     `json:"level"`
	// Modes lists the session modes the rendering covers; empty means every
	// mode the harness admits.
	Modes  []PromptSessionMode `json:"modes,omitempty"`
	Layers []EnforcingLayer    `json:"layers"`
}

func (r ExecutionSecurityRendering) find(dimension ExecutionSecurityDimension, level ExecutionSecurityLevel, mode PromptSessionMode) (RenderedExecutionSecurityLevel, bool) {
	for _, candidate := range r.Levels {
		if candidate.Dimension != dimension || candidate.Level != level || len(candidate.Layers) == 0 {
			continue
		}
		if len(candidate.Modes) == 0 {
			return candidate, true
		}
		for _, candidateMode := range candidate.Modes {
			if candidateMode == mode {
				return candidate, true
			}
		}
	}
	return RenderedExecutionSecurityLevel{}, false
}

// RenderExecutionSecurity decides, before any provider side effect, whether
// the exact harness can render every level spec stamps, and reports what it
// achieves. A stamped level above index 0 with no declared channel on this
// harness and session mode is execution_security_unrenderable naming every
// such dimension; the plan is never rendered weaker. Autonomous,
// human-controlled and interactive sessions are held to the same levels.
func RenderExecutionSecurity(spec Spec, manifest HarnessManifest) (ExecutionSecurityReport, error) {
	levels := EffectiveExecutionSecurityLevels(spec.ExecutionSecurity)
	if err := levels.Validate(); err != nil {
		return ExecutionSecurityReport{}, err
	}
	mode := PromptModeForSpec(spec)
	rendering := manifest.ExecutionSecurity
	var report ExecutionSecurityReport
	var unrenderable []ExecutionSecurityDimension
	for _, dimension := range ExecutionSecurityDimensions() {
		required := levels.Level(dimension)
		index, _ := ExecutionSecurityLevelIndex(dimension, required)
		entry := ExecutionSecurityDimensionReport{Required: required, AchievedLevel: required, EnforcingLayers: []EnforcingLayer{}}
		if index > 0 {
			declared, ok := rendering.find(dimension, required, mode)
			if !ok {
				unrenderable = append(unrenderable, dimension)
				continue
			}
			entry.EnforcingLayers = sortedLayers(declared.Layers)
		}
		if dimension == ExecutionSecurityToolApproval {
			entry.DenyBaseline = rendering.denyBaselineAt(index)
		}
		report.setDimension(dimension, entry)
	}
	if len(unrenderable) > 0 {
		return ExecutionSecurityReport{}, &ExecutionSecurityError{
			Code: ExecutionSecurityUnrenderable, Dimension: unrenderable[0], Dimensions: unrenderable,
			Level: levels.Level(unrenderable[0]), Harness: manifest.Name,
			Detail: "the exact harness and session mode have no channel for the stamped level",
		}
	}
	return report, nil
}

func (r ExecutionSecurityRendering) denyBaselineAt(toolApprovalIndex int) DenyBaselineStatus {
	if toolApprovalIndex > 0 {
		return DenyBaselineEnforced
	}
	if r.DenyBaseline == DenyBaselineBestEffort {
		return DenyBaselineBestEffort
	}
	return DenyBaselineUnavailable
}

func sortedLayers(layers []EnforcingLayer) []EnforcingLayer {
	out := append([]EnforcingLayer{}, layers...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ToolApprovalBypass reports whether spec's effective toolApproval level is
// bypass. At bypass no tool runs behind an approval or a policy gate the
// runner invented: only an explicitly configured allow list, the always-on
// deny entries and the built-in destructive-command denies apply.
func (s Spec) ToolApprovalBypass() bool {
	return EffectiveExecutionSecurityLevels(s.ExecutionSecurity).ToolApproval == ToolApprovalBypass
}

// ValidateExecutionSecurityReport checks a report's closed shape: every
// dimension carries known levels and layers, a level above index 0 names an
// enforcing layer, and only toolApproval carries a deny-baseline status.
func ValidateExecutionSecurityReport(report ExecutionSecurityReport) error {
	for _, dimension := range ExecutionSecurityDimensions() {
		entry := report.Dimension(dimension)
		if _, ok := ExecutionSecurityLevelIndex(dimension, entry.Required); !ok {
			return &ExecutionSecurityError{Code: ExecutionSecurityReceiptUnmet, Dimension: dimension, Level: entry.Required, Detail: "report echoes an unknown required level"}
		}
		index, ok := ExecutionSecurityLevelIndex(dimension, entry.AchievedLevel)
		if !ok {
			return &ExecutionSecurityError{Code: ExecutionSecurityReceiptUnmet, Dimension: dimension, Level: entry.AchievedLevel, Detail: "report achieves an unknown level"}
		}
		if entry.EnforcingLayers == nil || (index > 0 && len(entry.EnforcingLayers) == 0) {
			return &ExecutionSecurityError{Code: ExecutionSecurityReceiptUnmet, Dimension: dimension, Level: entry.AchievedLevel, Detail: "report names no enforcing layer"}
		}
		for _, layer := range entry.EnforcingLayers {
			if !knownEnforcingLayer(layer) {
				return &ExecutionSecurityError{Code: ExecutionSecurityReceiptUnmet, Dimension: dimension, Detail: "report names an unknown enforcing layer"}
			}
		}
		switch {
		case dimension == ExecutionSecurityToolApproval && !knownDenyBaseline(entry.DenyBaseline):
			return &ExecutionSecurityError{Code: ExecutionSecurityReceiptUnmet, Dimension: dimension, Detail: "toolApproval report carries no known deny-baseline status"}
		case dimension != ExecutionSecurityToolApproval && entry.DenyBaseline != "":
			return &ExecutionSecurityError{Code: ExecutionSecurityReceiptUnmet, Dimension: dimension, Detail: "only toolApproval carries a deny-baseline status"}
		}
	}
	return nil
}

func knownEnforcingLayer(layer EnforcingLayer) bool {
	switch layer {
	case LayerHarnessNative, LayerInjectedBoundary, LayerExecutorOSSandbox, LayerProviderSandbox, LayerEgressProxy, LayerCredentialBroker:
		return true
	default:
		return false
	}
}

func knownDenyBaseline(status DenyBaselineStatus) bool {
	switch status {
	case DenyBaselineEnforced, DenyBaselineBestEffort, DenyBaselineUnavailable:
		return true
	default:
		return false
	}
}

// ExecutionSecurityEnforcement is a placement's attestation (the
// executionSecurityEnforcement shape of 004-sandbox-capability-matrix.md):
// per dimension, the strongest level the executor enforces, proven by a
// negative probe on its exact version. An absent dimension is exactly index
// 0. toolApproval is rendered by the harness layer, not the substrate, so a
// placement leaves it absent.
type ExecutionSecurityEnforcement struct {
	ToolApproval ExecutionSecurityLevel `json:"toolApproval,omitempty"`
	FileRead     ExecutionSecurityLevel `json:"fileRead,omitempty"`
	FileWrite    ExecutionSecurityLevel `json:"fileWrite,omitempty"`
	Network      ExecutionSecurityLevel `json:"network,omitempty"`
	Credentials  ExecutionSecurityLevel `json:"credentials,omitempty"`
	Isolation    ExecutionSecurityLevel `json:"isolation,omitempty"`
}

// UncontainedHostEnforcement is what a host that applies no containment
// around the harness attests: index 0 on every substrate dimension. It is
// not a default level for a session — it is the honest attestation of a
// host with no executor sandbox, no egress proxy and no credential broker.
func UncontainedHostEnforcement() ExecutionSecurityEnforcement {
	return ExecutionSecurityEnforcement{
		FileRead: FileReadHost, FileWrite: FileWriteHost, Network: NetworkOpen,
		Credentials: CredentialsAmbientHostLogin, Isolation: IsolationHostUser,
	}
}

// Level returns the attested level for dimension, reading an absent
// dimension as index 0.
func (e ExecutionSecurityEnforcement) Level(dimension ExecutionSecurityDimension) ExecutionSecurityLevel {
	if level := ExecutionSecurityLevels(e).Level(dimension); level != "" {
		return level
	}
	ladder := executionSecurityLadders[dimension]
	if len(ladder) == 0 {
		return ""
	}
	return ladder[0]
}

// Validate refuses an attestation naming a level off its dimension's ladder.
func (e ExecutionSecurityEnforcement) Validate() error {
	for _, dimension := range ExecutionSecurityDimensions() {
		level := e.Level(dimension)
		if _, ok := ExecutionSecurityLevelIndex(dimension, level); !ok {
			return &ExecutionSecurityError{Code: ExecutionSecurityUnresolvable, Dimension: dimension, Level: level, Detail: "attested level is not on the dimension's ladder"}
		}
	}
	return nil
}

// AttestsSandbox reports whether the attestation proves an isolation boundary
// the harness cannot widen (os-sandbox or stronger) — the only thing that
// entitles a host to advertise a sandbox capability.
func (e ExecutionSecurityEnforcement) AttestsSandbox() bool {
	index, ok := ExecutionSecurityLevelIndex(ExecutionSecurityIsolation, e.Level(ExecutionSecurityIsolation))
	return ok && index > 0
}
