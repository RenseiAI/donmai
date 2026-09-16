// Package codeintelbridge owns the inert binder/runtime contract shared by
// runner preparation and the later Pi runtime bridge.
package codeintelbridge

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// Shared bridge contract identities.
const (
	ParameterContractID      = "donmai.code-intelligence-work/v1"
	RuntimeConfigVersion     = "donmai.code-intelligence-runtime-config/v1"
	DeliveryID               = "donmai-code-intelligence-native-v1"
	ExtensionContractVersion = "donmai.code-intelligence-extension/v1"
	BindResponseVersion      = "donmai.code-intelligence-bind-response/v1"
	ExtensionMarker          = "donmai-code-intelligence-v1"
	KindBind                 = "bind"
	KindInventory            = "inventory"
	KindCall                 = "call"
)

// RuntimeConfigV1 is the closed process-only code-intelligence configuration.
type RuntimeConfigV1 struct {
	ContractVersion string   `json:"contractVersion"`
	RepoPath        string   `json:"repoPath"`
	Tools           []string `json:"tools"`
}

// CanonicalRuntimeConfig validates, canonicalizes, and digests config.
func CanonicalRuntimeConfig(config RuntimeConfigV1) (json.RawMessage, string, error) {
	if config.ContractVersion != RuntimeConfigVersion || len(config.Tools) == 0 {
		return nil, "", fmt.Errorf("code-intelligence runtime config is malformed")
	}
	previous := ""
	for _, tool := range config.Tools {
		if tool == "" || tool <= previous || strings.TrimSpace(tool) != tool {
			return nil, "", fmt.Errorf("code-intelligence runtime tool set is not canonical")
		}
		previous = tool
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, "", err
	}
	return agent.CanonicalCapabilityRuntimeConfig(raw)
}

// DecodeRuntimeConfig decodes only canonical closed runtime config bytes.
func DecodeRuntimeConfig(raw json.RawMessage) (RuntimeConfigV1, string, error) {
	var config RuntimeConfigV1
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return RuntimeConfigV1{}, "", fmt.Errorf("decode code-intelligence runtime config: %w", err)
	}
	canonical, digest, err := CanonicalRuntimeConfig(config)
	if err != nil || string(canonical) != string(raw) {
		return RuntimeConfigV1{}, "", fmt.Errorf("code-intelligence runtime config is not canonical")
	}
	return config, digest, nil
}
