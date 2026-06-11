// Package daemon auto_update_verifier_test.go — sigstore bundle-mode
// auto-update verifier tests. Hermetic signing uses sigstore-go's
// pkg/testing/ca VirtualSigstore (same harness as kit_trust_test.go):
// it implements root.TrustedMaterial directly, so tests inject it via
// newSigstoreBinaryVerifierWithMaterial without touching the embedded
// production trust root.
package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"
)

const (
	updateSignerSAN    = "releases@example.com"
	updateSignerIssuer = "https://issuer.example"
)

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newHermeticBinaryVerifier(t *testing.T, signers []UpdateSigner) (*sigstoreBinaryVerifier, *ca.VirtualSigstore) {
	t.Helper()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("NewVirtualSigstore: %v", err)
	}
	v, err := newSigstoreBinaryVerifierWithMaterial(signers, vs)
	if err != nil {
		t.Fatalf("newSigstoreBinaryVerifierWithMaterial: %v", err)
	}
	return v, vs
}

func TestNewAutoUpdateVerifier_NoSigners_FailsClosed(t *testing.T) {
	v := newAutoUpdateVerifier(AutoUpdateConfig{})
	if _, ok := v.(failClosedVerifier); !ok {
		t.Fatalf("expected failClosedVerifier, got %T", v)
	}
	valid, reason := v.Verify(context.Background(), hexSHA256([]byte("bin")), "anything")
	if valid {
		t.Fatal("expected fail-closed Verify=false with no signers configured")
	}
	if !strings.Contains(reason, "no update signers configured") {
		t.Errorf("reason = %q, want no-signers explanation", reason)
	}
}

func TestNewAutoUpdateVerifier_MalformedSigner_FailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		signer UpdateSigner
	}{
		{"missing issuer", UpdateSigner{SAN: updateSignerSAN}},
		{"missing san", UpdateSigner{Issuer: updateSignerIssuer}},
		{"invalid san regex", UpdateSigner{SANRegex: "([", Issuer: updateSignerIssuer}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := newAutoUpdateVerifier(AutoUpdateConfig{Signers: []UpdateSigner{c.signer}})
			valid, reason := v.Verify(context.Background(), hexSHA256([]byte("bin")), "anything")
			if valid {
				t.Fatal("expected fail-closed Verify=false for malformed signer")
			}
			if !strings.Contains(reason, "sigstore verifier init failed") {
				t.Errorf("reason = %q, want init-failed explanation", reason)
			}
		})
	}
}

func TestNewAutoUpdateVerifier_SignersConfigured_BuildsSigstoreVerifier(t *testing.T) {
	v := newAutoUpdateVerifier(AutoUpdateConfig{
		Signers: []UpdateSigner{{SAN: updateSignerSAN, Issuer: updateSignerIssuer}},
	})
	sv, ok := v.(*sigstoreBinaryVerifier)
	if !ok {
		t.Fatalf("expected *sigstoreBinaryVerifier, got %T", v)
	}
	if sv.rootSource != "embedded" {
		t.Errorf("rootSource = %q, want embedded", sv.rootSource)
	}
}

func TestNewSigstoreBinaryVerifier_TrustRootPathOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trusted_root.json")
	if err := os.WriteFile(path, embeddedTrustRoot, 0o600); err != nil {
		t.Fatalf("write trust root: %v", err)
	}
	v, err := newSigstoreBinaryVerifier(AutoUpdateConfig{
		Signers:       []UpdateSigner{{SAN: updateSignerSAN, Issuer: updateSignerIssuer}},
		TrustRootPath: path,
	})
	if err != nil {
		t.Fatalf("newSigstoreBinaryVerifier: %v", err)
	}
	if v.rootSource != path {
		t.Errorf("rootSource = %q, want %q", v.rootSource, path)
	}
}

func TestNewSigstoreBinaryVerifier_TrustRootPathUnreadable(t *testing.T) {
	_, err := newSigstoreBinaryVerifier(AutoUpdateConfig{
		Signers:       []UpdateSigner{{SAN: updateSignerSAN, Issuer: updateSignerIssuer}},
		TrustRootPath: filepath.Join(t.TempDir(), "missing.json"),
	})
	if err == nil {
		t.Fatal("expected error for missing trust root file")
	}
	if !strings.Contains(err.Error(), "read trust root") {
		t.Errorf("err = %v, want read-trust-root explanation", err)
	}
}

