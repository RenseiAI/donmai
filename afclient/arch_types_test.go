package afclient

// arch_types_test.go — type-shape stability tests and mock round-trip tests for
// the architecture-aware types added in REN-1333.
//
// Two categories:
//   1. JSON round-trip tests: marshal + unmarshal and verify field names + values
//      do not silently change between builds. These act as the "golden shape" guard
//      described in the acceptance criteria.
//   2. Mock round-trip tests: call each new MockClient method, verify non-nil /
//      non-empty return, and round-trip the result through JSON to confirm
//      serialisation hygiene.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ── MachineStats ─────────────────────────────────────────────────────────────

func TestMachineStatsJSONRoundTrip(t *testing.T) {
	in := MachineStats{
		ID:             "mac-studio-office",
		Region:         "home-network",
		Status:         DaemonReady,
		Version:        "0.8.60",
		ActiveSessions: 3,
		Capacity: MachineCapacity{
			MaxConcurrentSessions: 8,
			MaxVCpuPerSession:     4,
			MaxMemoryMbPerSession: 8192,
			ReservedVCpu:          4,
			ReservedMemoryMb:      16384,
		},
		UptimeSeconds: 7860,
		LastSeenAt:    time.Now().Format(time.RFC3339),
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out MachineStats
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ID != in.ID {
		t.Errorf("ID: got %q, want %q", out.ID, in.ID)
	}
	if out.Status != in.Status {
		t.Errorf("Status: got %q, want %q", out.Status, in.Status)
	}
	if out.Capacity.MaxConcurrentSessions != in.Capacity.MaxConcurrentSessions {
		t.Errorf("Capacity.MaxConcurrentSessions: got %d, want %d",
			out.Capacity.MaxConcurrentSessions, in.Capacity.MaxConcurrentSessions)
	}
	// Field-name stability checks — ensure camelCase JSON tags are present.
	for _, field := range []string{"id", "region", "status", "version", "activeSessions", "capacity", "uptimeSeconds", "lastSeenAt"} {
		if !strings.Contains(string(data), `"`+field+`"`) {
			t.Errorf("marshalled output missing field %q: %s", field, data)
		}
	}
	for _, field := range []string{"maxConcurrentSessions", "maxVCpuPerSession", "maxMemoryMbPerSession", "reservedVCpu", "reservedMemoryMb"} {
		if !strings.Contains(string(data), `"`+field+`"`) {
			t.Errorf("marshalled output missing capacity field %q: %s", field, data)
		}
	}
}

func TestDaemonStatusConstants(t *testing.T) {
	cases := []DaemonStatus{DaemonReady, DaemonDraining, DaemonPaused, DaemonStopped, DaemonUpdating}
	for _, s := range cases {
		if string(s) == "" {
			t.Errorf("empty DaemonStatus constant")
		}
	}
}

// ── ProviderCost ─────────────────────────────────────────────────────────────

func TestProviderCostJSONRoundTrip(t *testing.T) {
	in := ProviderCost{Provider: "anthropic", CostUsd: 24.31, Sessions: 7}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out ProviderCost
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Provider != in.Provider || out.CostUsd != in.CostUsd || out.Sessions != in.Sessions {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in)
	}
	for _, field := range []string{"provider", "costUsd", "sessions"} {
		if !strings.Contains(string(data), `"`+field+`"`) {
			t.Errorf("missing field %q: %s", field, data)
		}
	}
}

// ── StatsResponseV2 ───────────────────────────────────────────────────────────

func TestStatsResponseV2EmbedBase(t *testing.T) {
	// Verify that StatsResponseV2 embeds StatsResponse and serialises the
	// base fields under their original keys.
	in := StatsResponseV2{
		StatsResponse: StatsResponse{
			WorkersOnline: 3,
			AgentsWorking: 5,
			QueueDepth:    2,
			Timestamp:     "2026-04-27T00:00:00Z",
		},
		Machines: []MachineStats{
			{ID: "m1", Status: DaemonReady},
		},
		Providers: []ProviderCost{
			{Provider: "e2b", CostUsd: 1.50, Sessions: 1},
		},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out StatsResponseV2
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.WorkersOnline != 3 {
		t.Errorf("WorkersOnline: got %d, want 3", out.WorkersOnline)
	}
	if len(out.Machines) != 1 {
		t.Fatalf("Machines len: got %d, want 1", len(out.Machines))
	}
	if out.Machines[0].ID != "m1" {
		t.Errorf("Machines[0].ID: got %q, want m1", out.Machines[0].ID)
	}
	if len(out.Providers) != 1 {
		t.Fatalf("Providers len: got %d, want 1", len(out.Providers))
	}
	if out.Providers[0].Provider != "e2b" {
		t.Errorf("Providers[0].Provider: got %q, want e2b", out.Providers[0].Provider)
	}
	// Field-name stability
	for _, field := range []string{"workersOnline", "machines", "providers"} {
		if !strings.Contains(string(data), `"`+field+`"`) {
			t.Errorf("missing field %q: %s", field, data)
		}
	}
}

func TestStatsResponseV2OmitsEmptySlices(t *testing.T) {
	// When Machines and Providers are nil/empty, they should be omitted from JSON
	// (omitempty tags) so the wire format stays backward-compatible with the v1 shape.
	in := StatsResponseV2{StatsResponse: StatsResponse{WorkersOnline: 1}}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), `"machines"`) {
		t.Errorf("empty Machines should be omitted: %s", data)
	}
	if strings.Contains(string(data), `"providers"`) {
		t.Errorf("empty Providers should be omitted: %s", data)
	}
}

