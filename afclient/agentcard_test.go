package afclient

import (
	"bytes"
	"encoding/json"
	"testing"
)

// ── AgentCard JSON round-trip tests (H — workType lane) ──────────────────────

// TestAgentCardRoundTrip_AllFields marshals and unmarshals an AgentCard with
// every field populated (including optional pointer fields) and verifies no
// data loss.
func TestAgentCardRoundTrip_AllFields(t *testing.T) {
	scopeOwner := "org_test_01"
	srcLocator := "backlog-writer@2"
	reconciledAt := "2026-05-12T10:00:00Z"
	ttl := 3600
	sig := "ed25519:abc1234"
	sigKey := "key-fingerprint-01"
	quality := 0.92
	profileID := "mp_claude_sonnet"
	published := "2026-05-12T09:00:00Z"
	pref0 := 0
	pref1 := 1
	partialOrder := 0

	in := AgentCard{
		ID:               "ag_test_01",
		MetadataID:       "backlog-writer",
		Name:             "Backlog Writer",
		Description:      "Drafts Linear issues from research findings.",
		Version:          3,
		Scope:            AgentCardScopeOrg,
		ScopeOwnerID:     &scopeOwner,
		SourceProviderID: "db:internal",
		SourceLocator:    &srcLocator,
		LastReconciledAt: &reconciledAt,
		ReconcileTTLSec:  &ttl,
		Partials: []PartialRef{
			{
				ID:         "par_code_guidelines",
				MetadataID: "code-review-guidelines",
				Scope:      AgentCardScopeSystem,
				Order:      &partialOrder,
			},
		},
		Capabilities: map[string]bool{
			"linearWrite": true,
			"gitWrite":    false,
		},
		Runtimes: []RuntimePath{
			{
				Kind:       RuntimeKindNative,
				Config:     map[string]any{"providerId": "claude", "modelProfileId": "mp_claude_sonnet"},
				Preference: &pref0,
			},
			{
				Kind:       RuntimeKindNPM,
				Config:     map[string]any{"package": "@rensei/backlog-agent", "entrypoint": "main"},
				Preference: &pref1,
			},
		},
		Auth: []AuthRequirement{
			{Kind: "oauth", OAuthProvider: ptr("linear")},
			{Kind: "api-key", EnvVar: ptr("LINEAR_API_KEY")},
		},
		Requires: []SubstrateRequirement{
			{
				Kind:   "network-egress",
				Config: map[string]any{"hosts": []any{"api.linear.app"}},
			},
			{
				Kind:   "workarea",
				Config: map[string]any{"mode": "persistent", "sizeMB": 1024},
			},
		},
		Trust: TrustClaims{
			Tier:        "partner",
			Signature:   &sig,
			SigningKeyID: &sigKey,
			Provenance: TrustProvenanceInfo{
				SourceURL:    ptr("https://github.com/renseiai/agents/tree/main/backlog-writer"),
				SourceCommit: ptr("abc123def456"),
				ImportedBy:   "operator@rensei.ai",
				ImportedAt:   "2026-05-01T00:00:00Z",
			},
			EvaluatedQualityScore: &quality,
		},
		WorkType:       WorkTypeBacklogWriting,
		Tools:          AgentCardToolSurface{Allow: []string{"Read", "Write"}, Disallow: []string{"Bash"}},
		ModelProfileID: &profileID,
		PublishedAt:    &published,
		Tags:           []string{"backlog", "linear"},
	}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out AgentCard
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Verify required string fields.
	if out.ID != in.ID {
		t.Errorf("ID: got %q, want %q", out.ID, in.ID)
	}
	if out.MetadataID != in.MetadataID {
		t.Errorf("MetadataID: got %q, want %q", out.MetadataID, in.MetadataID)
	}
	if out.WorkType != in.WorkType {
		t.Errorf("WorkType: got %q, want %q", out.WorkType, in.WorkType)
	}
	if out.Version != in.Version {
		t.Errorf("Version: got %d, want %d", out.Version, in.Version)
	}
	if out.Scope != in.Scope {
		t.Errorf("Scope: got %q, want %q", out.Scope, in.Scope)
	}

	// Verify optional pointer fields survive the round-trip.
	if out.ScopeOwnerID == nil || *out.ScopeOwnerID != scopeOwner {
		t.Errorf("ScopeOwnerID: got %v, want %q", out.ScopeOwnerID, scopeOwner)
	}
	if out.ReconcileTTLSec == nil || *out.ReconcileTTLSec != ttl {
		t.Errorf("ReconcileTTLSec: got %v, want %d", out.ReconcileTTLSec, ttl)
	}
	if out.Trust.Signature == nil || *out.Trust.Signature != sig {
		t.Errorf("Trust.Signature: got %v, want %q", out.Trust.Signature, sig)
	}
	if out.Trust.EvaluatedQualityScore == nil || *out.Trust.EvaluatedQualityScore != quality {
		t.Errorf("Trust.EvaluatedQualityScore: got %v, want %f", out.Trust.EvaluatedQualityScore, quality)
	}
	if out.ModelProfileID == nil || *out.ModelProfileID != profileID {
		t.Errorf("ModelProfileID: got %v, want %q", out.ModelProfileID, profileID)
	}

	// Verify slice lengths.
	if len(out.Runtimes) != 2 {
		t.Errorf("Runtimes len: got %d, want 2", len(out.Runtimes))
	}
	if len(out.Auth) != 2 {
		t.Errorf("Auth len: got %d, want 2", len(out.Auth))
	}
	if len(out.Requires) != 2 {
		t.Errorf("Requires len: got %d, want 2", len(out.Requires))
	}
	if len(out.Partials) != 1 {
		t.Errorf("Partials len: got %d, want 1", len(out.Partials))
	}

	// Verify runtime[0] kind and preference.
	if out.Runtimes[0].Kind != RuntimeKindNative {
		t.Errorf("Runtimes[0].Kind: got %q, want %q", out.Runtimes[0].Kind, RuntimeKindNative)
	}
	if out.Runtimes[0].Preference == nil || *out.Runtimes[0].Preference != 0 {
		t.Errorf("Runtimes[0].Preference: got %v, want 0", out.Runtimes[0].Preference)
	}

	// Verify capabilities map survives.
	if !out.Capabilities["linearWrite"] {
		t.Errorf("Capabilities[linearWrite]: got false, want true")
	}
	if out.Capabilities["gitWrite"] {
		t.Errorf("Capabilities[gitWrite]: got true, want false")
	}

	// Verify key JSON field names appear in the wire encoding.
	for _, key := range []string{
		"id", "metadataId", "workType", "scope", "scopeOwnerId",
		"sourceProviderId", "runtimes", "auth", "requires", "trust",
		"capabilities", "partials", "tools",
	} {
		if !bytes.Contains(data, []byte(`"`+key+`"`)) {
			t.Errorf("marshalled JSON missing field %q", key)
		}
	}
}

