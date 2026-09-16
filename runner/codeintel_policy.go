package runner

import (
	"fmt"
	"reflect"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/codeintelcontract"
)

func translateSpecForCodeIntelDelivery(qw QueuedWork, caps agent.Capabilities, in SpecInputs, selection codeIntelDeliverySelection, additionalDisallowed []string) (agent.Spec, error) {
	in.CodeIntelDeliveryRoute = selection.Route
	if selection.Route != codeIntelDeliveryNative || !in.Autonomous || qw.isInteractive() {
		spec := translateSpec(qw, caps, in)
		appendPostTranslationDisallows(&spec, caps, additionalDisallowed, qw.isInterview())
		return spec, nil
	}

	selected, err := selectedNativePolicyTools(selection.Tools)
	if err != nil {
		return agent.Spec{}, fmt.Errorf("runner: project native code-intelligence policy: %w", err)
	}
	policy := legacySpecToolPolicy(qw)
	policy.DisallowedTools = append(policy.DisallowedTools, additionalDisallowed...)
	if qw.isInterview() {
		policy.DisallowedTools = append(policy.DisallowedTools, "AskUserQuestion", "Write", "Edit", "Task", "Bash")
	}
	if len(qw.AllowedTools) == 0 {
		policy.AllowedTools = append(policy.AllowedTools, selected...)
	}
	policy.AllowedTools, err = normalizeNativePolicyList(policy.AllowedTools)
	if err != nil {
		return agent.Spec{}, fmt.Errorf("runner: project native code-intelligence allowed tools: %w", err)
	}
	policy.DisallowedTools, err = normalizeNativePolicyList(policy.DisallowedTools)
	if err != nil {
		return agent.Spec{}, fmt.Errorf("runner: project native code-intelligence disallowed tools: %w", err)
	}
	return translateSpecWithToolPolicy(qw, caps, in, policy), nil
}

func selectedNativePolicyTools(tools []string) ([]string, error) {
	if len(tools) == 0 {
		return nil, fmt.Errorf("selected native tool set is empty")
	}
	selected := make(map[string]bool, len(tools))
	previous := ""
	for _, tool := range tools {
		match, err := codeintelcontract.NormalizePolicyIdentity(tool)
		if err != nil || !match.Related || match.Canonical != tool || selected[tool] || tool <= previous {
			return nil, fmt.Errorf("selected native tool set is invalid")
		}
		selected[tool] = true
		previous = tool
	}
	out := make([]string, 0, len(selected))
	for _, name := range codeintelcontract.Names() {
		if selected[name] {
			out = append(out, name)
		}
	}
	if len(out) != len(selected) {
		return nil, fmt.Errorf("selected native tool set is invalid")
	}
	return out, nil
}

func normalizeNativePolicyList(values []string) ([]string, error) {
	if values == nil {
		return nil, nil
	}
	out := make([]string, len(values))
	for i, value := range values {
		if value == "*" {
			out[i] = value
			continue
		}
		match, err := codeintelcontract.NormalizePolicyIdentity(value)
		if err != nil {
			return nil, err
		}
		if match.Related {
			out[i] = match.Canonical
		} else {
			out[i] = value
		}
	}
	return out, nil
}

func appendPostTranslationDisallows(spec *agent.Spec, caps agent.Capabilities, additional []string, interview bool) {
	if len(additional) > 0 {
		spec.DisallowedTools = append(spec.DisallowedTools, additional...)
		if caps.NeedsPermissionConfig && !caps.AcceptsAllowedToolsList {
			if spec.PermissionConfig == nil {
				spec.PermissionConfig = &agent.PermissionConfig{}
			}
			spec.PermissionConfig.DisallowPatterns = append(spec.PermissionConfig.DisallowPatterns, additional...)
		}
	}
	if interview {
		spec.DisallowedTools = append(spec.DisallowedTools, "AskUserQuestion", "Write", "Edit", "Task", "Bash")
	}
}

type nativeCodeIntelPolicyAuthority struct {
	AllowedTools     []string
	DisallowedTools  []string
	PermissionConfig *agent.PermissionConfig
}

type nativeCodeIntelPolicyMismatchFields uint8

const (
	nativePolicyAllowedToolsMismatch nativeCodeIntelPolicyMismatchFields = 1 << iota
	nativePolicyDisallowedToolsMismatch
	nativePolicyPermissionConfigMismatch
)

type nativeCodeIntelPolicyAuthorityMismatchError struct {
	fields nativeCodeIntelPolicyMismatchFields
}

func (e *nativeCodeIntelPolicyAuthorityMismatchError) Error() string {
	return "runner: native code-intelligence policy authority mismatch"
}

func snapshotNativeCodeIntelPolicy(spec agent.Spec) nativeCodeIntelPolicyAuthority {
	return nativeCodeIntelPolicyAuthority{
		AllowedTools:     cloneStringSliceShape(spec.AllowedTools),
		DisallowedTools:  cloneStringSliceShape(spec.DisallowedTools),
		PermissionConfig: clonePermissionConfigShape(spec.PermissionConfig),
	}
}

func requirePreparedNativeCodeIntelPolicy(actual, prepared agent.Spec) error {
	a, p := snapshotNativeCodeIntelPolicy(actual), snapshotNativeCodeIntelPolicy(prepared)
	var fields nativeCodeIntelPolicyMismatchFields
	if !reflect.DeepEqual(a.AllowedTools, p.AllowedTools) {
		fields |= nativePolicyAllowedToolsMismatch
	}
	if !reflect.DeepEqual(a.DisallowedTools, p.DisallowedTools) {
		fields |= nativePolicyDisallowedToolsMismatch
	}
	if !reflect.DeepEqual(a.PermissionConfig, p.PermissionConfig) {
		fields |= nativePolicyPermissionConfigMismatch
	}
	if fields != 0 {
		return &nativeCodeIntelPolicyAuthorityMismatchError{fields: fields}
	}
	return nil
}

func cloneStringSliceShape(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func clonePermissionConfigShape(in *agent.PermissionConfig) *agent.PermissionConfig {
	if in == nil {
		return nil
	}
	out := *in
	out.AllowPatterns = cloneStringSliceShape(in.AllowPatterns)
	out.DisallowPatterns = cloneStringSliceShape(in.DisallowPatterns)
	return &out
}
