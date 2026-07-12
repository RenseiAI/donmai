package attachclient

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// hostClaims is the subset of the § 15 frozen claim set the client reads from
// the UNVERIFIED JWT payload. Signature verification is the relay's job (§ 1);
// the client only needs these fields to build the subscribe echo (sessionId,
// epoch) and to drive the degraded-lane endpoints. A malformed/absent field
// decodes to its zero value — the relay is authoritative and rejects a bad
// token, so the client parses leniently and never trusts these values for
// security.
type hostClaims struct {
	SessionID string `json:"sessionId"`
	RoomID    string `json:"roomId"`
	Epoch     int64  `json:"epoch"`
	Exp       int64  `json:"exp"`
	Aud       string `json:"aud"`
	Role      string `json:"role"`
	Jti       string `json:"jti"`

	// hasEpoch records whether the payload carried an epoch claim at all (0 is a
	// valid epoch, so a bare Epoch == 0 is ambiguous without this).
	hasEpoch bool
}

// parseHostClaims decodes the middle (payload) segment of a compact JWS JWT
// without verifying the signature. It does NOT validate the claim values — that
// is the relay's role — it only extracts what the host leg needs.
func parseHostClaims(token string) (hostClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return hostClaims{}, fmt.Errorf("attachclient: malformed JWT: want 3 dot-separated segments, got %d", len(parts))
	}
	raw, err := decodeJWTSegment(parts[1])
	if err != nil {
		return hostClaims{}, fmt.Errorf("attachclient: decoding JWT payload segment: %w", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return hostClaims{}, fmt.Errorf("attachclient: JWT payload is not a JSON object: %w", err)
	}
	var c hostClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return hostClaims{}, fmt.Errorf("attachclient: decoding JWT claims: %w", err)
	}
	_, c.hasEpoch = probe["epoch"]
	return c, nil
}

// decodeJWTSegment base64url-decodes a JWT segment, tolerating both the
// canonical unpadded form and an accidentally-padded one.
func decodeJWTSegment(seg string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(seg, "="))
}
