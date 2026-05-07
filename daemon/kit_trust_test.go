// Package daemon kit_trust_test.go — verifier tests covering the
// outcomes called out in WAVE12_PHASE2_AUDIT § 1.5:
//
//   - bundle-verifies-OK        — hermetic VirtualSigstore signs the
//     manifest; verifier accepts → KitTrustSignedVerified.
//   - tampered-bundle           — sign one manifest; verify against a
//     mutated copy → KitTrustSignedUnverified.
//   - unsigned-permissive       — no sibling .sigstore → KitTrustUnsigned;
//     permissive-mode trustGateAllows == true.
//   - signed-by-allowlist-rejects-unknown — allowlist mode + unsigned
//     manifest → KitRegistry.Install returns ErrKitTrustGateRejected.
//   - trustOverride-allowed-this-once-audit-logs — override path emits
//     the structured audit-log entry.
//
// The hermetic test signer is sigstore-go's pkg/testing/ca (Q4
// resolution). VirtualSigstore implements root.TrustedMaterial directly,
// so we inject it via newKitVerifierWithMaterial without touching the
// embedded production trust root.
package daemon

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

const minimalKitTOML = `api = "rensei.dev/v1"

[kit]
id = "rensei/example"
version = "0.1.0"
name = "Rensei Example"
authorIdentity = "did:web:rensei.dev"
`

func newHermeticVerifier(t *testing.T, mode TrustMode) (*kitVerifier, *ca.VirtualSigstore) {
	t.Helper()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("NewVirtualSigstore: %v", err)
	}
	cfg := TrustConfig{Mode: mode}
	return newKitVerifierWithMaterial(cfg, vs), vs
}

func TestKitVerifier_BundleVerifiesOK(t *testing.T) {
	v, vs := newHermeticVerifier(t, TrustModePermissive)
	manifestBytes := []byte(minimalKitTOML)

	// The hermetic CA signs the manifest, returning a SignedEntity that
	// the verifier accepts because vs is also our trusted material.
	entity, err := vs.Sign("kit-publisher@rensei.dev", "https://issuer.example", manifestBytes)
	if err != nil {
		t.Fatalf("vs.Sign: %v", err)
	}

	res := v.verifyEntity("rensei/example", entity, manifestBytes)
	if res.Trust != afclient.KitTrustSignedVerified {
		t.Fatalf("Trust: want signed-verified, got %q (details=%q)", res.Trust, res.Details)
	}
	if !res.OK {
		t.Errorf("OK: want true, got false (details=%q)", res.Details)
	}
	if res.SignerID == "" {
		t.Errorf("SignerID: want populated from cert SAN, got empty")
	}
	if !strings.Contains(res.SignerID, "kit-publisher@rensei.dev") {
		t.Errorf("SignerID: want to contain SAN, got %q", res.SignerID)
	}
	if res.SignedAt == "" {
		t.Errorf("SignedAt: want RFC3339 timestamp, got empty")
	}
}

func TestKitVerifier_TamperedBundleRejected(t *testing.T) {
	v, vs := newHermeticVerifier(t, TrustModePermissive)

	signedBytes := []byte(minimalKitTOML)
	tamperedBytes := []byte(strings.Replace(minimalKitTOML, "0.1.0", "9.9.9", 1))

	entity, err := vs.Sign("kit-publisher@rensei.dev", "https://issuer.example", signedBytes)
	if err != nil {
		t.Fatalf("vs.Sign: %v", err)
	}

	// Verify against the TAMPERED manifest bytes — the digest in our
	// policy will not match the digest the bundle attests to.
	res := v.verifyEntity("rensei/example", entity, tamperedBytes)
	if res.Trust != afclient.KitTrustSignedUnverified {
		t.Fatalf("Trust: want signed-unverified for tampered manifest, got %q (details=%q)", res.Trust, res.Details)
	}
	if !res.OK {
		t.Errorf("OK: want true (verifier ran), got false")
	}
	if res.Details == "" {
		t.Errorf("Details: want explanation of failure, got empty")
	}
}

