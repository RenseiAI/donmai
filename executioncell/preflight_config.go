package executioncell

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
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
)

var (
	preflightEnvironmentName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
	preflightRequirementID   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,255}$`)
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
