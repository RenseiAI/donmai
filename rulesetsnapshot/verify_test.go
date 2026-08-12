package rulesetsnapshot

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func mustGenerateKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return pub, priv
}

func TestParseAndVerify_Success(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerateKey(t)
	c := &Client{cfg: Config{TrustedKeys: map[string]ed25519.PublicKey{"ksk_test": pub}}}
	raw := buildSignedSnapshot(t, priv, signedSnapshotOpts{})

	snap, err := c.parseAndVerify(context.Background(), raw)
	if err != nil {
		t.Fatalf("parseAndVerify: %v", err)
	}
	if snap.OrgID != "org1" || snap.Revision != 1 {
		t.Fatalf("snapshot = %+v, want org1@1", snap)
	}
	if snap.Sections.PoolHostInventory.Pools[0].ID != "pool1" {
		t.Fatalf("decoded sections missing pool1: %+v", snap.Sections.PoolHostInventory)
	}
}

func TestParseAndVerify_RejectsHashMismatch(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerateKey(t)
	c := &Client{cfg: Config{TrustedKeys: map[string]ed25519.PublicKey{"ksk_test": pub}}}
	raw := buildSignedSnapshot(t, priv, signedSnapshotOpts{corruptHashAfterSigning: true})

	_, err := c.parseAndVerify(context.Background(), raw)
	if err == nil {
		t.Fatal("expected a hash-mismatch error, got nil")
	}
	if !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("error = %v, want ErrVerificationFailed", err)
	}
}

func TestParseAndVerify_RejectsBadSignature(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerateKey(t)
	c := &Client{cfg: Config{TrustedKeys: map[string]ed25519.PublicKey{"ksk_test": pub}}}
	raw := buildSignedSnapshot(t, priv, signedSnapshotOpts{corruptSignature: true})

	_, err := c.parseAndVerify(context.Background(), raw)
	if err == nil {
		t.Fatal("expected a bad-signature error, got nil")
	}
	if !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("error = %v, want ErrVerificationFailed", err)
	}
}

func TestParseAndVerify_RejectsWrongKey(t *testing.T) {
	t.Parallel()
	_, priv := mustGenerateKey(t)
	otherPub, _ := mustGenerateKey(t)
	c := &Client{cfg: Config{TrustedKeys: map[string]ed25519.PublicKey{"ksk_test": otherPub}}}
	raw := buildSignedSnapshot(t, priv, signedSnapshotOpts{})

	_, err := c.parseAndVerify(context.Background(), raw)
	if err == nil {
		t.Fatal("expected verification to fail against the wrong public key, got nil")
	}
	if !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("error = %v, want ErrVerificationFailed", err)
	}
}

func TestParseAndVerify_UnresolvedSigningKey(t *testing.T) {
	t.Parallel()
	_, priv := mustGenerateKey(t)
	c := &Client{cfg: Config{TrustedKeys: map[string]ed25519.PublicKey{"some-other-key": {}}}}
	raw := buildSignedSnapshot(t, priv, signedSnapshotOpts{signingKeyID: "ksk_unknown"})

	_, err := c.parseAndVerify(context.Background(), raw)
	if !errors.Is(err, ErrKeyUnresolved) {
		t.Fatalf("error = %v, want ErrKeyUnresolved", err)
	}
}

func TestParseAndVerify_RejectsNonEd25519Algorithm(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerateKey(t)
	c := &Client{cfg: Config{TrustedKeys: map[string]ed25519.PublicKey{"ksk_test": pub}}}
	raw := buildSignedSnapshot(t, priv, signedSnapshotOpts{})

	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	wire["algorithm"] = "rsa-pss"
	tampered, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.parseAndVerify(context.Background(), tampered)
	if !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("error = %v, want ErrVerificationFailed for a non-ed25519 algorithm", err)
	}
}

// TestResolvePublicKey_JWKS proves the JWKS resolution path end to end: a
// key absent from TrustedKeys resolves via a live (httptest) JWKS endpoint,
// including the base64url decode of the OKP "x" member.
func TestResolvePublicKey_JWKS(t *testing.T) {
	t.Parallel()
	pub, priv := mustGenerateKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{
				{
					"kid": "ksk_jwks",
					"kty": "OKP",
					"crv": "Ed25519",
					"alg": "EdDSA",
					"use": "sig",
					"x":   base64.RawURLEncoding.EncodeToString(pub),
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	c := &Client{cfg: Config{JWKSURL: srv.URL}}
	raw := buildSignedSnapshot(t, priv, signedSnapshotOpts{signingKeyID: "ksk_jwks"})

	snap, err := c.parseAndVerify(context.Background(), raw)
	if err != nil {
		t.Fatalf("parseAndVerify via JWKS: %v", err)
	}
	if snap.SigningKeyID != "ksk_jwks" {
		t.Fatalf("SigningKeyID = %q, want ksk_jwks", snap.SigningKeyID)
	}
}

func TestResolvePublicKey_JWKSKeyNotFound(t *testing.T) {
	t.Parallel()
	_, priv := mustGenerateKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{}})
	}))
	t.Cleanup(srv.Close)

	c := &Client{cfg: Config{JWKSURL: srv.URL}}
	raw := buildSignedSnapshot(t, priv, signedSnapshotOpts{signingKeyID: "ksk_missing"})

	_, err := c.parseAndVerify(context.Background(), raw)
	if !errors.Is(err, ErrKeyUnresolved) {
		t.Fatalf("error = %v, want ErrKeyUnresolved", err)
	}
}
