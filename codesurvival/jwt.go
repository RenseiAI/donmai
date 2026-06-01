package codesurvival

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// ErrOrgClaimMismatch is returned when the resultAuth JWT's org claim does not
// match the batch item's orgId — a cross-tenant guard (isolation contract row
// "Cross-tenant work"). The handler rejects + audits and never clones.
var ErrOrgClaimMismatch = errors.New("codesurvival: jwt org claim does not match batchWork.orgId")

// ErrOrgClaimMissing is returned when the resultAuth JWT carries no org claim at
// all, or cannot be decoded. Treated as a hard reject (fail-closed): the worker
// cannot prove the work is for the claimed org.
var ErrOrgClaimMissing = errors.New("codesurvival: jwt carries no decodable org claim")

// jwtOrgClaims is the minimal claim set the worker reads from the resultAuth
// envelope. The platform mints the JWT with org_id (see
// platform/src/lib/m2m-auth.ts); orgId is accepted as an alias for forward
// compatibility.
type jwtOrgClaims struct {
	OrgID      string `json:"org_id"`
	OrgIDAlias string `json:"orgId"`
}

// verifyOrgClaim re-verifies the org claim on the resultAuth JWT against the
// expected orgId. The worker does NOT hold the platform signing secret, so this
// is an UNVERIFIED-signature claim cross-check — a cross-tenant sanity guard,
// not full authentication. The platform ingestion endpoint performs the
// cryptographic verification on the result POST (defense in depth, per the
// isolation contract: org claim re-verified by worker on consume AND by
// ingestion on result POST).
//
// Returns nil when the claim matches expectedOrgID; ErrOrgClaimMismatch on a
// mismatch; ErrOrgClaimMissing when no org claim can be decoded.
func verifyOrgClaim(resultAuth, expectedOrgID string) error {
	claimOrg, err := decodeJWTOrgClaim(resultAuth)
	if err != nil {
		return ErrOrgClaimMissing
	}
	if claimOrg == "" {
		return ErrOrgClaimMissing
	}
	if claimOrg != expectedOrgID {
		return ErrOrgClaimMismatch
	}
	return nil
}

// decodeJWTOrgClaim base64url-decodes the payload segment of a JWT and returns
// its org_id (or orgId) claim. Signature is NOT validated here (see
// verifyOrgClaim doc). Returns an error when the token is not a well-formed JWT.
func decodeJWTOrgClaim(token string) (string, error) {
	token = strings.TrimSpace(token)
	token = strings.TrimPrefix(token, "Bearer ")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("codesurvival: malformed jwt (expected 3 segments)")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Tolerate padded base64url variants.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return "", err
		}
	}
	var claims jwtOrgClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", err
	}
	if claims.OrgID != "" {
		return claims.OrgID, nil
	}
	return claims.OrgIDAlias, nil
}