// TestAgentCardRoundTrip_MinimalRequired verifies that an AgentCard with only
// the required fields set marshals and unmarshals without errors, and that
// optional omitempty fields are absent from the wire encoding.
func TestAgentCardRoundTrip_MinimalRequired(t *testing.T) {
	in := AgentCard{
		ID:               "ag_minimal",
		MetadataID:       "minimal-agent",
		Name:             "Minimal",
		Description:      "Minimal agent for wire-format testing.",
		Version:          1,
		Scope:            AgentCardScopeSystem,
		ScopeOwnerID:     nil,
		SourceProviderID: "db:internal",
		Runtimes: []RuntimePath{
			{Kind: RuntimeKindNative, Config: map[string]any{"providerId": "claude"}},
		},
		Auth:     []AuthRequirement{{Kind: "none"}},
		Requires: []SubstrateRequirement{},
		Trust: TrustClaims{
			Tier: "system",
			Provenance: TrustProvenanceInfo{
				ImportedBy: "platform",
				ImportedAt: "2026-05-12T00:00:00Z",
			},
		},
		WorkType: WorkTypeResearch,
		Tools:    AgentCardToolSurface{Allow: []string{}, Disallow: []string{}},
	}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out AgentCard
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if out.ID != in.ID {
		t.Errorf("ID round-trip: got %q, want %q", out.ID, in.ID)
	}
	if out.WorkType != WorkTypeResearch {
		t.Errorf("WorkType: got %q, want %q", out.WorkType, WorkTypeResearch)
	}

	// Optional omitempty fields must be absent.
	for _, key := range []string{"partials", "capabilities", "modelProfileId", "publishedAt", "deprecatedAt", "tags"} {
		if bytes.Contains(data, []byte(`"`+key+`"`)) {
			t.Errorf("optional field %q should be omitted when zero/nil, but found in: %s", key, data)
		}
	}

	// scopeOwnerId MUST be present (not omitempty) and null when nil.
	if !bytes.Contains(data, []byte(`"scopeOwnerId":null`)) {
		t.Errorf("scopeOwnerId should be present as null for system scope, got: %s", data)
	}
}

