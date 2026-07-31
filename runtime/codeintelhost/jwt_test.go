package codeintelhost

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

const (
	testSecret   = "test-secret-value-not-real"
	testIssuer   = "rensei-platform"
	testAudience = "rensei-code-intel-host"
)

// testClaims mirrors the wire claim set this host verifies. aud may be a
// plain string or []string depending on the test case.
type testClaims struct {
	Sub     string      `json:"sub,omitempty"`
	Iss     string      `json:"iss,omitempty"`
	Aud     interface{} `json:"aud,omitempty"`
	Exp     *int64      `json:"exp,omitempty"`
	Iat     *int64      `json:"iat,omitempty"`
	Org     string      `json:"org,omitempty"`
	Proj    string      `json:"proj,omitempty"`
	Repo    string      `json:"repo,omitempty"`
	RevKind string      `json:"rev_kind,omitempty"`
	Rev     string      `json:"rev,omitempty"`
}

// signToken builds a standalone HS256 JWT from alg/secret/claims, entirely
// independent of the package under test, so the tests genuinely exercise
// Verifier's parsing and validation rather than round-tripping its own
// encoder.
func signToken(t *testing.T, alg, secret string, claims testClaims) string {
	t.Helper()
	header := map[string]string{"alg": alg, "typ": "JWT"}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signingInput := headerB64 + "." + payloadB64
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	sigB64 := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signingInput + "." + sigB64
}

func ptr(v int64) *int64 { return &v }

func validClaims(now time.Time) testClaims {
	exp := now.Add(time.Hour).Unix()
	iat := now.Add(-time.Minute).Unix()
	return testClaims{
		Sub:     "invocation-1",
		Iss:     testIssuer,
		Aud:     testAudience,
		Exp:     ptr(exp),
		Iat:     ptr(iat),
		Org:     "org-1",
		Proj:    "proj-1",
		Repo:    "repo-1",
		RevKind: string(RevisionResolvedRef),
		Rev:     "deadbeef",
	}
}

func newTestVerifier(t *testing.T, now func() time.Time) *Verifier {
	t.Helper()
	v, err := NewVerifier(VerifierConfig{
		Secret:   testSecret,
		Issuer:   testIssuer,
		Audience: testAudience,
		Now:      now,
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

func TestNewVerifierRequiresConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  VerifierConfig
	}{
		{"missing secret", VerifierConfig{Issuer: testIssuer, Audience: testAudience}},
		{"missing issuer", VerifierConfig{Secret: testSecret, Audience: testAudience}},
		{"missing audience", VerifierConfig{Secret: testSecret, Issuer: testIssuer}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewVerifier(tc.cfg); err == nil {
				t.Error("NewVerifier() error = nil, want error")
			}
		})
	}
}

func TestVerifierGoodPath(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(t, func() time.Time { return now })
	token := signToken(t, "HS256", testSecret, validClaims(now))

	claims, err := v.Verify(token)
	if err != nil {
		t.Fatalf("Verify() error = %v, want nil", err)
	}
	if claims.Subject != "invocation-1" {
		t.Errorf("Subject = %q, want %q", claims.Subject, "invocation-1")
	}
	if claims.Org != "org-1" || claims.Proj != "proj-1" || claims.Repo != "repo-1" ||
		claims.RevKind != string(RevisionResolvedRef) || claims.Rev != "deadbeef" {
		t.Errorf("decoded binding claims = %+v, mismatch", claims)
	}
}

func TestVerifierAudienceArrayForm(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(t, func() time.Time { return now })
	claims := validClaims(now)
	claims.Aud = []string{"some-other-audience", testAudience}
	token := signToken(t, "HS256", testSecret, claims)

	if _, err := v.Verify(token); err != nil {
		t.Fatalf("Verify() error = %v, want nil (array aud containing configured audience)", err)
	}
}

func TestVerifierRejections(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		token func(t *testing.T) string
	}{
		{
			name: "bad signature",
			token: func(t *testing.T) string {
				return signToken(t, "HS256", "wrong-secret", validClaims(now))
			},
		},
		{
			name: "wrong alg",
			token: func(t *testing.T) string {
				return signToken(t, "HS384", testSecret, validClaims(now))
			},
		},
		{
			name: "wrong issuer",
			token: func(t *testing.T) string {
				c := validClaims(now)
				c.Iss = "someone-else"
				return signToken(t, "HS256", testSecret, c)
			},
		},
		{
			name: "wrong audience",
			token: func(t *testing.T) string {
				c := validClaims(now)
				c.Aud = "someone-elses-host"
				return signToken(t, "HS256", testSecret, c)
			},
		},
		{
			name: "expired",
			token: func(t *testing.T) string {
				c := validClaims(now)
				c.Exp = ptr(now.Add(-time.Minute).Unix())
				return signToken(t, "HS256", testSecret, c)
			},
		},
		{
			name: "missing exp",
			token: func(t *testing.T) string {
				c := validClaims(now)
				c.Exp = nil
				return signToken(t, "HS256", testSecret, c)
			},
		},
		{
			name: "iat materially in the future",
			token: func(t *testing.T) string {
				c := validClaims(now)
				c.Iat = ptr(now.Add(time.Hour).Unix())
				return signToken(t, "HS256", testSecret, c)
			},
		},
		{
			name: "missing subject",
			token: func(t *testing.T) string {
				c := validClaims(now)
				c.Sub = ""
				return signToken(t, "HS256", testSecret, c)
			},
		},
		{
			name: "malformed token",
			token: func(_ *testing.T) string {
				return "not-a-jwt"
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := newTestVerifier(t, func() time.Time { return now })
			_, err := v.Verify(tc.token(t))
			if err == nil {
				t.Fatal("Verify() error = nil, want ErrUnauthorized")
			}
			if !errors.Is(err, ErrUnauthorized) {
				t.Errorf("Verify() error = %v, want errors.Is(err, ErrUnauthorized)", err)
			}
		})
	}
}

func TestClaimsMatchesRequest(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(t, func() time.Time { return now })
	token := signToken(t, "HS256", testSecret, validClaims(now))
	claims, err := v.Verify(token)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	binding := Binding{
		OrgID:            "org-1",
		ProjectID:        "proj-1",
		RepositoryPathID: "repo-1",
		RevisionKind:     RevisionResolvedRef,
		Revision:         "deadbeef",
	}

	if err := claims.MatchesRequest("invocation-1", binding); err != nil {
		t.Errorf("MatchesRequest() error = %v, want nil", err)
	}

	t.Run("subject mismatch", func(t *testing.T) {
		t.Parallel()
		if err := claims.MatchesRequest("some-other-invocation", binding); !errors.Is(err, ErrBindingMismatch) {
			t.Errorf("MatchesRequest() error = %v, want ErrBindingMismatch", err)
		}
	})

	t.Run("body binding mismatch", func(t *testing.T) {
		t.Parallel()
		mismatched := binding
		mismatched.Revision = "some-other-revision"
		if err := claims.MatchesRequest("invocation-1", mismatched); !errors.Is(err, ErrBindingMismatch) {
			t.Errorf("MatchesRequest() error = %v, want ErrBindingMismatch", err)
		}
	})
}
