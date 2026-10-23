package matrix

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func protocolFixture(t *testing.T) ([]HarnessRow, map[agent.HarnessName]HarnessRow, []ProtocolEndpointHarvest, []protocolCellDeclaration) {
	t.Helper()
	harnesses, byHarness, err := buildHarnesses()
	if err != nil {
		t.Fatal(err)
	}
	harvests := ProtocolEndpointHarvestList()
	// The external declaration fixture uses one legacy Chat row and one
	// Responses variant. The subject is the production qualified builder.
	for i := range harvests {
		if harvests[i].Legacy.Company != agent.CompanyOpenAI {
			continue
		}
		legacy := harvests[i].Legacy.Manifest()
		full := legacy
		full.ContractABI = "model-endpoint/v2"
		full.Hosts = append([]agent.HostDesc(nil), legacy.Hosts...)
		for _, host := range legacy.Hosts {
			if host.Host == agent.HostDirect {
				host.Protocol = agent.ProtoOpenAIResponses
				full.Hosts = append(full.Hosts, host)
			}
		}
		harvests[i].ProtocolManifest = func() agent.ModelEndpointManifest { return full }
	}
	declarations := append([]protocolCellDeclaration(nil), cellDeclarations...)
	for _, declaration := range cellDeclarations {
		cell := declaration.Cell
		if cell.Harness == agent.HarnessCodex && cell.Endpoint == agent.CompanyOpenAI && cell.Host == agent.HostDirect {
			cell.Protocol, cell.Stability, cell.Smoked = agent.ProtoOpenAIResponses, "experimental", false
			cell.LegacyProviderID = nil
			declarations = append(declarations, protocolCellDeclaration{Cell: cell})
			return harnesses, byHarness, harvests, declarations
		}
	}
	t.Fatal("legacy Codex direct fixture declaration unavailable")
	return nil, nil, nil, nil
}

