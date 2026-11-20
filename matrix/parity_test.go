package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/credentials"
)

// This is the load-bearing CI parity gate (P1-SPEC §5). It is a single pure
// test (no network, no daemon) asserting the six gate rules from 03's
// parity-gate plus the P1-specific manifest-agreement rule. Run target:
//
//	GOWORK=off go test -race ./matrix/...
//
// Treat the generated files as committed artifacts: rule 1 regenerates into
// buffers and byte-compares against the committed capability-matrix.json /
// harnesses.json / endpoints.json / matrix.json / capability-realizations.json.

// buildArtifacts is the shared regenerate helper for the byte-identical rule.
func buildArtifacts(t *testing.T) *Artifacts {
	t.Helper()
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	arts, err := built.Render()
	if err != nil {
		t.Fatalf("Render(): %v", err)
	}
	return arts
}

// TestParity_ByteIdenticalToFreshGenerate is rule 1: a fresh regenerate must be
// byte-identical to the committed artifacts. If this fails, run `make generate`.
func TestParity_ByteIdenticalToFreshGenerate(t *testing.T) {
	arts := buildArtifacts(t)
	for _, name := range []string{FileCapabilityMatrix, FileHarnesses, FileEndpoints, FileMatrix, FileRegistryGen, FileCapabilityRealizations, FileSupport} {
		want, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read committed %s: %v (did you run `make generate`?)", name, err)
		}
		got := arts.Files[name]
		if !bytes.Equal(got, want) {
			t.Errorf("%s is stale: committed bytes != fresh regenerate. Run `make generate`.\n"+
				"committed=%d bytes, fresh=%d bytes", name, len(want), len(got))
		}
	}
	// Sanity: the generated registry actually lives on disk as the path the
	// generator writes.
	if _, err := os.Stat(filepath.Join(".", FileRegistryGen)); err != nil {
		t.Fatalf("registry artifact missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(".", FileCapabilityRealizations)); err != nil {
		t.Fatalf("capability realization artifact missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(".", FileSupport)); err != nil {
		t.Fatalf("support page artifact missing: %v", err)
	}
}

// TestParity_SupportPageDerivedFromGeneratedHarnesses proves the support page
// is a derived projection, not a second source of truth: every page row maps
// to a generated harness row's per-mode deny baselines, and the page carries
// one row per generated execution-security record — no more, no fewer.
func TestParity_SupportPageDerivedFromGeneratedHarnesses(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	arts, err := built.Render()
	if err != nil {
		t.Fatalf("Render(): %v", err)
	}
	page := string(arts.Files[FileSupport])
	v1Page := page
	if idx := strings.Index(page, "## Harness support v2"); idx >= 0 {
		v1Page = page[:idx]
	}
	wantRows := 0
	for _, h := range built.Harnesses {
		wantRows += len(h.ExecutionSecurity)
		for _, row := range h.ExecutionSecurity {
			harnessCell := "| " + string(h.Name) + " | " + string(row.Mode) + " |"
			if !strings.Contains(v1Page, harnessCell) {
				t.Errorf("support page has no row for harness %q mode %q", h.Name, row.Mode)
			}
			toolCell := "| " + string(row.ToolDenyBaseline) + " | "
			if !strings.Contains(v1Page, toolCell) {
				t.Errorf("support page omits deny baseline %q for %q/%q", row.ToolDenyBaseline, h.Name, row.Mode)
			}
		}
	}
	lines := 0
	for _, line := range strings.Split(v1Page, "\n") {
		if strings.HasPrefix(line, "| ") {
			lines++
		}
	}
	// Header row + separator row + one row per execution-security record.
	if lines != wantRows+2 {
		t.Errorf("support page has %d table lines; want %d (header + separator + %d harness/mode rows)", lines, wantRows+2, wantRows)
	}
}

// TestParity_LegacyProviderIDsResolve is rule 2: every non-null cell
// legacyProviderId and every alias-map key is a known real ProviderName AND
// resolves to a cell.
func TestParity_LegacyProviderIDsResolve(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	cellByKey := map[CellKey]bool{}
	for _, c := range built.Matrix.Cells {
		cellByKey[c.Key()] = true
	}

	for _, c := range built.Matrix.Cells {
		if c.LegacyProviderID == nil {
			continue
		}
		if !isRealProviderName(*c.LegacyProviderID) {
			t.Errorf("cell %+v: legacyProviderId %q is not a real ProviderName", c.Key(), *c.LegacyProviderID)
		}
	}

	for _, a := range built.AliasMap {
		if !isRealProviderName(a.ProviderName) {
			t.Errorf("alias key %q is not a real ProviderName", a.ProviderName)
		}
		if !cellByKey[a.Cell] {
			t.Errorf("alias %q resolves to cell %+v which is not in the matrix", a.ProviderName, a.Cell)
		}
	}

	// LegacyAliasMap (the generated Go map) must match AliasMap exactly.
	if len(LegacyAliasMap) != len(built.AliasMap) {
		t.Errorf("LegacyAliasMap has %d entries; AliasMap has %d", len(LegacyAliasMap), len(built.AliasMap))
	}
	for _, a := range built.AliasMap {
		got, ok := LegacyAliasMap[a.ProviderName]
		if !ok {
			t.Errorf("LegacyAliasMap missing %q", a.ProviderName)
			continue
		}
		if got != a.Cell {
			t.Errorf("LegacyAliasMap[%q] = %+v; want %+v", a.ProviderName, got, a.Cell)
		}
	}
}

// TestParity_ProtocolIntersection is rule 3: for every cell, cell.protocol ∈
// harness.drives AND cell.protocol == endpoint.host(cell.host).protocol. Each
// canonical harness has its own concrete drive surface; duplicate ids fail.
func TestParity_ProtocolIntersection(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	harnessByName := map[agent.HarnessName]HarnessRow{}
	for _, h := range built.Harnesses {
		harnessByName[h.Name] = h
	}
	endpointByCompany := map[agent.Company]EndpointRow{}
	for _, e := range built.Endpoints {
		endpointByCompany[e.Company] = e
	}

	for _, c := range built.Matrix.Cells {
		h, ok := harnessByName[c.Harness]
		if !ok {
			t.Errorf("cell %+v: harness not found", c.Key())
			continue
		}
		if !containsProtocol(h.Drives, c.Protocol) {
			t.Errorf("cell %+v: protocol %q not in harness drives %v", c.Key(), c.Protocol, h.Drives)
		}
		ep, ok := endpointByCompany[c.Endpoint]
		if !ok {
			t.Errorf("cell %+v: endpoint company not found", c.Key())
			continue
		}
		host, ok := findHost(ep, c.Host)
		if !ok {
			t.Errorf("cell %+v: endpoint host not found", c.Key())
			continue
		}
		if host.Protocol != c.Protocol {
			t.Errorf("cell %+v: cell protocol %q != endpoint host protocol %q", c.Key(), c.Protocol, host.Protocol)
		}
	}
}

// TestParity_AuthModesSubsetAndDeclared is rule 4: cell.authModes ⊆
// endpoint.host(cell.host).authModes AND each authMode is in the canonical
// 5-mode enum.
func TestParity_AuthModesSubsetAndDeclared(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	endpointByCompany := map[agent.Company]EndpointRow{}
	for _, e := range built.Endpoints {
		endpointByCompany[e.Company] = e
	}

	for _, c := range built.Matrix.Cells {
		ep := endpointByCompany[c.Endpoint]
		host, ok := findHost(ep, c.Host)
		if !ok {
			t.Errorf("cell %+v: endpoint host not found", c.Key())
			continue
		}
		for _, am := range c.AuthModes {
			if !isCanonicalAuthMode(am) {
				t.Errorf("cell %+v: auth mode %q is not canonical", c.Key(), am)
			}
			if !containsAuth(host.AuthModes, am) {
				t.Errorf("cell %+v: auth mode %q not in endpoint host authModes %v", c.Key(), am, host.AuthModes)
			}
		}
	}
}

// TestParity_BlocklistNoCollision is rule 5: no HostDesc.EnvKeys entry across
// all endpoints collides with an AgentEnvBlocklist entry, and the blocklist has
// no duplicate entries.
func TestParity_BlocklistNoCollision(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}

	// Blocklist de-dup.
	seen := map[string]bool{}
	for _, k := range credentials.AgentEnvBlocklist {
		if seen[k] {
			t.Errorf("AgentEnvBlocklist has duplicate entry %q", k)
		}
		seen[k] = true
	}

	// No endpoint env key collides with a blocklist entry.
	for _, ep := range built.Endpoints {
		for _, host := range ep.Hosts {
			for _, key := range host.EnvKeys {
				if credentials.IsBlocked(key) {
					t.Errorf("endpoint %q host %q declares env key %q which is in AgentEnvBlocklist (snapshot would strip it)",
						ep.Company, host.Host, key)
				}
			}
		}
	}
}

