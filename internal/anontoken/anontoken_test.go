package anontoken

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// overrideTokenPath temporarily replaces tokenPath with a function that
// returns the given path and restores the original on test cleanup.
// Tests using this helper must NOT call t.Parallel() because they mutate
// a package-level variable.
func overrideTokenPath(t *testing.T, path string) {
	t.Helper()
	orig := tokenPath
	tokenPath = func() string { return path }
	t.Cleanup(func() { tokenPath = orig })
}

func TestMintAndStore_FormatRegex(t *testing.T) {
	dir := t.TempDir()
	overrideTokenPath(t, filepath.Join(dir, "token"))

	token, err := MintAndStore()
	if err != nil {
		t.Fatalf("MintAndStore() error: %v", err)
	}

	re := regexp.MustCompile(`^dmk_[0-9a-f]{48}$`)
	if !re.MatchString(token) {
		t.Errorf("token %q does not match dmk_[0-9a-f]{48}", token)
	}
}

func TestMintAndStore_FileMode0600(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	overrideTokenPath(t, tokenFile)

	if _, err := MintAndStore(); err != nil {
		t.Fatalf("MintAndStore() error: %v", err)
	}

	info, err := os.Stat(tokenFile)
	if err != nil {
		t.Fatalf("os.Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %04o, want 0600", perm)
	}
}

func TestMintAndStore_DirMode0700(t *testing.T) {
	// Use a fresh subdirectory that doesn't exist yet.
	parent := t.TempDir()
	newDir := filepath.Join(parent, "statesubdir")
	tokenFile := filepath.Join(newDir, "token")
	overrideTokenPath(t, tokenFile)

	if _, err := MintAndStore(); err != nil {
		t.Fatalf("MintAndStore() error: %v", err)
	}

	info, err := os.Stat(newDir)
	if err != nil {
		t.Fatalf("os.Stat dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("state dir mode = %04o, want 0700", perm)
	}
}

func TestEnsureToken_Mints_When_Missing(t *testing.T) {
	dir := t.TempDir()
	overrideTokenPath(t, filepath.Join(dir, "token"))

	token, justMinted, err := EnsureToken()
	if err != nil {
		t.Fatalf("EnsureToken() error: %v", err)
	}
	if !justMinted {
		t.Error("justMinted should be true on first call")
	}
	re := regexp.MustCompile(`^dmk_[0-9a-f]{48}$`)
	if !re.MatchString(token) {
		t.Errorf("token %q does not match expected format", token)
	}
}

func TestEnsureToken_Idempotent(t *testing.T) {
	dir := t.TempDir()
	overrideTokenPath(t, filepath.Join(dir, "token"))

	token1, justMinted1, err := EnsureToken()
	if err != nil {
		t.Fatalf("first EnsureToken() error: %v", err)
	}
	if !justMinted1 {
		t.Error("first call should mint (justMinted=true)")
	}

	token2, justMinted2, err := EnsureToken()
	if err != nil {
		t.Fatalf("second EnsureToken() error: %v", err)
	}
	if justMinted2 {
		t.Error("second call should not mint (justMinted=false)")
	}
	if token1 != token2 {
		t.Errorf("token changed between calls: %q vs %q", token1, token2)
	}
}

func TestReadToken_MissingFile(t *testing.T) {
	dir := t.TempDir()
	overrideTokenPath(t, filepath.Join(dir, "nonexistent_token"))

	tok, err := ReadToken()
	if err != nil {
		t.Fatalf("ReadToken() on missing file returned error: %v", err)
	}
	if tok != "" {
		t.Errorf("ReadToken() = %q, want \"\"", tok)
	}
}

func TestClaimURL_Format(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		token   string
		baseURL string
		want    string
	}{
		{
			name:    "explicit base URL",
			token:   "dmk_abc123",
			baseURL: "https://donmai.dev/dashboard",
			want:    "https://donmai.dev/dashboard/claim#token=dmk_abc123",
		},
		{
			name:    "default base URL",
			token:   "dmk_xyz456",
			baseURL: "",
			want:    "https://donmai.dev/dashboard/claim#token=dmk_xyz456",
		},
		{
			name:    "custom base URL",
			token:   "dmk_test",
			baseURL: "http://localhost:3000",
			want:    "http://localhost:3000/claim#token=dmk_test",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ClaimURL(tc.token, tc.baseURL)
			if got != tc.want {
				t.Errorf("ClaimURL(%q, %q) = %q, want %q", tc.token, tc.baseURL, got, tc.want)
			}
		})
	}
}

// TestClaimURL_TokenNeverInQueryOrPath pins the security contract: the dmk_
// machine token must ride ONLY in the URL fragment. A query-string (or path)
// regression would leak the secret into server access logs, proxy logs, and
// Referer headers — fragments are never sent over the wire.
func TestClaimURL_TokenNeverInQueryOrPath(t *testing.T) {
	t.Parallel()
	const token = "dmk_deadbeef" //nolint:gosec // G101: synthetic test fixture, not a credential

	for _, baseURL := range []string{"", "https://donmai.dev/dashboard", "http://localhost:3000"} {
		got := ClaimURL(token, baseURL)

		frag := strings.Index(got, "#")
		if frag < 0 {
			t.Fatalf("ClaimURL(%q, %q) = %q: no fragment — token must be fragment-carried", token, baseURL, got)
		}
		wire := got[:frag] // the part a browser actually sends
		if strings.Contains(wire, token) {
			t.Errorf("ClaimURL(%q, %q) = %q: token appears outside the fragment", token, baseURL, got)
		}
		if !strings.Contains(got[frag:], "token="+token) {
			t.Errorf("ClaimURL(%q, %q) = %q: fragment missing token=", token, baseURL, got)
		}
	}
}
