package rulesetsnapshot

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// jwksKeyCacheTTL bounds how long a fetched JWKS document is trusted before
// a re-fetch is attempted. Independent of the snapshot's own DegradedAfter/
// RefuseAfter — a stale JWKS cache only risks failing to resolve a BRAND
// NEW signing key promptly (fail-closed: an unresolved key never verifies),
// never accepting a bad one.
const jwksKeyCacheTTL = 5 * time.Minute

type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
}

type jwks struct {
	Keys []jwk `json:"keys"`
}

// resolvePublicKey resolves signingKeyID to a trusted Ed25519 public key.
// Resolution order: Config.TrustedKeys (a static, pinned map — always
// preferred, and the only option that never touches the network) then, if
// configured, Config.JWKSURL (an RFC 7517 JWKS document, cached for
// jwksKeyCacheTTL). Returns ErrKeyUnresolved wrapped with detail when
// neither resolves the key — this is a fail-closed leaf: no key ever means
// no verification, never an implicit trust.
func (c *Client) resolvePublicKey(ctx context.Context, signingKeyID string) (ed25519.PublicKey, error) {
	if key, ok := c.cfg.TrustedKeys[signingKeyID]; ok {
		return key, nil
	}
	if c.cfg.JWKSURL == "" {
		return nil, fmt.Errorf("%w: signing key %q is not in the trusted-key set and no JWKS URL is configured", ErrKeyUnresolved, signingKeyID)
	}
	set, err := c.fetchJWKS(ctx)
	if err != nil {
		return nil, err
	}
	key, ok := set[signingKeyID]
	if !ok {
		return nil, fmt.Errorf("%w: signing key %q was not found in the configured JWKS", ErrKeyUnresolved, signingKeyID)
	}
	return key, nil
}

func (c *Client) fetchJWKS(ctx context.Context) (map[string]ed25519.PublicKey, error) {
	c.jwksMu.Lock()
	defer c.jwksMu.Unlock()
	if c.jwks != nil && c.now().Sub(c.jwksFetchedAt) < jwksKeyCacheTTL {
		return c.jwks, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.JWKSURL, nil)
	if err != nil {
		return nil, fmt.Errorf("rulesetsnapshot: build JWKS request: %w", err)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("rulesetsnapshot: fetch JWKS: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("rulesetsnapshot: read JWKS response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rulesetsnapshot: JWKS endpoint returned status %d", resp.StatusCode)
	}
	var set jwks
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("rulesetsnapshot: decode JWKS: %w", err)
	}

	keys := make(map[string]ed25519.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" {
			continue // not an Ed25519 key; irrelevant to this contract
		}
		raw, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			continue // malformed entry; skip rather than fail the whole document
		}
		keys[k.Kid] = ed25519.PublicKey(raw)
	}
	c.jwks = keys
	c.jwksFetchedAt = c.now()
	return keys, nil
}

// verifySignature checks an Ed25519 signature over the raw bytes of a
// lower-case hex content hash — the exact payload the publisher's contract
// signs (the hash's hex-decoded bytes, not the hex string itself).
func verifySignature(pub ed25519.PublicKey, contentHashHex, signatureB64 string) error {
	hashBytes, err := hex.DecodeString(contentHashHex)
	if err != nil {
		return fmt.Errorf("%w: content hash is not valid hex", ErrVerificationFailed)
	}
	sig, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil {
		return fmt.Errorf("%w: signature is not valid base64", ErrVerificationFailed)
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: resolved public key has the wrong length", ErrVerificationFailed)
	}
	if !ed25519.Verify(pub, hashBytes, sig) {
		return fmt.Errorf("%w: signature does not verify", ErrVerificationFailed)
	}
	return nil
}
