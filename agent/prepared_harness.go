package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// HarnessAdaptationContractVersion identifies the sole host-compiled
// adaptation authority consumed by child runners and providers.
const HarnessAdaptationContractVersion = "harness-adaptation-plan/v1"

var requiredHarnessMaterializationChannels = map[string]struct{}{
	"worktree": {}, "environment": {}, "credentials": {}, "config": {},
	"endpoint_delivery": {}, "services": {}, "child_process": {}, "runtime": {}, "cleanup": {},
}

// HarnessMaterialization declares one runtime-only child operation bound to
// the admitted operational payload.
type HarnessMaterialization struct {
	Channel      string `json:"channel"`
	SourceDigest string `json:"sourceDigest"`
	Required     bool   `json:"required"`
}

// PreparedHarness is the digest-only host authority later materialized by the
// child. It contains no prompt, environment, credential, or config values.
type PreparedHarness struct {
	ContractVersion          string            `json:"contractVersion"`
	Harness                  string            `json:"harness"`
	Mode                     PromptSessionMode `json:"mode"`
	OperationalPayloadDigest string            `json:"operationalPayloadDigest"`
	AuthorityDigest          string            `json:"authorityDigest"`
	// AuthorityFieldDigests is a per-field SHA-256 digest of the same
	// harnessAuthorityProjection AuthorityDigest hashes as a whole — still
	// digest-only, never a raw value. AuthorityDigest alone is the pass/fail
	// gate; this map exists only so a mismatch can NAME which projection
	// field(s) drifted (see AuthorityDriftError) instead of surfacing one
	// undiagnosable 64-hex inequality. Keyed by the projection's JSON field
	// names (e.g. "model", "mcpServers").
	AuthorityFieldDigests map[string]string        `json:"authorityFieldDigests,omitempty"`
	RuntimeMCPNames       []string                 `json:"runtimeMcpNames"`
	Materializations      []HarnessMaterialization `json:"materializations"`
	PromptReceipt         PromptDeliveryReceipt    `json:"promptReceipt"`
	ToolLifecycleReceipt  ToolLifecycleReceipt     `json:"toolLifecycleReceipt"`
}

// AuthorityDriftError is returned by ApplyPreparedHarness when the child's
// materialized Spec does not byte-match the host-persisted authority plan.
// Fields names exactly which harnessAuthorityDigest projection fields
// disagree — never the field values, which may carry prompt text, tool
// names, or endpoint identity — so a production failure is diagnosable from
// the error string and log line alone, without an authority value ever
// appearing in either.
type AuthorityDriftError struct {
	// Fields is the sorted set of projection field names (e.g. "model",
	// "mcpServers", "systemPromptAppend") whose per-field digest differs
	// between the host-compiled plan and the child's recompute. Non-empty
	// whenever the combined AuthorityDigest differed, EXCEPT when the plan
	// predates AuthorityFieldDigests (an old plan with no per-field digests
	// to compare against) — that legacy case is reported as ["unknown"].
	Fields []string
}

func (e *AuthorityDriftError) Error() string {
	return fmt.Sprintf("agent: materialized Spec differs from host adaptation authority (fields: %s)", strings.Join(e.Fields, ", "))
}

// ToolLifecycleDriftError is returned by ApplyPreparedHarness when the
// child's recomputed ToolLifecycleReceipt does not byte-match the
// host-persisted plan.ToolLifecycleReceipt, even though the combined
// AuthorityDigest agreed (see AuthorityDriftError's doc comment — that check
// runs first and catches every drift within the harnessAuthorityProjection).
// Reaching this error means a Spec field the tool/lifecycle compiler
// consumes but the projection does not cover changed between host compile
// and child apply. Same doctrine as AuthorityDriftError: Fields names
// exactly which ToolLifecycleReceipt field(s) disagree — never their
// values, which may carry tool/MCP identity or denial detail — so a
// production refusal is diagnosable from the error string and log line
// alone.
type ToolLifecycleDriftError struct {
	// Fields is the sorted set of toolLifecycleReceiptFieldDigests key names
	// (e.g. "entries", "decision") whose per-field digest differs between
	// the host-compiled receipt and the child's recompute.
	Fields []string
}

func (e *ToolLifecycleDriftError) Error() string {
	return fmt.Sprintf("agent: tool/lifecycle application differs from host adaptation receipt (fields: %s)", strings.Join(e.Fields, ", "))
}