// TestParity_CapsNarrowingOnly is rule 6: every per-cell caps override only
// narrows (sets a bool false where the harness sets it true). The generator
// already enforces this in Build(); this re-asserts it directly on every cell
// that carries an override.
func TestParity_CapsNarrowingOnly(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	harnessByName := map[agent.HarnessName]HarnessRow{}
	for _, h := range built.Harnesses {
		harnessByName[h.Name] = h
	}
	for _, c := range built.Matrix.Cells {
		if c.Caps == nil {
			continue
		}
		h := harnessByName[c.Harness]
		if err := validateNarrowing(c.Key(), c.Caps, h.Caps); err != nil {
			t.Errorf("%v", err)
		}
	}
}

// TestParity_ManifestAgreesWithCapabilities is rule 7 (the P1 additive-safety
// guard): for each of the 8 harness providers, the Manifest().Caps agent-loop
// fields equal the corresponding Capabilities() fields — proving the manifest
// is a faithful additive projection, not a divergent second source of truth.
func TestParity_ManifestAgreesWithCapabilities(t *testing.T) {
	providers := HarnessProvidersForParity()
	if len(providers) != 9 {
		t.Fatalf("expected 9 harness providers, got %d", len(providers))
	}
	for _, p := range providers {
		if p == nil {
			t.Fatalf("nil harness provider in parity list")
		}
		mf := p.Manifest()
		caps := p.Capabilities()
		name := mf.Name

		check := func(field string, manifestVal, capsVal any) {
			if !reflect.DeepEqual(manifestVal, capsVal) {
				t.Errorf("harness %q: Manifest().Caps.%s=%v but Capabilities().%s=%v (manifest must project Capabilities)",
					name, field, manifestVal, field, capsVal)
			}
		}
		check("SupportsMessageInjection", mf.Caps.SupportsMessageInjection, caps.SupportsMessageInjection)
		check("SupportsSessionResume", mf.Caps.SupportsSessionResume, caps.SupportsSessionResume)
		check("SupportsToolPlugins", mf.Caps.SupportsToolPlugins, caps.SupportsToolPlugins)
		check("AcceptsMcpServerSpec", mf.Caps.AcceptsMcpServerSpec, caps.AcceptsMcpServerSpec)
		check("AcceptsAllowedToolsList", mf.Caps.AcceptsAllowedToolsList, caps.AcceptsAllowedToolsList)
		check("EmitsSubagentEvents", mf.Caps.EmitsSubagentEvents, caps.EmitsSubagentEvents)
		check("SupportsReasoningEffort", mf.Caps.SupportsReasoningEffort, caps.SupportsReasoningEffort)
		check("ToolPermissionFormat", mf.Caps.ToolPermissionFormat, caps.ToolPermissionFormat)
	}
}

