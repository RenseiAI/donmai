package codeintelhost

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// defaultMaxIssuedAtSkew bounds how far into the future a token's iat claim
// may sit before it is rejected as not-yet-valid. Clocks between the token
// issuer and this host are never perfectly synchronized; a small forward
// skew is tolerated, but a materially future iat (per the design's "issued-
// at may not be materially in the future" rule) is refused.
const defaultMaxIssuedAtSkew = 5 * time.Minute

// Claims is the decoded, verified payload of a code-intel-host bearer JWT.
// Field names mirror the short claim names on the wire (org/proj/repo/
// rev_kind/rev), NOT the body Binding's camelCase field names — Handler
// compares the two explicitly via MatchesRequest.
type Claims struct {
	Subject  string
	Issuer   string
	Audience []string
	IssuedAt time.Time
	Expires  time.Time

	Org     string
	Proj    string
	Repo    string
	RevKind string
	Rev     string
}

// MatchesRequest reports whether c authorizes exactly the given invocation
// id and body binding. Both the subject/invocationId equality and the
// per-field claim/binding equality are required by the design's
// authentication contract; a mismatch on either is ErrBindingMismatch.
func (c Claims) MatchesRequest(invocationID string, binding Binding) error {
	if c.Subject != invocationID {
		return fmt.Errorf("%w: token subject does not match invocationId", ErrBindingMismatch)
	}
	if c.Org != binding.OrgID ||
		c.Proj != binding.ProjectID ||
		c.Repo != binding.RepositoryPathID ||
		c.RevKind != string(binding.RevisionKind) ||
		c.Rev != binding.Revision {
		return fmt.Errorf("%w: token claims do not match request binding", ErrBindingMismatch)
	}
	return nil
}

// VerifierConfig configures a Verifier. Secret, Issuer, and Audience are all
// required: this package never assumes a closed-platform identity, so the
// caller (afcli/code_host.go) must supply the deployment's configured
// values explicitly.
type VerifierConfig struct {
	// Secret is the shared HS256 signing secret. Never logged.
	Secret string
	Issuer string
	// Audience is the single audience value this host accepts. A token's aud
	// claim may be a JSON string or array; it must contain this value.
	Audience string

	// Now, when non-nil, replaces time.Now for expiry/issued-at checks
	// (tests only). Nil uses the real clock.
	Now func() time.Time
	// MaxIssuedAtSkew, when non-zero, overrides defaultMaxIssuedAtSkew.
	MaxIssuedAtSkew time.Duration
}

// Verifier is a stdlib-only HS256 bearer-JWT verifier. It intentionally
// supports nothing beyond the fixed contract this host requires: exactly
// one algorithm, one issuer, one audience, and the six required/compared
// claims. No third-party JWT library is used or required.
type Verifier struct {
	secret          []byte
	issuer          string
	audience        string
	now             func() time.Time
	maxIssuedAtSkew time.Duration
}

// NewVerifier validates cfg and constructs a Verifier. It fails closed: an
// empty secret, issuer, or audience is a configuration error, never a
// silently-permissive default.
func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	if cfg.Secret == "" {
		return nil, errors.New("code intel host: jwt verifier requires a non-empty secret")
	}
	if cfg.Issuer == "" {
		return nil, errors.New("code intel host: jwt verifier requires a configured issuer")
	}
	if cfg.Audience == "" {
		return nil, errors.New("code intel host: jwt verifier requires a configured audience")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	skew := cfg.MaxIssuedAtSkew
	if skew <= 0 {
		skew = defaultMaxIssuedAtSkew
	}
	return &Verifier{
		secret:          []byte(cfg.Secret),
		issuer:          cfg.Issuer,
		audience:        cfg.Audience,
		now:             now,
		maxIssuedAtSkew: skew,
	}, nil
}

// jwtHeader is the subset of the JOSE header this verifier inspects. Any
// other header field is ignored.
type jwtHeader struct {
	Alg string `json:"alg"`
}