// toolLifecycleReceiptFieldDigests hashes each ToolLifecycleReceipt field
// independently, digest-only — see authorityFieldDigests's doc comment for
// why: Entries may carry tool/MCP identity or denial detail in a future
// contract version, so no field value is ever returned or logged. Keyed by
// the receipt's JSON field names, the exact set ToolLifecycleDriftError.Fields
// reports on a mismatch.
func toolLifecycleReceiptFieldDigests(r ToolLifecycleReceipt) map[string]string {
	return map[string]string{
		"contractVersion":          digestValue(r.ContractVersion),
		"admissionReceiptId":       digestValue(r.AdmissionReceiptID),
		"claimReceiptId":           digestValue(r.ClaimReceiptID),
		"operationalPayloadDigest": digestValue(r.OperationalPayloadDigest),
		"profileId":                digestValue(r.ProfileID),
		"decision":                 digestValue(r.Decision),
		"evidenceTier":             digestValue(r.EvidenceTier),
		"productionEligible":       digestValue(r.ProductionEligible),
		"entries":                  digestValue(r.Entries),
	}
}

// CompilePreparedHarness compiles the exact source Spec and harness profile
// into the secret-free authority persisted before child side effects.
func CompilePreparedHarness(spec Spec, manifest HarnessManifest, operationalDigest string, runtimeMCPNames []string, materializations []HarnessMaterialization) (*PreparedHarness, error) {
	mode := PromptModeForSpec(spec)
	plan := &PreparedHarness{
		ContractVersion: HarnessAdaptationContractVersion,
		Harness:         string(manifest.Name), Mode: mode,
		OperationalPayloadDigest: operationalDigest,
		RuntimeMCPNames:          append([]string(nil), runtimeMCPNames...),
		Materializations:         append([]HarnessMaterialization(nil), materializations...),
	}
	sort.Strings(plan.RuntimeMCPNames)
	projection := newHarnessAuthorityProjection(spec, plan)
	plan.AuthorityDigest = digestAuthorityProjection(projection)
	plan.AuthorityFieldDigests = authorityFieldDigests(projection)
	promptProfile, ok := manifest.PromptProfile(mode)
	if !ok {
		return plan, &PromptAdaptationError{Code: PromptDenialDeliveryUnsupported, Detail: "manifest has no prompt profile for admitted session mode"}
	}
	_, promptReceipt, promptErr := AdaptPrompt(spec, promptProfile)
	plan.PromptReceipt = promptReceipt
	if promptErr != nil {
		return plan, promptErr
	}
	toolProfile, ok := manifest.ToolLifecycleProfile(mode)
	if !ok {
		return plan, &ToolAdaptationError{Code: ToolDenialDeliveryUnsupported, Detail: "manifest has no tool/lifecycle profile for admitted session mode"}
	}
	normalized := normalizeHarnessAuthoritySpec(spec, plan)
	_, toolReceipt, toolErr := AdaptToolLifecycle(normalized, toolProfile)
	plan.ToolLifecycleReceipt = toolReceipt
	if toolErr != nil {
		return plan, toolErr
	}
	return plan, nil
}