func TestProtocolViewSeparatesVariantsAndPreservesLegacyDefault(t *testing.T) {
	harnesses, byHarness, harvests, declarations := protocolFixture(t)
	view, err := buildProtocolView(harnesses, byHarness, harvests, declarations)
	if err != nil {
		t.Fatal(err)
	}
	key := CellKey{Harness: agent.HarnessCodex, Endpoint: agent.CompanyOpenAI, Host: agent.HostDirect}
	var protocols []agent.WireProtocol
	for _, cell := range view.Cells {
		if cell.Key() == key {
			protocols = append(protocols, cell.QualifiedKey().Protocol)
		}
	}
	if !reflect.DeepEqual(protocols, []agent.WireProtocol{agent.ProtoOpenAIChat, agent.ProtoOpenAIResponses}) {
		t.Fatalf("qualified variants lost or unsorted: %v", protocols)
	}
	var defaults []agent.WireProtocol
	for _, selected := range view.LegacyDefaults {
		if selected.Cell == key {
			defaults = append(defaults, selected.Protocol)
		}
	}
	if !reflect.DeepEqual(defaults, []agent.WireProtocol{agent.ProtoOpenAIChat}) {
		t.Fatalf("legacy default broadened: %v", defaults)
	}
	for _, alias := range view.LegacyAliases {
		if alias.Cell == key {
			t.Fatal("unproven variant acquired a legacy alias")
		}
	}
	_, legacy, full, err := buildProtocolEndpoints(harvests, retiredProtocolTuples(declarations))
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range legacy[agent.CompanyOpenAI].Hosts {
		if host.Host == agent.HostDirect && host.Protocol != agent.ProtoOpenAIChat {
			t.Fatal("legacy endpoint selected Responses")
		}
	}
	count := 0
	for _, host := range full[agent.CompanyOpenAI].Hosts {
		if host.Host == agent.HostDirect {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("full endpoint view has %d direct tuples", count)
	}
}

func TestProtocolViewRejectsInvalidDeclarations(t *testing.T) {
	for _, name := range []string{"duplicate qualified cell", "duplicate default", "wrong default reference", "empty protocol", "premature smoked", "premature stable", "new alias", "missing tuple"} {
		t.Run(name, func(t *testing.T) {
			harnesses, byHarness, harvests, declarations := protocolFixture(t)
			last := len(declarations) - 1
			switch name {
			case "duplicate qualified cell":
				declarations = append(declarations, declarations[last])
			case "duplicate default":
				key := declarations[last].Cell.QualifiedKey()
				declarations[last].LegacyDefault = &key
			case "wrong default reference":
				key := declarations[last].Cell.QualifiedKey()
				declarations[0].LegacyDefault = &key
			case "empty protocol":
				declarations[last].Cell.Protocol = ""
			case "premature smoked":
				declarations[last].Cell.Smoked = true
			case "premature stable":
				declarations[last].Cell.Stability = "stable"
			case "new alias":
				declarations[last].Cell.LegacyProviderID = pn(agent.ProviderCodex)
			case "missing tuple":
				declarations[last].Cell.Protocol = agent.ProtoGeminiGenerate
			}
			if _, err := buildProtocolView(harnesses, byHarness, harvests, declarations); err == nil {
				t.Fatal("invalid declaration was accepted")
			}
		})
	}
}

func TestProtocolViewRejectsDuplicateOrChangedHostTuples(t *testing.T) {
	for _, name := range []string{"duplicate equal tuple", "changed legacy default", "duplicate legacy host"} {
		t.Run(name, func(t *testing.T) {
			harnesses, byHarness, harvests, declarations := protocolFixture(t)
			for i := range harvests {
				if harvests[i].Legacy.Company != agent.CompanyOpenAI {
					continue
				}
				full := harvests[i].ProtocolManifest()
				legacy := harvests[i].Legacy.Manifest()
				switch name {
				case "duplicate equal tuple":
					full.Hosts = append(full.Hosts, full.Hosts[0])
				case "changed legacy default":
					full.Hosts = append([]agent.HostDesc(nil), full.Hosts...)
					full.Hosts[0].NeedsAPIKey = !full.Hosts[0].NeedsAPIKey
				case "duplicate legacy host":
					legacy.Hosts = append(append([]agent.HostDesc(nil), legacy.Hosts...), legacy.Hosts[0])
				}
				harvests[i].Legacy.Manifest = func() agent.ModelEndpointManifest { return legacy }
				harvests[i].ProtocolManifest = func() agent.ModelEndpointManifest { return full }
			}
			if _, err := buildProtocolView(harnesses, byHarness, harvests, declarations); err == nil {
				t.Fatal("invalid host tuples were accepted")
			}
		})
	}
}

func TestProtocolViewBuildAndRenderParity(t *testing.T) {
	view, err := BuildProtocolView()
	if err != nil {
		t.Fatal(err)
	}
	if view.SchemaVersion != ProtocolSchemaVersion || view.ContractABI != ProtocolContractABI {
		t.Fatal("qualified view version mismatch")
	}
	artifacts, err := view.Render()
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile(FileProtocolMatrix)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(artifacts.Files[FileProtocolMatrix], committed) {
		t.Fatal("qualified artifact is stale")
	}
	again, err := BuildProtocolView()
	if err != nil {
		t.Fatal(err)
	}
	other, err := again.Render()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(artifacts.Files[FileProtocolMatrix], other.Files[FileProtocolMatrix]) {
		t.Fatal("qualified generation is nondeterministic")
	}
	legacy, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	legacyArtifacts, err := legacy.Render()
	if err != nil {
		t.Fatal(err)
	}
	for name, generated := range legacyArtifacts.Files {
		old, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(old, generated) {
			t.Fatalf("legacy artifact changed: %s", name)
		}
		if _, collision := artifacts.Files[name]; collision {
			t.Fatalf("qualified artifact replaces v1 %s", name)
		}
	}
	view.Cells[0].Smoked = !view.Cells[0].Smoked
	view.Cells[0].AuthModes[0] = agent.AuthLocal
	if _, err := view.Render(); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatal("caller-authored view metadata was rendered")
	}
	freshLegacy, err := Build()
	if err != nil {
		t.Fatal(err)
	}
	freshArtifacts, err := freshLegacy.Render()
	if err != nil {
		t.Fatal(err)
	}
	for name, original := range legacyArtifacts.Files {
		if !bytes.Equal(original, freshArtifacts.Files[name]) {
			t.Fatalf("returned qualified metadata mutated legacy source: %s", name)
		}
	}
}

