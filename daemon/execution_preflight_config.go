package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/runtime/statehome"
)

type operationalPreflightConfigSource struct {
	Env          map[string]string `json:"env"`
	MCPAuthToken string            `json:"mcpAuthToken"`
}

type preflightOwnedFile struct {
	path    string
	rootDir string
	name    string
	info    os.FileInfo
	owned   bool
}

type preflightConfigLease struct {
	sessionID string
	files     []preflightOwnedFile
}

func (l preflightConfigLease) cleanup() {
	for _, file := range l.files {
		if !file.owned {
			continue
		}
		root, err := os.OpenRoot(file.rootDir)
		if err != nil {
			continue
		}
		current, err := root.Lstat(file.name)
		if err != nil || !os.SameFile(file.info, current) {
			_ = root.Close()
			continue
		}
		_ = root.Remove(file.name)
		_ = root.Close()
	}
}

type preflightConfigOwnership struct {
	mu             sync.Mutex
	nextGeneration uint64
	leases         map[string]struct {
		generation uint64
		lease      preflightConfigLease
	}
}

func newPreflightConfigRegistry() *preflightConfigOwnership {
	return &preflightConfigOwnership{leases: make(map[string]struct {
		generation uint64
		lease      preflightConfigLease
	})}
}

// Alias keeps the daemon field terse without exposing lifecycle internals.
type preflightConfigRegistry = preflightConfigOwnership

func (r *preflightConfigOwnership) reserve(sessionID string) (uint64, bool) {
	if r == nil || sessionID == "" {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.leases[sessionID]; exists {
		return 0, false
	}
	r.nextGeneration++
	if r.nextGeneration == 0 {
		r.nextGeneration++
	}
	generation := r.nextGeneration
	r.leases[sessionID] = struct {
		generation uint64
		lease      preflightConfigLease
	}{generation: generation, lease: preflightConfigLease{sessionID: sessionID}}
	return generation, true
}

func (r *preflightConfigOwnership) complete(sessionID string, generation uint64, lease preflightConfigLease) bool {
	if r == nil || sessionID == "" || generation == 0 || lease.sessionID != sessionID {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, exists := r.leases[sessionID]
	if !exists || entry.generation != generation || len(entry.lease.files) != 0 {
		return false
	}
	entry.lease = lease
	r.leases[sessionID] = entry
	return true
}

func (r *preflightConfigOwnership) cleanupCurrent(sessionID string) bool {
	if r == nil || sessionID == "" {
		return false
	}
	r.mu.Lock()
	entry, exists := r.leases[sessionID]
	if exists {
		delete(r.leases, sessionID)
	}
	r.mu.Unlock()
	if exists {
		entry.lease.cleanup()
	}
	return exists
}

func (r *preflightConfigOwnership) cleanupIfOwner(sessionID string, generation uint64) bool {
	if r == nil || sessionID == "" || generation == 0 {
		return false
	}
	r.mu.Lock()
	entry, exists := r.leases[sessionID]
	if !exists || entry.generation != generation {
		r.mu.Unlock()
		return false
	}
	delete(r.leases, sessionID)
	r.mu.Unlock()
	entry.lease.cleanup()
	return true
}

func digestConfigValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func applySpecEnvironment(spec *SessionSpec, name, value string) error {
	if spec.Env == nil {
		spec.Env = make(map[string]string)
	}
	if existing, ok := spec.Env[name]; ok && existing != value {
		return fmt.Errorf("preflight config target %s conflicts with session spec", name)
	}
	spec.Env[name] = value
	return nil
}

func verifyConfigFile(path, bearer string) (os.FileInfo, error) {
	clean := filepath.Clean(path)
	root, err := os.OpenRoot(filepath.Dir(clean))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	name := filepath.Base(clean)
	info, err := root.Stat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("preflight config bearer file must be regular mode 0600")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, int64(len(bearer)+1)))
	if readErr == nil && string(raw) != bearer {
		readErr = errors.New("preflight config bearer file content mismatch")
	}
	if readErr == nil {
		readErr = file.Sync()
	}
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return info, nil
}

