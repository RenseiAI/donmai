package executioncell

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"

	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ContractError is the stable, typed failure returned by closed contract
// decoders. Path contains JSON object keys or array indexes leading to the
// rejected value when that information is available.
type ContractError struct {
	Code ContractErrorCode
	Path []string
	Err  error
}

func (e *ContractError) Error() string {
	if len(e.Path) == 0 {
		return fmt.Sprintf("execution-cell %s: %v", e.Code, e.Err)
	}
	return fmt.Sprintf("execution-cell %s at %s: %v", e.Code, strings.Join(e.Path, "."), e.Err)
}

func (e *ContractError) Unwrap() error { return e.Err }

func contractError(code ContractErrorCode, path []string, format string, args ...any) *ContractError {
	return &ContractError{Code: code, Path: slices.Clone(path), Err: fmt.Errorf(format, args...)}
}

//go:embed contract.schema.json
var contractSchemaJSON []byte

//go:embed fixtures.json
var fixtureSuiteJSON []byte

// ContractSchema returns an isolated copy of the language-neutral schema.
func ContractSchema() []byte { return bytes.Clone(contractSchemaJSON) }

// FixtureSuite returns an isolated copy of the normative fixture suite.
func FixtureSuite() []byte { return bytes.Clone(fixtureSuiteJSON) }

type schemaEnvelope struct {
	ContractVersion string                     `json:"contractVersion"`
	Schemas         map[string]json.RawMessage `json:"schemas"`
}

var (
	compiledOnce sync.Once
	compiled     map[string]*jsonschema.Schema
	compiledErr  error
)

func compileSchemas() {
	var envelope schemaEnvelope
	if err := json.Unmarshal(contractSchemaJSON, &envelope); err != nil {
		compiledErr = fmt.Errorf("decode embedded contract schema: %w", err)
		return
	}
	if envelope.ContractVersion != ContractVersion {
		compiledErr = fmt.Errorf("embedded schema version %q does not match %q", envelope.ContractVersion, ContractVersion)
		return
	}
	compiled = make(map[string]*jsonschema.Schema, len(envelope.Schemas))
	for name, raw := range envelope.Schemas {
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			compiledErr = fmt.Errorf("decode %s schema: %w", name, err)
			return
		}
		uri := "execution-cell://v1alpha1/" + name
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource(uri, document); err != nil {
			compiledErr = fmt.Errorf("add %s schema: %w", name, err)
			return
		}
		schema, err := compiler.Compile(uri)
		if err != nil {
			compiledErr = fmt.Errorf("compile %s schema: %w", name, err)
			return
		}
		compiled[name] = schema
	}
}

func schemaFor(name string) (*jsonschema.Schema, error) {
	compiledOnce.Do(compileSchemas)
	if compiledErr != nil {
		return nil, compiledErr
	}
	schema, ok := compiled[name]
	if !ok {
		return nil, fmt.Errorf("embedded schema %q is missing", name)
	}
	return schema, nil
}

var discriminatorFields = []string{
	"decision", "mechanism", "commercialMode", "bindingScope", "portability",
	"delivery", "kind", "resolution", "sessionMode", "mode", "evidenceTier", "transport",
}

func classifyValidationError(err error) *ContractError {
	message := err.Error()
	lower := strings.ToLower(message)
	if strings.Contains(lower, "additionalproperties") || strings.Contains(lower, "additional properties") {
		return contractError(ErrorUnknownField, nil, "%s", message)
	}
	if strings.Contains(lower, "required") {
		return contractError(ErrorMissingRequiredField, nil, "%s", message)
	}
	if strings.Contains(lower, "'allof' failed") {
		return contractError(ErrorInvalidReference, nil, "%s", message)
	}
	for _, field := range discriminatorFields {
		if strings.Contains(message, field) {
			return contractError(ErrorUnknownDiscriminator, []string{field}, "%s", message)
		}
	}
	return contractError(ErrorInvalidReference, nil, "%s", message)
}

