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

	"github.com/RenseiAI/donmai/afclient"
)

const minimalKitTOML = `api = "donmai.dev/v1"

[kit]
id = "rensei/example"
version = "0.1.0"
name = "Rensei Example"
authorIdentity = "did:web:example.com"
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

// newHermeticVerifierWithIssuerSet builds a hermetic verifier whose trust
// config carries the given mode AND issuer allowlist, so tests can exercise
// the SAN/issuer identity policy (not just the mode gate).
func newHermeticVerifierWithIssuerSet(t *testing.T, mode TrustMode, issuerSet []string) (*kitVerifier, *ca.VirtualSigstore) {
	t.Helper()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("NewVirtualSigstore: %v", err)
	}
	cfg := TrustConfig{Mode: mode, IssuerSet: issuerSet}
	return newKitVerifierWithMaterial(cfg, vs), vs
}

// TestDefaultVendorIssuerSet asserts the baked-in vendor allowlist contains
// exactly the official donmai-kits signing identity. If this identity ever
// changes, the founder-confirmed SAN must be updated here in lock-step with
// the donmai-kits sign workflow's OIDC subject.
func TestDefaultVendorIssuerSet(t *testing.T) {
	got := defaultVendorIssuerSet()
	if len(got) != 1 {
		t.Fatalf("defaultVendorIssuerSet: want exactly 1 entry, got %d: %v", len(got), got)
	}
	const wantSAN = "https://github.com/RenseiAI/donmai-kits/.github/workflows/sign.yml@refs/heads/main"
	if got[0] != wantSAN {
		t.Errorf("defaultVendorIssuerSet[0] (SAN): want %q, got %q", wantSAN, got[0])
	}
	if vendorSignerSAN != wantSAN {
		t.Errorf("vendorSignerSAN: want %q, got %q", wantSAN, vendorSignerSAN)
	}
	const wantIssuer = "https://token.actions.githubusercontent.com"
	if vendorSignerIssuer != wantIssuer {
		t.Errorf("vendorSignerIssuer: want %q, got %q", wantIssuer, vendorSignerIssuer)
	}
}

// TestApplyDefaultsSeedsVendorIssuerSet asserts a daemon with no configured
// trust.issuerSet ends up trusting the official signer by default — the
// whole point of the vendor trust root: official kits install under the
// default signed-by-allowlist mode without --allow-unsigned.
func TestApplyDefaultsSeedsVendorIssuerSet(t *testing.T) {
	t.Setenv(envKitTrustMode, "") // ensure the secure default mode.
	var c Config
	applyDefaults(&c)
	if c.Trust.Mode != TrustModeSignedByAllowlist {
		t.Fatalf("default trust mode: want %q, got %q", TrustModeSignedByAllowlist, c.Trust.Mode)
	}
	if len(c.Trust.IssuerSet) != 1 || c.Trust.IssuerSet[0] != vendorSignerSAN {
		t.Fatalf("default issuerSet: want [%q], got %v", vendorSignerSAN, c.Trust.IssuerSet)
	}
	// The seeded config must NOT be a misconfiguration (non-empty allowlist).
	if err := validateTrustConfig(c.Trust); err != nil {
		t.Errorf("validateTrustConfig on default-seeded config: want nil, got %v", err)
	}

	// An operator-configured issuerSet must NOT be overwritten by the seed.
	c2 := Config{Trust: TrustConfig{IssuerSet: []string{"operator@example.com"}}}
	applyDefaults(&c2)
	if len(c2.Trust.IssuerSet) != 1 || c2.Trust.IssuerSet[0] != "operator@example.com" {
		t.Errorf("operator issuerSet must survive applyDefaults: got %v", c2.Trust.IssuerSet)
	}
}

// TestKitVerifier_DefaultIssuerSetAcceptsVendorIdentity verifies the
// end-to-end trust loop conceptually: a manifest signed by the official CI
// identity (vendor SAN + GitHub Actions OIDC issuer) verifies against the
// daemon's DEFAULT issuerSet under the default signed-by-allowlist mode.
func TestKitVerifier_DefaultIssuerSetAcceptsVendorIdentity(t *testing.T) {
	v, vs := newHermeticVerifierWithIssuerSet(t, TrustModeSignedByAllowlist, defaultVendorIssuerSet())
	manifestBytes := []byte(minimalKitTOML)

	// Sign as the official CI would: vendor SAN + vendor OIDC issuer.
	entity, err := vs.Sign(vendorSignerSAN, vendorSignerIssuer, manifestBytes)
	if err != nil {
		t.Fatalf("vs.Sign as vendor identity: %v", err)
	}

	res := v.verifyEntity("default/go", entity, manifestBytes)
	if res.Trust != afclient.KitTrustSignedVerified {
		t.Fatalf("Trust: want signed-verified for vendor identity under default issuerSet, got %q (details=%q)", res.Trust, res.Details)
	}
	if !v.trustGateAllows(res.Trust) {
		t.Errorf("signed-by-allowlist gate must allow the vendor-signed kit, got false")
	}
}

// TestKitVerifier_DefaultIssuerSetRejectsOtherSAN asserts a kit signed by a
// DIFFERENT SAN (e.g., a fork's workflow or an attacker's repo) is rejected
// by the default issuerSet — the allowlist is exact-match on SAN.
func TestKitVerifier_DefaultIssuerSetRejectsOtherSAN(t *testing.T) {
	v, vs := newHermeticVerifierWithIssuerSet(t, TrustModeSignedByAllowlist, defaultVendorIssuerSet())
	manifestBytes := []byte(minimalKitTOML)

	// Same OIDC issuer, different SAN (a fork / unrelated workflow).
	const otherSAN = "https://github.com/attacker/donmai-kits/.github/workflows/sign.yml@refs/heads/main"
	entity, err := vs.Sign(otherSAN, vendorSignerIssuer, manifestBytes)
	if err != nil {
		t.Fatalf("vs.Sign as other SAN: %v", err)
	}

	res := v.verifyEntity("default/go", entity, manifestBytes)
	if res.Trust != afclient.KitTrustSignedUnverified {
		t.Fatalf("Trust: want signed-unverified for non-allowlisted SAN, got %q (details=%q)", res.Trust, res.Details)
	}
	if v.trustGateAllows(res.Trust) {
		t.Errorf("gate must reject a kit signed by a non-allowlisted SAN")
	}
}

// TestKitVerifier_DefaultIssuerSetRejectsOtherIssuer asserts the vendor
// identity pins the OIDC issuer too: a kit with the EXACT vendor SAN but a
// different issuer (a forged or non-GitHub token presenting the same SAN
// string) is rejected. This is the issuer-pin guarantee that distinguishes
// the official identity from any other holder of the same SAN string.
func TestKitVerifier_DefaultIssuerSetRejectsOtherIssuer(t *testing.T) {
	v, vs := newHermeticVerifierWithIssuerSet(t, TrustModeSignedByAllowlist, defaultVendorIssuerSet())
	manifestBytes := []byte(minimalKitTOML)

	// Exact vendor SAN, but a different OIDC issuer.
	entity, err := vs.Sign(vendorSignerSAN, "https://evil-issuer.example", manifestBytes)
	if err != nil {
		t.Fatalf("vs.Sign with wrong issuer: %v", err)
	}

	res := v.verifyEntity("default/go", entity, manifestBytes)
	if res.Trust != afclient.KitTrustSignedUnverified {
		t.Fatalf("Trust: want signed-unverified for wrong issuer, got %q (details=%q)", res.Trust, res.Details)
	}
	if v.trustGateAllows(res.Trust) {
		t.Errorf("gate must reject the vendor SAN under a non-vendor issuer")
	}
}

func TestKitVerifier_BundleVerifiesOK(t *testing.T) {
	v, vs := newHermeticVerifier(t, TrustModePermissive)
	manifestBytes := []byte(minimalKitTOML)

	// The hermetic CA signs the manifest, returning a SignedEntity that
	// the verifier accepts because vs is also our trusted material.
	entity, err := vs.Sign("kit-publisher@example.com", "https://issuer.example", manifestBytes)
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
	if !strings.Contains(res.SignerID, "kit-publisher@example.com") {
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

	entity, err := vs.Sign("kit-publisher@example.com", "https://issuer.example", signedBytes)
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
		{"empty-defaults-allowlist-rejects-unsigned", TrustMode(""), afclient.KitTrustUnsigned, false},
		{"empty-defaults-allowlist-rejects-unverified", TrustMode(""), afclient.KitTrustSignedUnverified, false},
		{"empty-defaults-allowlist-allows-verified", TrustMode(""), afclient.KitTrustSignedVerified, true},
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

func TestResolveDefaultTrustMode(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want TrustMode
	}{
		{"unset-defaults-allowlist", "", TrustModeSignedByAllowlist},
		{"permissive-opt-out", "permissive", TrustModePermissive},
		{"explicit-allowlist", "signed-by-allowlist", TrustModeSignedByAllowlist},
		{"attested-recognised", "attested", TrustModeAttested},
		{"unrecognised-falls-back-to-allowlist", "anything-goes", TrustModeSignedByAllowlist},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envKitTrustMode, tc.env)
			if got := resolveDefaultTrustMode(); got != tc.want {
				t.Errorf("resolveDefaultTrustMode() with %s=%q: want %q, got %q", envKitTrustMode, tc.env, tc.want, got)
			}
		})
	}
}

func TestValidateTrustConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     TrustConfig
		wantErr bool
	}{
		{"allowlist-empty-issuerset-rejected", TrustConfig{Mode: TrustModeSignedByAllowlist}, true},
		{"attested-empty-issuerset-rejected", TrustConfig{Mode: TrustModeAttested}, true},
		{"allowlist-with-issuerset-ok", TrustConfig{Mode: TrustModeSignedByAllowlist, IssuerSet: []string{"kit-publisher@example.com"}}, false},
		{"permissive-empty-issuerset-ok", TrustConfig{Mode: TrustModePermissive}, false},
		{"empty-mode-ok", TrustConfig{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTrustConfig(tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateTrustConfig(%+v): err = %v, wantErr %v", tc.cfg, err, tc.wantErr)
			}
		})
	}

	// The misconfiguration error must spell out both remediation paths.
	err := validateTrustConfig(TrustConfig{Mode: TrustModeSignedByAllowlist})
	if err == nil {
		t.Fatal("validateTrustConfig: want error for allowlist + empty issuer set")
	}
	for _, want := range []string{"trust.issuerSet", envKitTrustMode} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error: want substring %q, got: %s", want, err.Error())
		}
	}
}

// TestKitRegistry_InstallTrustGateRejectionIsActionable asserts the
// gate-rejection error names the kit's trust state and every remediation
// path (allowlist the signer, one-time override, switch mode) — the CLI
// shows this text to OSS users verbatim.
func TestKitRegistry_InstallTrustGateRejectionIsActionable(t *testing.T) {
	repoURL := newLocalGitFixture(t, fixtureFile{name: "rensei-example.kit.toml", body: minimalKitTOML})
	scan := t.TempDir()

	r := NewKitRegistryWithTrust([]string{scan}, TrustConfig{
		Mode:      TrustModeSignedByAllowlist,
		IssuerSet: []string{"kit-publisher@example.com"},
	})
	_, err := r.Install("rensei/example", afclient.KitInstallRequest{
		Source: &afclient.KitInstallSource{Kind: "git", URL: repoURL},
	})
	if !errors.Is(err, ErrKitTrustGateRejected) {
		t.Fatalf("Install: want ErrKitTrustGateRejected, got %v", err)
	}
	for _, want := range []string{
		string(afclient.KitTrustUnsigned),
		"trust.issuerSet",
		"--allow-unsigned",
		envKitTrustMode,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("rejection error: want substring %q, got: %s", want, err.Error())
		}
	}
}

// These tests cover the install-time trust gate end-to-end through the
// git-source install path (Phase 4 / S3): they build a local git repo
// containing the manifest, point Install at file://<repo>, and assert
// the gate behaviour. The Phase 3 ancestors of these tests gated on
// EXISTING manifests in scanPaths; Phase 4's "fetch then verify then
// persist" flow is the canonical exercise vector going forward.

func TestKitRegistry_InstallTrustGateRejectsUnsigned(t *testing.T) {
	repoURL := newLocalGitFixture(t, fixtureFile{name: "rensei-example.kit.toml", body: minimalKitTOML})
	scan := t.TempDir()

	r := NewKitRegistryWithTrust([]string{scan}, TrustConfig{Mode: TrustModeSignedByAllowlist})

	_, err := r.Install("rensei/example", afclient.KitInstallRequest{
		Source: &afclient.KitInstallSource{Kind: "git", URL: repoURL},
	})
	if !errors.Is(err, ErrKitTrustGateRejected) {
		t.Fatalf("Install: want ErrKitTrustGateRejected for unsigned + allowlist, got %v", err)
	}
}

func TestKitRegistry_InstallTrustGatePassesPermissive(t *testing.T) {
	repoURL := newLocalGitFixture(t, fixtureFile{name: "rensei-example.kit.toml", body: minimalKitTOML})
	scan := t.TempDir()

	r := NewKitRegistryWithTrust([]string{scan}, TrustConfig{Mode: TrustModePermissive})

	res, err := r.Install("rensei/example", afclient.KitInstallRequest{
		Source: &afclient.KitInstallSource{Kind: "git", URL: repoURL},
	})
	if err != nil {
		t.Fatalf("Install: want success under permissive mode for unsigned manifest, got %v", err)
	}
	if res.Kit.ID != "rensei/example" {
		t.Errorf("Result.Kit.ID: want rensei/example, got %q", res.Kit.ID)
	}
	if res.Kit.Trust != afclient.KitTrustUnsigned {
		t.Errorf("Result.Kit.Trust: want unsigned, got %q", res.Kit.Trust)
	}
}

func TestKitRegistry_InstallTrustOverrideAuditLogs(t *testing.T) {
	repoURL := newLocalGitFixture(t, fixtureFile{name: "rensei-example.kit.toml", body: minimalKitTOML})
	scan := t.TempDir()

	// Capture slog output via JSON handler over an in-memory buffer.
	buf := captureSlogTrust(t)

	r := NewKitRegistryWithTrust([]string{scan}, TrustConfig{
		Mode:  TrustModeSignedByAllowlist,
		Actor: "operator@example.com",
	})

	_, err := r.Install("rensei/example", afclient.KitInstallRequest{
		Source:        &afclient.KitInstallSource{Kind: "git", URL: repoURL},
		TrustOverride: afclient.TrustOverrideAllowedThisOnce,
	})
	if err != nil {
		t.Fatalf("Install with override: want success after gate bypass, got %v", err)
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
		// authorIdentity backfill in installFromGit.
		if got := rec["signerId"]; got != "did:web:example.com" {
			t.Errorf("audit signerId: want did:web:example.com, got %v", got)
		}
		if got := rec["actor"]; got != "operator@example.com" {
			t.Errorf("audit actor: want operator@example.com, got %v", got)
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
	if res.SignerID != "did:web:example.com" {
		t.Errorf("SignerID: want did:web:example.com backfill, got %q", res.SignerID)
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
