package codeintelhost

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// fullObjectID returns a deterministic, syntactically valid 40-character
// hexadecimal object id derived from label — for tests that need a
// Binding.Revision to pass Validate's full-object-id check but don't care
// about its exact value. Distinct labels always produce distinct ids.
func fullObjectID(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])[:40]
}

func validBinding() Binding {
	return Binding{
		OrgID:            "org-1",
		ProjectID:        "proj-1",
		RepositoryPathID: "repo-1",
		RevisionKind:     RevisionResolvedRef,
		Revision:         fullObjectID("valid-binding"),
	}
}

func TestBindingValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(b Binding) Binding
		wantErr bool
	}{
		{"valid resolved-ref", func(b Binding) Binding { return b }, false},
		{"valid session-checkout", func(b Binding) Binding { b.RevisionKind = RevisionSessionCheckout; return b }, false},
		{"valid 64-hex revision", func(b Binding) Binding { b.Revision = strings.Repeat("a", 64); return b }, false},
		{"missing org", func(b Binding) Binding { b.OrgID = ""; return b }, true},
		{"missing project", func(b Binding) Binding { b.ProjectID = ""; return b }, true},
		{"missing repository", func(b Binding) Binding { b.RepositoryPathID = ""; return b }, true},
		{"missing revision", func(b Binding) Binding { b.Revision = ""; return b }, true},
		{"revision HEAD rejected", func(b Binding) Binding { b.Revision = "HEAD"; return b }, true},
		{"revision branch name rejected", func(b Binding) Binding { b.Revision = "main"; return b }, true},
		{"revision short hex rejected", func(b Binding) Binding { b.Revision = "deadbeef"; return b }, true},
		{"revision non-hex rejected", func(b Binding) Binding { b.Revision = strings.Repeat("z", 40); return b }, true},
		{"unknown revision kind", func(b Binding) Binding { b.RevisionKind = "branch-head"; return b }, true},
		{"empty revision kind", func(b Binding) Binding { b.RevisionKind = ""; return b }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.mutate(validBinding()).Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestBindingEqual(t *testing.T) {
	t.Parallel()
	a := validBinding()
	b := validBinding()
	if !a.Equal(b) {
		t.Error("identical bindings should be Equal")
	}
	b.Revision = "other-revision"
	if a.Equal(b) {
		t.Error("bindings differing only in revision should not be Equal")
	}
}

func TestBindingKeyDistinguishesEveryField(t *testing.T) {
	t.Parallel()
	base := validBinding()
	variants := []Binding{
		base,
		func() Binding { b := base; b.OrgID = "org-2"; return b }(),
		func() Binding { b := base; b.ProjectID = "proj-2"; return b }(),
		func() Binding { b := base; b.RepositoryPathID = "repo-2"; return b }(),
		func() Binding { b := base; b.RevisionKind = RevisionSessionCheckout; return b }(),
		func() Binding { b := base; b.Revision = "cafebabe"; return b }(),
	}
	seen := make(map[string]bool, len(variants))
	for i, v := range variants {
		k := v.Key()
		if seen[k] {
			t.Errorf("variant %d produced a Key() collision: %q", i, k)
		}
		seen[k] = true
	}
}

func TestBindingFingerprintStableAndFilesystemSafe(t *testing.T) {
	t.Parallel()
	b := validBinding()
	fp1 := b.Fingerprint()
	fp2 := b.Fingerprint()
	if fp1 != fp2 {
		t.Errorf("Fingerprint() not stable: %q != %q", fp1, fp2)
	}
	if len(fp1) != 64 {
		t.Errorf("Fingerprint() length = %d, want 64 (sha256 hex)", len(fp1))
	}
	for _, r := range fp1 {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("Fingerprint() contains non-hex rune %q", r)
		}
	}
}
