package executioncell

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Applied preflight config contract versions, source discriminators, and the
// two fixed Donmai-owned environment targets.
const (
	PreflightConfigRequirementContractVersion     = "execution-preflight-config-requirement/v1"
	PreflightConfigMaterializationContractVersion = "execution-preflight-config-materialization/v1"
	PreflightConfigSourceOperationalEnvironment   = "operational_environment"
	PreflightConfigSourceDaemonSessionID          = "daemon_session_id"
	PreflightConfigSourceSessionMCPBearerFile     = "session_mcp_bearer_file" //nolint:gosec // G101: public discriminator, never credential bytes.
	PreflightConfigSessionIDTarget                = "DONMAI_SESSION_ID"
	PreflightConfigSessionMCPBearerFileTarget     = "MCP_GATEWAY_TOKEN_FILE" //nolint:gosec // G101: public environment name, never credential bytes.
	PreflightConfigPrivateFileMode                = "0600"
	// ProtectedRuntimeMCPConfigContractVersion identifies the v1 requirement
	// and digest-only materialization for one protected HTTP MCP server.
	ProtectedRuntimeMCPConfigContractVersion = "execution-preflight-protected-mcp-config/v1"
	// ProtectedRuntimeMCPConfigContractVersionV2 adds a closed authorization source.
	ProtectedRuntimeMCPConfigContractVersionV2 = "execution-preflight-protected-mcp-config/v2"
	ProtectedRuntimeMCPTransportHTTP           = "http"
)

// ProtectedRuntimeMCPHeaderV1 binds one HTTP header name to the digest of its
// exact runtime value. The value itself never enters the receipt.
type ProtectedRuntimeMCPHeaderV1 struct {
	Name        string `json:"name"`
	ValueDigest string `json:"valueDigest"`
}

// ProtectedRuntimeMCPConfigRequirementV1 commits the protected server that a
// selected capability expects the daemon to materialize for the child.
type ProtectedRuntimeMCPConfigRequirementV1 struct {
	ContractVersion          string                        `json:"contractVersion"`
	RequirementID            string                        `json:"requirementId"`
	AuthorityBindingDigest   string                        `json:"authorityBindingDigest"`
	OperationalPayloadDigest string                        `json:"operationalPayloadDigest"`
	ServerName               string                        `json:"serverName"`
	Transport                string                        `json:"transport"`
	EndpointDigest           string                        `json:"endpointDigest"`
	Headers                  []ProtectedRuntimeMCPHeaderV1 `json:"headers"`
}

// ProtectedRuntimeMCPConfigMaterializationV1 is the daemon's digest-only
// evidence that it re-derived the exact required runtime server configuration.
type ProtectedRuntimeMCPConfigMaterializationV1 struct {
	ContractVersion          string                        `json:"contractVersion"`
	RequirementID            string                        `json:"requirementId"`
	AuthorityBindingDigest   string                        `json:"authorityBindingDigest"`
	OperationalPayloadDigest string                        `json:"operationalPayloadDigest"`
	ServerName               string                        `json:"serverName"`
	Transport                string                        `json:"transport"`
	EndpointDigest           string                        `json:"endpointDigest"`
	Headers                  []ProtectedRuntimeMCPHeaderV1 `json:"headers"`
	ConfigReferenceDigest    string                        `json:"configReferenceDigest"`
}

// ProtectedRuntimeMCPAuthorizationSourceV2 identifies the common private-file
// requirement that a protected v2 configuration must join during materialization.
type ProtectedRuntimeMCPAuthorizationSourceV2 struct {
	Kind                string `json:"kind"`
	ConfigRequirementID string `json:"configRequirementId"`
	TargetEnv           string `json:"targetEnv"`
	Mode                string `json:"mode"`
}

// ProtectedRuntimeMCPAuthorizationSourceMaterializationV2 is structural,
// digest-only evidence. Its validity does not prove that the referenced common
// materialization or file binding exists; the runtime join must prove that.
type ProtectedRuntimeMCPAuthorizationSourceMaterializationV2 struct {
	Kind                  string `json:"kind"`
	ConfigRequirementID   string `json:"configRequirementId"`
	TargetEnv             string `json:"targetEnv"`
	Mode                  string `json:"mode"`
	ConfigReferenceDigest string `json:"configReferenceDigest"`
	FileReferenceDigest   string `json:"fileReferenceDigest"`
	HelperCommandDigest   string `json:"helperCommandDigest"`
}