// TestParity_PromptDeliveryProfilesHarvested proves prompt semantics are part
// of the generated manifest contract. Each provider contributes profiles only
// to its own canonical harness row.
func TestParity_PromptDeliveryProfilesHarvested(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	got := make(map[agent.HarnessName]map[string]bool, len(built.Harnesses))
	for _, row := range built.Harnesses {
		got[row.Name] = make(map[string]bool, len(row.PromptDelivery))
		for _, profile := range row.PromptDelivery {
			if profile.ID == "" || profile.Mode == "" || profile.SystemDelivery == "" || profile.BaseAppendDelivery == "" || profile.BaseReplaceDelivery == "" || profile.ContextDelivery == "" || profile.UserDelivery == "" || profile.AmendmentDelivery == "" {
				t.Errorf("harness %q has incomplete prompt profile: %+v", row.Name, profile)
			}
			if got[row.Name][profile.ID] {
				t.Errorf("harness %q has duplicate prompt profile %q", row.Name, profile.ID)
			}
			got[row.Name][profile.ID] = true
		}
	}
	for _, harvested := range HarnessHarvestList() {
		manifest := harvested.Manifest()
		if manifest.Name != agent.HarnessStub && len(manifest.PromptDelivery) == 0 {
			t.Errorf("concrete harness %q declares no prompt delivery profile", manifest.Name)
		}
		for _, profile := range manifest.PromptDelivery {
			if !got[manifest.Name][profile.ID] {
				t.Errorf("generated harness %q omits prompt profile %q", manifest.Name, profile.ID)
			}
		}
	}
}