func TestKitVerifier_UnsignedManifestPermissive(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "rensei-example.kit.toml")
	if err := os.WriteFile(manifestPath, []byte(minimalKitTOML), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	v, _ := newHermeticVerifier(t, TrustModePermissive)
	res, err := v.VerifyManifest("rensei/example", manifestPath)
	if err != nil {
		t.Fatalf("VerifyManifest: %v", err)
	}
	if res.Trust != afclient.KitTrustUnsigned {
		t.Errorf("Trust: want unsigned for missing .sigstore, got %q", res.Trust)
	}
	if !res.OK {
		t.Errorf("OK: want true, got false")
	}
	if !v.trustGateAllows(res.Trust) {
		t.Errorf("permissive mode: trustGateAllows must allow unsigned, got false")
	}
}

func TestKitVerifier_TrustGateAllowsByMode(t *testing.T) {
	tests := []struct {
		name  string
		mode  TrustMode
		trust afclient.KitTrustState
		want  bool
	}{
		{"permissive-allows-unsigned", TrustModePermissive, afclient.KitTrustUnsigned, true},
		{"permissive-allows-unverified", TrustModePermissive, afclient.KitTrustSignedUnverified, true},
		{"permissive-allows-verified", TrustModePermissive, afclient.KitTrustSignedVerified, true},
		{"empty-defaults-permissive", TrustMode(""), afclient.KitTrustUnsigned, true},
		{"allowlist-rejects-unsigned", TrustModeSignedByAllowlist, afclient.KitTrustUnsigned, false},
		{"allowlist-rejects-unverified", TrustModeSignedByAllowlist, afclient.KitTrustSignedUnverified, false},
		{"allowlist-allows-verified", TrustModeSignedByAllowlist, afclient.KitTrustSignedVerified, true},
		{"attested-rejects-unsigned", TrustModeAttested, afclient.KitTrustUnsigned, false},
		{"attested-allows-verified", TrustModeAttested, afclient.KitTrustSignedVerified, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := newKitVerifierWithMaterial(TrustConfig{Mode: tc.mode}, nil)
			if got := v.trustGateAllows(tc.trust); got != tc.want {
				t.Errorf("trustGateAllows(%q) under mode %q: want %v, got %v", tc.trust, tc.mode, tc.want, got)
			}
		})
	}
}

func TestKitRegistry_InstallTrustGateRejectsUnsigned(t *testing.T) {
	// Stage a real on-disk manifest with no sibling .sigstore.
	dir := t.TempDir()
	writeManifest(t, dir, "rensei-example", minimalKitTOML)

	r := NewKitRegistryWithTrust([]string{dir}, TrustConfig{Mode: TrustModeSignedByAllowlist})

	_, err := r.Install("rensei/example", afclient.KitInstallRequest{})
	if !errors.Is(err, ErrKitTrustGateRejected) {
		t.Fatalf("Install: want ErrKitTrustGateRejected for unsigned + allowlist, got %v", err)
	}
}

func TestKitRegistry_InstallTrustGatePassesPermissive(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "rensei-example", minimalKitTOML)

	r := NewKitRegistryWithTrust([]string{dir}, TrustConfig{Mode: TrustModePermissive})

	_, err := r.Install("rensei/example", afclient.KitInstallRequest{})
	// The gate passes; Phase 4 will own the actual install body so the
	// post-gate stub still returns ErrKitInstallUnimplemented.
	if !errors.Is(err, ErrKitInstallUnimplemented) {
		t.Fatalf("Install: want ErrKitInstallUnimplemented after gate passes, got %v", err)
	}
}

