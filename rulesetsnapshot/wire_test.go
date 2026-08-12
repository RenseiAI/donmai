package rulesetsnapshot

import (
	"encoding/json"
	"testing"
)

func TestContentHash_Deterministic(t *testing.T) {
	t.Parallel()
	sections := testSections(t)
	h1, err := contentHash(sections)
	if err != nil {
		t.Fatalf("contentHash: %v", err)
	}
	h2, err := contentHash(sections)
	if err != nil {
		t.Fatalf("contentHash: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("contentHash is not deterministic: %s != %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("contentHash length = %d, want 64 (sha256 hex)", len(h1))
	}
}

func TestContentHash_ChangesWithContent(t *testing.T) {
	t.Parallel()
	base := testSections(t)
	changed := testSections(t)
	changed.PoolHostInventory = json.RawMessage(`{"pools":[],"hosts":[]}`)

	h1, err := contentHash(base)
	if err != nil {
		t.Fatalf("contentHash: %v", err)
	}
	h2, err := contentHash(changed)
	if err != nil {
		t.Fatalf("contentHash: %v", err)
	}
	if h1 == h2 {
		t.Fatal("contentHash did not change when poolHostInventory changed")
	}
}

// TestContentHash_KeyOrderIndependent is the load-bearing RFC 8785 property
// this whole verification scheme depends on: two JSON payloads that differ
// only in object key order (but are otherwise semantically identical) must
// canonicalize to the SAME bytes and therefore the SAME hash. Without this,
// a publisher and this client could disagree on a hash for byte-reasons
// that have nothing to do with content.
func TestContentHash_KeyOrderIndependent(t *testing.T) {
	t.Parallel()
	a := rawSections{
		PolicyBundle:        json.RawMessage(`{"workspaceId":"org1","policies":[]}`),
		CapacityProfiles:    json.RawMessage(`{"profiles":[],"grants":[]}`),
		PoolHostInventory:   json.RawMessage(`{"pools":[{"id":"p1","status":"active","costWeight":1}],"hosts":[]}`),
		ExecutionCellMatrix: json.RawMessage(`{"providers":[],"modelProfiles":[]}`),
		PosteriorSummary:    json.RawMessage(`{"posteriors":[]}`),
	}
	// Same pool object, keys in reverse order, and a differently-ordered
	// numeric literal presentation — canonicalization must still agree.
	b := a
	b.PoolHostInventory = json.RawMessage(`{"hosts":[],"pools":[{"costWeight":1.0,"status":"active","id":"p1"}]}`)

	h1, err := contentHash(a)
	if err != nil {
		t.Fatalf("contentHash(a): %v", err)
	}
	h2, err := contentHash(b)
	if err != nil {
		t.Fatalf("contentHash(b): %v", err)
	}
	if h1 != h2 {
		t.Fatalf("contentHash is not key-order independent: %s != %s", h1, h2)
	}
}

func TestContentHash_MissingSectionNormalizesToNull(t *testing.T) {
	t.Parallel()
	withExplicitNull := rawSections{
		PolicyBundle:        json.RawMessage(`null`),
		CapacityProfiles:    json.RawMessage(`{"profiles":[],"grants":[]}`),
		PoolHostInventory:   json.RawMessage(`{"pools":[],"hosts":[]}`),
		ExecutionCellMatrix: json.RawMessage(`{"providers":[],"modelProfiles":[]}`),
		PosteriorSummary:    json.RawMessage(`{"posteriors":[]}`),
	}
	withAbsent := withExplicitNull
	withAbsent.PolicyBundle = nil

	h1, err := contentHash(withExplicitNull)
	if err != nil {
		t.Fatalf("contentHash(explicit null): %v", err)
	}
	h2, err := contentHash(withAbsent)
	if err != nil {
		t.Fatalf("contentHash(absent): %v", err)
	}
	if h1 != h2 {
		t.Fatal("an absent section did not hash identically to an explicit null section")
	}
}

func TestDecodeTypedSections_RoundTrip(t *testing.T) {
	t.Parallel()
	sections, err := decodeTypedSections(testSections(t))
	if err != nil {
		t.Fatalf("decodeTypedSections: %v", err)
	}
	if sections.PoolHostInventory.Pools[0].ID != "pool1" {
		t.Fatalf("pool id = %q, want pool1", sections.PoolHostInventory.Pools[0].ID)
	}
	if sections.PoolHostInventory.Pools[0].Status != "active" {
		t.Fatalf("pool status = %q, want active", sections.PoolHostInventory.Pools[0].Status)
	}
	if len(sections.CapacityProfiles.Profiles) != 1 || sections.CapacityProfiles.Profiles[0].PoolIDs[0] != "pool1" {
		t.Fatalf("capacity profile did not decode pool1: %+v", sections.CapacityProfiles)
	}
	if sections.ExecutionCellMatrix.Providers[0].HarnessByAuthMode["metered"] != "harness1" {
		t.Fatalf("execution cell matrix did not decode harness1: %+v", sections.ExecutionCellMatrix)
	}
}

func TestDecodeTypedSections_UnknownFieldsIgnored(t *testing.T) {
	t.Parallel()
	sections := testSections(t)
	sections.PoolHostInventory = json.RawMessage(`{"pools":[{"id":"pool1","status":"active","somethingNew":true}],"hosts":[]}`)
	if _, err := decodeTypedSections(sections); err != nil {
		t.Fatalf("decodeTypedSections should ignore unknown fields, got error: %v", err)
	}
}
