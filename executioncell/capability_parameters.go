package executioncell

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
)

// DigestCapabilityParameters returns the admission-time digest of one exact
// JSON value. It is the sole canonicalizer used by both legacy adaptation and
// parameter-bound realization preparation.
func DigestCapabilityParameters(raw json.RawMessage) (string, error) {
	if err := rejectDuplicateFields(raw); err != nil {
		return "", err
	}
	canonical, err := jsoncanonicalizer.Transform(raw)
	if err != nil {
		return "", contractError(ErrorInvalidReference, nil, "canonicalize operational payload value: %v", err)
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}
