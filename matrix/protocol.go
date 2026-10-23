package matrix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"github.com/RenseiAI/donmai/agent"
)

// Qualified protocol artifact identity is distinct from the retained v1 view.
const (
	ProtocolSchemaVersion = "p2.0"
	ProtocolContractABI   = "capability-matrix/v2"
	FileProtocolMatrix    = "capability-matrix-v2.json"
)

// ProtocolCellKey is the opt-in identity. CellKey and Key retain their v1 layout.
type ProtocolCellKey struct {
	Cell     CellKey            `json:"cell"`
	Protocol agent.WireProtocol `json:"protocol"`
}

// QualifiedKey returns the opt-in host/protocol cell identity.
func (c HarnessEndpointCell) QualifiedKey() ProtocolCellKey {
	return ProtocolCellKey{Cell: c.Key(), Protocol: c.Protocol}
}

// protocolCellDeclaration is the single authored cell source. A default is a
// reference to the original tuple, not another cell roster or a permission bit.
type protocolCellDeclaration struct {
	Cell          HarnessEndpointCell
	LegacyDefault *ProtocolCellKey
	// legacyOnly retains historical compatibility; it never grants a run.
	legacyOnly bool
}

func legacyDefaultCell(cell HarnessEndpointCell) protocolCellDeclaration {
	key := cell.QualifiedKey()
	return protocolCellDeclaration{Cell: cell, LegacyDefault: &key}
}

func legacyOnlyCell(cell HarnessEndpointCell) protocolCellDeclaration {
	declaration := legacyDefaultCell(cell)
	declaration.legacyOnly = true
	return declaration
}

type protocolHostKey struct {
	company  agent.Company
	host     agent.ServingHost
	protocol agent.WireProtocol
}

func hostKey(cell HarnessEndpointCell) protocolHostKey {
	return protocolHostKey{cell.Endpoint, cell.Host, cell.Protocol}
}

func retiredProtocolTuples(declarations []protocolCellDeclaration) map[protocolHostKey]bool {
	retired := map[protocolHostKey]bool{}
	for _, declaration := range declarations {
		if declaration.legacyOnly {
			retired[hostKey(declaration.Cell)] = true
		}
	}
	return retired
}

func legacyCellProjection(declarations []protocolCellDeclaration) []HarnessEndpointCell {
	out := make([]HarnessEndpointCell, 0, len(declarations))
	for _, declaration := range declarations {
		if declaration.LegacyDefault != nil {
			out = append(out, declaration.Cell)
		}
	}
	return out
}

func validateProtocolDeclarations(declarations []protocolCellDeclaration) ([]protocolCellDeclaration, error) {
	seen := map[ProtocolCellKey]bool{}
	defaults := map[CellKey]bool{}
	retired := retiredProtocolTuples(declarations)
	for _, declaration := range declarations {
		cell := declaration.Cell
		key := cell.QualifiedKey()
		if key.Protocol == "" || seen[key] {
			return nil, fmt.Errorf("protocol matrix: empty or duplicate qualified cell %+v", key)
		}
		seen[key] = true
		if declaration.legacyOnly && declaration.LegacyDefault == nil {
			return nil, fmt.Errorf("protocol matrix: legacy-only tuple has no original default reference")
		}
		if !declaration.legacyOnly && retired[hostKey(cell)] {
			return nil, fmt.Errorf("protocol matrix: active cell uses a retired legacy-only tuple")
		}
		if declaration.LegacyDefault != nil {
			if *declaration.LegacyDefault != key || defaults[key.Cell] {
				return nil, fmt.Errorf("protocol matrix: invalid or duplicate legacy default %+v", key)
			}
			defaults[key.Cell] = true
		} else if cell.Stability != "experimental" || cell.Smoked || cell.LegacyProviderID != nil {
			return nil, fmt.Errorf("protocol matrix: unproven variant %+v must be experimental, unsmoked and have no legacy alias", key)
		}
	}
	return declarations, nil
}

// ProtocolEndpointHarvest is additive; EndpointHarvest remains source compatible.
type ProtocolEndpointHarvest struct {
	Legacy           EndpointHarvest
	ProtocolManifest func() agent.ModelEndpointManifest
}

// ProtocolEndpointHarvestList uses the same factories as EndpointHarvestList.
func ProtocolEndpointHarvestList() []ProtocolEndpointHarvest {
	providers := endpointProviders()
	out := make([]ProtocolEndpointHarvest, 0, len(providers))
	for _, provider := range providers {
		manifest := provider.Manifest
		if capable, ok := provider.(agent.ProtocolEndpointResolver); ok {
			manifest = capable.ProtocolManifest
		}
		out = append(out, ProtocolEndpointHarvest{
			Legacy:           EndpointHarvest{Company: provider.Manifest().Company, Manifest: provider.Manifest},
			ProtocolManifest: manifest,
		})
	}
	return out
}

