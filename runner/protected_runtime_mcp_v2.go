package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/runtime/mcpheaders"
)

func digestProtectedRuntimeValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func canonicalProtectedRuntimeMCPHelperCommand(tokenFilePath string) (string, error) {
	executable, err := mcpheaders.ResolveExecutablePath()
	if err != nil {
		return "", err
	}
	return mcpheaders.BuildHelperCommand(executable, tokenFilePath)
}

// applyProtectedRuntimeMCPV2 joins the retained protected materialization to
// the actual common file binding and only then replaces the launch header with
// the process-private native helper.
func applyProtectedRuntimeMCPV2(
	qw QueuedWork,
	selection harnessSelection,
	realizations *agent.CapabilityRealizationRegistry,
	selector ProtectedRuntimeMCPV2Selector,
	servers []agent.MCPServerConfig,
	effectiveTokenFile string,
) ([]agent.MCPServerConfig, error) {
	for _, server := range servers {
		if _, present := agent.ProtectedRuntimeMCPHeadersHelper(server); present {
			return nil, errors.New("runner: protected runtime MCP helper was present before trusted transformation")
		}
	}
	host, err := executioncell.DecodeHostAdaptationReceipt(qw.HostAdaptationReceipt)
	if err != nil {
		return nil, err
	}
	requirement, err := resolveProtectedRuntimeMCPRequirementV2(qw, selection, realizations, selector, host)
	if err != nil {
		return nil, err
	}
	if requirement == nil {
		if host.ContractVersion == executioncell.HostAdaptationV3ContractVersion {
			return nil, errors.New("runner: unexpected protected runtime MCP v2 materialization for an unselected realization")
		}
		return append([]agent.MCPServerConfig(nil), servers...), nil
	}
	if host.ContractVersion != executioncell.HostAdaptationV3ContractVersion || len(host.ProtectedRuntimeMCPConfigsV2) != 1 || len(host.ProtectedRuntimeMCPConfigs) != 0 {
		return nil, errors.New("runner: selected protected runtime MCP v2 capability requires one v3 v2 materialization")
	}
	if strings.TrimSpace(effectiveTokenFile) == "" {
		return nil, errors.New("runner: selected protected runtime MCP v2 capability requires an effective bearer file")
	}
	materialization := host.ProtectedRuntimeMCPConfigsV2[0]
	if materialization.ContractVersion != requirement.ContractVersion || materialization.RequirementID != requirement.RequirementID ||
		materialization.AuthorityBindingDigest != requirement.AuthorityBindingDigest || materialization.OperationalPayloadDigest != requirement.OperationalPayloadDigest ||
		materialization.ServerName != requirement.ServerName || materialization.Transport != requirement.Transport ||
		materialization.EndpointDigest != requirement.EndpointDigest || !reflect.DeepEqual(materialization.Headers, requirement.Headers) {
		return nil, errors.New("runner: protected runtime MCP v2 materialization differs from current runtime authority")
	}
	source := materialization.AuthorizationSource
	if source.Kind != requirement.AuthorizationSource.Kind || source.ConfigRequirementID != requirement.AuthorizationSource.ConfigRequirementID ||
		source.TargetEnv != requirement.AuthorizationSource.TargetEnv || source.Mode != requirement.AuthorizationSource.Mode {
		return nil, errors.New("runner: protected runtime MCP v2 authorization source differs from current runtime authority")
	}
	var common []executioncell.PreflightConfigMaterializationV1
	for _, candidate := range host.ConfigMaterializations {
		if candidate.RequirementID == source.ConfigRequirementID {
			common = append(common, candidate)
		}
	}
	if len(common) != 1 || common[0].ConfigReferenceDigest != source.ConfigReferenceDigest || common[0].OperationalPayloadDigest != requirement.OperationalPayloadDigest {
		return nil, errors.New("runner: protected runtime MCP v2 requires one exact common materialization")
	}
	var bindings []executioncell.PreflightConfigBindingMaterializationV1
	for _, binding := range common[0].Bindings {
		if binding.TargetEnv == source.TargetEnv && binding.Source.Kind == source.Kind && binding.Source.Mode == source.Mode && binding.Mode == source.Mode {
			bindings = append(bindings, binding)
		}
	}
	if len(bindings) != 1 {
		return nil, errors.New("runner: protected runtime MCP v2 requires one exact common file binding")
	}
	binding := bindings[0]
	if binding.BearerContentDigest != digestProtectedRuntimeValue(qw.McpAuthToken) ||
		binding.FileReferenceDigest != source.FileReferenceDigest || source.FileReferenceDigest != digestProtectedRuntimeValue(effectiveTokenFile) {
		return nil, errors.New("runner: protected runtime MCP v2 file evidence differs from launch authority")
	}
	command, err := canonicalProtectedRuntimeMCPHelperCommand(effectiveTokenFile)
	if err != nil {
		return nil, fmt.Errorf("runner: build protected runtime MCP v2 helper command: %w", err)
	}
	if source.HelperCommandDigest != digestProtectedRuntimeValue(command) {
		return nil, errors.New("runner: protected runtime MCP v2 helper command differs from admitted evidence")
	}

	out := append([]agent.MCPServerConfig(nil), servers...)
	launchAuthorizationDigest := ""
	for _, header := range requirement.Headers {
		if header.Name == "Authorization" {
			launchAuthorizationDigest = header.ValueDigest
		}
	}
	if launchAuthorizationDigest == "" {
		return nil, errors.New("runner: protected runtime MCP v2 Authorization evidence is missing")
	}
	matches := 0
	for i := range out {
		if out[i].Name != materialization.ServerName {
			continue
		}
		matches++
		if out[i].Type != materialization.Transport || digestProtectedRuntimeValue(out[i].URL) != materialization.EndpointDigest {
			return nil, errors.New("runner: protected runtime MCP v2 server differs from admitted endpoint")
		}
		headers := make(map[string]string, len(out[i].Headers))
		removedAuthorization := false
		for name, value := range out[i].Headers {
			if strings.EqualFold(name, "Authorization") {
				if name != "Authorization" || removedAuthorization || digestProtectedRuntimeValue(value) != launchAuthorizationDigest {
					return nil, errors.New("runner: protected runtime MCP v2 Authorization projection differs from launch authority")
				}
				removedAuthorization = true
				continue
			}
			headers[name] = value
		}
		if !removedAuthorization {
			return nil, errors.New("runner: protected runtime MCP v2 launch Authorization is missing")
		}
		out[i].Headers = headers
		out[i], err = agent.WithProtectedRuntimeMCPHeadersHelper(out[i], command)
		if err != nil {
			return nil, err
		}
	}
	if matches != 1 {
		return nil, errors.New("runner: protected runtime MCP v2 server is missing or duplicated")
	}
	return out, nil
}