func TestKitRegistry_InstallTrustOverrideAuditLogs(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "rensei-example", minimalKitTOML)

	// Capture slog output via JSON handler over an in-memory buffer.
	buf := captureSlogTrust(t)

	r := NewKitRegistryWithTrust([]string{dir}, TrustConfig{
		Mode:  TrustModeSignedByAllowlist,
		Actor: "operator@rensei.dev",
	})

	_, err := r.Install("rensei/example", afclient.KitInstallRequest{
		TrustOverride: afclient.TrustOverrideAllowedThisOnce,
	})
	// Override bypasses the gate; post-gate stub returns
	// ErrKitInstallUnimplemented (Phase 4 owns the install body).
	if !errors.Is(err, ErrKitInstallUnimplemented) {
		t.Fatalf("Install with override: want ErrKitInstallUnimplemented, got %v", err)
	}

	// Decode the audit-log line — last record in the buffer.
	var saw bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode slog line %q: %v", line, err)
		}
		if msg, _ := rec["msg"].(string); !strings.Contains(msg, "trust gate bypassed") {
			continue
		}
		saw = true
		if got := rec["kitId"]; got != "rensei/example" {
			t.Errorf("audit kitId: want rensei/example, got %v", got)
		}
		// SignerID for an unsigned manifest comes from the manifest's
		// authorIdentity backfill in Install.
		if got := rec["signerId"]; got != "did:web:rensei.dev" {
			t.Errorf("audit signerId: want did:web:rensei.dev, got %v", got)
		}
		if got := rec["actor"]; got != "operator@rensei.dev" {
			t.Errorf("audit actor: want operator@rensei.dev, got %v", got)
		}
		if got, _ := rec["at"].(string); got == "" {
			t.Errorf("audit at: want RFC3339 timestamp, got empty")
		}
	}
	if !saw {
		t.Fatalf("audit log line not emitted; buffer=%s", buf.String())
	}
}

func TestKitVerifier_ResolveActorFallback(t *testing.T) {
	v := newKitVerifierWithMaterial(TrustConfig{}, nil)
	got := v.resolveActor()
	if !strings.HasPrefix(got, "uid:") {
		t.Errorf("resolveActor with no Actor: want 'uid:N' fallback, got %q", got)
	}

	v2 := newKitVerifierWithMaterial(TrustConfig{Actor: "named-operator"}, nil)
	if got := v2.resolveActor(); got != "named-operator" {
		t.Errorf("resolveActor with Actor: want 'named-operator', got %q", got)
	}
}

func TestKitRegistry_VerifySignatureMissingBundle(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "rensei-example", minimalKitTOML)
	r := NewKitRegistryWithTrust([]string{dir}, TrustConfig{Mode: TrustModePermissive})

	res, err := r.VerifySignature("rensei/example")
	if err != nil {
		t.Fatalf("VerifySignature: %v", err)
	}
	if res.Trust != afclient.KitTrustUnsigned {
		t.Errorf("Trust: want unsigned, got %q", res.Trust)
	}
	// Backfilled from manifest authorIdentity since the bundle path
	// returned no SignerID.
	if res.SignerID != "did:web:rensei.dev" {
		t.Errorf("SignerID: want did:web:rensei.dev backfill, got %q", res.SignerID)
	}
}

// captureSlogTrust mirrors child_log_test.go's captureSlog but returns
// only the buffer (the cleanup is registered via t.Cleanup so callers
// don't have to thread a restore func). Decoupled from captureSlog so
// either test file can be edited without coordinating order.
func captureSlogTrust(t *testing.T) *strings.Builder {
	t.Helper()
	// Use a strings.Builder-backed bytes.Buffer-like adapter so we can
	// decode JSON lines from it.
	buf := &strings.Builder{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(stringsBuilderWriter{buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// stringsBuilderWriter adapts strings.Builder to io.Writer.
type stringsBuilderWriter struct{ b *strings.Builder }

func (w stringsBuilderWriter) Write(p []byte) (int, error) {
	return w.b.Write(p)
}
