package matrix

import (
	"fmt"
	"sort"

	"github.com/RenseiAI/donmai/agent"
)

// ---------------------------------------------------------------------------
// Output schema types (the committed JSON shape).
// ---------------------------------------------------------------------------

// HarnessRow is one harness in the generated harnesses[] section. For the two
// "raw" packages (gemini + ollama) the generator MERGES them into one row with
// union drives/hosts.
type HarnessRow struct {
	Name        agent.HarnessName    `json:"name"`
	HumanLabel  string               `json:"humanLabel"`
	Family      agent.Family         `json:"family"`
	ContractABI string               `json:"contractAbi"`
	Caps        agent.HarnessCaps    `json:"capabilities"`
	Drives      []agent.WireProtocol `json:"drives"`
	DrivesHosts []agent.ServingHost  `json:"drivesHosts"`
}

// EndpointRow is one model-endpoint company in the generated endpoints[].
type EndpointRow struct {
	Company     agent.Company        `json:"company"`
	HumanLabel  string               `json:"humanLabel"`
	Family      agent.Family         `json:"family"`
	ContractABI string               `json:"contractAbi"`
	Speaks      []agent.WireProtocol `json:"speaks"`
	Hosts       []agent.HostDesc     `json:"hosts"`
	Models      []agent.ModelDesc    `json:"models"`
}

// PeerFamilies is an empty-but-present section for the unchanged peer families
// (sandbox / issue-tracker / version-control). P1 emits empty arrays with the
// keys present so the schema is complete and docs never silently omit a family.
type PeerFamilies struct {
	Sandbox        []any `json:"sandbox"`
	IssueTracker   []any `json:"issueTracker"`
	VersionControl []any `json:"versionControl"`
}

// CapabilityMatrix is the top-level generated document (capability-matrix.json).
type CapabilityMatrix struct {
	SchemaVersion string                `json:"schemaVersion"`
	ContractABI   string                `json:"contractAbi"`
	GeneratedFrom string                `json:"generatedFrom"`
	Harnesses     []HarnessRow          `json:"harnesses"`
	Endpoints     []EndpointRow         `json:"endpoints"`
	Cells         []HarnessEndpointCell `json:"cells"`
	Denylist      []CellKey             `json:"denylist"`
	LegacyAliases []LegacyAlias         `json:"legacyAliases"`
	PeerFamilies  PeerFamilies          `json:"peerFamilies"`
}

// LegacyAlias is one (ProviderName → CellKey) row of the back-compat map,
// emitted in both matrix.json and (as Go source) registry_gen.go.
type LegacyAlias struct {
	ProviderName agent.ProviderName `json:"providerName"`
	Cell         CellKey            `json:"cell"`
}

// ---------------------------------------------------------------------------
// Build — the deterministic harvest+validate+assemble pipeline. Shared by the
// generator (matrix/gen) and the parity test, so both produce byte-identical
// artifacts.
// ---------------------------------------------------------------------------

// Built holds the four artifacts plus the registry source, all derived from
// the harvested manifests + the hand-authored cells. Construct via Build().
type Built struct {
	Matrix    CapabilityMatrix
	Harnesses []HarnessRow
	Endpoints []EndpointRow
	// AliasMap is the legacy ProviderName → CellKey map (also embedded in
	// Matrix.LegacyAliases as a sorted slice for stable JSON).
	AliasMap []LegacyAlias
}

// Build harvests the manifests, validates every hand-authored cell against
// them, assembles the deterministic matrix, and returns the artifacts. It is
// pure (no network, no filesystem) and deterministic (sorted everywhere).
// Returns an error for any invalid hand-authored cell so `go generate` fails
// loudly.
func Build() (*Built, error) {
	harnesses, harnessByName, err := buildHarnesses()
	if err != nil {
		return nil, err
	}
	endpoints, endpointByCompany := buildEndpoints()

	cells, err := buildCells(harnessByName, endpointByCompany)
	if err != nil {
		return nil, err
	}

	aliases, err := buildAliases(cells)
	if err != nil {
		return nil, err
	}

	dl := append([]CellKey{}, denylist...)
	sort.Slice(dl, func(i, j int) bool { return cellKeyLess(dl[i], dl[j]) })

	m := CapabilityMatrix{
		SchemaVersion: SchemaVersion,
		ContractABI:   ContractABI,
		GeneratedFrom: GeneratedFrom,
		Harnesses:     harnesses,
		Endpoints:     endpoints,
		Cells:         cells,
		Denylist:      dl,
		LegacyAliases: aliases,
		PeerFamilies: PeerFamilies{
			Sandbox:        []any{},
			IssueTracker:   []any{},
			VersionControl: []any{},
		},
	}

	return &Built{
		Matrix:    m,
		Harnesses: harnesses,
		Endpoints: endpoints,
		AliasMap:  aliases,
	}, nil
}