func TestSigstoreBinaryVerifier_VerifiesSignedBinary(t *testing.T) {
	v, vs := newHermeticBinaryVerifier(t, []UpdateSigner{{SAN: updateSignerSAN, Issuer: updateSignerIssuer}})
	binary := []byte("fake-binary-content-v2")

	entity, err := vs.Sign(updateSignerSAN, updateSignerIssuer, binary)
	if err != nil {
		t.Fatalf("vs.Sign: %v", err)
	}

	valid, reason := v.verifyEntity(entity, hexSHA256(binary))
	if !valid {
		t.Fatalf("verifyEntity: want valid, got invalid (reason=%q)", reason)
	}
	if !strings.Contains(reason, updateSignerSAN) {
		t.Errorf("reason = %q, want signer SAN included", reason)
	}
}

func TestSigstoreBinaryVerifier_VerifiesViaSANRegex(t *testing.T) {
	v, vs := newHermeticBinaryVerifier(t, []UpdateSigner{{
		SANRegex: `^releases@example\.com$`,
		Issuer:   updateSignerIssuer,
	}})
	binary := []byte("fake-binary-content-v2")

	entity, err := vs.Sign(updateSignerSAN, updateSignerIssuer, binary)
	if err != nil {
		t.Fatalf("vs.Sign: %v", err)
	}

	valid, reason := v.verifyEntity(entity, hexSHA256(binary))
	if !valid {
		t.Fatalf("verifyEntity: want valid via sanRegex, got invalid (reason=%q)", reason)
	}
}

func TestSigstoreBinaryVerifier_RejectsUnknownSigner(t *testing.T) {
	v, vs := newHermeticBinaryVerifier(t, []UpdateSigner{{SAN: "someone-else@example.com", Issuer: updateSignerIssuer}})
	binary := []byte("fake-binary-content-v2")

	entity, err := vs.Sign(updateSignerSAN, updateSignerIssuer, binary)
	if err != nil {
		t.Fatalf("vs.Sign: %v", err)
	}

	valid, reason := v.verifyEntity(entity, hexSHA256(binary))
	if valid {
		t.Fatal("want invalid: signer is not on the allowlist")
	}
	if !strings.Contains(reason, "sigstore verify") {
		t.Errorf("reason = %q, want sigstore verify failure", reason)
	}
}

func TestSigstoreBinaryVerifier_RejectsTamperedBinary(t *testing.T) {
	v, vs := newHermeticBinaryVerifier(t, []UpdateSigner{{SAN: updateSignerSAN, Issuer: updateSignerIssuer}})

	entity, err := vs.Sign(updateSignerSAN, updateSignerIssuer, []byte("signed-content"))
	if err != nil {
		t.Fatalf("vs.Sign: %v", err)
	}

	// Verify against the hash of DIFFERENT content — the digest in the
	// policy will not match the digest the bundle attests to.
	valid, reason := v.verifyEntity(entity, hexSHA256([]byte("tampered-content")))
	if valid {
		t.Fatal("want invalid for tampered binary")
	}
	if reason == "" {
		t.Error("want explanation of failure, got empty reason")
	}
}

func TestSigstoreBinaryVerifier_RejectsMalformedHash(t *testing.T) {
	v, _ := newHermeticBinaryVerifier(t, []UpdateSigner{{SAN: updateSignerSAN, Issuer: updateSignerIssuer}})
	for _, hash := range []string{"", "zz", "abcd"} {
		valid, reason := v.verifyEntity(nil, hash)
		if valid {
			t.Fatalf("want invalid for malformed hash %q", hash)
		}
		if !strings.Contains(reason, "malformed binary sha256") {
			t.Errorf("hash %q: reason = %q, want malformed-hash explanation", hash, reason)
		}
	}
}