func materializeOwnedBearerFile(dir, sessionID, requirementID, target, bearer string) (preflightOwnedFile, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return preflightOwnedFile{}, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return preflightOwnedFile{}, err
	}
	defer func() { _ = root.Close() }()
	nameDigest := sha256.Sum256([]byte(sessionID + "\x00" + requirementID + "\x00" + target))
	name := hex.EncodeToString(nameDigest[:]) + ".token"
	path := filepath.Join(dir, name)
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	created := err == nil
	if err != nil && !errors.Is(err, os.ErrExist) {
		return preflightOwnedFile{}, err
	}
	if created {
		if _, err = file.WriteString(bearer); err == nil {
			err = file.Sync()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = root.Remove(name)
			return preflightOwnedFile{}, err
		}
		directory, openErr := root.Open(".")
		if openErr != nil {
			_ = root.Remove(name)
			return preflightOwnedFile{}, openErr
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if syncErr != nil || closeErr != nil {
			_ = root.Remove(name)
			return preflightOwnedFile{}, errors.Join(syncErr, closeErr)
		}
	}
	info, err := verifyConfigFile(path, bearer)
	if err != nil {
		if created {
			_ = root.Remove(name)
		}
		return preflightOwnedFile{}, err
	}
	return preflightOwnedFile{path: path, rootDir: dir, name: name, info: info, owned: true}, nil
}