func rejectDuplicateFields(raw []byte) *ContractError {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := scanJSONValue(decoder, nil); err != nil {
		var contractErr *ContractError
		if errors.As(err, &contractErr) {
			return contractErr
		}
		return contractError(ErrorInvalidReference, nil, "invalid JSON: %v", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return contractError(ErrorInvalidReference, nil, "invalid JSON: trailing value")
		}
		return contractError(ErrorInvalidReference, nil, "invalid JSON: %v", err)
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, path []string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			childPath := append(slices.Clone(path), key)
			if _, duplicate := seen[key]; duplicate {
				return contractError(ErrorUnknownField, childPath, "duplicate field %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder, childPath); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("object is not closed")
		}
	case '[':
		for index := 0; decoder.More(); index++ {
			childPath := append(slices.Clone(path), fmt.Sprintf("%d", index))
			if err := scanJSONValue(decoder, childPath); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("array is not closed")
		}
	default:
		return fmt.Errorf("unexpected delimiter %q", delimiter)
	}
	return nil
}

func decodeVersioned[T any](raw []byte, schemaName string) (T, error) {
	var zero T
	if err := rejectDuplicateFields(raw); err != nil {
		return zero, err
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(raw, &header); err != nil || header == nil {
		return zero, contractError(ErrorInvalidReference, nil, "contract document must be a JSON object")
	}
	versionRaw, ok := header["contractVersion"]
	if !ok {
		return zero, contractError(ErrorMissingRequiredField, []string{"contractVersion"}, "contractVersion is required")
	}
	var version string
	if err := json.Unmarshal(versionRaw, &version); err != nil {
		return zero, contractError(ErrorUnsupportedContractVersion, []string{"contractVersion"}, "contractVersion must be a string")
	}
	if version != ContractVersion {
		return zero, contractError(ErrorUnsupportedContractVersion, []string{"contractVersion"}, "unsupported contractVersion %q", version)
	}
	schema, err := schemaFor(schemaName)
	if err != nil {
		return zero, contractError(ErrorInvalidReference, nil, "load %s schema: %v", schemaName, err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return zero, contractError(ErrorInvalidReference, nil, "decode contract JSON: %v", err)
	}
	if err := schema.Validate(document); err != nil {
		return zero, classifyValidationError(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var result T
	if err := decoder.Decode(&result); err != nil {
		return zero, contractError(ErrorInvalidReference, nil, "decode %s: %v", schemaName, err)
	}
	return result, nil
}

// CanonicalJSON implements RFC 8785 JSON canonicalization for stable digests.
func CanonicalJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, contractError(ErrorInvalidReference, nil, "marshal contract value: %v", err)
	}
	canonical, err := jsoncanonicalizer.Transform(raw)
	if err != nil {
		return nil, contractError(ErrorInvalidReference, nil, "canonicalize contract value: %v", err)
	}
	return canonical, nil
}

// DigestContractValue returns the SHA-256 of RFC 8785 canonical JSON bytes.
func DigestContractValue(value any) (string, error) {
	canonical, err := CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

// ExecutionSelectorRegistry is an optional fail-closed catalog for intent decoding.
type ExecutionSelectorRegistry struct {
	HarnessVersions map[string][]string
	Models          []string
	Endpoints       []string
	AuthBindings    []string
	Placements      []string
	Capabilities    []string
}

func hasSelector(values []string, value string) bool {
	return values == nil || slices.Contains(values, value)
}

func assertAuthBindingDelivery(binding *AuthBindingRef, path []string) *ContractError {
	if binding != nil && binding.Mechanism == AuthNone && binding.Delivery != DeliveryNone {
		return contractError(
			ErrorInvalidReference,
			append(slices.Clone(path), "delivery"),
			"auth mechanism %q requires delivery %q; no-auth bindings never authorize ambient credentials",
			AuthNone,
			DeliveryNone,
		)
	}
	return nil
}

func assertDispatchAuthBindings(intent DispatchIntent) *ContractError {
	if err := assertAuthBindingDelivery(intent.AuthBinding, []string{"authBinding"}); err != nil {
		return err
	}
	for index := range intent.FallbackAlternatives {
		if err := assertAuthBindingDelivery(
			intent.FallbackAlternatives[index].AuthBinding,
			[]string{"fallbackAlternatives", fmt.Sprintf("%d", index), "authBinding"},
		); err != nil {
			return err
		}
	}
	return nil
}

func assertKnownSelectors(intent DispatchIntent, registry *ExecutionSelectorRegistry) *ContractError {
	type candidate struct {
		harness     *HarnessRef
		model       *ModelRef
		endpoint    *ServingEndpointRef
		authBinding *AuthBindingRef
		placement   *PlacementRef
		path        []string
	}
	candidates := []candidate{{intent.Harness, &intent.Model, intent.Endpoint, intent.AuthBinding, intent.Placement, nil}}
	for index := range intent.FallbackAlternatives {
		alternative := &intent.FallbackAlternatives[index]
		candidates = append(candidates, candidate{
			alternative.Harness, alternative.Model, alternative.Endpoint,
			alternative.AuthBinding, alternative.Placement,
			[]string{"fallbackAlternatives", fmt.Sprintf("%d", index)},
		})
	}
	for _, candidate := range candidates {
		if candidate.harness != nil && registry.HarnessVersions != nil {
			versions, ok := registry.HarnessVersions[candidate.harness.ID]
			if !ok || !slices.Contains(versions, candidate.harness.Version) {
				return contractError(ErrorInvalidReference, append(candidate.path, "harness"), "unknown harness selector %s@%s", candidate.harness.ID, candidate.harness.Version)
			}
		}
		if candidate.model != nil && !hasSelector(registry.Models, candidate.model.Author+"/"+candidate.model.ID) {
			return contractError(ErrorInvalidReference, append(candidate.path, "model"), "unknown model selector %s/%s", candidate.model.Author, candidate.model.ID)
		}
		if candidate.endpoint != nil && !hasSelector(registry.Endpoints, candidate.endpoint.ID) {
			return contractError(ErrorInvalidReference, append(candidate.path, "endpoint"), "unknown endpoint selector %s", candidate.endpoint.ID)
		}
		if candidate.authBinding != nil && !hasSelector(registry.AuthBindings, candidate.authBinding.ID) {
			return contractError(ErrorInvalidReference, append(candidate.path, "authBinding"), "unknown auth binding selector %s", candidate.authBinding.ID)
		}
		if candidate.placement != nil && !hasSelector(registry.Placements, candidate.placement.ID) {
			return contractError(ErrorInvalidReference, append(candidate.path, "placement"), "unknown placement selector %s", candidate.placement.ID)
		}
	}
	for index, capability := range intent.RequiredCapabilities {
		if !hasSelector(registry.Capabilities, capability.Name) {
			return contractError(ErrorInvalidReference, []string{"requiredCapabilities", fmt.Sprintf("%d", index), "name"}, "unknown capability selector %s", capability.Name)
		}
	}
	for index, capability := range intent.OptionalCapabilities {
		if !hasSelector(registry.Capabilities, capability.Name) {
			return contractError(ErrorInvalidReference, []string{"optionalCapabilities", fmt.Sprintf("%d", index), "name"}, "unknown capability selector %s", capability.Name)
		}
	}
	return nil
}

// DecodeDispatchIntent strictly decodes and optionally checks known selectors.
func DecodeDispatchIntent(raw []byte, registry *ExecutionSelectorRegistry) (DispatchIntent, error) {
	intent, err := decodeVersioned[DispatchIntent](raw, "DispatchIntent")
	if err != nil {
		return DispatchIntent{}, err
	}
	if err := assertDispatchAuthBindings(intent); err != nil {
		return DispatchIntent{}, err
	}
	if err := AssertSecretFreeReceipt(intent); err != nil {
		return DispatchIntent{}, err
	}
	if registry != nil {
		if err := assertKnownSelectors(intent, registry); err != nil {
			return DispatchIntent{}, err
		}
	}
	return intent, nil
}

// DecodeResolvedExecutionCell strictly decodes one resolved cell.
func DecodeResolvedExecutionCell(raw []byte) (ResolvedExecutionCell, error) {
	cell, err := decodeVersioned[ResolvedExecutionCell](raw, "ResolvedExecutionCell")
	if err != nil {
		return cell, err
	}
	if contractErr := assertAuthBindingDelivery(&cell.AuthBinding, []string{"authBinding"}); contractErr != nil {
		return cell, contractErr
	}
	return cell, AssertSecretFreeReceipt(cell)
}

// DecodeDelegationEdgeIntent strictly decodes one parent-child edge intent.
func DecodeDelegationEdgeIntent(raw []byte) (DelegationEdgeIntent, error) {
	edge, err := decodeVersioned[DelegationEdgeIntent](raw, "DelegationEdgeIntent")
	if err == nil {
		err = AssertSecretFreeReceipt(edge)
	}
	return edge, err
}

var (
	forbiddenField  = regexp.MustCompile(`(?i)^(api[_-]?key|access[_-]?token|refresh[_-]?token|token|secret|password|authorization|cookie|headers?|env|credential[_-]?value|endpoint[_-]?url|base[_-]?url|url|file[_-]?contents?|delivery[_-]?payload)$`)
	forbiddenValues = []*regexp.Regexp{
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
		regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9._~+/-]+=*`),
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
		regexp.MustCompile(`\b(sk|rk|ghp|gho|github_pat|xox[baprs])-[_A-Za-z0-9-]{8,}\b`),
		regexp.MustCompile(`\b[A-Z][A-Z0-9_]{2,}=[^\s]+`),
		regexp.MustCompile(`://`),
	}
)

func assertSecretFree(value any, path []string) *ContractError {
	switch typed := value.(type) {
	case string:
		for _, pattern := range forbiddenValues {
			if pattern.MatchString(typed) {
				return contractError(ErrorSecretMaterialForbidden, path, "receipt contains secret-bearing or endpoint-delivery material")
			}
		}
	case []any:
		for index, child := range typed {
			if err := assertSecretFree(child, append(slices.Clone(path), fmt.Sprintf("%d", index))); err != nil {
				return err
			}
		}
	case map[string]any:
		for key, child := range typed {
			childPath := append(slices.Clone(path), key)
			if forbiddenField.MatchString(key) {
				return contractError(ErrorSecretMaterialForbidden, childPath, "receipt field %s is secret-bearing or delivery-specific", key)
			}
			if err := assertSecretFree(child, childPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// AssertSecretFreeReceipt rejects secret or delivery material at any depth.
func AssertSecretFreeReceipt(value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return contractError(ErrorInvalidReference, nil, "marshal receipt: %v", err)
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return contractError(ErrorInvalidReference, nil, "decode receipt: %v", err)
	}
	if err := assertSecretFree(document, nil); err != nil {
		return err
	}
	return nil
}

// ImmutableAdmissionReceipt owns canonical bytes and returns defensive value
// copies, preventing callers from mutating receipt evidence after decoding.
type ImmutableAdmissionReceipt struct {
	canonical []byte
}

// DecodeAdmissionReceipt validates receipt shape and stores canonical immutable bytes.
func DecodeAdmissionReceipt(raw []byte) (ImmutableAdmissionReceipt, error) {
	receipt, err := decodeVersioned[AdmissionReceipt](raw, "AdmissionReceipt")
	if err != nil {
		return ImmutableAdmissionReceipt{}, err
	}
	if receipt.Cell != nil {
		if err := assertAuthBindingDelivery(&receipt.Cell.AuthBinding, []string{"cell", "authBinding"}); err != nil {
			return ImmutableAdmissionReceipt{}, err
		}
	}
	if err := AssertSecretFreeReceipt(receipt); err != nil {
		return ImmutableAdmissionReceipt{}, err
	}
	canonical, canonicalErr := CanonicalJSON(receipt)
	if canonicalErr != nil {
		return ImmutableAdmissionReceipt{}, canonicalErr
	}
	return ImmutableAdmissionReceipt{canonical: canonical}, nil
}

// Value returns a defensive copy of the decoded admission receipt.
func (r ImmutableAdmissionReceipt) Value() AdmissionReceipt {
	var value AdmissionReceipt
	_ = json.Unmarshal(r.canonical, &value)
	return value
}

// Bytes returns a defensive copy of the canonical receipt bytes.
func (r ImmutableAdmissionReceipt) Bytes() []byte { return bytes.Clone(r.canonical) }

// MarshalJSON implements json.Marshaler without exposing mutable backing bytes.
func (r ImmutableAdmissionReceipt) MarshalJSON() ([]byte, error) { return r.Bytes(), nil }

// ImmutableClaimReceipt owns canonical bytes for claim evidence.
type ImmutableClaimReceipt struct {
	canonical []byte
}

// DecodeClaimReceipt validates receipt shape and stores canonical immutable bytes.
func DecodeClaimReceipt(raw []byte) (ImmutableClaimReceipt, error) {
	receipt, err := decodeVersioned[ClaimReceipt](raw, "ClaimReceipt")
	if err != nil {
		return ImmutableClaimReceipt{}, err
	}
	if receipt.EffectiveCell != nil {
		if err := assertAuthBindingDelivery(&receipt.EffectiveCell.AuthBinding, []string{"effectiveCell", "authBinding"}); err != nil {
			return ImmutableClaimReceipt{}, err
		}
	}
	if err := AssertSecretFreeReceipt(receipt); err != nil {
		return ImmutableClaimReceipt{}, err
	}
	canonical, canonicalErr := CanonicalJSON(receipt)
	if canonicalErr != nil {
		return ImmutableClaimReceipt{}, canonicalErr
	}
	return ImmutableClaimReceipt{canonical: canonical}, nil
}

// Value returns a defensive copy of the decoded claim receipt.
func (r ImmutableClaimReceipt) Value() ClaimReceipt {
	var value ClaimReceipt
	_ = json.Unmarshal(r.canonical, &value)
	return value
}

// Bytes returns a defensive copy of the canonical claim receipt bytes.
func (r ImmutableClaimReceipt) Bytes() []byte { return bytes.Clone(r.canonical) }

// MarshalJSON implements json.Marshaler without exposing mutable backing bytes.
func (r ImmutableClaimReceipt) MarshalJSON() ([]byte, error) { return r.Bytes(), nil }

// ImmutableSessionRef owns canonical bytes for a lifecycle reference.
type ImmutableSessionRef struct {
	canonical []byte
}

// DecodeSessionRef validates a session reference and stores canonical immutable bytes.
func DecodeSessionRef(raw []byte) (ImmutableSessionRef, error) {
	ref, err := decodeVersioned[SessionRef](raw, "SessionRef")
	if err != nil {
		return ImmutableSessionRef{}, err
	}
	if err := AssertSecretFreeReceipt(ref); err != nil {
		return ImmutableSessionRef{}, err
	}
	canonical, canonicalErr := CanonicalJSON(ref)
	if canonicalErr != nil {
		return ImmutableSessionRef{}, canonicalErr
	}
	return ImmutableSessionRef{canonical: canonical}, nil
}

// Value returns a defensive copy of the decoded session reference.
func (r ImmutableSessionRef) Value() SessionRef {
	var value SessionRef
	_ = json.Unmarshal(r.canonical, &value)
	return value
}

// Bytes returns a defensive copy of the canonical session-reference bytes.
func (r ImmutableSessionRef) Bytes() []byte { return bytes.Clone(r.canonical) }

// MarshalJSON implements json.Marshaler without exposing mutable backing bytes.
func (r ImmutableSessionRef) MarshalJSON() ([]byte, error) { return r.Bytes(), nil }

func sameValue(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

// AssertNarrowClaim proves claim-time resolution only binds a claim-bound pool
// to an exact host and refreshes runtimeInventoryDigest.
func AssertNarrowClaim(admission ImmutableAdmissionReceipt, claim ImmutableClaimReceipt) error {
	beforeReceipt := admission.Value()
	afterReceipt := claim.Value()
	if beforeReceipt.Decision != AdmissionAdmitted || beforeReceipt.Cell == nil {
		return contractError(ErrorInvalidReference, nil, "a claim must reference an admitted receipt")
	}
	if afterReceipt.AdmissionReceiptID != beforeReceipt.ReceiptID {
		return contractError(ErrorInvalidReference, []string{"admissionReceiptId"}, "claim does not reference the admission receipt")
	}
	before := *beforeReceipt.Cell
	if before.Placement.Kind != PlacementPool || before.Placement.Resolution != PlacementClaimBound {
		return contractError(ErrorInvalidReference, []string{"admissionReceiptId"}, "claim receipts require a claim-bound pool admission")
	}
	if afterReceipt.Decision == ClaimDenied {
		return nil
	}
	if afterReceipt.EffectiveCell == nil {
		return contractError(ErrorMissingRequiredField, []string{"effectiveCell"}, "claimed receipt has no effective cell")
	}
	after := *afterReceipt.EffectiveCell
	if after.Placement.Kind != PlacementHost || after.Placement.Resolution != PlacementExact {
		return contractError(ErrorInvalidReference, []string{"effectiveCell", "placement"}, "claim must narrow placement to one exact host")
	}
	checks := []struct {
		name          string
		before, after any
	}{
		{"contractVersion", before.ContractVersion, after.ContractVersion},
		{"harness", before.Harness, after.Harness},
		{"model", before.Model, after.Model},
		{"endpoint", before.Endpoint, after.Endpoint},
		{"authBinding", before.AuthBinding, after.AuthBinding},
		{"sessionMode", before.SessionMode, after.SessionMode},
		{"grantedCapabilities", before.GrantedCapabilities, after.GrantedCapabilities},
		{"evidenceTier", before.EvidenceTier, after.EvidenceTier},
		{"compatibilityDigest", before.CompatibilityDigest, after.CompatibilityDigest},
	}
	for _, check := range checks {
		if !sameValue(check.before, check.after) {
			return contractError(ErrorInvalidReference, []string{"effectiveCell", check.name}, "claim changed immutable admitted axis %s", check.name)
		}
	}
	return nil
}

func fallbackContains(intent DispatchIntent, alternative FallbackAlternative, selected ResolvedExecutionCell, fallbackFields, nonFallbackFields map[string]struct{}) bool {
	return fallbackAxisContains(alternative.Harness, intent.Harness, selected.Harness, hasField(fallbackFields, "harness"), hasField(nonFallbackFields, "harness")) &&
		fallbackAxisContains(alternative.Model, &intent.Model, selected.Model, hasField(fallbackFields, "model"), hasField(nonFallbackFields, "model")) &&
		fallbackAxisContains(alternative.Endpoint, intent.Endpoint, selected.Endpoint, hasField(fallbackFields, "endpoint"), hasField(nonFallbackFields, "endpoint")) &&
		fallbackAxisContains(alternative.AuthBinding, intent.AuthBinding, selected.AuthBinding, hasField(fallbackFields, "authBinding"), hasField(nonFallbackFields, "authBinding")) &&
		fallbackAxisContains(alternative.Placement, intent.Placement, selected.Placement, hasField(fallbackFields, "placement"), hasField(nonFallbackFields, "placement"))
}

func fallbackAxisContains[T any](fallback, requested *T, selected T, hasFallbackProvenance, hasNonFallbackProvenance bool) bool {
	if fallback != nil {
		if !sameValue(fallback, selected) {
			return false
		}
		if requested == nil || !sameValue(requested, selected) {
			return hasFallbackProvenance
		}
		return true
	}
	if hasFallbackProvenance {
		return false
	}
	if requested != nil {
		return sameValue(requested, selected)
	}
	return hasNonFallbackProvenance
}

func hasField(fields map[string]struct{}, field string) bool {
	_, ok := fields[field]
	return ok
}

func fallbackDecisionSelectedRef(field string, cell ResolvedExecutionCell) (string, bool) {
	switch field {
	case "harness":
		return "harness:" + cell.Harness.ID + "@" + cell.Harness.Version, true
	case "model":
		return "model:" + cell.Model.Author + "/" + cell.Model.ID, true
	case "endpoint":
		return "endpoint:" + cell.Endpoint.ID, true
	case "authBinding":
		return "auth-binding:" + cell.AuthBinding.ID, true
	case "placement":
		return "placement:" + cell.Placement.ID, true
	default:
		return "", false
	}
}

// AssertAdmissionProvenance requires every default, inheritance, fallback, or
// legacy inference to remain visible in the immutable receipt.
func AssertAdmissionProvenance(intent DispatchIntent, receipt ImmutableAdmissionReceipt) error {
	if err := assertDispatchAuthBindings(intent); err != nil {
		return err
	}
	value := receipt.Value()
	if value.RequestID != intent.RequestID {
		return contractError(ErrorInvalidReference, []string{"requestId"}, "receipt request does not match intent")
	}
	digest, err := DigestContractValue(intent)
	if err != nil {
		return err
	}
	if value.IntentDigest != digest {
		return contractError(ErrorInvalidReference, []string{"intentDigest"}, "receipt intent digest does not match intent")
	}
	if value.Decision == AdmissionDenied {
		return nil
	}
	if value.Cell == nil {
		return contractError(ErrorMissingRequiredField, []string{"cell"}, "admitted receipt has no cell")
	}
	if value.Cell.SessionMode != intent.SessionMode {
		return contractError(ErrorInvalidReference, []string{"cell", "sessionMode"}, "session mode changed during admission")
	}
	fields := []struct {
		name             string
		requestedPresent bool
		requested        any
		selected         any
	}{
		{"harness", intent.Harness != nil, intent.Harness, value.Cell.Harness},
		{"model", true, intent.Model, value.Cell.Model},
		{"endpoint", intent.Endpoint != nil, intent.Endpoint, value.Cell.Endpoint},
		{"authBinding", intent.AuthBinding != nil, intent.AuthBinding, value.Cell.AuthBinding},
		{"placement", intent.Placement != nil, intent.Placement, value.Cell.Placement},
	}
	var selectedFallback *FallbackAlternative
	selectedFallbackID := ""
	alternativesByID := make(map[string]*FallbackAlternative, len(intent.FallbackAlternatives))
	for index := range intent.FallbackAlternatives {
		alternative := &intent.FallbackAlternatives[index]
		if _, duplicate := alternativesByID[alternative.ID]; duplicate {
			return contractError(ErrorInvalidReference, []string{"fallbackAlternatives", fmt.Sprintf("%d", index), "id"}, "duplicate fallbackAlternative id %q", alternative.ID)
		}
		alternativesByID[alternative.ID] = alternative
	}
	// Every execution-cell axis has one authoritative resolver decision.  A
	// receipt that names an axis twice is ambiguous even if neither decision is
	// a fallback (for example, a default followed by legacy inference).  Reject
	// that before interpreting decision kinds so callers cannot smuggle a
	// competing provenance path alongside the effective one.
	decisionFields := make(map[string]struct{})
	fallbackDecisionFields := make(map[string]struct{})
	nonFallbackFields := make(map[string]struct{})
	for index, decision := range value.ResolverDecisions {
		decisionPath := []string{"resolverDecisions", fmt.Sprintf("%d", index)}
		expectedSelectedRef, knownField := fallbackDecisionSelectedRef(decision.Field, *value.Cell)
		if knownField {
			if _, duplicate := decisionFields[decision.Field]; duplicate {
				return contractError(ErrorInvalidReference, append(decisionPath, "field"), "duplicate resolver decision for %s", decision.Field)
			}
			decisionFields[decision.Field] = struct{}{}
		}
		if knownField && decision.SelectedRef != expectedSelectedRef {
			return contractError(ErrorInvalidReference, append(decisionPath, "selectedRef"), "resolver decision selectedRef %q does not match resolved %s %q", decision.SelectedRef, decision.Field, expectedSelectedRef)
		}
		if knownField && decision.Kind != DecisionFallback && decision.Kind != DecisionExplicit {
			nonFallbackFields[decision.Field] = struct{}{}
		}
		if decision.Kind != DecisionFallback {
			continue
		}
		if !knownField {
			return contractError(ErrorInvalidReference, append(decisionPath, "field"), "fallback resolver decision names unknown execution-cell axis %q", decision.Field)
		}
		if _, duplicate := fallbackDecisionFields[decision.Field]; duplicate {
			return contractError(ErrorInvalidReference, append(decisionPath, "field"), "duplicate fallback resolver decision for %s", decision.Field)
		}
		fallbackDecisionFields[decision.Field] = struct{}{}
		path := slices.Clone(decisionPath)
		path = append(path, "sourceRef")
		if decision.SourceRef == "" {
			return contractError(ErrorInvalidReference, path, "fallback resolver decision must name its fallbackAlternative id in sourceRef")
		}
		alternative := alternativesByID[decision.SourceRef]
		if alternative == nil {
			return contractError(ErrorInvalidReference, path, "fallback resolver decision names unknown fallbackAlternative %q", decision.SourceRef)
		}
		if selectedFallbackID != "" && selectedFallbackID != decision.SourceRef {
			return contractError(ErrorInvalidReference, path, "fallback resolver decisions name mixed alternatives %q and %q", selectedFallbackID, decision.SourceRef)
		}
		selectedFallbackID = decision.SourceRef
		selectedFallback = alternative
	}
	for _, field := range fields {
		if field.requestedPresent && sameValue(field.requested, field.selected) {
			continue
		}
		found := false
		for _, decision := range value.ResolverDecisions {
			if decision.Field != field.name || decision.Kind == DecisionExplicit {
				continue
			}
			if field.requestedPresent && decision.Kind != DecisionFallback {
				continue
			}
			found = true
			break
		}
		if !found {
			return contractError(ErrorInvalidReference, []string{"resolverDecisions"}, "resolved %s lacks resolver provenance", field.name)
		}
	}
	if selectedFallback != nil && !fallbackContains(intent, *selectedFallback, *value.Cell, fallbackDecisionFields, nonFallbackFields) {
		return contractError(ErrorInvalidReference, []string{"cell"}, "resolved execution cell was not wholly named by fallbackAlternative %q", selectedFallbackID)
	}
	for _, required := range intent.RequiredCapabilities {
		granted := false
		for _, capability := range value.Cell.GrantedCapabilities {
			if sameValue(required, capability) {
				granted = true
				break
			}
		}
		if !granted {
			return contractError(ErrorInvalidReference, []string{"cell", "grantedCapabilities"}, "required capability %s was not granted", required.Name)
		}
	}
	return nil
}