// ── WorkareaPoolMember + WorkareaPoolStats ─────────────────────────────────────

func TestWorkareaPoolMemberJSONRoundTrip(t *testing.T) {
	in := WorkareaPoolMember{
		ID:                 "pool-001",
		Repository:         "github.com/renseiai/agentfactory",
		Ref:                "main",
		ToolchainKey:       "node-20",
		Status:             PoolMemberReady,
		CleanStateChecksum: "sha256:abc123",
		AcquiredBy:         "",
		CreatedAt:          "2026-04-27T00:00:00Z",
		DiskUsageMb:        1240,
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out WorkareaPoolMember
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ID != in.ID || out.Status != in.Status || out.DiskUsageMb != in.DiskUsageMb {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in)
	}
	// AcquiredBy should be omitted when empty.
	if strings.Contains(string(data), `"acquiredBy"`) {
		t.Errorf("acquiredBy should be omitted when empty: %s", data)
	}
	for _, field := range []string{"id", "repository", "ref", "toolchainKey", "status", "cleanStateChecksum", "createdAt", "diskUsageMb"} {
		if !strings.Contains(string(data), `"`+field+`"`) {
			t.Errorf("missing field %q: %s", field, data)
		}
	}
}

func TestWorkareaPoolMemberStatusConstants(t *testing.T) {
	cases := []WorkareaPoolMemberStatus{
		PoolMemberWarming, PoolMemberReady, PoolMemberAcquired,
		PoolMemberReleasing, PoolMemberInvalid, PoolMemberRetired,
	}
	for _, s := range cases {
		if string(s) == "" {
			t.Errorf("empty WorkareaPoolMemberStatus constant")
		}
	}
}

func TestWorkareaPoolStatsJSONRoundTrip(t *testing.T) {
	in := WorkareaPoolStats{
		Members:          []WorkareaPoolMember{{ID: "p1", Status: PoolMemberReady}},
		TotalMembers:     1,
		ReadyMembers:     1,
		AcquiredMembers:  0,
		WarmingMembers:   0,
		InvalidMembers:   0,
		TotalDiskUsageMb: 500,
		Timestamp:        "2026-04-27T00:00:00Z",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out WorkareaPoolStats
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.TotalMembers != in.TotalMembers || out.ReadyMembers != in.ReadyMembers {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in)
	}
	for _, field := range []string{"totalMembers", "readyMembers", "acquiredMembers", "warmingMembers", "invalidMembers", "totalDiskUsageMb", "timestamp"} {
		if !strings.Contains(string(data), `"`+field+`"`) {
			t.Errorf("missing field %q: %s", field, data)
		}
	}
}

// ── SandboxProviderStats ──────────────────────────────────────────────────────

func TestSandboxProviderStatsJSONRoundTrip(t *testing.T) {
	in := SandboxProviderStats{
		ID:                  "e2b",
		DisplayName:         "E2B Cloud Sandbox",
		TransportModel:      TransportDialIn,
		BillingModel:        BillingWallClock,
		ProvisionedActive:   2,
		ProvisionedPaused:   4,
		MaxConcurrent:       -1,
		Regions:             []string{"us-east-1", "eu-west-1"},
		SupportsPauseResume: true,
		SupportsFsSnapshot:  true,
		IsA2ARemote:         false,
		Healthy:             true,
		CapturedAt:          "2026-04-27T00:00:00Z",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out SandboxProviderStats
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ID != in.ID || out.TransportModel != in.TransportModel || out.BillingModel != in.BillingModel {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in)
	}
	if len(out.Regions) != 2 {
		t.Errorf("Regions: got %v, want 2 elements", out.Regions)
	}
	for _, field := range []string{
		"id", "displayName", "transportModel", "billingModel", "provisionedActive",
		"provisionedPaused", "maxConcurrent", "regions", "supportsPauseResume",
		"supportsFsSnapshot", "isA2ARemote", "healthy", "capturedAt",
	} {
		if !strings.Contains(string(data), `"`+field+`"`) {
			t.Errorf("missing field %q: %s", field, data)
		}
	}
}