func TestProtocolViewRetainsLegacyOnlyWithoutAdvertisingIt(t *testing.T) {
	harnesses, byHarness, harvests, declarations := protocolFixture(t)
	view, err := buildProtocolView(harnesses, byHarness, harvests, declarations)
	if err != nil {
		t.Fatal(err)
	}
	retained := false
	for _, cell := range legacyCellProjection(declarations) {
		if cell.Harness == agent.HarnessAntigravity && cell.Protocol == agent.ProtoAntigravityOAuth {
			retained = true
		}
	}
	if !retained {
		t.Fatal("original legacy declaration was removed")
	}
	shell := false
	for _, row := range view.Harnesses {
		if row.Name == agent.HarnessAntigravity {
			t.Fatal("retired harness advertised in qualified view")
		}
		if row.Name == agent.HarnessShell {
			shell = true
		}
	}
	if !shell {
		t.Fatal("cellless standalone shell was incorrectly classified as retired")
	}
	for _, endpoint := range view.Endpoints {
		for _, host := range endpoint.Hosts {
			if host.Protocol == agent.ProtoAntigravityOAuth {
				t.Fatal("retired host protocol advertised in qualified view")
			}
		}
	}
	for _, cell := range view.Cells {
		if cell.Harness == agent.HarnessAntigravity || cell.Protocol == agent.ProtoAntigravityOAuth {
			t.Fatal("retired cell advertised in qualified view")
		}
	}
	for _, key := range view.LegacyDefaults {
		if key.Cell.Harness == agent.HarnessAntigravity {
			t.Fatal("qualified legacy reference re-enables retired cell")
		}
	}
	for _, alias := range view.LegacyAliases {
		if alias.Cell.Harness == agent.HarnessAntigravity {
			t.Fatal("qualified alias re-enables retired cell")
		}
	}
}

func TestProtocolViewRejectsUnmarkedOmissionAndRetiredReuse(t *testing.T) {
	for _, name := range []string{"unmarked omission", "active retired tuple", "retired tuple in full manifest", "legacy-only without original reference", "unrelated legacy host omission"} {
		t.Run(name, func(t *testing.T) {
			harnesses, byHarness, harvests, declarations := protocolFixture(t)
			for i, declaration := range declarations {
				if !declaration.legacyOnly {
					continue
				}
				switch name {
				case "unmarked omission":
					declarations[i].legacyOnly = false
				case "active retired tuple":
					cell := declaration.Cell
					cell.Harness, cell.Stability, cell.Smoked = agent.HarnessStub, "experimental", false
					cell.LegacyProviderID = nil
					declarations = append(declarations, protocolCellDeclaration{Cell: cell})
				case "legacy-only without original reference":
					declarations[i].LegacyDefault = nil
				}
				break
			}
			for i := range harvests {
				if name == "retired tuple in full manifest" && harvests[i].Legacy.Company == agent.CompanyGoogle {
					full := harvests[i].ProtocolManifest()
					full.Hosts = append([]agent.HostDesc(nil), full.Hosts...)
					for _, host := range harvests[i].Legacy.Manifest().Hosts {
						if host.Protocol == agent.ProtoAntigravityOAuth {
							full.Hosts = append(full.Hosts, host)
						}
					}
					harvests[i].ProtocolManifest = func() agent.ModelEndpointManifest { return full }
				}
				if name == "unrelated legacy host omission" && harvests[i].Legacy.Company == agent.CompanyOpenAI {
					full := harvests[i].ProtocolManifest()
					kept := make([]agent.HostDesc, 0, len(full.Hosts))
					for _, host := range full.Hosts {
						if host.Host != agent.HostAzure {
							kept = append(kept, host)
						}
					}
					full.Hosts = kept
					harvests[i].ProtocolManifest = func() agent.ModelEndpointManifest { return full }
				}
			}
			if _, err := buildProtocolView(harnesses, byHarness, harvests, declarations); err == nil {
				t.Fatal("unmarked omission or retirement revival was accepted")
			}
		})
	}
}