// ProtocolMatrixView is separately versioned; no v1 schema or field is extended.
// Rows are compatibility declarations, never execution eligibility grants.
type ProtocolMatrixView struct {
	SchemaVersion  string                `json:"schemaVersion"`
	ContractABI    string                `json:"contractAbi"`
	GeneratedFrom  string                `json:"generatedFrom"`
	Harnesses      []HarnessRow          `json:"harnesses"`
	Endpoints      []EndpointRow         `json:"endpoints"`
	Cells          []HarnessEndpointCell `json:"cells"`
	LegacyDefaults []ProtocolCellKey     `json:"legacyDefaults"`
	LegacyAliases  []LegacyAlias         `json:"legacyAliases"`
	Denylist       []CellKey             `json:"denylist"`
	BinaryPins     []BinaryPinRow        `json:"binaryPins"`
	canonical      []byte
}

// BuildProtocolView builds only the opt-in view. Legacy Build uses the marked
// defaults and the provider's legacy Manifest, including single-default hosts.
func BuildProtocolView() (*ProtocolMatrixView, error) {
	harnesses, byHarness, err := buildHarnesses()
	if err != nil {
		return nil, err
	}
	return buildProtocolView(harnesses, byHarness, ProtocolEndpointHarvestList(), cellDeclarations)
}

func buildProtocolView(harnesses []HarnessRow, byHarness map[agent.HarnessName]HarnessRow,
	harvests []ProtocolEndpointHarvest, declarations []protocolCellDeclaration,
) (*ProtocolMatrixView, error) {
	declarations, err := validateProtocolDeclarations(declarations)
	if err != nil {
		return nil, err
	}
	endpoints, legacyByCompany, fullByCompany, err := buildProtocolEndpoints(harvests, retiredProtocolTuples(declarations))
	if err != nil {
		return nil, err
	}
	legacyCells, err := buildCells(legacyCellProjection(declarations), byHarness, legacyByCompany)
	if err != nil {
		return nil, fmt.Errorf("protocol matrix: legacy projection: %w", err)
	}
	aliases, err := buildAliases(legacyCells)
	if err != nil {
		return nil, err
	}
	cells := make([]HarnessEndpointCell, 0, len(declarations))
	defaults := make([]ProtocolCellKey, 0, len(legacyCells))
	activeAliases := make([]LegacyAlias, 0, len(aliases))
	activeKeys := map[CellKey]bool{}
	retiredHarnesses := map[agent.HarnessName]bool{}
	activeHarnesses := map[agent.HarnessName]bool{}
	for _, declaration := range declarations {
		cell := declaration.Cell
		if declaration.legacyOnly {
			retiredHarnesses[cell.Harness] = true
			continue
		}
		activeKeys[cell.Key()] = true
		activeHarnesses[cell.Harness] = true
		endpoint, ok := fullByCompany[cell.Endpoint]
		if !ok {
			return nil, fmt.Errorf("protocol matrix: unknown endpoint %q", cell.Endpoint)
		}
		host, ok := exactProtocolHost(endpoint.Hosts, cell.Host, cell.Protocol)
		if !ok {
			return nil, fmt.Errorf("protocol matrix: missing host/protocol tuple %q/%q", cell.Host, cell.Protocol)
		}
		// Reuse all v1 auth/cost/capability/narrowing predicates after selecting
		// the exact tuple. One cell at a time permits qualified variants while
		// the original v1 builder still refuses duplicate legacy triples.
		endpoint.Hosts = []agent.HostDesc{host}
		validated, err := buildCells([]HarnessEndpointCell{cell}, byHarness, map[agent.Company]EndpointRow{cell.Endpoint: endpoint})
		if err != nil {
			return nil, err
		}
		cells = append(cells, validated[0])
		if declaration.LegacyDefault != nil {
			defaults = append(defaults, *declaration.LegacyDefault)
		}
	}
	sort.Slice(cells, func(i, j int) bool { return protocolCellKeyLess(cells[i].QualifiedKey(), cells[j].QualifiedKey()) })
	sort.Slice(defaults, func(i, j int) bool { return protocolCellKeyLess(defaults[i], defaults[j]) })
	dl := append([]CellKey{}, denylist...)
	sort.Slice(dl, func(i, j int) bool { return cellKeyLess(dl[i], dl[j]) })
	activeRows := make([]HarnessRow, 0, len(harnesses))
	activeByHarness := map[agent.HarnessName]HarnessRow{}
	for _, row := range harnesses {
		if retiredHarnesses[row.Name] && !activeHarnesses[row.Name] {
			continue
		}
		activeRows = append(activeRows, row)
		activeByHarness[row.Name] = row
	}
	for _, alias := range aliases {
		if activeKeys[alias.Cell] {
			activeAliases = append(activeAliases, alias)
		}
	}
	pins, err := buildBinaryPins(activeByHarness)
	if err != nil {
		return nil, err
	}
	view := &ProtocolMatrixView{
		SchemaVersion: ProtocolSchemaVersion, ContractABI: ProtocolContractABI,
		GeneratedFrom: GeneratedFrom, Harnesses: activeRows, Endpoints: endpoints, Cells: cells,
		LegacyDefaults: defaults, LegacyAliases: activeAliases, Denylist: dl, BinaryPins: pins,
	}
	raw, err := marshalJSON(view)
	if err != nil {
		return nil, err
	}
	// Detach all exported slices and pointer metadata from the source
	// declarations: changing a returned view cannot alter a later v1 build.
	var detached ProtocolMatrixView
	if err := json.Unmarshal(raw, &detached); err != nil {
		return nil, fmt.Errorf("protocol matrix: detach view: %w", err)
	}
	detached.canonical = raw
	return &detached, nil
}

