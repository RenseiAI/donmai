package codex

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/gateway"
)

// interactiveCodexEndpoint is derived only from the already selected binding.
// Its key value never enters Codex configuration or process arguments.
type interactiveCodexEndpoint struct {
	nativeHostLogin bool
	baseURL         string
	envKey          string
	envValue        string
}

func prepareInteractiveCodexEndpoint(spec agent.Spec) (*interactiveCodexEndpoint, error) {
	if spec.Endpoint == nil || spec.Endpoint.IsZero() {
		return nil, nil
	}
	binding := spec.Endpoint
	if binding.Protocol != agent.ProtoOpenAIResponses {
		return nil, agent.ErrEndpointProtocolUnsupported
	}
	if binding.Company == "" || binding.Host == "" || strings.TrimSpace(binding.Model) == "" ||
		(spec.Model != "" && spec.Model != binding.Model) {
		return nil, agent.ErrEndpointProtocolMismatch
	}
	if binding.Host == agent.HostOAuthCLI {
		if binding.Company != agent.CompanyOpenAI || binding.Mechanism != agent.AuthCLISession ||
			binding.Auth != agent.AuthHostSession || binding.BaseURL != "" || len(binding.Env) != 0 {
			return nil, agent.ErrEndpointProtocolMismatch
		}
		return &interactiveCodexEndpoint{nativeHostLogin: true}, nil
	}
	if binding.BaseURL == "" || agent.ValidateEndpointBindingBaseURL(binding.BaseURL) != nil {
		return nil, agent.ErrEndpointProtocolMismatch
	}
	parsed, err := url.Parse(binding.BaseURL)
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		// The private config carries a URL, never a credential-bearing query.
		return nil, agent.ErrEndpointProtocolMismatch
	}
	selection := &interactiveCodexEndpoint{baseURL: binding.BaseURL}
	switch binding.Host {
	case agent.HostGateway:
		if !isCodexLoopbackHost(parsed.Hostname()) || binding.Mechanism != agent.AuthAPIKey ||
			(binding.Auth != agent.AuthBYOK && binding.Auth != agent.AuthMetered) {
			return nil, agent.ErrEndpointProtocolMismatch
		}
		selection.envKey = gateway.TokenEnvVar
	case agent.HostDirect:
		if binding.Company != agent.CompanyOpenAI || binding.Mechanism != agent.AuthAPIKey ||
			(binding.Auth != agent.AuthBYOK && binding.Auth != agent.AuthMetered) {
			return nil, agent.ErrEndpointProtocolMismatch
		}
		selection.envKey = "OPENAI_API_KEY"
	case agent.HostLocal:
		if !isCodexLoopbackHost(parsed.Hostname()) || binding.Mechanism != agent.AuthNone ||
			binding.Auth != agent.AuthLocal || len(binding.Env) != 0 {
			return nil, agent.ErrEndpointProtocolMismatch
		}
	default:
		return nil, agent.ErrEndpointProtocolUnsupported
	}
	if selection.envKey != "" {
		for key := range binding.Env {
			if key != selection.envKey {
				return nil, agent.ErrEndpointProtocolMismatch
			}
		}
		selected := binding.Env[selection.envKey]
		fromSpec := spec.Env[selection.envKey]
		if selected != "" && fromSpec != "" && selected != fromSpec {
			return nil, agent.ErrEndpointProtocolMismatch
		}
		if selected == "" {
			selected = fromSpec
		}
		if strings.TrimSpace(selected) == "" {
			return nil, agent.ErrEndpointProtocolMismatch
		}
		selection.envValue = selected
	}
	return selection, nil
}

func isCodexLoopbackHost(hostname string) bool {
	return strings.EqualFold(hostname, "localhost") || net.ParseIP(hostname).IsLoopback()
}

// writeInteractiveEndpointConfig writes only selected nonsecret metadata in an
// isolated 0700 home. The child receives envValue separately through Spec.Env.
func writeInteractiveEndpointConfig(boundary *codexConfigBoundary, selected *interactiveCodexEndpoint) error {
	if selected == nil || selected.nativeHostLogin {
		return errors.New("codex endpoint config requires a custom selection")
	}
	if err := boundary.validate(); err != nil {
		return fmt.Errorf("validate isolated Codex endpoint config: %w", err)
	}
	root, err := os.OpenRoot(boundary.home)
	if err != nil {
		return fmt.Errorf("open isolated Codex endpoint home: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.OpenFile(codexConfigFileName, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return fmt.Errorf("open isolated Codex endpoint config: %w", err)
	}
	body := "model_provider = \"donmai_session\"\n" +
		"[model_providers.donmai_session]\n" +
		"name = \"Donmai session\"\n" +
		"base_url = " + tomlBasicString(selected.baseURL) + "\n" +
		"wire_api = \"responses\"\n" +
		"requires_openai_auth = false\n"
	if selected.envKey != "" {
		body += "env_key = " + tomlBasicString(selected.envKey) + "\n"
	}
	_, writeErr := file.WriteString(body)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("write isolated Codex endpoint config: %w", err)
	}
	if err := boundary.validate(); err != nil {
		return fmt.Errorf("validate written Codex endpoint config: %w", err)
	}
	return nil
}