// ApplyPreparedHarness is an idempotent application/equality check. It cannot
// emit callbacks or mint a new decision: any recomputed result must byte-equal
// the host-persisted receipts before adapted fields are returned.
func ApplyPreparedHarness(spec Spec, manifest HarnessManifest) (Spec, error) {
	plan := spec.PreparedHarness
	if plan == nil {
		return spec, errors.New("agent: prepared harness plan is required")
	}
	if err := ValidatePreparedHarness(plan, plan.OperationalPayloadDigest); err != nil {
		return spec, err
	}
	if plan.ContractVersion != HarnessAdaptationContractVersion || plan.Harness != string(manifest.Name) || plan.Mode != PromptModeForSpec(spec) {
		return spec, errors.New("agent: prepared harness identity does not match exact harness and mode")
	}
	gotProjection := newHarnessAuthorityProjection(spec, plan)
	if got := digestAuthorityProjection(gotProjection); got != plan.AuthorityDigest {
		return spec, &AuthorityDriftError{Fields: driftingAuthorityFields(plan.AuthorityFieldDigests, authorityFieldDigests(gotProjection))}
	}
	promptProfile, ok := manifest.PromptProfile(plan.Mode)
	if !ok {
		return spec, errors.New("agent: prepared prompt profile is no longer available")
	}
	adapted, promptReceipt, err := AdaptPrompt(spec, promptProfile)
	if err != nil || !equalJSON(promptReceipt, plan.PromptReceipt) {
		return spec, errors.New("agent: prompt application differs from host adaptation receipt")
	}
	toolProfile, ok := manifest.ToolLifecycleProfile(plan.Mode)
	if !ok {
		return spec, errors.New("agent: prepared tool/lifecycle profile is no longer available")
	}
	normalized := normalizeHarnessAuthoritySpec(adapted, plan)
	_, toolReceipt, err := AdaptToolLifecycle(normalized, toolProfile)
	if err != nil {
		return spec, fmt.Errorf("agent: tool/lifecycle application failed: %w", err)
	}
	if !equalJSON(toolReceipt, plan.ToolLifecycleReceipt) {
		// The combined AuthorityDigest check above (line ~125) already caught
		// every drifting field IN THE harnessAuthorityProjection. Reaching
		// here means the recomputed ToolLifecycleReceipt itself differs from
		// the host-persisted one even though that projection agreed — e.g. a
		// Spec field the tool/lifecycle compiler consumes but the projection
		// does not cover (Spec.ToolSurfaceRequired, Spec.AdditionalExtensions)
		// changed after preflight. Name the drifting receipt field(s) with the
		// same digest-only, never-raw-value doctrine AuthorityDriftError
		// uses, so a production refusal is diagnosable from the error string
		// and log line alone instead of the bare, undiagnosable
		// "differs from host adaptation receipt" a production incident
		// reported (repository_sandbox_reconcile.go's doc comment).
		return spec, &ToolLifecycleDriftError{Fields: driftingAuthorityFields(
			toolLifecycleReceiptFieldDigests(plan.ToolLifecycleReceipt),
			toolLifecycleReceiptFieldDigests(toolReceipt),
		)}
	}
	// The recompute above ran on the redacted normalized copy, so its adapted
	// Spec cannot be returned — re-apply the receipt-recorded drop of a
	// denied-but-optional additional-extensions batch to the real Spec here,
	// or the exact adapter would materialize deliveries the host-persisted
	// receipt refused (the recompute matching byte-for-byte proves the drop
	// decision is the same one the host made).
	adapted = dropDeniedAdvisoryExtensions(adapted, plan.ToolLifecycleReceipt.Entries)
	adapted.PromptReceipt = copyPromptReceipt(&plan.PromptReceipt)
	adapted.ToolLifecycleReceipt = copyToolReceipt(&plan.ToolLifecycleReceipt)
	return adapted, nil
}