// ProtectedRuntimeMCPConfigRequirementV2 retains the v1 protected-server
// projection and identifies its sole permitted authorization source.
type ProtectedRuntimeMCPConfigRequirementV2 struct {
	ContractVersion          string                                   `json:"contractVersion"`
	RequirementID            string                                   `json:"requirementId"`
	AuthorityBindingDigest   string                                   `json:"authorityBindingDigest"`
	OperationalPayloadDigest string                                   `json:"operationalPayloadDigest"`
	ServerName               string                                   `json:"serverName"`
	Transport                string                                   `json:"transport"`
	EndpointDigest           string                                   `json:"endpointDigest"`
	Headers                  []ProtectedRuntimeMCPHeaderV1            `json:"headers"`
	AuthorizationSource      ProtectedRuntimeMCPAuthorizationSourceV2 `json:"authorizationSource"`
}

// ProtectedRuntimeMCPConfigMaterializationV2 binds the complete protected
// projection, including structural references to common config and helper evidence.
type ProtectedRuntimeMCPConfigMaterializationV2 struct {
	ContractVersion          string                                                  `json:"contractVersion"`
	RequirementID            string                                                  `json:"requirementId"`
	AuthorityBindingDigest   string                                                  `json:"authorityBindingDigest"`
	OperationalPayloadDigest string                                                  `json:"operationalPayloadDigest"`
	ServerName               string                                                  `json:"serverName"`
	Transport                string                                                  `json:"transport"`
	EndpointDigest           string                                                  `json:"endpointDigest"`
	Headers                  []ProtectedRuntimeMCPHeaderV1                           `json:"headers"`
	AuthorizationSource      ProtectedRuntimeMCPAuthorizationSourceMaterializationV2 `json:"authorizationSource"`
	ConfigReferenceDigest    string                                                  `json:"configReferenceDigest"`
}

var (
	preflightEnvironmentName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
	preflightRequirementID   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,255}$`)
	protectedMCPHeaderName   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,127}$`)
)

// PreflightConfigBindingSourceV1 selects one daemon-owned common-config source.
type PreflightConfigBindingSourceV1 struct {
	Kind            string `json:"kind"`
	EnvironmentName string `json:"environmentName,omitempty"`
	Mode            string `json:"mode,omitempty"`
}

// PreflightConfigBindingV1 maps one closed source onto an environment target.
type PreflightConfigBindingV1 struct {
	TargetEnv string                         `json:"targetEnv"`
	Source    PreflightConfigBindingSourceV1 `json:"source"`
}

// PreflightConfigRequirementV1 is a trusted post-compilation config request.
type PreflightConfigRequirementV1 struct {
	ContractVersion          string                     `json:"contractVersion"`
	RequirementID            string                     `json:"requirementId"`
	AuthorityBindingDigest   string                     `json:"authorityBindingDigest"`
	OperationalPayloadDigest string                     `json:"operationalPayloadDigest"`
	Bindings                 []PreflightConfigBindingV1 `json:"bindings"`
}

// PreflightConfigBindingMaterializationV1 is digest-only applied binding evidence.
type PreflightConfigBindingMaterializationV1 struct {
	TargetEnv           string                         `json:"targetEnv"`
	Source              PreflightConfigBindingSourceV1 `json:"source"`
	ValueDigest         string                         `json:"valueDigest,omitempty"`
	BearerContentDigest string                         `json:"bearerContentDigest,omitempty"`
	FileReferenceDigest string                         `json:"fileReferenceDigest,omitempty"`
	Mode                string                         `json:"mode,omitempty"`
}

