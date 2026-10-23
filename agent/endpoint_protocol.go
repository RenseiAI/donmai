package agent

import (
	"context"
	"errors"
	"fmt"
)

// ProtocolEndpointResolver is an optional capability. Existing endpoint
// providers continue to implement ModelEndpointProvider unchanged.
type ProtocolEndpointResolver interface {
	ProtocolManifest() ModelEndpointManifest
	ResolveProtocol(context.Context, EndpointRequest, WireProtocol) (EndpointBinding, error)
}

// Protocol resolution sentinels describe declaration or binding refusals.
var (
	ErrEndpointProtocolRequired      = errors.New("endpoint protocol required")
	ErrEndpointProtocolUnsupported   = errors.New("endpoint protocol unsupported")
	ErrEndpointHostProtocolAmbiguous = errors.New("endpoint host/protocol ambiguous")
	ErrEndpointProtocolMismatch      = errors.New("endpoint protocol binding mismatch")
)

// ResolveEndpointProtocol resolves one declared host/protocol tuple. It never
// consults process environment or puts credential values in an error.
func ResolveEndpointProtocol(ctx context.Context, provider ModelEndpointProvider, req EndpointRequest, protocol WireProtocol) (EndpointBinding, error) {
	if protocol == "" {
		return EndpointBinding{}, ErrEndpointProtocolRequired
	}
	if provider == nil {
		return EndpointBinding{}, fmt.Errorf("endpoint provider missing: %w", ErrEndpointProtocolUnsupported)
	}
	aware, hasProtocolCapability := provider.(ProtocolEndpointResolver)
	legacyManifest := provider.Manifest()
	manifest := legacyManifest
	if hasProtocolCapability {
		manifest = aware.ProtocolManifest()
	}
	if manifest.Company == "" || manifest.Family != FamilyModelEndpoint ||
		legacyManifest.Company != manifest.Company || legacyManifest.Family != manifest.Family ||
		legacyManifest.ContractABI != "model-endpoint/v1" {
		return EndpointBinding{}, fmt.Errorf("endpoint declaration invalid: %w", ErrEndpointProtocolMismatch)
	}
	if hasProtocolCapability && manifest.ContractABI != "model-endpoint/v2" {
		return EndpointBinding{}, fmt.Errorf("endpoint protocol declaration ABI invalid: %w", ErrEndpointProtocolMismatch)
	}
	if !hasProtocolCapability && manifest.ContractABI != "model-endpoint/v1" {
		return EndpointBinding{}, fmt.Errorf("endpoint declaration ABI invalid: %w", ErrEndpointProtocolMismatch)
	}
	declared := false
	for _, candidate := range manifest.Speaks {
		if candidate == protocol {
			declared = true
			break
		}
	}
	if !declared {
		return EndpointBinding{}, ErrEndpointProtocolUnsupported
	}
	seen := make(map[[2]string]bool, len(manifest.Hosts))
	var selected *HostDesc
	hostRows := 0
	for i := range manifest.Hosts {
		row := &manifest.Hosts[i]
		key := [2]string{string(row.Host), string(row.Protocol)}
		if row.Host == "" || row.Protocol == "" || seen[key] {
			return EndpointBinding{}, ErrEndpointHostProtocolAmbiguous
		}
		seen[key] = true
		if row.Host != req.Host {
			continue
		}
		hostRows++
		if row.Protocol == protocol {
			if selected != nil {
				return EndpointBinding{}, ErrEndpointHostProtocolAmbiguous
			}
			selected = row
		}
	}
	if selected == nil {
		return EndpointBinding{}, ErrEndpointProtocolUnsupported
	}
	if !hasProtocolCapability && hostRows != 1 {
		return EndpointBinding{}, ErrEndpointHostProtocolAmbiguous
	}
	if !containsEndpointAuth(selected.AuthModes, req.Auth) {
		return EndpointBinding{}, ErrEndpointProtocolUnsupported
	}
	var (
		binding EndpointBinding
		err     error
	)
	if hasProtocolCapability {
		binding, err = aware.ResolveProtocol(ctx, req, protocol)
	} else {
		binding, err = provider.Resolve(ctx, req)
	}
	if err != nil {
		// A third-party resolver error may contain a credential value. Preserve
		// the public sentinels, but never return arbitrary error text.
		for _, known := range []error{ErrEndpointProtocolRequired, ErrEndpointProtocolUnsupported, ErrEndpointHostProtocolAmbiguous, ErrEndpointProtocolMismatch} {
			if errors.Is(err, known) {
				return EndpointBinding{}, known
			}
		}
		return EndpointBinding{}, ErrEndpointProtocolMismatch
	}
	if binding.Company != manifest.Company || binding.Host != selected.Host || binding.Protocol != protocol ||
		binding.Model != req.Model || binding.Mechanism != req.Mechanism || binding.Auth != req.Auth ||
		binding.Region != req.Region || binding.CostModel != selected.CostModel ||
		binding.BringsOwnAuth != selected.BringsOwnAuth || !bindingEndpointEnvMatches(binding.Env, selected.EnvKeys, req.EnvProvided) {
		return EndpointBinding{}, ErrEndpointProtocolMismatch
	}
	if ValidateEndpointBindingBaseURL(binding.BaseURL) != nil {
		return EndpointBinding{}, ErrEndpointProtocolMismatch
	}
	return binding, nil
}

func containsEndpointAuth(modes []AuthMode, selected AuthMode) bool {
	for _, mode := range modes {
		if mode == selected {
			return true
		}
	}
	return false
}

func bindingEndpointEnvMatches(got map[string]string, declared []string, provided map[string]string) bool {
	expected := make(map[string]string, len(declared))
	for _, key := range declared {
		if value, ok := provided[key]; ok {
			expected[key] = value
		}
	}
	if len(got) != len(expected) {
		return false
	}
	for key, value := range expected {
		actual, ok := got[key]
		if !ok || actual != value {
			return false
		}
	}
	return true
}