// buildHarnesses harvests + merges the harness manifests. The two "raw"
// packages (gemini + ollama) merge into one row with union drives/hosts.
// Returns the sorted rows and a lookup keyed by harness name. The merged raw
// caps take the UNION (logical-OR) of the agent-loop bools across the two raw
// packages so the matrix is permissive enough to validate either package's
// cells; per-package caps still live on each provider's Manifest().
func buildHarnesses() ([]HarnessRow, map[agent.HarnessName]HarnessRow, error) {
	byName := map[agent.HarnessName]*HarnessRow{}
	for _, h := range HarnessHarvestList() {
		mf := h.Manifest()
		if mf.Name != h.Name {
			return nil, nil, fmt.Errorf("harvest: harness %q returned manifest name %q", h.Name, mf.Name)
		}
		row, ok := byName[mf.Name]
		if !ok {
			r := HarnessRow{
				Name:        mf.Name,
				HumanLabel:  mf.HumanLabel,
				Family:      mf.Family,
				ContractABI: mf.ContractABI,
				Caps:        mf.Caps,
				Drives:      dedupProtocols(mf.Caps.Drives),
				DrivesHosts: dedupHosts(mf.Caps.DrivesHosts),
			}
			byName[mf.Name] = &r
			continue
		}
		// Merge a second package into the same harness id (the raw case).
		row.Drives = dedupProtocols(append(row.Drives, mf.Caps.Drives...))
		row.DrivesHosts = dedupHosts(append(row.DrivesHosts, mf.Caps.DrivesHosts...))
		row.Caps.Drives = row.Drives
		row.Caps.DrivesHosts = row.DrivesHosts
		row.Caps = unionCaps(row.Caps, mf.Caps)
	}

	rows := make([]HarnessRow, 0, len(byName))
	out := map[agent.HarnessName]HarnessRow{}
	for _, r := range byName {
		// Ensure the row's Caps.Drives/Hosts mirror the merged union.
		r.Caps.Drives = r.Drives
		r.Caps.DrivesHosts = r.DrivesHosts
		rows = append(rows, *r)
		out[r.Name] = *r
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, out, nil
}

// buildEndpoints harvests the endpoint manifests into sorted rows + a lookup.
func buildEndpoints() ([]EndpointRow, map[agent.Company]EndpointRow) {
	rows := make([]EndpointRow, 0)
	byCompany := map[agent.Company]EndpointRow{}
	for _, e := range EndpointHarvestList() {
		mf := e.Manifest()
		r := EndpointRow{
			Company:     mf.Company,
			HumanLabel:  mf.HumanLabel,
			Family:      mf.Family,
			ContractABI: mf.ContractABI,
			Speaks:      mf.Speaks,
			Hosts:       mf.Hosts,
			Models:      mf.Models,
		}
		rows = append(rows, r)
		byCompany[mf.Company] = r
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Company < rows[j].Company })
	return rows, byCompany
}

