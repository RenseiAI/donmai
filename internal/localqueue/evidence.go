package localqueue

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/matrix"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// Producer evidence is a closed, canonical, non-secret account of the local
// configuration, installed runtime observation and published compatibility
// cell selected before admission. Hashes bind these bytes; they are not a
// signature or a replacement for the trusted producer's live checks.
type producerEvidence struct {
	Configuration json.RawMessage `json:"configuration"`
	Inventory     json.RawMessage `json:"inventory"`
	Compatibility json.RawMessage `json:"compatibility"`
}

type producerConfiguration struct {
	RuntimeTransportMode string                 `json:"RuntimeTransportMode"`
	ScopeID              string                 `json:"ScopeID"`
	WorkerID             string                 `json:"WorkerID"`
	PlacementID          string                 `json:"PlacementID"`
	Revision             string                 `json:"Revision"`
	EndpointID           string                 `json:"EndpointID"`
	AuthBindingID        string                 `json:"AuthBindingID"`
	Harness              string                 `json:"Harness"`
	Model                executioncell.ModelRef `json:"Model"`
	Catalog              json.RawMessage        `json:"Catalog"`
}

type operatorCatalog struct {
	Kind  string `json:"Kind"`
	Entry struct {
		Model    executioncell.ModelRef `json:"model"`
		Host     string                 `json:"host"`
		Revision string                 `json:"revision"`
	} `json:"Entry"`
}

type builtinCatalog struct {
	Kind            string          `json:"Kind"`
	Model           agent.ModelDesc `json:"Model"`
	ManifestVersion string          `json:"ManifestVersion"`
}

type producerInventory struct {
	Manifest             agent.HarnessManifest `json:"manifest"`
	BinarySHA256         string                `json:"binarySha256"`
	ConfigurationDigest  string                `json:"configurationDigest"`
	AuthReferencePresent bool                  `json:"authReferencePresent"`
}

type producerCompatibility struct {
	Cell     matrix.HarnessEndpointCell `json:"Cell"`
	Manifest agent.HarnessManifest      `json:"Manifest"`
}