func TestSigstoreBinaryVerifier_RejectsGarbageBundle(t *testing.T) {
	v, _ := newHermeticBinaryVerifier(t, []UpdateSigner{{SAN: updateSignerSAN, Issuer: updateSignerIssuer}})
	valid, reason := v.Verify(context.Background(), hexSHA256([]byte("bin")), "not-a-bundle")
	if valid {
		t.Fatal("want invalid for garbage bundle JSON")
	}
	if !strings.Contains(reason, "parse sigstore bundle") {
		t.Errorf("reason = %q, want bundle-parse failure", reason)
	}
}

// TestUpdater_RunUpdate_SigstoreWired_RejectsGarbageBundle proves the
// production wiring end-to-end: with signers configured (and no test
// verifier injected), RunUpdate engages the real sigstore verifier, which
// rejects a CDN-served signature artifact that is not a bundle.
func TestUpdater_RunUpdate_SigstoreWired_RejectsGarbageBundle(t *testing.T) {
	binary := []byte("fakebinary")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/latest.json"):
			_ = json.NewEncoder(w).Encode(VersionManifest{Version: "9.9.9", SHA256: hexSHA256(binary)})
		case strings.HasSuffix(r.URL.Path, ".sigstore"):
			_, _ = w.Write([]byte("not-a-bundle"))
		default:
			_, _ = w.Write(binary)
		}
	}))
	t.Cleanup(srv.Close)
	u := NewUpdater(UpdaterOptions{
		CurrentVersion: "0.1.0",
		Config: AutoUpdateConfig{
			Channel: ChannelStable,
			Signers: []UpdateSigner{{SAN: updateSignerSAN, Issuer: updateSignerIssuer}},
		},
		CDNBase:  srv.URL,
		SkipExit: true,
	})
	res, err := u.RunUpdate(context.Background())
	if err != nil {
		t.Fatalf("RunUpdate: %v", err)
	}
	if res.Updated {
		t.Errorf("expected Updated=false, got %+v", res)
	}
	if !strings.Contains(res.Reason, "sig-rejected: parse sigstore bundle") {
		t.Errorf("Reason = %q, want sigstore bundle-parse rejection", res.Reason)
	}
}

// passVerifier accepts everything — used to cover the swap path itself.
type passVerifier struct{}

func (passVerifier) Verify(_ context.Context, _, _ string) (bool, string) { return true, "test" }

// TestUpdater_RunUpdate_SwapsWhenVerified covers the post-verification
// swap: a passing verifier lets RunUpdate replace the current binary.
func TestUpdater_RunUpdate_SwapsWhenVerified(t *testing.T) {
	newBinary := []byte("new-binary-content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/latest.json"):
			_ = json.NewEncoder(w).Encode(VersionManifest{Version: "9.9.9", SHA256: hexSHA256(newBinary)})
		case strings.HasSuffix(r.URL.Path, ".sigstore"):
			_, _ = w.Write([]byte("test-bundle"))
		default:
			_, _ = w.Write(newBinary)
		}
	}))
	t.Cleanup(srv.Close)

	current := filepath.Join(t.TempDir(), "donmai-daemon")
	if err := os.WriteFile(current, []byte("old-binary-content"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("write current binary: %v", err)
	}

	exitCode := -1
	u := NewUpdater(UpdaterOptions{
		CurrentVersion:    "0.1.0",
		CurrentBinaryPath: current,
		Config:            AutoUpdateConfig{Channel: ChannelStable},
		CDNBase:           srv.URL,
		Verifier:          passVerifier{},
		ExitFn:            func(code int) { exitCode = code },
	})
	res, err := u.RunUpdate(context.Background())
	if err != nil {
		t.Fatalf("RunUpdate: %v", err)
	}
	if !res.Updated || res.Version != "9.9.9" {
		t.Fatalf("expected applied update to 9.9.9, got %+v", res)
	}
	if exitCode != ExitCodeRestart {
		t.Errorf("ExitFn code = %d, want %d", exitCode, ExitCodeRestart)
	}
	swapped, err := os.ReadFile(current) //nolint:gosec
	if err != nil {
		t.Fatalf("read swapped binary: %v", err)
	}
	if string(swapped) != string(newBinary) {
		t.Errorf("swapped binary content = %q, want %q", swapped, newBinary)
	}
}