// buildCells validates every hand-authored cell against the harvested manifests
// and returns the sorted cell list. Validation enforces:
//   - protocol intersection: cell.protocol ∈ harness.drives AND
//     cell.protocol == endpoint.host(cell.host).protocol
//   - authMode subset: cell.authModes ⊆ endpoint.host(cell.host).authModes
//   - caps narrowing-only: any override only narrows (false where harness=true)
//   - not denylisted
func buildCells(
	harnessByName map[agent.HarnessName]HarnessRow,
	endpointByCompany map[agent.Company]EndpointRow,
) ([]HarnessEndpointCell, error) {
	denied := map[CellKey]bool{}
	for _, k := range denylist {
		denied[k] = true
	}

	out := make([]HarnessEndpointCell, 0, len(validCells))
	for _, c := range validCells {
		key := c.Key()
		if denied[key] {
			return nil, fmt.Errorf("cell %+v is on the denylist", key)
		}

		h, ok := harnessByName[c.Harness]
		if !ok {
			return nil, fmt.Errorf("cell %+v: unknown harness %q", key, c.Harness)
		}
		ep, ok := endpointByCompany[c.Endpoint]
		if !ok {
			return nil, fmt.Errorf("cell %+v: unknown endpoint company %q", key, c.Endpoint)
		}
		host, ok := findHost(ep, c.Host)
		if !ok {
			return nil, fmt.Errorf("cell %+v: endpoint %q has no host %q", key, c.Endpoint, c.Host)
		}

		// Protocol intersection.
		if !containsProtocol(h.Drives, c.Protocol) {
			return nil, fmt.Errorf("cell %+v: protocol %q not in harness %q drives %v", key, c.Protocol, c.Harness, h.Drives)
		}
		if host.Protocol != c.Protocol {
			return nil, fmt.Errorf("cell %+v: protocol %q != endpoint host protocol %q", key, c.Protocol, host.Protocol)
		}

		// AuthMode subset + enum membership.
		for _, am := range c.AuthModes {
			if !isCanonicalAuthMode(am) {
				return nil, fmt.Errorf("cell %+v: auth mode %q is not a canonical mode", key, am)
			}
			if !containsAuth(host.AuthModes, am) {
				return nil, fmt.Errorf("cell %+v: auth mode %q not in endpoint host authModes %v", key, am, host.AuthModes)
			}
		}

		// Caps narrowing-only.
		if err := validateNarrowing(key, c.Caps, h.Caps); err != nil {
			return nil, err
		}

		out = append(out, c)
	}

	sort.Slice(out, func(i, j int) bool { return cellKeyLess(out[i].Key(), out[j].Key()) })
	return out, nil
}

// buildAliases builds the sorted legacy ProviderName → CellKey alias slice from
// the cells' LegacyProviderID anchors. Asserts each anchor names a real
// ProviderName and resolves to a cell. opencode's default anchor is the
// opencode×openai×direct cell.
func buildAliases(cells []HarnessEndpointCell) ([]LegacyAlias, error) {
	byProvider := map[agent.ProviderName]CellKey{}
	for _, c := range cells {
		if c.LegacyProviderID == nil {
			continue
		}
		pid := *c.LegacyProviderID
		if !isRealProviderName(pid) {
			return nil, fmt.Errorf("alias: legacyProviderId %q is not a real provider", pid)
		}
		if existing, dup := byProvider[pid]; dup {
			return nil, fmt.Errorf("alias: legacyProviderId %q maps to two cells (%+v and %+v)", pid, existing, c.Key())
		}
		byProvider[pid] = c.Key()
	}

	aliases := make([]LegacyAlias, 0, len(byProvider))
	for p, k := range byProvider {
		aliases = append(aliases, LegacyAlias{ProviderName: p, Cell: k})
	}
	sort.Slice(aliases, func(i, j int) bool { return aliases[i].ProviderName < aliases[j].ProviderName })
	return aliases, nil
}

// ---------------------------------------------------------------------------
// Small pure helpers.
// ---------------------------------------------------------------------------

func findHost(ep EndpointRow, host agent.ServingHost) (agent.HostDesc, bool) {
	for _, h := range ep.Hosts {
		if h.Host == host {
			return h, true
		}
	}
	return agent.HostDesc{}, false
}

func containsProtocol(s []agent.WireProtocol, p agent.WireProtocol) bool {
	for _, x := range s {
		if x == p {
			return true
		}
	}
	return false
}

func containsAuth(s []agent.AuthMode, a agent.AuthMode) bool {
	for _, x := range s {
		if x == a {
			return true
		}
	}
	return false
}

func isCanonicalAuthMode(a agent.AuthMode) bool {
	switch a {
	case agent.AuthBYOK, agent.AuthMetered, agent.AuthShared, agent.AuthHostSession, agent.AuthLocal:
		return true
	default:
		return false
	}
}