func TestSandboxTransportModelConstants(t *testing.T) {
	cases := []SandboxTransportModel{TransportDialIn, TransportDialOut, TransportEither}
	for _, c := range cases {
		if string(c) == "" {
			t.Errorf("empty SandboxTransportModel constant")
		}
	}
}

func TestSandboxBillingModelConstants(t *testing.T) {
	cases := []SandboxBillingModel{BillingWallClock, BillingActiveCPU, BillingInvocation, BillingFixed}
	for _, c := range cases {
		if string(c) == "" {
			t.Errorf("empty SandboxBillingModel constant")
		}
	}
}

// ── KitDetection + KitContribution ───────────────────────────────────────────

func TestKitDetectionJSONRoundTrip(t *testing.T) {
	in := KitDetection{
		KitID:      "ts/nextjs",
		KitVersion: "1.2.0",
		Applies:    true,
		Confidence: 0.97,
		Reason:     "Detected next.config.ts",
		ToolchainDemand: map[string]string{
			"node": ">=20",
		},
		DetectPhase: "declarative",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out KitDetection
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.KitID != in.KitID || out.Confidence != in.Confidence || !out.Applies {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in)
	}
	if out.ToolchainDemand["node"] != ">=20" {
		t.Errorf("ToolchainDemand[node]: got %q, want >=20", out.ToolchainDemand["node"])
	}
	for _, field := range []string{"kitId", "kitVersion", "applies", "confidence", "reason", "toolchainDemand", "detectPhase"} {
		if !strings.Contains(string(data), `"`+field+`"`) {
			t.Errorf("missing field %q: %s", field, data)
		}
	}
}

func TestKitDetectionOmitsEmptyOptionals(t *testing.T) {
	// When Reason, ToolchainDemand, and DetectPhase are zero, they should be omitted.
	in := KitDetection{KitID: "ts/nextjs", Applies: false, Confidence: 0.0}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{"reason", "toolchainDemand", "detectPhase"} {
		if strings.Contains(string(data), `"`+field+`"`) {
			t.Errorf("field %q should be omitted when zero: %s", field, data)
		}
	}
}

func TestKitContributionJSONRoundTrip(t *testing.T) {
	in := KitContribution{
		KitID:               "ts/nextjs",
		KitVersion:          "1.2.0",
		Commands:            map[string]string{"build": "pnpm build", "test": "pnpm test"},
		PromptFragmentCount: 2,
		ToolPermissionCount: 3,
		MCPServerNames:      []string{"nextjs-context"},
		SkillRefs:           []string{"skills/nextjs-debugging/SKILL.md"},
		WorkareaCleanDirs:   []string{".next", ".turbo"},
		AppliedAt:           "2026-04-27T00:00:00Z",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out KitContribution
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.KitID != in.KitID || out.PromptFragmentCount != in.PromptFragmentCount {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in)
	}
	if out.Commands["build"] != "pnpm build" {
		t.Errorf("Commands[build]: got %q, want pnpm build", out.Commands["build"])
	}
	for _, field := range []string{
		"kitId", "kitVersion", "commands", "promptFragmentCount",
		"toolPermissionCount", "mcpServerNames", "skillRefs", "workareaCleanDirs", "appliedAt",
	} {
		if !strings.Contains(string(data), `"`+field+`"`) {
			t.Errorf("missing field %q: %s", field, data)
		}
	}
}

// ── Attestation + AuditChainEntry ─────────────────────────────────────────────

func TestAttestationJSONRoundTrip(t *testing.T) {
	in := Attestation{
		KeyAlgorithm:   AttestationEd25519,
		KeyFingerprint: "abc1234…d4f2",
		Signature:      "base64sig==",
		SignedAt:       "2026-04-27T00:00:00Z",
		Verified:       true,
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Attestation
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.KeyAlgorithm != in.KeyAlgorithm || out.Verified != in.Verified {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in)
	}
	for _, field := range []string{"keyAlgorithm", "keyFingerprint", "signature", "signedAt", "verified"} {
		if !strings.Contains(string(data), `"`+field+`"`) {
			t.Errorf("missing field %q: %s", field, data)
		}
	}
}