// TestParity_ToolLifecycleProfilesHarvested proves tool/MCP/policy/lifecycle
// semantics are generated independently from the legacy capability booleans.
func TestParity_ToolLifecycleProfilesHarvested(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	got := make(map[agent.HarnessName]map[string]bool, len(built.Harnesses))
	for _, row := range built.Harnesses {
		got[row.Name] = make(map[string]bool, len(row.ToolLifecycle))
		for _, profile := range row.ToolLifecycle {
			if profile.ID == "" || profile.Mode == "" || profile.MCPDelivery == "" || profile.NativeToolPolicyDelivery == "" || profile.PermissionConfigDelivery == "" || profile.LifecycleDelivery == "" || profile.ReplayDelivery == "" || profile.CleanupDelivery == "" || profile.EvidenceTier == "" {
				t.Errorf("harness %q has incomplete tool/lifecycle profile: %+v", row.Name, profile)
			}
			if got[row.Name][profile.ID] {
				t.Errorf("harness %q has duplicate tool/lifecycle profile %q", row.Name, profile.ID)
			}
			got[row.Name][profile.ID] = true
		}
	}
	for _, harvested := range HarnessHarvestList() {
		manifest := harvested.Manifest()
		if len(manifest.ToolLifecycle) == 0 {
			t.Errorf("harness %q declares no tool/lifecycle profile", manifest.Name)
		}
		for _, profile := range manifest.ToolLifecycle {
			if !got[manifest.Name][profile.ID] {
				t.Errorf("generated harness %q omits tool/lifecycle profile %q", manifest.Name, profile.ID)
			}
		}
	}
}

// TestParity_AliasCoverageCompleteness covers §6's alias-coverage obligation:
// every real provider's ProviderName maps to a cell, and the platform-reserved
// name (jules) is intentionally absent.
func TestParity_AliasCoverageCompleteness(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	aliasByName := map[agent.ProviderName]bool{}
	for _, a := range built.AliasMap {
		aliasByName[a.ProviderName] = true
	}

	for _, p := range realProviderNames() {
		if !aliasByName[p] {
			t.Errorf("real provider %q has no legacy alias (P1b would silently lose it)", p)
		}
	}

	for _, reserved := range []agent.ProviderName{agent.ProviderJules} {
		if aliasByName[reserved] {
			t.Errorf("reserved-but-unimplemented provider %q must NOT have a cell/alias", reserved)
		}
	}

	if len(built.AliasMap) != len(realProviderNames()) {
		t.Errorf("alias map has %d entries; expected exactly %d (one per real provider)",
			len(built.AliasMap), len(realProviderNames()))
	}
}

// TestParity_NoCrossProtocolOpencodeAnthropic guards the §6 drift warning:
// opencode drives ONLY openai-chat, so there must be no opencode×anthropic
// cell, and opencode must not appear with the anthropic-messages protocol.
func TestParity_NoCrossProtocolOpencodeAnthropic(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	for _, c := range built.Matrix.Cells {
		if c.Harness == agent.HarnessOpenCode {
			if c.Endpoint == agent.CompanyAnthropic {
				t.Errorf("invalid opencode×anthropic cell present: %+v", c.Key())
			}
			if c.Protocol == agent.ProtoAnthropicMessages {
				t.Errorf("opencode cell %+v drives anthropic-messages (opencode is openai-chat only)", c.Key())
			}
		}
	}
}