// DigestPreparedHarness returns the stable SHA-256 digest of plan's JSON form.
func DigestPreparedHarness(plan *PreparedHarness) string {
	if plan == nil {
		return ""
	}
	raw, _ := json.Marshal(plan)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// harnessAuthorityProjection is the exact set of Spec fields the host
// adaptation authority covers. Adding a field to Spec that must be part of
// the authority contract requires adding it here — see the field-name keys
// authorityFieldDigests derives from the json tags below, which
// AuthorityDriftError.Fields reports on a mismatch.
type harnessAuthorityProjection struct {
	Prompt             string                `json:"prompt"`
	Autonomous         bool                  `json:"autonomous"`
	SandboxEnabled     bool                  `json:"sandboxEnabled"`
	SandboxLevel       SandboxLevel          `json:"sandboxLevel"`
	AllowedTools       []string              `json:"allowedTools"`
	DisallowedTools    []string              `json:"disallowedTools"`
	MCPServers         []MCPServerConfig     `json:"mcpServers"`
	MCPToolNames       []string              `json:"mcpToolNames"`
	MaxTurns           *int                  `json:"maxTurns"`
	Model              string                `json:"model"`
	Endpoint           any                   `json:"endpoint"`
	Effort             EffortLevel           `json:"effort"`
	ResponseSchema     json.RawMessage       `json:"responseSchema"`
	Interactive        *InteractiveSpec      `json:"interactive"`
	BaseInstructions   string                `json:"baseInstructions"`
	SystemPromptAppend string                `json:"systemPromptAppend"`
	InitialContext     string                `json:"initialContext"`
	PermissionConfig   *PermissionConfig     `json:"permissionConfig"`
	CodeIntel          *CodeIntelEnforcement `json:"codeIntel"`
	ProviderConfig     map[string]any        `json:"providerConfig"`
	SubAgentProvider   ProviderName          `json:"subAgentProvider"`
	PromptPlan         *PromptPlan           `json:"promptPlan"`
	ToolLifecyclePlan  *ToolLifecyclePlan    `json:"toolLifecyclePlan"`
	PromptMode         PromptSessionMode     `json:"promptMode"`
}

func newHarnessAuthorityProjection(spec Spec, plan *PreparedHarness) harnessAuthorityProjection {
	normalized := normalizeHarnessAuthoritySpec(spec, plan)
	return harnessAuthorityProjection{
		Prompt:             normalized.Prompt,
		Autonomous:         normalized.Autonomous,
		SandboxEnabled:     normalized.SandboxEnabled,
		SandboxLevel:       normalized.SandboxLevel,
		AllowedTools:       normalized.AllowedTools,
		DisallowedTools:    normalized.DisallowedTools,
		MCPServers:         normalized.MCPServers,
		MCPToolNames:       normalized.MCPToolNames,
		MaxTurns:           normalized.MaxTurns,
		Model:              normalized.Model,
		Endpoint:           stableEndpointAuthority(normalized.Endpoint),
		Effort:             normalized.Effort,
		ResponseSchema:     normalized.ResponseSchema,
		Interactive:        normalized.Interactive,
		BaseInstructions:   normalized.BaseInstructions,
		SystemPromptAppend: normalized.SystemPromptAppend,
		InitialContext:     normalized.InitialContext,
		PermissionConfig:   normalized.PermissionConfig,
		CodeIntel:          normalized.CodeIntelEnforcement,
		ProviderConfig:     normalized.ProviderConfig,
		SubAgentProvider:   normalized.SubAgentProvider,
		PromptPlan:         normalized.PromptPlan,
		ToolLifecyclePlan:  normalized.ToolLifecyclePlan,
		PromptMode:         normalized.PromptMode,
	}
}

func digestValue(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func digestAuthorityProjection(p harnessAuthorityProjection) string {
	return digestValue(p)
}

// authorityFieldDigests hashes each harnessAuthorityProjection field
// independently, keyed by its json tag. Still digest-only — no field value
// is ever returned or logged — but broken out per field so a mismatch on the
// combined AuthorityDigest can be localized to the field(s) that actually
// drifted instead of surfacing one undiagnosable 64-hex inequality.
func authorityFieldDigests(p harnessAuthorityProjection) map[string]string {
	return map[string]string{
		"prompt":             digestValue(p.Prompt),
		"autonomous":         digestValue(p.Autonomous),
		"sandboxEnabled":     digestValue(p.SandboxEnabled),
		"sandboxLevel":       digestValue(p.SandboxLevel),
		"allowedTools":       digestValue(p.AllowedTools),
		"disallowedTools":    digestValue(p.DisallowedTools),
		"mcpServers":         digestValue(p.MCPServers),
		"mcpToolNames":       digestValue(p.MCPToolNames),
		"maxTurns":           digestValue(p.MaxTurns),
		"model":              digestValue(p.Model),
		"endpoint":           digestValue(p.Endpoint),
		"effort":             digestValue(p.Effort),
		"responseSchema":     digestValue(p.ResponseSchema),
		"interactive":        digestValue(p.Interactive),
		"baseInstructions":   digestValue(p.BaseInstructions),
		"systemPromptAppend": digestValue(p.SystemPromptAppend),
		"initialContext":     digestValue(p.InitialContext),
		"permissionConfig":   digestValue(p.PermissionConfig),
		"codeIntel":          digestValue(p.CodeIntel),
		"providerConfig":     digestValue(p.ProviderConfig),
		"subAgentProvider":   digestValue(p.SubAgentProvider),
		"promptPlan":         digestValue(p.PromptPlan),
		"toolLifecyclePlan":  digestValue(p.ToolLifecyclePlan),
		"promptMode":         digestValue(p.PromptMode),
	}
}

// driftingAuthorityFields returns the sorted set of field names present in
// want or got whose digest disagrees (including a field missing from one
// side, e.g. want being empty because the persisted plan predates
// AuthorityFieldDigests — every field is then reported, since which single
// field actually drifted is genuinely unknown for that legacy shape).
func driftingAuthorityFields(want, got map[string]string) []string {
	if len(want) == 0 {
		return []string{"unknown"}
	}
	seen := make(map[string]struct{}, len(want)+len(got))
	for name := range want {
		seen[name] = struct{}{}
	}
	for name := range got {
		seen[name] = struct{}{}
	}
	var fields []string
	for name := range seen {
		if want[name] != got[name] {
			fields = append(fields, name)
		}
	}
	sort.Strings(fields)
	return fields
}

func normalizeHarnessAuthoritySpec(spec Spec, plan *PreparedHarness) Spec {
	out := spec
	out.Cwd, out.Env = "", nil
	out.OnPromptAdapted, out.OnToolLifecycleAdapted, out.OnProcessSpawned = nil, nil, nil
	runtimeNames := map[string]bool{}
	if plan != nil {
		for _, name := range plan.RuntimeMCPNames {
			runtimeNames[name] = true
		}
	}
	out.MCPServers = append([]MCPServerConfig(nil), spec.MCPServers...)
	for i := range out.MCPServers {
		if runtimeNames[out.MCPServers[i].Name] {
			out.MCPServers[i].Command = "<runtime>"
			out.MCPServers[i].Args = nil
			out.MCPServers[i].Env = nil
			out.MCPServers[i].URL = "https://runtime.invalid"
			out.MCPServers[i].Headers = nil
		}
	}
	if out.Interactive != nil {
		interactiveCopy := *out.Interactive
		interactiveCopy.RecordPath = ""
		out.Interactive = &interactiveCopy
	}
	return out
}

func stableEndpointAuthority(endpoint *EndpointBinding) any {
	if endpoint == nil {
		return nil
	}
	return map[string]any{
		"company": endpoint.Company, "model": endpoint.Model, "protocol": endpoint.Protocol, "host": endpoint.Host,
		"endpointId": endpoint.EndpointID, "endpointOperator": endpoint.EndpointOperator, "endpointRevision": endpoint.EndpointRevision,
		"modelAuthor": endpoint.ModelAuthor, "authBindingId": endpoint.AuthBindingID, "authAuthority": endpoint.AuthAuthority,
		"authCommercialMode": endpoint.AuthCommercialMode, "authBindingScope": endpoint.AuthBindingScope,
		"authPortability": endpoint.AuthPortability, "authDelivery": endpoint.AuthDelivery, "mechanism": endpoint.Mechanism,
	}
}

func equalJSON(left, right any) bool {
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return string(a) == string(b)
}

func copyPromptReceipt(in *PromptDeliveryReceipt) *PromptDeliveryReceipt {
	if in == nil {
		return nil
	}
	out := *in
	out.Entries = append([]PromptDeliveryEntry(nil), in.Entries...)
	return &out
}

func copyToolReceipt(in *ToolLifecycleReceipt) *ToolLifecycleReceipt {
	if in == nil {
		return nil
	}
	out := *in
	out.Entries = append([]ToolLifecycleEntry(nil), in.Entries...)
	return &out
}

// ValidatePreparedHarness verifies a persisted authority before materializing
// any runtime-only child operation.
func ValidatePreparedHarness(plan *PreparedHarness, operationalDigest string) error {
	if plan == nil || plan.ContractVersion != HarnessAdaptationContractVersion {
		return errors.New("agent: invalid prepared harness contract")
	}
	if strings.TrimSpace(plan.Harness) == "" || plan.Mode == "" || plan.OperationalPayloadDigest != operationalDigest || plan.AuthorityDigest == "" {
		return errors.New("agent: incomplete prepared harness authority")
	}
	if plan.PromptReceipt.Decision != "ready" || plan.ToolLifecycleReceipt.Decision != "ready" {
		return errors.New("agent: prepared harness is not ready")
	}
	seen := map[string]bool{}
	for _, entry := range plan.Materializations {
		_, requiredChannel := requiredHarnessMaterializationChannels[entry.Channel]
		if !requiredChannel || !entry.Required || entry.SourceDigest != operationalDigest || seen[entry.Channel] {
			return fmt.Errorf("agent: invalid materialization channel %q", entry.Channel)
		}
		seen[entry.Channel] = true
	}
	if len(seen) != len(requiredHarnessMaterializationChannels) {
		return errors.New("agent: prepared harness omits a required materialization channel")
	}
	seenMCP := map[string]bool{}
	for _, name := range plan.RuntimeMCPNames {
		if strings.TrimSpace(name) == "" || seenMCP[name] {
			return fmt.Errorf("agent: invalid runtime MCP authority %q", name)
		}
		seenMCP[name] = true
	}
	return nil
}