// TestAgentCardRoundTrip_WorkTypeValues verifies that all system-seeded
// workType constants survive a JSON round-trip without mutation.
func TestAgentCardRoundTrip_WorkTypeValues(t *testing.T) {
	types := []AgentWorkType{
		WorkTypeResearch,
		WorkTypeBacklogWriting,
		WorkTypeDevelopment,
		WorkTypeCoordination,
		WorkTypeQA,
		WorkTypeAcceptance,
		WorkTypeOther,
	}

	for _, wt := range types {
		wt := wt
		t.Run(string(wt), func(t *testing.T) {
			in := AgentCard{
				ID:               "ag_wt_test",
				MetadataID:       "wt-test",
				Name:             "WorkType Test",
				Description:      "testing workType=" + wt,
				Version:          1,
				Scope:            AgentCardScopeSystem,
				ScopeOwnerID:     nil,
				SourceProviderID: "db:internal",
				Runtimes:         []RuntimePath{{Kind: RuntimeKindNative, Config: map[string]any{"providerId": "claude"}}},
				Auth:             []AuthRequirement{{Kind: "none"}},
				Requires:         []SubstrateRequirement{},
				Trust:            TrustClaims{Tier: "system", Provenance: TrustProvenanceInfo{ImportedBy: "test", ImportedAt: "2026-05-12T00:00:00Z"}},
				WorkType:         wt,
				Tools:            AgentCardToolSurface{},
			}

			data, err := json.Marshal(in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var out AgentCard
			if err := json.Unmarshal(data, &out); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if out.WorkType != wt {
				t.Errorf("WorkType round-trip: got %q, want %q", out.WorkType, wt)
			}
		})
	}
}

// TestRuntimePathRoundTrip verifies that all eight RuntimeKind constants
// survive a round-trip through RuntimePath.Config (which is map[string]any).
func TestRuntimePathRoundTrip(t *testing.T) {
	paths := []RuntimePath{
		{Kind: RuntimeKindNative, Config: map[string]any{"providerId": "claude"}},
		{Kind: RuntimeKindNPM, Config: map[string]any{"package": "@elevenlabs/elevenlabs-js"}},
		{Kind: RuntimeKindPythonPip, Config: map[string]any{"package": "elevenlabs", "entrypoint": "ElevenLabs"}},
		{Kind: RuntimeKindHTTP, Config: map[string]any{"method": "POST", "url": "https://api.example.com/run"}},
		{Kind: RuntimeKindMCPServer, Config: map[string]any{"transport": "stdio", "command": "node", "args": []any{"server.js"}}},
		{Kind: RuntimeKindA2AProtocol, Config: map[string]any{"endpoint": "https://peer.example.com/api/a2a", "skillsAdvertised": []any{"research"}}},
		{Kind: RuntimeKindVendorHosted, Config: map[string]any{"vendor": "openai-assistant", "vendorAgentId": "asst_abc123", "sdk": "openai-ts"}},
		{Kind: RuntimeKindLangchainRunnable, Config: map[string]any{"hubRef": "owner/prompt", "sdk": "langchain-py"}},
	}

	for _, p := range paths {
		p := p
		t.Run(p.Kind, func(t *testing.T) {
			data, err := json.Marshal(p)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var out RuntimePath
			if err := json.Unmarshal(data, &out); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if out.Kind != p.Kind {
				t.Errorf("Kind: got %q, want %q", out.Kind, p.Kind)
			}
			if len(out.Config) != len(p.Config) {
				t.Errorf("Config len: got %d, want %d", len(out.Config), len(p.Config))
			}
		})
	}
}

// TestTrustClaimsRoundTrip verifies TrustClaims including the nested
// TrustProvenanceInfo struct.
func TestTrustClaimsRoundTrip(t *testing.T) {
	src := "https://github.com/renseiai/agents"
	commit := "deadbeef"
	sig := "ed25519:fakesig"
	key := "key-fp"
	score := 0.85
	in := TrustClaims{
		Tier:        "partner",
		Signature:   &sig,
		SigningKeyID: &key,
		Provenance: TrustProvenanceInfo{
			SourceURL:    &src,
			SourceCommit: &commit,
			ImportedBy:   "ops@rensei.ai",
			ImportedAt:   "2026-05-12T00:00:00Z",
		},
		EvaluatedQualityScore: &score,
	}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out TrustClaims
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Tier != in.Tier {
		t.Errorf("Tier: got %q, want %q", out.Tier, in.Tier)
	}
	if out.Signature == nil || *out.Signature != sig {
		t.Errorf("Signature: got %v, want %q", out.Signature, sig)
	}
	if out.EvaluatedQualityScore == nil || *out.EvaluatedQualityScore != score {
		t.Errorf("EvaluatedQualityScore: got %v, want %f", out.EvaluatedQualityScore, score)
	}
	if out.Provenance.ImportedBy != in.Provenance.ImportedBy {
		t.Errorf("Provenance.ImportedBy: got %q, want %q", out.Provenance.ImportedBy, in.Provenance.ImportedBy)
	}
	if out.Provenance.SourceURL == nil || *out.Provenance.SourceURL != src {
		t.Errorf("Provenance.SourceURL: got %v, want %q", out.Provenance.SourceURL, src)
	}
}

// TestMockListAgents verifies MockClient.ListAgents returns cards and that
// the returned cards contain expected fields.
func TestMockListAgents(t *testing.T) {
	m := NewMockClient()
	cards, err := m.ListAgents(AgentScopeQuery{})
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(cards) == 0 {
		t.Fatal("expected at least one AgentCard, got 0")
	}
	for i, c := range cards {
		if c.ID == "" {
			t.Errorf("cards[%d].ID is empty", i)
		}
		if c.WorkType == "" {
			t.Errorf("cards[%d].WorkType is empty (D21 violation)", i)
		}
		if len(c.Runtimes) == 0 {
			t.Errorf("cards[%d].Runtimes is empty (≥1 required by ontology)", i)
		}
		if c.Trust.Tier == "" {
			t.Errorf("cards[%d].Trust.Tier is empty", i)
		}
	}
}

// TestMockGetAgent_Found verifies GetAgent returns the correct card for a
// known ID.
func TestMockGetAgent_Found(t *testing.T) {
	m := NewMockClient()
	card, err := m.GetAgent("ag_mock_research")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if card.MetadataID != "research-agent" {
		t.Errorf("MetadataID: got %q, want %q", card.MetadataID, "research-agent")
	}
	if card.WorkType != WorkTypeResearch {
		t.Errorf("WorkType: got %q, want %q", card.WorkType, WorkTypeResearch)
	}
}

// TestMockGetAgent_NotFound verifies GetAgent returns ErrNotFound for an
// unknown ID.
func TestMockGetAgent_NotFound(t *testing.T) {
	m := NewMockClient()
	_, err := m.GetAgent("ag_does_not_exist")
	if err == nil {
		t.Fatal("expected error for unknown ID, got nil")
	}
}

// TestAgentListResponseRoundTrip verifies the list response wrapper marshals
// and unmarshals cleanly including embedded AgentCard slice.
func TestAgentListResponseRoundTrip(t *testing.T) {
	in := AgentListResponse{
		Agents: []AgentCard{
			{
				ID:               "ag_test",
				MetadataID:       "test",
				Name:             "Test",
				Description:      "test agent",
				Version:          1,
				Scope:            AgentCardScopeSystem,
				ScopeOwnerID:     nil,
				SourceProviderID: "db:internal",
				Runtimes:         []RuntimePath{{Kind: RuntimeKindNative, Config: map[string]any{"providerId": "claude"}}},
				Auth:             []AuthRequirement{{Kind: "none"}},
				Requires:         []SubstrateRequirement{},
				Trust:            TrustClaims{Tier: "system", Provenance: TrustProvenanceInfo{ImportedBy: "test", ImportedAt: "2026-05-12T00:00:00Z"}},
				WorkType:         WorkTypeDevelopment,
				Tools:            AgentCardToolSurface{},
			},
		},
		Count:     1,
		Timestamp: "2026-05-12T00:00:00Z",
	}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out AgentListResponse
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Count != 1 {
		t.Errorf("Count: got %d, want 1", out.Count)
	}
	if len(out.Agents) != 1 {
		t.Fatalf("Agents len: got %d, want 1", len(out.Agents))
	}
	if out.Agents[0].WorkType != WorkTypeDevelopment {
		t.Errorf("Agents[0].WorkType: got %q, want %q", out.Agents[0].WorkType, WorkTypeDevelopment)
	}
}