// TestParity_NoRetiredAntigravityCell is the retirement guard: the removed
// harness must not reappear as a harvested row, a valid cell, or a legacy
// alias. Re-adding the harvest entry, the cell, or the provider anchor flips
// this test red — see the retirement revert proof in the PR description.
func TestParity_NoRetiredAntigravityCell(t *testing.T) {
	t.Parallel()
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	for _, h := range built.Harnesses {
		if h.Name == agent.HarnessName("antigravity") {
			t.Errorf("retired antigravity harness present in generated harnesses")
		}
	}
	for _, c := range built.Matrix.Cells {
		if c.Harness == agent.HarnessName("antigravity") {
			t.Errorf("retired antigravity cell present: %+v", c.Key())
		}
	}
	for _, a := range built.AliasMap {
		if a.ProviderName == agent.ProviderName("agy-cli") {
			t.Errorf("retired agy-cli legacy alias present: %+v", a.Cell)
		}
	}
}

// TestParity_GenericCostHonesty asserts the structural invariant across ALL
// cells: a cell that needs an API key can never bring its own auth.
func TestParity_GenericCostHonesty(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	for _, c := range built.Matrix.Cells {
		if c.NeedsAPIKey && c.BringsOwnAuth {
			t.Errorf("cell %+v violates cost honesty: needsApiKey && bringsOwnAuth", c.Key())
		}
	}
}

