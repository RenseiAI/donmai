package token

import "testing"

func TestMint_UniqueAndNonEmpty(t *testing.T) {
	seen := map[Token]bool{}
	for i := 0; i < 100; i++ {
		tok, err := Mint()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if tok == "" {
			t.Fatal("minted empty token")
		}
		if seen[tok] {
			t.Fatalf("duplicate token minted: %q", tok)
		}
		seen[tok] = true
	}
}

func TestEqual_ConstantTimeMatch(t *testing.T) {
	a, _ := Mint()
	if !Equal(a, a) {
		t.Error("token should equal itself")
	}
	b, _ := Mint()
	if Equal(a, b) {
		t.Error("distinct tokens should not be equal")
	}
	// Different lengths must not match.
	if Equal("short", "longer-token-value") {
		t.Error("different-length tokens should not be equal")
	}
}

func TestFromBearer(t *testing.T) {
	cases := map[string]Token{
		"Bearer abc123": "abc123",
		"bearer abc123": "abc123",
		"BEARER xyz":    "xyz",
		"rawtoken":      "rawtoken", // schemeless api key
		"":              "",
	}
	for header, want := range cases {
		if got := FromBearer(header); got != want {
			t.Errorf("FromBearer(%q) = %q, want %q", header, got, want)
		}
	}
}
