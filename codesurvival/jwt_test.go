package codesurvival

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
)

func jwtWith(payload map[string]string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	pj, _ := json.Marshal(payload)
	return header + "." + base64.RawURLEncoding.EncodeToString(pj) + ".sig"
}

func TestVerifyOrgClaim(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		expected string
		wantErr  error
	}{
		{"match org_id", jwtWith(map[string]string{"org_id": "org-1"}), "org-1", nil},
		{"match orgId alias", jwtWith(map[string]string{"orgId": "org-2"}), "org-2", nil},
		{"mismatch", jwtWith(map[string]string{"org_id": "evil"}), "org-1", ErrOrgClaimMismatch},
		{"missing claim", jwtWith(map[string]string{"sub": "x"}), "org-1", ErrOrgClaimMissing},
		{"malformed token", "not-a-jwt", "org-1", ErrOrgClaimMissing},
		{"empty token", "", "org-1", ErrOrgClaimMissing},
		{"bearer prefix tolerated", "Bearer " + jwtWith(map[string]string{"org_id": "org-1"}), "org-1", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyOrgClaim(tt.token, tt.expected)
			if tt.wantErr == nil && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