// realProviderNames is the set of ProviderName consts that name a real,
// implemented provider (8 of them). Reserved-but-unimplemented names
// (spring-ai, a2a, jules) are intentionally absent.
func realProviderNames() []agent.ProviderName {
	return []agent.ProviderName{
		agent.ProviderClaude,
		agent.ProviderCodex,
		agent.ProviderGemini,
		agent.ProviderAGYCLI,
		agent.ProviderOllama,
		agent.ProviderOpenCode,
		agent.ProviderAmp,
		agent.ProviderStub,
	}
}

func isRealProviderName(p agent.ProviderName) bool {
	for _, x := range realProviderNames() {
		if x == p {
			return true
		}
	}
	return false
}

func dedupProtocols(in []agent.WireProtocol) []agent.WireProtocol {
	seen := map[agent.WireProtocol]bool{}
	out := make([]agent.WireProtocol, 0, len(in))
	for _, x := range in {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func dedupHosts(in []agent.ServingHost) []agent.ServingHost {
	seen := map[agent.ServingHost]bool{}
	out := make([]agent.ServingHost, 0, len(in))
	for _, x := range in {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// unionCaps OR-merges the agent-loop bools of two harness caps (the raw merge).
// Drives/DrivesHosts are merged separately by the caller. ToolPermissionFormat
// / StreamingTransport keep the first non-empty value (a's), since the two raw
// packages declare the same family-level framing intent.
func unionCaps(a, b agent.HarnessCaps) agent.HarnessCaps {
	out := a
	out.SupportsMessageInjection = a.SupportsMessageInjection || b.SupportsMessageInjection
	out.SupportsSessionResume = a.SupportsSessionResume || b.SupportsSessionResume
	out.SupportsToolPlugins = a.SupportsToolPlugins || b.SupportsToolPlugins
	out.AcceptsMcpServerSpec = a.AcceptsMcpServerSpec || b.AcceptsMcpServerSpec
	out.AcceptsAllowedToolsList = a.AcceptsAllowedToolsList || b.AcceptsAllowedToolsList
	out.EmitsSubagentEvents = a.EmitsSubagentEvents || b.EmitsSubagentEvents
	out.SupportsReasoningEffort = a.SupportsReasoningEffort || b.SupportsReasoningEffort
	out.SupportsOneShot = a.SupportsOneShot || b.SupportsOneShot
	out.NativeJSONMode = a.NativeJSONMode || b.NativeJSONMode
	if out.ToolPermissionFormat == "" {
		out.ToolPermissionFormat = b.ToolPermissionFormat
	}
	if out.StreamingTransport == "" {
		out.StreamingTransport = b.StreamingTransport
	}
	return out
}

// validateNarrowing asserts a per-cell caps override only narrows (sets a bool
// false where the harness sets it true). A cell may remove, never add, a
// capability.
func validateNarrowing(key CellKey, o *CapsOverride, h agent.HarnessCaps) error {
	if o == nil {
		return nil
	}
	check := func(name string, override *bool, harness bool) error {
		if override == nil {
			return nil
		}
		if *override && !harness {
			return fmt.Errorf("cell %+v: caps override %q adds a capability the harness lacks (narrowing-only)", key, name)
		}
		return nil
	}
	if err := check("inject", o.SupportsMessageInjection, h.SupportsMessageInjection); err != nil {
		return err
	}
	if err := check("resume", o.SupportsSessionResume, h.SupportsSessionResume); err != nil {
		return err
	}
	if err := check("tools", o.SupportsToolPlugins, h.SupportsToolPlugins); err != nil {
		return err
	}
	if err := check("mcp", o.AcceptsMcpServerSpec, h.AcceptsMcpServerSpec); err != nil {
		return err
	}
	if err := check("allowedToolsList", o.AcceptsAllowedToolsList, h.AcceptsAllowedToolsList); err != nil {
		return err
	}
	if err := check("subagents", o.EmitsSubagentEvents, h.EmitsSubagentEvents); err != nil {
		return err
	}
	if err := check("reasoningEffort", o.SupportsReasoningEffort, h.SupportsReasoningEffort); err != nil {
		return err
	}
	return nil
}

func cellKeyLess(a, b CellKey) bool {
	if a.Harness != b.Harness {
		return a.Harness < b.Harness
	}
	if a.Endpoint != b.Endpoint {
		return a.Endpoint < b.Endpoint
	}
	return a.Host < b.Host
}