func sameCanonical(left, right any) bool {
	a, leftErr := executioncell.CanonicalJSON(left)
	b, rightErr := executioncell.CanonicalJSON(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(a, b)
}

func validateProducerEvidence(raw []byte, envelope AdmissionEnvelope, binding ConfiguredHostBinding, cell executioncell.ResolvedExecutionCell, receipt executioncell.AdmissionReceipt) error {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return fmt.Errorf("%w: producer evidence is missing or oversized", ErrInvalidEnvelope)
	}
	canonical, err := executioncell.CanonicalJSON(json.RawMessage(raw))
	if err != nil || !bytes.Equal(canonical, raw) {
		return fmt.Errorf("%w: producer evidence must be canonical unique-key JSON", ErrInvalidEnvelope)
	}
	var outer producerEvidence
	if outer, err = decodeTransactionData[producerEvidence](raw); err != nil ||
		len(outer.Configuration) == 0 || len(outer.Inventory) == 0 || len(outer.Compatibility) == 0 {
		return fmt.Errorf("%w: producer evidence has an unknown or absent section", ErrInvalidEnvelope)
	}
	configuration, err := decodeTransactionData[producerConfiguration](outer.Configuration)
	if err == nil && configuration.RuntimeTransportMode != "local" && configuration.RuntimeTransportMode != "local/v2" {
		return fmt.Errorf("%w: unsupported local runtime transport evidence; explicit local mode required", ErrInvalidEnvelope)
	}
	if err != nil || strings.TrimSpace(configuration.Revision) == "" || len(configuration.Catalog) == 0 ||
		configuration.ScopeID != binding.ScopeID || configuration.ScopeID != envelope.ScopeID ||
		configuration.WorkerID != binding.WorkerID || configuration.PlacementID != binding.HostID ||
		configuration.EndpointID != cell.Endpoint.ID || configuration.AuthBindingID != cell.AuthBinding.ID ||
		configuration.Harness != cell.Harness.ID || configuration.Model != cell.Model {
		return fmt.Errorf("%w: producer configuration does not match admitted exact-host cell", ErrInvalidEnvelope)
	}
	if err = validateBaseBranchEvidence(envelope.OperationalPayload, configuration.RuntimeTransportMode); err != nil {
		return err
	}
	compatibility, err := decodeTransactionData[producerCompatibility](outer.Compatibility)
	if err != nil {
		return fmt.Errorf("%w: compatibility evidence has unknown fields", ErrInvalidEnvelope)
	}
	inventory, err := decodeTransactionData[producerInventory](outer.Inventory)
	if err != nil || !validSHA256(inventory.BinarySHA256) || !inventory.AuthReferencePresent {
		return fmt.Errorf("%w: runtime inventory is incomplete", ErrInvalidEnvelope)
	}
	configurationDigest, err := executioncell.DigestContractValue(json.RawMessage(outer.Configuration))
	if err != nil || inventory.ConfigurationDigest != configurationDigest || cell.Endpoint.Revision != configurationDigest {
		return fmt.Errorf("%w: producer configuration digest mismatch", ErrInvalidEnvelope)
	}
	inventoryDigest, err := executioncell.DigestContractValue(json.RawMessage(outer.Inventory))
	if err != nil || cell.RuntimeInventoryDigest != inventoryDigest {
		return fmt.Errorf("%w: runtime inventory digest mismatch", ErrInvalidEnvelope)
	}
	compatibilityDigest, err := executioncell.DigestContractValue(json.RawMessage(outer.Compatibility))
	if err != nil || cell.CompatibilityDigest != compatibilityDigest {
		return fmt.Errorf("%w: compatibility digest mismatch", ErrInvalidEnvelope)
	}
	if !sameCanonical(inventory.Manifest, compatibility.Manifest) ||
		string(inventory.Manifest.Name) != cell.Harness.ID || inventory.Manifest.ContractABI != cell.Harness.Version ||
		string(compatibility.Cell.Harness) != cell.Harness.ID ||
		string(compatibility.Cell.Endpoint) != cell.Endpoint.Operator ||
		string(compatibility.Cell.Endpoint) != cell.Model.Author ||
		string(compatibility.Cell.Protocol) != cell.Endpoint.Protocol ||
		string(compatibility.Cell.Host) != "oauth-cli" ||
		!slices.Contains(inventory.Manifest.Caps.Drives, compatibility.Cell.Protocol) ||
		!slices.Contains(inventory.Manifest.Caps.DrivesHosts, compatibility.Cell.Host) ||
		inventory.Manifest.Caps.Transport != compatibility.Cell.Transport ||
		!inventory.Manifest.Caps.SupportsOneShot ||
		!slices.Contains(compatibility.Cell.AuthModes, agent.AuthHostSession) ||
		!compatibility.Cell.BringsOwnAuth || compatibility.Cell.NeedsAPIKey || !compatibility.Cell.OneShot ||
		cell.AuthBinding.Mechanism != executioncell.AuthCLISession ||
		cell.AuthBinding.BindingScope != executioncell.ScopeHost ||
		cell.AuthBinding.Portability != executioncell.HostBound ||
		cell.AuthBinding.Delivery != executioncell.DeliveryHostCLIHomeReference {
		return fmt.Errorf("%w: published matrix cell/manifest does not match host-login selection", ErrInvalidEnvelope)
	}
	switch {
	case configuration.Harness == "codex", configuration.Harness == "claude-code":
	default:
		return fmt.Errorf("%w: local v1 harness is not admitted", ErrInvalidEnvelope)
	}
	var catalogKind struct {
		Kind string `json:"Kind"`
	}
	if err := json.Unmarshal(configuration.Catalog, &catalogKind); err != nil {
		return fmt.Errorf("%w: catalog kind is invalid", ErrInvalidEnvelope)
	}
	switch catalogKind.Kind {
	case "operator":
		catalog, err := decodeTransactionData[operatorCatalog](configuration.Catalog)
		if err != nil || catalog.Entry.Model != configuration.Model ||
			catalog.Entry.Host != string(compatibility.Cell.Host) || strings.TrimSpace(catalog.Entry.Revision) == "" {
			return fmt.Errorf("%w: operator model catalog disagrees with selection", ErrInvalidEnvelope)
		}
	case "builtin":
		catalog, err := decodeTransactionData[builtinCatalog](configuration.Catalog)
		if err != nil || catalog.Model.ID != configuration.Model.ID ||
			!slices.Contains(catalog.Model.Hosts, compatibility.Cell.Host) || strings.TrimSpace(catalog.ManifestVersion) == "" {
			return fmt.Errorf("%w: builtin model catalog disagrees with selection", ErrInvalidEnvelope)
		}
	default:
		return fmt.Errorf("%w: unknown model catalog kind", ErrInvalidEnvelope)
	}
	for _, decision := range receipt.ResolverDecisions {
		if decision.SourceRef != "local-config:"+configurationDigest {
			return fmt.Errorf("%w: resolver decision lacks bound local configuration provenance", ErrInvalidEnvelope)
		}
	}
	if err := executioncell.AssertSecretFreeReceipt(json.RawMessage(raw)); err != nil {
		return fmt.Errorf("%w: producer evidence carries secret material", ErrInvalidEnvelope)
	}
	return nil
}

func validateBaseBranchEvidence(payload []byte, mode string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return fmt.Errorf("%w: invalid operational source", ErrInvalidEnvelope)
	}
	raw, present := fields["baseRef"]
	if !present {
		return nil
	}
	var base string
	if mode != "local/v2" || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &base) != nil || worktree.ValidateBranchBaseRef(base) != nil {
		return fmt.Errorf("%w: unsupported or invalid base-branch transport evidence", ErrInvalidEnvelope)
	}
	if raw, present := fields["ref"]; present {
		var ref string
		if json.Unmarshal(raw, &ref) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || ref != "" {
			return fmt.Errorf("%w: base branch conflicts with amend-existing ref", ErrInvalidEnvelope)
		}
	}
	return nil
}