func TestAttestationKeyAlgorithmConstants(t *testing.T) {
	cases := []AttestationKeyAlgorithm{AttestationEd25519, AttestationECDSA}
	for _, c := range cases {
		if string(c) == "" {
			t.Errorf("empty AttestationKeyAlgorithm constant")
		}
	}
}

func TestAuditChainEntryJSONRoundTrip(t *testing.T) {
	attest := &Attestation{
		KeyAlgorithm:   AttestationEd25519,
		KeyFingerprint: "abc1234…d4f2",
		Signature:      "base64sig==",
		SignedAt:       "2026-04-27T00:00:00Z",
		Verified:       true,
	}
	in := AuditChainEntry{
		Sequence:     1,
		EventKind:    "session-accepted",
		SessionID:    "sess-1",
		ActorID:      "orchestrator",
		Payload:      map[string]any{"project": "renseiai/agentfactory"},
		PreviousHash: "0000000000000000000000000000000000000000000000000000000000000000",
		EntryHash:    "a1b2c3d4",
		Attestation:  attest,
		OccurredAt:   "2026-04-27T00:00:00Z",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out AuditChainEntry
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Sequence != in.Sequence || out.EventKind != in.EventKind || out.SessionID != in.SessionID {
		t.Errorf("round-trip mismatch: got %+v, want %+v", out, in)
	}
	if out.Attestation == nil {
		t.Fatal("Attestation should not be nil after round-trip")
	}
	if out.Attestation.Verified != true {
		t.Errorf("Attestation.Verified: got %v, want true", out.Attestation.Verified)
	}
	for _, field := range []string{"sequence", "eventKind", "sessionId", "actorId", "payload", "previousHash", "entryHash", "attestation", "occurredAt"} {
		if !strings.Contains(string(data), `"`+field+`"`) {
			t.Errorf("missing field %q: %s", field, data)
		}
	}
}

func TestAuditChainEntryAttestationOmittedWhenNil(t *testing.T) {
	in := AuditChainEntry{
		Sequence:     2,
		EventKind:    "workarea-acquired",
		SessionID:    "sess-1",
		ActorID:      "daemon",
		PreviousHash: "abc",
		EntryHash:    "def",
		OccurredAt:   "2026-04-27T00:00:00Z",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), `"attestation"`) {
		t.Errorf("attestation should be omitted when nil: %s", data)
	}
}

// ── Mock round-trip tests ─────────────────────────────────────────────────────

func TestMockClientGetStatsV2(t *testing.T) {
	m := NewMockClient()
	resp, err := m.GetStatsV2()
	if err != nil {
		t.Fatalf("GetStatsV2: %v", err)
	}
	if resp == nil {
		t.Fatal("response is nil")
	}
	// Base fields should be populated (delegated to GetStats).
	if resp.WorkersOnline == 0 && resp.AgentsWorking == 0 {
		t.Error("expected base StatsResponse fields to be non-zero")
	}
	if len(resp.Machines) == 0 {
		t.Error("expected at least one MachineStats entry")
	}
	if len(resp.Providers) == 0 {
		t.Error("expected at least one ProviderCost entry")
	}
	// JSON round-trip
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out StatsResponseV2
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Machines) != len(resp.Machines) {
		t.Errorf("Machines count: got %d, want %d", len(out.Machines), len(resp.Machines))
	}
}

func TestMockClientGetMachineStats(t *testing.T) {
	m := NewMockClient()
	machines, err := m.GetMachineStats()
	if err != nil {
		t.Fatalf("GetMachineStats: %v", err)
	}
	if len(machines) == 0 {
		t.Fatal("expected at least one machine")
	}
	for _, machine := range machines {
		if machine.ID == "" {
			t.Error("machine ID should not be empty")
		}
		if machine.Status == "" {
			t.Error("machine Status should not be empty")
		}
		if machine.Capacity.MaxConcurrentSessions == 0 {
			t.Error("machine MaxConcurrentSessions should be > 0")
		}
	}
}

