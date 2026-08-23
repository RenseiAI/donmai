package attachclient

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	attachwirev2 "github.com/RenseiAI/donmai/attachwire/v2"
)

type v2HostClaims struct {
	SessionID    string `json:"sessionId"`
	RoomID       string `json:"roomId"`
	Role         string `json:"role"`
	Epoch        uint64 `json:"epoch"`
	CarrierEpoch uint64 `json:"carrier_epoch"`
	Protocol     string `json:"protocol"`
	OrgID        string `json:"orgId"`
	IssuedAt     int64  `json:"iat"`
	ExpiresAt    int64  `json:"exp"`
	Audience     string `json:"aud"`
	// Secret/correlation fields are decoded only for strict shape validation and
	// never retained in this result, logs, errors, or diagnostics.
}

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func parseV2HostClaims(token string, now time.Time) (v2HostClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || strings.Contains(parts[0], "=") || strings.Contains(parts[1], "=") || strings.Contains(parts[2], "=") {
		return v2HostClaims{}, fmt.Errorf("attachclient: malformed v2 host credential")
	}
	signature, signatureErr := base64.RawURLEncoding.DecodeString(parts[2])
	if signatureErr != nil || len(signature) != 64 {
		return v2HostClaims{}, fmt.Errorf("attachclient: malformed v2 host credential signature")
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return v2HostClaims{}, fmt.Errorf("attachclient: malformed v2 host credential header")
	}
	headerFields, err := strictClaimsObject(headerRaw)
	if err != nil || len(headerFields) != 2 {
		return v2HostClaims{}, fmt.Errorf("attachclient: v2 host credential header is not exact")
	}
	var algorithm, tokenType string
	if json.Unmarshal(headerFields["alg"], &algorithm) != nil ||
		json.Unmarshal(headerFields["typ"], &tokenType) != nil ||
		algorithm != "EdDSA" || tokenType != "JWT" {
		return v2HostClaims{}, fmt.Errorf("attachclient: invalid v2 host credential header")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return v2HostClaims{}, fmt.Errorf("attachclient: malformed v2 host credential payload")
	}
	fields, err := strictClaimsObject(raw)
	if err != nil {
		return v2HostClaims{}, err
	}
	wantFields := []string{
		"sessionId", "roomId", "role", "epoch", "carrier_epoch",
		"handoff_nonce", "prepared_correlation_digest", "protocol", "orgId",
		"iat", "exp", "aud", "jti",
	}
	if len(fields) != len(wantFields) {
		return v2HostClaims{}, fmt.Errorf("attachclient: v2 host credential claim set is not exact")
	}
	for _, name := range wantFields {
		if _, ok := fields[name]; !ok {
			return v2HostClaims{}, fmt.Errorf("attachclient: v2 host credential missing claim %q", name)
		}
	}
	var claims v2HostClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return v2HostClaims{}, fmt.Errorf("attachclient: malformed v2 host credential claims")
	}
	if claims.SessionID == "" || claims.RoomID != claims.SessionID || claims.Role != "host" ||
		claims.Epoch > uint64(^uint64(0)>>1) || claims.CarrierEpoch == 0 || claims.Protocol != attachwirev2.ProtocolVersion ||
		claims.OrgID == "" || claims.Audience != "relay" || claims.IssuedAt <= 0 ||
		claims.ExpiresAt <= claims.IssuedAt || (!now.IsZero() && claims.ExpiresAt <= now.Unix()) {
		return v2HostClaims{}, fmt.Errorf("attachclient: invalid v2 host credential claims")
	}
	var nonce, digest, jti string
	if json.Unmarshal(fields["handoff_nonce"], &nonce) != nil ||
		json.Unmarshal(fields["prepared_correlation_digest"], &digest) != nil ||
		json.Unmarshal(fields["jti"], &jti) != nil {
		return v2HostClaims{}, fmt.Errorf("attachclient: invalid v2 host credential correlation claims")
	}
	decodedNonce, nonceErr := base64.RawURLEncoding.DecodeString(nonce)
	decodedDigest, digestErr := hex.DecodeString(digest)
	if nonceErr != nil || len(nonce) != 43 || len(decodedNonce) != 32 ||
		digestErr != nil || len(digest) != 64 || len(decodedDigest) != 32 || strings.ToLower(digest) != digest ||
		!canonicalUUID.MatchString(jti) {
		return v2HostClaims{}, fmt.Errorf("attachclient: invalid v2 host credential correlation claims")
	}
	return claims, nil
}

func strictClaimsObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("attachclient: v2 host credential payload is not an object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		nameToken, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("attachclient: malformed v2 host credential payload")
		}
		name, ok := nameToken.(string)
		if !ok {
			return nil, fmt.Errorf("attachclient: malformed v2 host credential payload")
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, fmt.Errorf("attachclient: duplicate v2 host credential claim %q", name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("attachclient: malformed v2 host credential claim %q", name)
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("attachclient: malformed v2 host credential payload")
	}
	if trailing, err := decoder.Token(); err != io.EOF || trailing != nil {
		return nil, fmt.Errorf("attachclient: trailing v2 host credential payload")
	}
	return fields, nil
}