// TestParity_SmokeEvidenceDerivedNotAuthored is the parity-gate half of the
// computed-evidence rule: every generated cell's ProvingLanes/Stability/
// Smoked must equal what the registry plus the gate outcomes derive — the
// committed JSON carries no independent claim. It additionally pins that
// every generated proving lane is actually registered (a stale committed
// artifact carrying a renamed lane fails here, not just in the smoke
// suite's own existence check).
func TestParity_SmokeEvidenceDerivedNotAuthored(t *testing.T) {
	t.Parallel()
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	byCell := SmokeProofsByCell()
	for _, c := range built.Matrix.Cells {
		stability, smoked, proving, err := deriveCellSmokeEvidence(c.Key(), byCell[c.Key()], lastSmokeGateOutcomes)
		if err != nil {
			t.Errorf("cell %+v: derive: %v", c.Key(), err)
			continue
		}
		if c.Stability != stability || c.Smoked != smoked {
			t.Errorf("cell %+v = stability %q smoked %v; derived %q/%v (evidence must be derived, not authored)",
				c.Key(), c.Stability, c.Smoked, stability, smoked)
		}
		if len(c.ProvingLanes) != len(proving) {
			t.Errorf("cell %+v provingLanes = %v; derived %v", c.Key(), c.ProvingLanes, proving)
			continue
		}
		for i := range proving {
			if c.ProvingLanes[i] != proving[i] {
				t.Errorf("cell %+v provingLanes = %v; derived %v", c.Key(), c.ProvingLanes, proving)
				break
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Harness support v2 parity suite: contract completeness, conservative
// fallback, sub-agent separation, and doc/machine parity.

func supportAxisLookup(t *testing.T, support HarnessSupportV2, axis string) SupportAxisEntry {
	t.Helper()
	for _, entry := range support.Axes {
		if entry.Axis == axis {
			return entry
		}
	}
	t.Fatalf("support contract omits axis %q (want every v2 axis in canonical order)", axis)
	return SupportAxisEntry{}
}

func assertEvidenceSorted(t *testing.T, harness agent.HarnessName, axis string, evidence []string) {
	t.Helper()
	if !sort.StringsAreSorted(evidence) {
		t.Errorf("harness %q axis %q evidence not sorted: %q", harness, axis, evidence)
	}
	seen := map[string]bool{}
	for _, ref := range evidence {
		if ref == "" || seen[ref] {
			t.Errorf("harness %q axis %q has empty or duplicate evidence %q", harness, axis, ref)
		}
		seen[ref] = true
	}
}

func assertSupportValue(t *testing.T, harness agent.HarnessName, axis string, value *SupportValue) {
	t.Helper()
	if value == nil {
		t.Errorf("harness %q axis %q misses its fact", harness, axis)
		return
	}
	if value.Level != SupportSupported && value.Level != SupportUnsupported && value.Level != SupportUnverified {
		t.Errorf("harness %q axis %q has bad level %q", harness, axis, value.Level)
	}
	if value.Level == SupportUnverified && len(value.Evidence) != 0 {
		t.Errorf("harness %q axis %q unverified carries evidence", harness, axis)
	}
	if (value.Level == SupportSupported || value.Level == SupportUnsupported) && len(value.Evidence) == 0 {
		t.Errorf("harness %q axis %q claims without evidence", harness, axis)
	}
	assertEvidenceSorted(t, harness, axis, value.Evidence)
}

// TestSupportV2_Completeness is AC-1: every harness emits every v2 axis in
// canonical order with its level and evidence references.
func TestSupportV2_Completeness(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	if len(built.Harnesses) == 0 {
		t.Fatal("no harnesses harvested")
	}
	for _, h := range built.Harnesses {
		if h.SupportV2.ContractVersion != HarnessSupportContractVersion {
			t.Errorf("harness %q contract %q; want %q", h.Name, h.SupportV2.ContractVersion, HarnessSupportContractVersion)
		}
		if len(h.SupportV2.Axes) != len(supportAxisOrder) {
			t.Errorf("harness %q has %d axes; want %d", h.Name, len(h.SupportV2.Axes), len(supportAxisOrder))
			continue
		}
		for i, axis := range supportAxisOrder {
			entry := h.SupportV2.Axes[i]
			if entry.Axis != axis {
				t.Errorf("harness %q axis %d = %q; want %q in canonical order", h.Name, i, entry.Axis, axis)
			}
			if axis == SupportAxisSubagentControl {
				assertSupportValue(t, h.Name, axis+".dispatchSpawnedWorkspaces", entry.DispatchSpawnedWorkspaces)
				assertSupportValue(t, h.Name, axis+".harnessNativeControls", entry.HarnessNativeControls)
				continue
			}
			assertSupportValue(t, h.Name, axis, &SupportValue{Level: entry.Level, Evidence: entry.Evidence})
		}
	}
	sources := []CapabilityRealizationEvidenceSource{}
	for _, capability := range []string{supportCapCode, supportCapA2A, supportCapArchitecture, supportCapTokenCost, supportCapDispatchSpaces} {
		source := realizationEvidenceFixture(t, capability, agent.HarnessPi, "pi/headless/tool-lifecycle-v2", agent.PromptModeAutonomous)
		source.Executor = CapabilityFixtureExecutorFunc(func(_ context.Context, declaration agent.CapabilityRealizationDeclaration) (agent.CapabilityFixtureExecution, error) {
			return sourceExecution(t, declaration), nil
		})
		sources = append(sources, source)
	}
	native := piSubagentSource(t, []string{"dispatch_child"}, true, false)
	qualified, err := BuildWithSubagentFixtures(context.Background(), sources, append(NativeSubagentFixtures(), native))
	if err != nil {
		t.Fatalf("qualified BuildWithSubagentFixtures(): %v", err)
	}
	var piRow *HarnessRow
	for i := range qualified.Harnesses {
		if qualified.Harnesses[i].Name == agent.HarnessPi {
			piRow = &qualified.Harnesses[i]
		}
	}
	if piRow == nil {
		t.Fatal("pi harness missing from qualified build")
	}
	for _, capability := range []string{SupportAxisCodeIntelligence, SupportAxisA2A, SupportAxisArchitectureIntelligence, SupportAxisTokenCostAccuracy} {
		entry := supportAxisLookup(t, piRow.SupportV2, capability)
		if entry.Level != SupportSupported || len(entry.Evidence) == 0 {
			t.Errorf("qualifying fixture left pi axis %q at %q (want supported with evidence)", capability, entry.Level)
		}
	}
	subagent := supportAxisLookup(t, piRow.SupportV2, SupportAxisSubagentControl)
	if subagent.DispatchSpawnedWorkspaces.Level != SupportSupported || len(subagent.DispatchSpawnedWorkspaces.Evidence) == 0 {
		t.Errorf("qualifying dispatch fixture left dispatchSpawnedWorkspaces at %q", subagent.DispatchSpawnedWorkspaces.Level)
	}
	if subagent.HarnessNativeControls.Level != SupportSupported || len(subagent.HarnessNativeControls.Evidence) == 0 {
		t.Errorf("qualifying native fixture left harnessNativeControls at %q", subagent.HarnessNativeControls.Level)
	}
}

// TestSupportV2_ConservativeFallback is AC-2: missing, skipped, malformed, or
// non-qualifying evidence emits unverified, never a manifest or adapter claim.
func TestSupportV2_ConservativeFallback(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	byName := map[agent.HarnessName]HarnessRow{}
	for _, h := range built.Harnesses {
		byName[h.Name] = h
	}
	for _, axis := range []string{SupportAxisCodeIntelligence, SupportAxisA2A, SupportAxisArchitectureIntelligence, SupportAxisTokenCostAccuracy} {
		for _, h := range built.Harnesses {
			entry := supportAxisLookup(t, h.SupportV2, axis)
			if entry.Level != SupportUnverified {
				t.Errorf("plain build harness %q axis %q = %q; want unverified without fixture evidence", h.Name, axis, entry.Level)
			}
		}
	}
	for _, name := range []agent.HarnessName{agent.HarnessGeminiDirect, agent.HarnessOllama, agent.HarnessOpenCode} {
		entry := supportAxisLookup(t, byName[name].SupportV2, SupportAxisInteractiveUse)
		if entry.Level != SupportUnverified {
			t.Errorf("harness %q interactive = %q without an interactive profile; want unverified", name, entry.Level)
		}
	}
	piRow := byName[agent.HarnessPi]
	nonQualifying := []CapabilityEligibilityRow{{
		CapabilityID: supportCapCode, HarnessID: agent.HarnessPi, AdapterVersion: "pi/headless/tool-lifecycle-unknown", Mode: agent.PromptModeAutonomous,
		EvidenceTier: agent.CapabilityEvidenceSmoked, ProductionEligible: true,
	}}
	if level, _ := capabilityAxisSupport(piRow, nonQualifying, supportCapCode); level != SupportUnverified {
		t.Errorf("unknown adapter promoted code intelligence to %q; want unverified", level)
	}
	skipped, err := BuildWithCapabilityRealizations(context.Background(), nil)
	if err != nil {
		t.Fatalf("skipped BuildWithCapabilityRealizations(): %v", err)
	}
	for _, h := range skipped.Harnesses {
		if h.Name != agent.HarnessPi {
			continue
		}
		if entry := supportAxisLookup(t, h.SupportV2, SupportAxisCodeIntelligence); entry.Level != SupportUnverified {
			t.Errorf("skipped evidence promoted code intelligence to %q", entry.Level)
		}
	}
}

// TestSupportV2_SubagentSeparation is AC-3: dispatch-spawned workspaces and
// harness-native controls are distinct facts from distinct evidence.
func TestSupportV2_SubagentSeparation(t *testing.T) {
	native := piSubagentSource(t, []string{"dispatch_child"}, true, false)
	dispatchOnly := realizationEvidenceFixture(t, supportCapDispatchSpaces, agent.HarnessPi, "pi/headless/tool-lifecycle-v2", agent.PromptModeAutonomous)
	dispatchOnly.Executor = CapabilityFixtureExecutorFunc(func(_ context.Context, declaration agent.CapabilityRealizationDeclaration) (agent.CapabilityFixtureExecution, error) {
		return sourceExecution(t, declaration), nil
	})
	built, err := BuildWithSubagentFixtures(context.Background(), []CapabilityRealizationEvidenceSource{dispatchOnly}, append(NativeSubagentFixtures(), native))
	if err != nil {
		t.Fatalf("BuildWithSubagentFixtures(): %v", err)
	}
	var piRow *HarnessRow
	for i := range built.Harnesses {
		if built.Harnesses[i].Name == agent.HarnessPi {
			piRow = &built.Harnesses[i]
		}
	}
	if piRow == nil {
		t.Fatal("pi harness missing")
	}
	subagent := supportAxisLookup(t, piRow.SupportV2, SupportAxisSubagentControl)
	if subagent.DispatchSpawnedWorkspaces.Level != SupportSupported {
		t.Errorf("dispatch evidence left dispatchSpawnedWorkspaces at %q; want supported", subagent.DispatchSpawnedWorkspaces.Level)
	}
	if subagent.HarnessNativeControls.Level != SupportSupported {
		t.Errorf("native evidence left harnessNativeControls at %q; want supported", subagent.HarnessNativeControls.Level)
	}
	dispatchAlone, err := BuildWithSubagentFixtures(context.Background(), []CapabilityRealizationEvidenceSource{dispatchOnly}, NativeSubagentFixtures())
	if err != nil {
		t.Fatalf("dispatch-alone BuildWithSubagentFixtures(): %v", err)
	}
	for _, h := range dispatchAlone.Harnesses {
		if h.Name != agent.HarnessPi {
			continue
		}
		entry := supportAxisLookup(t, h.SupportV2, SupportAxisSubagentControl)
		if entry.DispatchSpawnedWorkspaces.Level != SupportSupported {
			t.Errorf("dispatch-alone dispatchSpawnedWorkspaces = %q; want supported", entry.DispatchSpawnedWorkspaces.Level)
		}
		if entry.HarnessNativeControls.Level != SupportUnverified {
			t.Errorf("dispatch evidence promoted harnessNativeControls to %q; want unverified", entry.HarnessNativeControls.Level)
		}
	}
	nativeAlone, err := BuildWithSubagentFixtures(context.Background(), nil, append(NativeSubagentFixtures(), native))
	if err != nil {
		t.Fatalf("native-alone BuildWithSubagentFixtures(): %v", err)
	}
	for _, h := range nativeAlone.Harnesses {
		if h.Name != agent.HarnessPi {
			continue
		}
		entry := supportAxisLookup(t, h.SupportV2, SupportAxisSubagentControl)
		if entry.HarnessNativeControls.Level != SupportSupported {
			t.Errorf("native-alone harnessNativeControls = %q; want supported", entry.HarnessNativeControls.Level)
		}
		if entry.DispatchSpawnedWorkspaces.Level != SupportUnverified {
			t.Errorf("native evidence promoted dispatchSpawnedWorkspaces to %q; want unverified", entry.DispatchSpawnedWorkspaces.Level)
		}
	}
	plain, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	for _, h := range plain.Harnesses {
		if h.Name != agent.HarnessClaudeCode {
			continue
		}
		entry := supportAxisLookup(t, h.SupportV2, SupportAxisSubagentControl)
		if entry.HarnessNativeControls.Level != SupportUnverified {
			t.Errorf("claude-code event observation promoted harnessNativeControls to %q; want unverified without a delivery digest", entry.HarnessNativeControls.Level)
		}
	}
}

// TestSupportV2_DocMachineParity is AC-4: fresh generation makes
// harnesses.json and SUPPORT.md project identical v2 rows.
func TestSupportV2_DocMachineParity(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	arts, err := built.Render()
	if err != nil {
		t.Fatalf("Render(): %v", err)
	}
	var harnessesDoc struct {
		Harnesses []HarnessRow `json:"harnesses"`
	}
	if err := json.Unmarshal(arts.Files[FileHarnesses], &harnessesDoc); err != nil {
		t.Fatalf("unmarshal harnesses.json: %v", err)
	}
	page := string(arts.Files[FileSupport])
	if !strings.Contains(page, "## Harness support v2 (`"+HarnessSupportContractVersion+"`)") {
		t.Fatal("support page omits the v2 section")
	}
	for _, h := range harnessesDoc.Harnesses {
		var builtRow *HarnessRow
		for i := range built.Harnesses {
			if built.Harnesses[i].Name == h.Name {
				builtRow = &built.Harnesses[i]
			}
		}
		if builtRow == nil {
			t.Errorf("harnesses.json harness %q has no built row", h.Name)
			continue
		}
		if !reflect.DeepEqual(h.SupportV2, builtRow.SupportV2) {
			t.Errorf("harnesses.json supportV2 for %q disagrees with the built row", h.Name)
		}
		for _, axis := range h.SupportV2.Axes {
			if axis.Axis == SupportAxisSubagentControl {
				for _, fact := range []struct {
					suffix string
					value  *SupportValue
				}{
					{"dispatchSpawnedWorkspaces", axis.DispatchSpawnedWorkspaces},
					{"harnessNativeControls", axis.HarnessNativeControls},
				} {
					if fact.value == nil {
						t.Errorf("harness %q subagent fact %q missing from machine artifact", h.Name, fact.suffix)
						continue
					}
					want := "| " + string(h.Name) + " | " + axis.Axis + "." + fact.suffix + " | " + string(fact.value.Level) + " | " + strings.Join(fact.value.Evidence, ", ") + " |"
					if !strings.Contains(page, want) {
						t.Errorf("support page omits machine row %q", want)
					}
				}
				continue
			}
			want := "| " + string(h.Name) + " | " + axis.Axis + " | " + string(axis.Level) + " | " + strings.Join(axis.Evidence, ", ") + " |"
			if !strings.Contains(page, want) {
				t.Errorf("support page omits machine row %q", want)
			}
		}
	}
}
