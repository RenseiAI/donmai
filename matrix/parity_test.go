package matrix

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
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
// harnesses.json / endpoints.json / matrix.json.

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
	for _, name := range []string{FileCapabilityMatrix, FileHarnesses, FileEndpoints, FileMatrix, FileRegistryGen} {
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
// harness.drives AND cell.protocol == endpoint.host(cell.host).protocol. For
// "raw", harness.drives is the union of the two raw packages.
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
	if len(providers) != 8 {
		t.Fatalf("expected 8 harness providers, got %d", len(providers))
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

// TestParity_AmpCostHonesty guards the §6 cost-honesty invariant: the amp cell
// is metered + needsApiKey + NOT bringsOwnAuth (a key-needing cell cannot be
// bringsOwnAuth).
func TestParity_AmpCostHonesty(t *testing.T) {
	built, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	found := false
	for _, c := range built.Matrix.Cells {
		if c.Harness != agent.HarnessAmp {
			continue
		}
		found = true
		if c.CostModel != agent.CostMeteredPerToken {
			t.Errorf("amp cell %+v: costModel=%q, want metered", c.Key(), c.CostModel)
		}
		if !c.NeedsAPIKey {
			t.Errorf("amp cell %+v: needsApiKey=false, want true", c.Key())
		}
		if c.BringsOwnAuth {
			t.Errorf("amp cell %+v: bringsOwnAuth=true — a key-needing metered cell cannot bring its own auth", c.Key())
		}
	}
	if !found {
		t.Errorf("amp cell missing from the matrix")
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