func buildProtocolEndpoints(harvests []ProtocolEndpointHarvest, retired map[protocolHostKey]bool) ([]EndpointRow, map[agent.Company]EndpointRow, map[agent.Company]EndpointRow, error) {
	rows := make([]EndpointRow, 0, len(harvests))
	legacyRows := map[agent.Company]EndpointRow{}
	fullRows := map[agent.Company]EndpointRow{}
	for _, harvest := range harvests {
		if harvest.Legacy.Manifest == nil || harvest.ProtocolManifest == nil {
			return nil, nil, nil, fmt.Errorf("protocol matrix: endpoint manifest is unavailable")
		}
		legacy, full := harvest.Legacy.Manifest(), harvest.ProtocolManifest()
		if legacy.Company != harvest.Legacy.Company || full.Company != legacy.Company || fullRows[full.Company].Company != "" {
			return nil, nil, nil, fmt.Errorf("protocol matrix: duplicate or mismatched endpoint company")
		}
		seen := map[ProtocolCellKey]bool{}
		for _, host := range full.Hosts {
			if retired[protocolHostKey{full.Company, host.Host, host.Protocol}] {
				return nil, nil, nil, fmt.Errorf("protocol matrix: retired legacy-only tuple present in active manifest")
			}
			key := ProtocolCellKey{Cell: CellKey{Endpoint: full.Company, Host: host.Host}, Protocol: host.Protocol}
			if host.Protocol == "" || seen[key] || !containsProtocol(full.Speaks, host.Protocol) {
				return nil, nil, nil, fmt.Errorf("protocol matrix: invalid or duplicate endpoint host/protocol tuple %+v", key)
			}
			seen[key] = true
		}
		legacyHosts := map[agent.ServingHost]bool{}
		for _, host := range legacy.Hosts {
			selected, ok := exactProtocolHost(full.Hosts, host.Host, host.Protocol)
			retainedOnly := retired[protocolHostKey{legacy.Company, host.Host, host.Protocol}]
			if legacyHosts[host.Host] || (!ok && !retainedOnly) || (ok && !reflect.DeepEqual(host, selected)) {
				return nil, nil, nil, fmt.Errorf("protocol matrix: legacy host default is missing, ambiguous or changed")
			}
			legacyHosts[host.Host] = true
		}
		row := endpointRow(full)
		sort.Slice(row.Hosts, func(i, j int) bool {
			if row.Hosts[i].Host != row.Hosts[j].Host {
				return row.Hosts[i].Host < row.Hosts[j].Host
			}
			return row.Hosts[i].Protocol < row.Hosts[j].Protocol
		})
		rows = append(rows, row)
		fullRows[full.Company], legacyRows[legacy.Company] = row, endpointRow(legacy)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Company < rows[j].Company })
	return rows, legacyRows, fullRows, nil
}

func endpointRow(manifest agent.ModelEndpointManifest) EndpointRow {
	return EndpointRow{
		Company: manifest.Company, HumanLabel: manifest.HumanLabel, Family: manifest.Family,
		ContractABI: manifest.ContractABI, Speaks: dedupProtocols(manifest.Speaks),
		Hosts: append([]agent.HostDesc(nil), manifest.Hosts...), Models: append([]agent.ModelDesc(nil), manifest.Models...),
	}
}

func exactProtocolHost(hosts []agent.HostDesc, host agent.ServingHost, protocol agent.WireProtocol) (agent.HostDesc, bool) {
	for _, candidate := range hosts {
		if candidate.Host == host && candidate.Protocol == protocol {
			return candidate, true
		}
	}
	return agent.HostDesc{}, false
}

func protocolCellKeyLess(a, b ProtocolCellKey) bool {
	if a.Cell != b.Cell {
		return cellKeyLess(a.Cell, b.Cell)
	}
	return a.Protocol < b.Protocol
}

// Render refuses caller-authored changes to the generated compatibility view.
func (v *ProtocolMatrixView) Render() (*Artifacts, error) {
	if v == nil || len(v.canonical) == 0 {
		return nil, fmt.Errorf("render protocol matrix: view was not built")
	}
	raw, err := marshalJSON(v)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(raw, v.canonical) {
		return nil, fmt.Errorf("render protocol matrix: view differs from canonical declarations")
	}
	return &Artifacts{Files: map[string][]byte{FileProtocolMatrix: raw}}, nil
}