// jwtPayload is the wire shape of the token body. Aud is decoded via
// decodeAudience because RFC 7519 permits it as either a string or an array.
type jwtPayload struct {
	Sub     string          `json:"sub"`
	Iss     string          `json:"iss"`
	Aud     json.RawMessage `json:"aud"`
	Exp     *int64          `json:"exp"`
	Iat     *int64          `json:"iat"`
	Org     string          `json:"org"`
	Proj    string          `json:"proj"`
	Repo    string          `json:"repo"`
	RevKind string          `json:"rev_kind"`
	Rev     string          `json:"rev"`
}

// Verify checks token's structure, signature, and claims and returns the
// decoded Claims on success. Every failure is wrapped in ErrUnauthorized so
// callers can classify it (HTTP 401) without inspecting error text. Secret
// material is never included in any returned error.
func (v *Verifier) Verify(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, fmt.Errorf("%w: malformed token", ErrUnauthorized)
	}
	headerB64, payloadB64, sigB64 := parts[0], parts[1], parts[2]

	headerRaw, err := base64.RawURLEncoding.DecodeString(headerB64)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: malformed header encoding", ErrUnauthorized)
	}
	var header jwtHeader
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		return Claims{}, fmt.Errorf("%w: malformed header", ErrUnauthorized)
	}
	if header.Alg != "HS256" {
		return Claims{}, fmt.Errorf("%w: unsupported alg", ErrUnauthorized)
	}

	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: malformed signature encoding", ErrUnauthorized)
	}
	mac := hmac.New(sha256.New, v.secret)
	mac.Write([]byte(headerB64 + "." + payloadB64))
	expected := mac.Sum(nil)
	if !hmac.Equal(expected, sig) {
		return Claims{}, fmt.Errorf("%w: bad signature", ErrUnauthorized)
	}

	payloadRaw, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: malformed payload encoding", ErrUnauthorized)
	}
	var payload jwtPayload
	if err := json.Unmarshal(payloadRaw, &payload); err != nil {
		return Claims{}, fmt.Errorf("%w: malformed payload", ErrUnauthorized)
	}

	if payload.Iss != v.issuer {
		return Claims{}, fmt.Errorf("%w: issuer mismatch", ErrUnauthorized)
	}
	aud, err := decodeAudience(payload.Aud)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %s", ErrUnauthorized, err)
	}
	if !containsString(aud, v.audience) {
		return Claims{}, fmt.Errorf("%w: audience mismatch", ErrUnauthorized)
	}
	if payload.Exp == nil {
		return Claims{}, fmt.Errorf("%w: exp claim is required", ErrUnauthorized)
	}
	now := v.now()
	expires := time.Unix(*payload.Exp, 0)
	if !now.Before(expires) {
		return Claims{}, fmt.Errorf("%w: token expired", ErrUnauthorized)
	}
	var issuedAt time.Time
	if payload.Iat != nil {
		issuedAt = time.Unix(*payload.Iat, 0)
		if issuedAt.After(now.Add(v.maxIssuedAtSkew)) {
			return Claims{}, fmt.Errorf("%w: iat is materially in the future", ErrUnauthorized)
		}
	}
	if payload.Sub == "" {
		return Claims{}, fmt.Errorf("%w: sub claim is required", ErrUnauthorized)
	}

	return Claims{
		Subject:  payload.Sub,
		Issuer:   payload.Iss,
		Audience: aud,
		IssuedAt: issuedAt,
		Expires:  expires,
		Org:      payload.Org,
		Proj:     payload.Proj,
		Repo:     payload.Repo,
		RevKind:  payload.RevKind,
		Rev:      payload.Rev,
	}, nil
}

// decodeAudience accepts the RFC 7519 string-or-array aud encoding.
func decodeAudience(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}, nil
	}
	var multi []string
	if err := json.Unmarshal(raw, &multi); err == nil {
		return multi, nil
	}
	return nil, errors.New("aud claim must be a string or an array of strings")
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