// PreflightConfigMaterializationV1 binds all applied config for one requirement.
type PreflightConfigMaterializationV1 struct {
	ContractVersion          string                                    `json:"contractVersion"`
	RequirementID            string                                    `json:"requirementId"`
	AuthorityBindingDigest   string                                    `json:"authorityBindingDigest"`
	OperationalPayloadDigest string                                    `json:"operationalPayloadDigest"`
	Bindings                 []PreflightConfigBindingMaterializationV1 `json:"bindings"`
	ConfigReferenceDigest    string                                    `json:"configReferenceDigest"`
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validatePreflightConfigSource(target string, source PreflightConfigBindingSourceV1) error {
	if !preflightEnvironmentName.MatchString(target) {
		return errors.New("executioncell: preflight config target environment name is invalid")
	}
	switch source.Kind {
	case PreflightConfigSourceOperationalEnvironment:
		if source.EnvironmentName != target || !preflightEnvironmentName.MatchString(source.EnvironmentName) || source.Mode != "" {
			return errors.New("executioncell: operational environment config source must be a same-name copy")
		}
	case PreflightConfigSourceDaemonSessionID:
		if target != PreflightConfigSessionIDTarget || source.EnvironmentName != "" || source.Mode != "" {
			return errors.New("executioncell: daemon session config source has an invalid target")
		}
	case PreflightConfigSourceSessionMCPBearerFile:
		if target != PreflightConfigSessionMCPBearerFileTarget || source.EnvironmentName != "" || source.Mode != PreflightConfigPrivateFileMode {
			return errors.New("executioncell: session MCP bearer config source requires the private file target and mode")
		}
	default:
		return fmt.Errorf("executioncell: unsupported preflight config source %q", source.Kind)
	}
	return nil
}

// ValidatePreflightConfigRequirement enforces the closed source/target grammar.
func ValidatePreflightConfigRequirement(value PreflightConfigRequirementV1) error {
	if value.ContractVersion != PreflightConfigRequirementContractVersion || !preflightRequirementID.MatchString(value.RequirementID) {
		return errors.New("executioncell: invalid preflight config requirement identity")
	}
	if !validSHA256(value.AuthorityBindingDigest) || !validSHA256(value.OperationalPayloadDigest) || len(value.Bindings) == 0 {
		return errors.New("executioncell: preflight config requirement digests and bindings are required")
	}
	for i, binding := range value.Bindings {
		if err := validatePreflightConfigSource(binding.TargetEnv, binding.Source); err != nil {
			return err
		}
		if i > 0 && value.Bindings[i-1].TargetEnv >= binding.TargetEnv {
			return errors.New("executioncell: preflight config bindings must be unique and sorted")
		}
	}
	return nil
}

// DigestPreflightConfigReference hashes every materialization field except itself.
func DigestPreflightConfigReference(value PreflightConfigMaterializationV1) (string, error) {
	projection := value
	projection.ConfigReferenceDigest = ""
	raw, err := CanonicalJSON(projection)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// ValidatePreflightConfigMaterialization verifies digest-only applied evidence.
func ValidatePreflightConfigMaterialization(value PreflightConfigMaterializationV1) error {
	if value.ContractVersion != PreflightConfigMaterializationContractVersion || !preflightRequirementID.MatchString(value.RequirementID) {
		return errors.New("executioncell: invalid preflight config materialization identity")
	}
	if !validSHA256(value.AuthorityBindingDigest) || !validSHA256(value.OperationalPayloadDigest) || len(value.Bindings) == 0 {
		return errors.New("executioncell: preflight config materialization digests and bindings are required")
	}
	for i, binding := range value.Bindings {
		if err := validatePreflightConfigSource(binding.TargetEnv, binding.Source); err != nil {
			return err
		}
		if i > 0 && value.Bindings[i-1].TargetEnv >= binding.TargetEnv {
			return errors.New("executioncell: preflight config materializations must be unique and sorted")
		}
		switch binding.Source.Kind {
		case PreflightConfigSourceSessionMCPBearerFile:
			if binding.ValueDigest != "" || !validSHA256(binding.BearerContentDigest) || !validSHA256(binding.FileReferenceDigest) || binding.Mode != PreflightConfigPrivateFileMode {
				return errors.New("executioncell: file config materialization requires content, reference, and mode evidence")
			}
		default:
			if !validSHA256(binding.ValueDigest) || binding.BearerContentDigest != "" || binding.FileReferenceDigest != "" || binding.Mode != "" {
				return errors.New("executioncell: environment config materialization has invalid evidence")
			}
		}
	}
	expected, err := DigestPreflightConfigReference(value)
	if err != nil || value.ConfigReferenceDigest != expected {
		return errors.New("executioncell: preflight config reference digest mismatch")
	}
	return nil
}

// ValidatePreflightConfigMaterializations verifies unique requirement ordering.
func ValidatePreflightConfigMaterializations(values []PreflightConfigMaterializationV1) error {
	if len(values) == 0 {
		return errors.New("executioncell: preflight config materializations are required")
	}
	if !sort.SliceIsSorted(values, func(i, j int) bool { return values[i].RequirementID < values[j].RequirementID }) {
		return errors.New("executioncell: preflight config materializations must be sorted")
	}
	for i := range values {
		if i > 0 && values[i-1].RequirementID == values[i].RequirementID {
			return errors.New("executioncell: duplicate preflight config materialization")
		}
		if err := ValidatePreflightConfigMaterialization(values[i]); err != nil {
			return err
		}
	}
	return nil
}

func validateProtectedRuntimeMCPConfig(
	expectedContractVersion string,
	contractVersion, requirementID, authorityDigest, operationalDigest, serverName, transport, endpointDigest string,
	headers []ProtectedRuntimeMCPHeaderV1,
) error {
	if contractVersion != expectedContractVersion || !preflightRequirementID.MatchString(requirementID) {
		return errors.New("executioncell: invalid protected runtime MCP config identity")
	}
	if !validSHA256(authorityDigest) || !validSHA256(operationalDigest) || !validSHA256(endpointDigest) {
		return errors.New("executioncell: protected runtime MCP config digests are required")
	}
	if !validRuntimeRef(serverName) || transport != ProtectedRuntimeMCPTransportHTTP || len(headers) == 0 {
		return errors.New("executioncell: protected runtime MCP server is invalid")
	}
	for i, header := range headers {
		if !protectedMCPHeaderName.MatchString(header.Name) || !validSHA256(header.ValueDigest) {
			return errors.New("executioncell: protected runtime MCP header is invalid")
		}
		if i > 0 && headers[i-1].Name >= header.Name {
			return errors.New("executioncell: protected runtime MCP headers must be unique and sorted")
		}
	}
	return nil
}

// ValidateProtectedRuntimeMCPConfigRequirement validates the closed protected
// HTTP MCP requirement without accepting raw endpoint or header values.
func ValidateProtectedRuntimeMCPConfigRequirement(value ProtectedRuntimeMCPConfigRequirementV1) error {
	return validateProtectedRuntimeMCPConfig(
		ProtectedRuntimeMCPConfigContractVersion,
		value.ContractVersion, value.RequirementID, value.AuthorityBindingDigest,
		value.OperationalPayloadDigest, value.ServerName, value.Transport,
		value.EndpointDigest, value.Headers,
	)
}

// DigestProtectedRuntimeMCPConfigReference binds every materialization field
// except the digest itself.
func DigestProtectedRuntimeMCPConfigReference(value ProtectedRuntimeMCPConfigMaterializationV1) (string, error) {
	projection := value
	projection.ConfigReferenceDigest = ""
	raw, err := CanonicalJSON(projection)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// ValidateProtectedRuntimeMCPConfigMaterialization validates one digest-only
// protected server materialization.
func ValidateProtectedRuntimeMCPConfigMaterialization(value ProtectedRuntimeMCPConfigMaterializationV1) error {
	if err := validateProtectedRuntimeMCPConfig(
		ProtectedRuntimeMCPConfigContractVersion,
		value.ContractVersion, value.RequirementID, value.AuthorityBindingDigest,
		value.OperationalPayloadDigest, value.ServerName, value.Transport,
		value.EndpointDigest, value.Headers,
	); err != nil {
		return err
	}
	expected, err := DigestProtectedRuntimeMCPConfigReference(value)
	if err != nil || value.ConfigReferenceDigest != expected {
		return errors.New("executioncell: protected runtime MCP config reference digest mismatch")
	}
	return nil
}

func validateProtectedRuntimeMCPAuthorizationSourceV2(value ProtectedRuntimeMCPAuthorizationSourceV2) error {
	if value.Kind != PreflightConfigSourceSessionMCPBearerFile ||
		!preflightRequirementID.MatchString(value.ConfigRequirementID) ||
		value.TargetEnv != PreflightConfigSessionMCPBearerFileTarget ||
		value.Mode != PreflightConfigPrivateFileMode {
		return errors.New("executioncell: protected runtime MCP v2 authorization source is invalid")
	}
	return nil
}

func validateProtectedRuntimeMCPHeadersV2(headers []ProtectedRuntimeMCPHeaderV1) error {
	seen := make(map[string]struct{}, len(headers))
	foundAuthorization := false
	for _, header := range headers {
		canonical := strings.ToLower(header.Name)
		if _, duplicate := seen[canonical]; duplicate {
			return errors.New("executioncell: protected runtime MCP v2 header names must be HTTP-case unique")
		}
		seen[canonical] = struct{}{}
		if strings.EqualFold(header.Name, "Authorization") {
			if header.Name != "Authorization" {
				return errors.New("executioncell: protected runtime MCP v2 Authorization header name is not canonical")
			}
			foundAuthorization = true
		}
	}
	if !foundAuthorization {
		return errors.New("executioncell: protected runtime MCP v2 Authorization header is required")
	}
	return nil
}

// ValidateProtectedRuntimeMCPConfigRequirementV2 validates the closed v2
// requirement shape. It does not perform the runtime common-materialization join.
func ValidateProtectedRuntimeMCPConfigRequirementV2(value ProtectedRuntimeMCPConfigRequirementV2) error {
	if err := validateProtectedRuntimeMCPConfig(
		ProtectedRuntimeMCPConfigContractVersionV2,
		value.ContractVersion, value.RequirementID, value.AuthorityBindingDigest,
		value.OperationalPayloadDigest, value.ServerName, value.Transport,
		value.EndpointDigest, value.Headers,
	); err != nil {
		return err
	}
	if err := validateProtectedRuntimeMCPHeadersV2(value.Headers); err != nil {
		return err
	}
	return validateProtectedRuntimeMCPAuthorizationSourceV2(value.AuthorizationSource)
}

// DigestProtectedRuntimeMCPConfigReferenceV2 binds every v2 materialization
// field except the outer self-digest. Nested reference digests remain bound.
func DigestProtectedRuntimeMCPConfigReferenceV2(value ProtectedRuntimeMCPConfigMaterializationV2) (string, error) {
	projection := value
	projection.ConfigReferenceDigest = ""
	raw, err := CanonicalJSON(projection)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// ValidateProtectedRuntimeMCPConfigMaterializationV2 validates structural v2
// evidence and its self-digest. It does not establish runtime authorization;
// callers must separately join and verify the actual common materialization.
func ValidateProtectedRuntimeMCPConfigMaterializationV2(value ProtectedRuntimeMCPConfigMaterializationV2) error {
	if err := validateProtectedRuntimeMCPConfig(
		ProtectedRuntimeMCPConfigContractVersionV2,
		value.ContractVersion, value.RequirementID, value.AuthorityBindingDigest,
		value.OperationalPayloadDigest, value.ServerName, value.Transport,
		value.EndpointDigest, value.Headers,
	); err != nil {
		return err
	}
	if err := validateProtectedRuntimeMCPHeadersV2(value.Headers); err != nil {
		return err
	}
	source := value.AuthorizationSource
	if err := validateProtectedRuntimeMCPAuthorizationSourceV2(ProtectedRuntimeMCPAuthorizationSourceV2{
		Kind: source.Kind, ConfigRequirementID: source.ConfigRequirementID,
		TargetEnv: source.TargetEnv, Mode: source.Mode,
	}); err != nil {
		return err
	}
	if !validSHA256(source.ConfigReferenceDigest) || !validSHA256(source.FileReferenceDigest) || !validSHA256(source.HelperCommandDigest) {
		return errors.New("executioncell: protected runtime MCP v2 authorization source digests are invalid")
	}
	expected, err := DigestProtectedRuntimeMCPConfigReferenceV2(value)
	if err != nil || value.ConfigReferenceDigest != expected {
		return errors.New("executioncell: protected runtime MCP v2 config reference digest mismatch")
	}
	return nil
}

// ValidateProtectedRuntimeMCPConfigMaterializations validates non-empty,
// unique requirement ordering.
func ValidateProtectedRuntimeMCPConfigMaterializations(values []ProtectedRuntimeMCPConfigMaterializationV1) error {
	if len(values) == 0 {
		return errors.New("executioncell: protected runtime MCP config materializations are required")
	}
	if !sort.SliceIsSorted(values, func(i, j int) bool { return values[i].RequirementID < values[j].RequirementID }) {
		return errors.New("executioncell: protected runtime MCP config materializations must be sorted")
	}
	for i := range values {
		if i > 0 && values[i-1].RequirementID == values[i].RequirementID {
			return errors.New("executioncell: duplicate protected runtime MCP config materialization")
		}
		if err := ValidateProtectedRuntimeMCPConfigMaterialization(values[i]); err != nil {
			return err
		}
	}
	return nil
}