func hostReceiptWithConfigMaterializations(receipt json.RawMessage, materializations []executioncell.PreflightConfigMaterializationV1) (json.RawMessage, error) {
	decoded, err := executioncell.DecodeHostAdaptationReceipt(receipt)
	if err != nil {
		return nil, err
	}
	if decoded.ContractVersion != executioncell.HostAdaptationContractVersion || decoded.Decision != "ready" {
		return nil, errors.New("preflight config requires one ready host-adaptation/v1 source")
	}
	decoded.ContractVersion = executioncell.HostAdaptationV2ContractVersion
	decoded.ConfigMaterializations = materializations
	raw, err := json.Marshal(decoded)
	if err != nil {
		return nil, err
	}
	if _, err := executioncell.DecodeHostAdaptationReceipt(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func hostReceiptWithProtectedRuntimeMCPConfigs(receipt json.RawMessage, materializations []executioncell.ProtectedRuntimeMCPConfigMaterializationV1) (json.RawMessage, error) {
	decoded, err := executioncell.DecodeHostAdaptationReceipt(receipt)
	if err != nil {
		return nil, err
	}
	if (decoded.ContractVersion != executioncell.HostAdaptationContractVersion && decoded.ContractVersion != executioncell.HostAdaptationV2ContractVersion) || decoded.Decision != "ready" {
		return nil, errors.New("protected runtime MCP config requires one ready host-adaptation/v1 or v2 source")
	}
	decoded.ContractVersion = executioncell.HostAdaptationV3ContractVersion
	decoded.ProtectedRuntimeMCPConfigs = materializations
	raw, err := json.Marshal(decoded)
	if err != nil {
		return nil, err
	}
	if _, err := executioncell.DecodeHostAdaptationReceipt(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func hostReceiptWithProtectedRuntimeMCPConfigsV2(receipt json.RawMessage, materializations []executioncell.ProtectedRuntimeMCPConfigMaterializationV2) (json.RawMessage, error) {
	decoded, err := executioncell.DecodeHostAdaptationReceipt(receipt)
	if err != nil {
		return nil, err
	}
	if (decoded.ContractVersion != executioncell.HostAdaptationContractVersion && decoded.ContractVersion != executioncell.HostAdaptationV2ContractVersion) || decoded.Decision != "ready" {
		return nil, errors.New("protected runtime MCP v2 config requires one ready host-adaptation/v1 or v2 source")
	}
	decoded.ContractVersion = executioncell.HostAdaptationV3ContractVersion
	decoded.ProtectedRuntimeMCPConfigsV2 = materializations
	raw, err := json.Marshal(decoded)
	if err != nil {
		return nil, err
	}
	if _, err := executioncell.DecodeHostAdaptationReceipt(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func materializeProtectedRuntimeMCPConfigs(detail *SessionDetail, requirements []executioncell.ProtectedRuntimeMCPConfigRequirementV1) ([]executioncell.ProtectedRuntimeMCPConfigMaterializationV1, error) {
	if len(requirements) == 0 {
		return nil, nil
	}
	if detail == nil || !sort.SliceIsSorted(requirements, func(i, j int) bool { return requirements[i].RequirementID < requirements[j].RequirementID }) {
		return nil, errors.New("protected runtime MCP config requirements are invalid")
	}
	operationalDigest, err := executioncell.DigestOperationalPayload(detail.OperationalPayload)
	if err != nil {
		return nil, fmt.Errorf("digest protected runtime MCP operational source: %w", err)
	}
	if detail.SessionID == "" || detail.PlatformURL == "" || detail.McpAuthToken == "" || strings.TrimSpace(detail.McpAuthToken) != detail.McpAuthToken {
		return nil, errors.New("protected runtime MCP gateway configuration is unavailable")
	}
	endpointDigest := digestConfigValue(strings.TrimRight(detail.PlatformURL, "/") + "/api/mcp/" + detail.SessionID)
	headers := []executioncell.ProtectedRuntimeMCPHeaderV1{{Name: "Authorization", ValueDigest: digestConfigValue("Bearer " + detail.McpAuthToken)}}
	serverName := statehome.Brand() + "-platform"
	materializations := make([]executioncell.ProtectedRuntimeMCPConfigMaterializationV1, 0, len(requirements))
	for i, requirement := range requirements {
		if i > 0 && requirements[i-1].RequirementID == requirement.RequirementID {
			return nil, errors.New("duplicate protected runtime MCP config requirement")
		}
		if err := executioncell.ValidateProtectedRuntimeMCPConfigRequirement(requirement); err != nil {
			return nil, err
		}
		if requirement.OperationalPayloadDigest != operationalDigest || requirement.ServerName != serverName ||
			requirement.Transport != executioncell.ProtectedRuntimeMCPTransportHTTP || requirement.EndpointDigest != endpointDigest ||
			!reflect.DeepEqual(requirement.Headers, headers) {
			return nil, errors.New("protected runtime MCP config requirement differs from current runtime authority")
		}
		materialization := executioncell.ProtectedRuntimeMCPConfigMaterializationV1{
			ContractVersion: requirement.ContractVersion, RequirementID: requirement.RequirementID,
			AuthorityBindingDigest:   requirement.AuthorityBindingDigest,
			OperationalPayloadDigest: requirement.OperationalPayloadDigest,
			ServerName:               requirement.ServerName, Transport: requirement.Transport,
			EndpointDigest: requirement.EndpointDigest,
			Headers:        append([]executioncell.ProtectedRuntimeMCPHeaderV1(nil), requirement.Headers...),
		}
		materialization.ConfigReferenceDigest, err = executioncell.DigestProtectedRuntimeMCPConfigReference(materialization)
		if err != nil {
			return nil, err
		}
		if err := executioncell.ValidateProtectedRuntimeMCPConfigMaterialization(materialization); err != nil {
			return nil, err
		}
		materializations = append(materializations, materialization)
	}
	return materializations, nil
}

func joinedProtectedRuntimeMCPCommonBinding(
	requirement executioncell.ProtectedRuntimeMCPConfigRequirementV2,
	common []executioncell.PreflightConfigMaterializationV1,
) (executioncell.PreflightConfigMaterializationV1, executioncell.PreflightConfigBindingMaterializationV1, error) {
	if err := executioncell.ValidatePreflightConfigMaterializations(common); err != nil {
		return executioncell.PreflightConfigMaterializationV1{}, executioncell.PreflightConfigBindingMaterializationV1{}, err
	}
	var joined []executioncell.PreflightConfigMaterializationV1
	for _, materialization := range common {
		if materialization.RequirementID == requirement.AuthorizationSource.ConfigRequirementID {
			joined = append(joined, materialization)
		}
	}
	if len(joined) != 1 {
		return executioncell.PreflightConfigMaterializationV1{}, executioncell.PreflightConfigBindingMaterializationV1{}, errors.New("protected runtime MCP v2 requires exactly one common config materialization")
	}
	var bindings []executioncell.PreflightConfigBindingMaterializationV1
	for _, binding := range joined[0].Bindings {
		if binding.TargetEnv == requirement.AuthorizationSource.TargetEnv &&
			binding.Source.Kind == requirement.AuthorizationSource.Kind &&
			binding.Source.Mode == requirement.AuthorizationSource.Mode &&
			binding.Mode == requirement.AuthorizationSource.Mode {
			bindings = append(bindings, binding)
		}
	}
	if len(bindings) != 1 {
		return executioncell.PreflightConfigMaterializationV1{}, executioncell.PreflightConfigBindingMaterializationV1{}, errors.New("protected runtime MCP v2 requires exactly one matching common file binding")
	}
	return joined[0], bindings[0], nil
}

func materializeProtectedRuntimeMCPConfigsV2(
	detail *SessionDetail,
	spec *SessionSpec,
	requirements []executioncell.ProtectedRuntimeMCPConfigRequirementV2,
	common []executioncell.PreflightConfigMaterializationV1,
	buildHelperCommand ProtectedRuntimeMCPHelperCommandBuilder,
) ([]executioncell.ProtectedRuntimeMCPConfigMaterializationV2, error) {
	if len(requirements) == 0 {
		return nil, nil
	}
	if detail == nil || spec == nil || buildHelperCommand == nil ||
		!sort.SliceIsSorted(requirements, func(i, j int) bool { return requirements[i].RequirementID < requirements[j].RequirementID }) {
		return nil, errors.New("protected runtime MCP v2 config requirements or trusted helper builder are unavailable")
	}
	operationalDigest, err := executioncell.DigestOperationalPayload(detail.OperationalPayload)
	if err != nil {
		return nil, fmt.Errorf("digest protected runtime MCP v2 operational source: %w", err)
	}
	bearer := strings.TrimSpace(detail.McpAuthToken)
	if detail.SessionID == "" || detail.PlatformURL == "" || bearer == "" || bearer != detail.McpAuthToken {
		return nil, errors.New("protected runtime MCP v2 gateway configuration is unavailable")
	}
	endpointDigest := digestConfigValue(strings.TrimRight(detail.PlatformURL, "/") + "/api/mcp/" + detail.SessionID)
	headers := []executioncell.ProtectedRuntimeMCPHeaderV1{{Name: "Authorization", ValueDigest: digestConfigValue("Bearer " + bearer)}}
	serverName := statehome.Brand() + "-platform"
	materializations := make([]executioncell.ProtectedRuntimeMCPConfigMaterializationV2, 0, len(requirements))
	for i, requirement := range requirements {
		if i > 0 && requirements[i-1].RequirementID == requirement.RequirementID {
			return nil, errors.New("duplicate protected runtime MCP v2 config requirement")
		}
		if err := executioncell.ValidateProtectedRuntimeMCPConfigRequirementV2(requirement); err != nil {
			return nil, err
		}
		if requirement.OperationalPayloadDigest != operationalDigest || requirement.ServerName != serverName ||
			requirement.Transport != executioncell.ProtectedRuntimeMCPTransportHTTP || requirement.EndpointDigest != endpointDigest ||
			!reflect.DeepEqual(requirement.Headers, headers) {
			return nil, errors.New("protected runtime MCP v2 config requirement differs from current runtime authority")
		}
		joined, binding, err := joinedProtectedRuntimeMCPCommonBinding(requirement, common)
		if err != nil {
			return nil, err
		}
		if joined.OperationalPayloadDigest != operationalDigest || binding.BearerContentDigest != digestConfigValue(bearer) {
			return nil, errors.New("protected runtime MCP v2 common materialization differs from initial authority")
		}
		path := spec.Env[requirement.AuthorizationSource.TargetEnv]
		if strings.TrimSpace(path) == "" || binding.FileReferenceDigest != digestConfigValue(path) {
			return nil, errors.New("protected runtime MCP v2 file reference differs from applied session config")
		}
		command, err := buildHelperCommand(path)
		if err != nil {
			return nil, fmt.Errorf("build protected runtime MCP v2 helper command: %w", err)
		}
		if strings.TrimSpace(command) == "" || !utf8.ValidString(command) {
			return nil, errors.New("protected runtime MCP v2 helper command is invalid")
		}
		materialization := executioncell.ProtectedRuntimeMCPConfigMaterializationV2{
			ContractVersion: requirement.ContractVersion, RequirementID: requirement.RequirementID,
			AuthorityBindingDigest: requirement.AuthorityBindingDigest, OperationalPayloadDigest: requirement.OperationalPayloadDigest,
			ServerName: requirement.ServerName, Transport: requirement.Transport, EndpointDigest: requirement.EndpointDigest,
			Headers: append([]executioncell.ProtectedRuntimeMCPHeaderV1(nil), requirement.Headers...),
			AuthorizationSource: executioncell.ProtectedRuntimeMCPAuthorizationSourceMaterializationV2{
				Kind: requirement.AuthorizationSource.Kind, ConfigRequirementID: requirement.AuthorizationSource.ConfigRequirementID,
				TargetEnv: requirement.AuthorizationSource.TargetEnv, Mode: requirement.AuthorizationSource.Mode,
				ConfigReferenceDigest: joined.ConfigReferenceDigest, FileReferenceDigest: binding.FileReferenceDigest,
				HelperCommandDigest: digestConfigValue(command),
			},
		}
		materialization.ConfigReferenceDigest, err = executioncell.DigestProtectedRuntimeMCPConfigReferenceV2(materialization)
		if err != nil {
			return nil, err
		}
		if err := executioncell.ValidateProtectedRuntimeMCPConfigMaterializationV2(materialization); err != nil {
			return nil, err
		}
		materializations = append(materializations, materialization)
	}
	return materializations, nil
}

func materializeExecutionPreflightConfig(spec *SessionSpec, detail *SessionDetail, requirements []executioncell.PreflightConfigRequirementV1, dir string) ([]executioncell.PreflightConfigMaterializationV1, preflightConfigLease, error) {
	lease := preflightConfigLease{sessionID: detail.SessionID}
	fail := func(err error) ([]executioncell.PreflightConfigMaterializationV1, preflightConfigLease, error) {
		lease.cleanup()
		return nil, preflightConfigLease{}, err
	}
	if len(requirements) == 0 {
		return nil, lease, nil
	}
	if !sort.SliceIsSorted(requirements, func(i, j int) bool { return requirements[i].RequirementID < requirements[j].RequirementID }) {
		return fail(errors.New("preflight config requirements must be sorted"))
	}
	var source operationalPreflightConfigSource
	if err := json.Unmarshal(detail.OperationalPayload, &source); err != nil {
		return fail(fmt.Errorf("decode preflight config operational source: %w", err))
	}
	operationalDigest, err := executioncell.DigestOperationalPayload(detail.OperationalPayload)
	if err != nil {
		return fail(fmt.Errorf("digest preflight config operational source: %w", err))
	}
	if source.MCPAuthToken != detail.McpAuthToken {
		return fail(errors.New("preflight config session bearer differs from operational source"))
	}
	seenTargets := make(map[string]struct{})
	materializations := make([]executioncell.PreflightConfigMaterializationV1, 0, len(requirements))
	for i, requirement := range requirements {
		if i > 0 && requirements[i-1].RequirementID == requirement.RequirementID {
			return fail(errors.New("duplicate preflight config requirement"))
		}
		if err := executioncell.ValidatePreflightConfigRequirement(requirement); err != nil {
			return fail(err)
		}
		if requirement.OperationalPayloadDigest != operationalDigest {
			return fail(errors.New("preflight config requirement operational payload mismatch"))
		}
		bindings := make([]executioncell.PreflightConfigBindingMaterializationV1, 0, len(requirement.Bindings))
		for _, binding := range requirement.Bindings {
			if _, exists := seenTargets[binding.TargetEnv]; exists {
				return fail(fmt.Errorf("duplicate preflight config target %s", binding.TargetEnv))
			}
			seenTargets[binding.TargetEnv] = struct{}{}
			applied := executioncell.PreflightConfigBindingMaterializationV1{TargetEnv: binding.TargetEnv, Source: binding.Source}
			switch binding.Source.Kind {
			case executioncell.PreflightConfigSourceOperationalEnvironment:
				value, ok := source.Env[binding.Source.EnvironmentName]
				if !ok || strings.TrimSpace(value) == "" {
					return fail(fmt.Errorf("preflight config operational environment %s is unavailable", binding.Source.EnvironmentName))
				}
				if err := applySpecEnvironment(spec, binding.TargetEnv, value); err != nil {
					return fail(err)
				}
				applied.ValueDigest = digestConfigValue(value)
			case executioncell.PreflightConfigSourceDaemonSessionID:
				if err := applySpecEnvironment(spec, binding.TargetEnv, detail.SessionID); err != nil {
					return fail(err)
				}
				applied.ValueDigest = digestConfigValue(detail.SessionID)
			case executioncell.PreflightConfigSourceSessionMCPBearerFile:
				bearer := strings.TrimSpace(detail.McpAuthToken)
				if bearer == "" {
					return fail(errors.New("preflight config session MCP bearer is unavailable"))
				}
				var owned preflightOwnedFile
				existing := strings.TrimSpace(spec.Env[binding.TargetEnv])
				if existing == "" {
					existing = strings.TrimSpace(os.Getenv(binding.TargetEnv))
				}
				if existing != "" {
					info, err := verifyConfigFile(existing, bearer)
					if err != nil {
						return fail(fmt.Errorf("verify existing preflight config bearer file: %w", err))
					}
					owned = preflightOwnedFile{path: existing, info: info}
				} else {
					var err error
					owned, err = materializeOwnedBearerFile(dir, detail.SessionID, requirement.RequirementID, binding.TargetEnv, bearer)
					if err != nil {
						return fail(fmt.Errorf("materialize preflight config bearer file: %w", err))
					}
				}
				lease.files = append(lease.files, owned)
				if err := applySpecEnvironment(spec, binding.TargetEnv, owned.path); err != nil {
					return fail(err)
				}
				applied.BearerContentDigest = digestConfigValue(bearer)
				applied.FileReferenceDigest = digestConfigValue(owned.path)
				applied.Mode = executioncell.PreflightConfigPrivateFileMode
			default:
				return fail(fmt.Errorf("unsupported preflight config source %s", binding.Source.Kind))
			}
			bindings = append(bindings, applied)
		}
		materialization := executioncell.PreflightConfigMaterializationV1{
			ContractVersion:          executioncell.PreflightConfigMaterializationContractVersion,
			RequirementID:            requirement.RequirementID,
			AuthorityBindingDigest:   requirement.AuthorityBindingDigest,
			OperationalPayloadDigest: requirement.OperationalPayloadDigest,
			Bindings:                 bindings,
		}
		digest, err := executioncell.DigestPreflightConfigReference(materialization)
		if err != nil {
			return fail(err)
		}
		materialization.ConfigReferenceDigest = digest
		if err := executioncell.ValidatePreflightConfigMaterialization(materialization); err != nil {
			return fail(err)
		}
		materializations = append(materializations, materialization)
	}
	return materializations, lease, nil
}