func TestMockClientGetWorkareaPoolStats(t *testing.T) {
	m := NewMockClient()
	// Empty machineID should return aggregate.
	resp, err := m.GetWorkareaPoolStats("")
	if err != nil {
		t.Fatalf("GetWorkareaPoolStats: %v", err)
	}
	if resp == nil {
		t.Fatal("response is nil")
	}
	if resp.TotalMembers == 0 {
		t.Error("TotalMembers should be > 0")
	}
	if len(resp.Members) != resp.TotalMembers {
		t.Errorf("Members len %d != TotalMembers %d", len(resp.Members), resp.TotalMembers)
	}
	// Verify the total disk usage is consistent with member-level data.
	var totalDisk int64
	for _, mem := range resp.Members {
		totalDisk += mem.DiskUsageMb
	}
	if totalDisk != resp.TotalDiskUsageMb {
		t.Errorf("TotalDiskUsageMb %d != sum of member disk usage %d", resp.TotalDiskUsageMb, totalDisk)
	}
	// Machine-scoped call should return the same mock data.
	resp2, err := m.GetWorkareaPoolStats("mac-studio-marks-office")
	if err != nil {
		t.Fatalf("GetWorkareaPoolStats(machine): %v", err)
	}
	if len(resp2.Members) != len(resp.Members) {
		t.Errorf("machine-scoped pool len: got %d, want %d", len(resp2.Members), len(resp.Members))
	}
	// JSON round-trip
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out WorkareaPoolStats
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.TotalMembers != resp.TotalMembers {
		t.Errorf("TotalMembers after round-trip: got %d, want %d", out.TotalMembers, resp.TotalMembers)
	}
}

func TestMockClientGetSandboxProviderStats(t *testing.T) {
	m := NewMockClient()
	providers, err := m.GetSandboxProviderStats()
	if err != nil {
		t.Fatalf("GetSandboxProviderStats: %v", err)
	}
	if len(providers) == 0 {
		t.Fatal("expected at least one provider")
	}
	for _, p := range providers {
		if p.ID == "" {
			t.Error("provider ID should not be empty")
		}
		if p.TransportModel == "" {
			t.Error("provider TransportModel should not be empty")
		}
		if p.BillingModel == "" {
			t.Error("provider BillingModel should not be empty")
		}
	}
	// JSON round-trip
	data, err := json.Marshal(providers)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out []SandboxProviderStats
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out) != len(providers) {
		t.Errorf("provider count after round-trip: got %d, want %d", len(out), len(providers))
	}
}

func TestMockClientGetKitDetections(t *testing.T) {
	m := NewMockClient()
	kits, err := m.GetKitDetections("mock-001")
	if err != nil {
		t.Fatalf("GetKitDetections: %v", err)
	}
	if len(kits) == 0 {
		t.Fatal("expected at least one detection result")
	}
	// At least one should be positive.
	anyApplies := false
	for _, k := range kits {
		if k.Applies {
			anyApplies = true
		}
		if k.KitID == "" {
			t.Error("KitID should not be empty")
		}
	}
	if !anyApplies {
		t.Error("expected at least one kit detection with Applies=true")
	}
}

func TestMockClientGetKitContributions(t *testing.T) {
	m := NewMockClient()
	contribs, err := m.GetKitContributions("mock-001")
	if err != nil {
		t.Fatalf("GetKitContributions: %v", err)
	}
	if len(contribs) == 0 {
		t.Fatal("expected at least one contribution")
	}
	for _, c := range contribs {
		if c.KitID == "" {
			t.Error("KitID should not be empty")
		}
		if c.AppliedAt == "" {
			t.Error("AppliedAt should not be empty")
		}
	}
}

func TestMockClientGetAuditChain(t *testing.T) {
	m := NewMockClient()
	chain, err := m.GetAuditChain("mock-001")
	if err != nil {
		t.Fatalf("GetAuditChain: %v", err)
	}
	if len(chain) == 0 {
		t.Fatal("expected at least one audit chain entry")
	}
	// Verify monotonic sequence.
	for i, entry := range chain {
		want := uint64(i + 1)
		if entry.Sequence != want {
			t.Errorf("entry[%d].Sequence = %d, want %d", i, entry.Sequence, want)
		}
		if entry.EventKind == "" {
			t.Error("EventKind should not be empty")
		}
		if entry.EntryHash == "" {
			t.Error("EntryHash should not be empty")
		}
	}
	// Verify hash chaining: each entry's PreviousHash equals the prior entry's EntryHash.
	for i := 1; i < len(chain); i++ {
		if chain[i].PreviousHash != chain[i-1].EntryHash {
			t.Errorf("chain[%d].PreviousHash %q != chain[%d].EntryHash %q",
				i, chain[i].PreviousHash, i-1, chain[i-1].EntryHash)
		}
	}
	// JSON round-trip
	data, err := json.Marshal(chain)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out []AuditChainEntry
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out) != len(chain) {
		t.Errorf("chain len after round-trip: got %d, want %d", len(out), len(chain))
	}
}

// ── Compile-time interface check ──────────────────────────────────────────────

// Ensures MockClient satisfies DataSource including the new REN-1333 methods.
// This fails at compile time if a method is missing — no runtime cost.
var _ DataSource = (*MockClient)(nil)
