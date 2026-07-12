package attachclient

import "testing"

func TestParseHostClaims(t *testing.T) {
	t.Parallel()
	tok := mkHostToken("sess-42", 7, "jti-1", true)
	cl, err := parseHostClaims(tok)
	if err != nil {
		t.Fatalf("parseHostClaims: %v", err)
	}
	if cl.SessionID != "sess-42" {
		t.Errorf("SessionID = %q, want sess-42", cl.SessionID)
	}
	if cl.RoomID != "sess-42" {
		t.Errorf("RoomID = %q, want sess-42", cl.RoomID)
	}
	if cl.Epoch != 7 || !cl.hasEpoch {
		t.Errorf("Epoch = %d hasEpoch = %v, want 7 true", cl.Epoch, cl.hasEpoch)
	}
	if cl.Aud != "relay" {
		t.Errorf("Aud = %q, want relay", cl.Aud)
	}
	if cl.Role != "host" {
		t.Errorf("Role = %q, want host", cl.Role)
	}
}

func TestParseHostClaimsEpochAbsent(t *testing.T) {
	t.Parallel()
	// A viewer-style token carries no epoch: hasEpoch must be false even though
	// Epoch decodes to its zero value.
	tok := mkViewerToken("sess-1", "user-1", "jti-9", "viewer")
	cl, err := parseHostClaims(tok)
	if err != nil {
		t.Fatalf("parseHostClaims: %v", err)
	}
	if cl.hasEpoch {
		t.Errorf("hasEpoch = true, want false for a token without an epoch claim")
	}
	if cl.Epoch != 0 {
		t.Errorf("Epoch = %d, want 0", cl.Epoch)
	}
}

func TestParseHostClaimsMalformed(t *testing.T) {
	t.Parallel()
	for _, tok := range []string{"", "onlyonepart", "a.b", "a.b.c.d"} {
		if _, err := parseHostClaims(tok); err == nil {
			t.Errorf("parseHostClaims(%q) = nil error, want error", tok)
		}
	}
}
