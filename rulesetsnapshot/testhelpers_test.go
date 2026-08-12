package rulesetsnapshot

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

// testSections returns a minimal, valid rawSections fixture: one pool
// ("pool1", active, in provider "prov1"), one host ("host1", ready, in
// "pool1"), one capacity profile naming "pool1", and one matrix provider
// whose harnessByAuthMode names "harness1".
func testSections(t *testing.T) rawSections {
	t.Helper()
	return rawSections{
		PolicyBundle:        json.RawMessage(`{"workspaceId":"org1","policies":[]}`),
		CapacityProfiles:    json.RawMessage(`{"profiles":[{"id":"prof1","name":"default","orderingPolicy":"declared","preferenceVector":null,"reservationPosture":{"mode":"none","timeoutMs":null},"burstPosture":"off","disconnectLostAfterMs":0,"disconnectReplace":false,"disconnectReconcile":"keep_original","isOrgDefault":true,"revision":1,"poolIds":["pool1"]}],"grants":[]}`),
		PoolHostInventory:   json.RawMessage(`{"pools":[{"id":"pool1","providerId":"prov1","displayName":"Pool 1","servesPersistent":true,"servesOnDemand":true,"status":"active","costWeight":1,"priority":1,"substrateClass":"local","allowedProjectIds":null}],"hosts":[{"id":"host1","executionPoolId":"pool1","status":"ready","capabilities":[],"os":"linux","arch":"amd64","maxSessions":1,"activeSessions":0,"lastHeartbeatMs":1000}]}`),
		ExecutionCellMatrix: json.RawMessage(`{"providers":[{"id":"prov1","displayName":"Prov","configNamespace":"prov","supportedAuthModes":["metered"],"category":"cli","harnessByAuthMode":{"metered":"harness1"}}],"modelProfiles":[]}`),
		PosteriorSummary:    json.RawMessage(`{"posteriors":[]}`),
	}
}

// signedSnapshotOpts lets a test perturb the otherwise-valid fixture before
// signing (e.g. to build a snapshot that will fail a specific check).
type signedSnapshotOpts struct {
	sections     rawSections
	orgID        string
	revision     int
	signingKeyID string
	compiledAt   time.Time
	// corruptHashAfterSigning, when true, appends a byte to the emitted
	// contentHash AFTER computing the real signature — producing bytes
	// whose claimed hash does not match a fresh recomputation, while the
	// signature (over the ORIGINAL correct hash) stays well-formed base64.
	// Exercises the "hash mismatch" rejection path distinctly from a bad
	// signature.
	corruptHashAfterSigning bool
	// corruptSignature, when true, flips a byte of the real signature.
	corruptSignature bool
}

// buildSignedSnapshot builds and signs a wire snapshot response body with
// priv, applying opts. Returns the raw JSON bytes ready to hand to
// parseAndVerify or serve from an httptest server.
func buildSignedSnapshot(t *testing.T, priv ed25519.PrivateKey, opts signedSnapshotOpts) []byte {
	t.Helper()
	sections := opts.sections
	if sections.PolicyBundle == nil {
		sections = testSections(t)
	}
	orgID := opts.orgID
	if orgID == "" {
		orgID = "org1"
	}
	revision := opts.revision
	if revision == 0 {
		revision = 1
	}
	signingKeyID := opts.signingKeyID
	if signingKeyID == "" {
		signingKeyID = "ksk_test"
	}
	compiledAt := opts.compiledAt
	if compiledAt.IsZero() {
		compiledAt = time.Now().UTC()
	}

	hash, err := contentHash(sections)
	if err != nil {
		t.Fatalf("contentHash: %v", err)
	}
	hashBytes, err := hex.DecodeString(hash)
	if err != nil {
		t.Fatalf("decode hash: %v", err)
	}
	sig := ed25519.Sign(priv, hashBytes)
	if opts.corruptSignature {
		sig[0] ^= 0xFF
	}

	claimedHash := hash
	if opts.corruptHashAfterSigning {
		claimedHash = hash[:len(hash)-1] + flipHexNibble(hash[len(hash)-1])
	}

	wire := wireSnapshot{
		OrgID:          orgID,
		Revision:       revision,
		RulesetRev:     orgID + "@" + strconv.Itoa(revision),
		ContentHash:    claimedHash,
		SectionDigests: map[string]string{},
		Signature:      base64.StdEncoding.EncodeToString(sig),
		SigningKeyID:   signingKeyID,
		Algorithm:      "ed25519",
		Validators:     []string{},
		CompiledAt:     compiledAt.Format(time.RFC3339Nano),
		Sections:       sections,
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal wire snapshot: %v", err)
	}
	return raw
}

func flipHexNibble(b byte) string {
	if b == '0' {
		return "1"
	}
	return "0"
}
