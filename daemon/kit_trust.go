// Package daemon kit_trust.go — sigstore bundle-mode kit signature
// verifier (Wave 12 / Theme C / S2).
//
// The verifier consumes a sibling `<manifest>.sigstore` file (Q1 of
// WAVE12_PLAN — "bundle file shape: sibling .sigstore"), validates it
// against the configured trust root, and reports back a populated
// afclient.KitSignatureResult. Three trust outcomes:
//
//   - KitTrustSignedVerified   — bundle present and verifies against
//     the trust root + issuer set.
//   - KitTrustSignedUnverified — bundle present but verification failed
//     (tampered manifest, untrusted issuer, expired chain, etc.).
//   - KitTrustUnsigned         — no sibling .sigstore file exists.
//
// At install time the verifier outcome maps to a trust gate. The gate
// runs in the registry's Install path, NOT here — see the
// ErrKitTrustGateRejected sentinel in kit_registry.go and the
// trustOverride: "allowed-this-once" handling per audit § 1.3 / § 2.2.
//
// Trust modes (§ "Signing and trust" in 002-provider-base-contract.md):
//
//   - permissive            — verifier still runs and reports state, but
//     never blocks Install. Was the OSS default per Q2 of WAVE12_PLAN;
//     replaced by signed-by-allowlist as the compiled-in default (secfix).
//     Opt in via DONMAI_KIT_TRUST_MODE=permissive or daemon.yaml trust.mode.
//   - signed-by-allowlist   — Install rejects KitTrustUnsigned and
//     KitTrustSignedUnverified. Compiled-in default (resolveDefaultTrustMode).
//   - attested              — same as allowlist for Wave 12 (the SLSA
//     attestation graph hookup lands in Wave 13+).
//
// The embedded trust root is the public Sigstore production trust root
// (https://raw.githubusercontent.com/sigstore/sigstore-go/main/examples/trusted-root-public-good.json).
// It will be replaced with a Rensei-published trust root once the
// productionized signing CI from REN-1344 emits a Rensei-signed Fulcio +
// Rekor cert chain (Wave 13+ work).
//
// Q-audit-2 resolution (taken 2026-05-07 by /loop coordinator):
// trust-actor lookup falls back to os.Getuid() when daemon.yaml's
// `trust.actor` is empty. The trustOverride audit log is best-effort
// identification; the override is still timestamped and key fields
// (kitId, signerId) are always populated.
package daemon

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/RenseiAI/donmai/afclient"
)

// embeddedTrustRoot is the public Sigstore production trust root JSON
// (Fulcio + Rekor + CT log + TSA cert chain). Sourced from
// https://raw.githubusercontent.com/sigstore/sigstore-go/main/examples/trusted-root-public-good.json.
//
// Ship-with-the-binary so the OSS daemon verifies bundles offline. To
// be replaced with a Rensei-published trust root once REN-1344's
// productionized signing CI emits Rensei-signed Fulcio + Rekor cert
// chain (Wave 13+).
//
//go:embed trust_root_embedded.json
var embeddedTrustRoot []byte

// TrustMode is the operator-configured policy for how the install
// gate reacts to verifier outcomes.
type TrustMode string

// Trust modes accepted on daemon.yaml `trust.mode`.
const (
	// TrustModePermissive allows install regardless of verifier outcome.
	// The verifier still runs and the trust state is reported; this
	// matches OSS-execution-layer expectations vs the npm/pip/cargo
	// precedent.
	//
	// NOTE: permissive is no longer the compiled-in default. The daemon
	// now defaults to TrustModeSignedByAllowlist. Operators who knowingly
	// accept unsigned-kit risk can opt back to permissive by setting
	// DONMAI_KIT_TRUST_MODE=permissive or by setting trust.mode in
	// daemon.yaml explicitly.
	TrustModePermissive TrustMode = "permissive"
	// TrustModeSignedByAllowlist rejects unsigned and unverified kits at
	// install time; verified-signed kits whose signer matches the
	// configured issuer set install normally.
	//
	// This is the compiled-in default trust mode (enforced by
	// resolveDefaultTrustMode and kitRegistryOrEmpty). An empty IssuerSet
	// under this mode is treated as a misconfiguration: the daemon refuses
	// to install any kit until the operator either populates trust.issuerSet
	// or explicitly sets DONMAI_KIT_TRUST_MODE=permissive.
	TrustModeSignedByAllowlist TrustMode = "signed-by-allowlist"
	// TrustModeAttested is allowlist + (future) SLSA attestation-graph
	// requirement. Wave 12 treats it as an alias for allowlist; the
	// attestation requirement lands in Wave 13+ alongside the SLSA
	// provenance parser.
	TrustModeAttested TrustMode = "attested"
)

// envKitTrustMode is the environment-variable name operators can set to
// opt out of the secure default trust mode. Setting it to "permissive"
// re-enables unsigned-kit installs; any other recognised TrustMode value
// overrides the compiled-in default. This mirrors the
// DONMAI_ORCHESTRATOR_URL / DONMAI_DAEMON_TOKEN env-override pattern
// used by config.go.
const envKitTrustMode = "DONMAI_KIT_TRUST_MODE"

// resolveDefaultTrustMode returns the effective default TrustMode when
// daemon.yaml has no explicit trust.mode set. The compiled-in default is
// TrustModeSignedByAllowlist; operators may override it via the
// DONMAI_KIT_TRUST_MODE environment variable.
//
// This function is used by kitRegistryOrEmpty (handle_kit.go) and by
// applyDefaults (config.go) so that all code paths that fill in a missing
// trust.mode agree on the safe default.
func resolveDefaultTrustMode() TrustMode {
	if v := os.Getenv(envKitTrustMode); v != "" {
		switch TrustMode(v) {
		case TrustModePermissive, TrustModeSignedByAllowlist, TrustModeAttested:
			return TrustMode(v)
		default:
			slog.Warn("kit trust: DONMAI_KIT_TRUST_MODE value unrecognised; falling back to signed-by-allowlist",
				"value", v,
				"recognised", []string{string(TrustModePermissive), string(TrustModeSignedByAllowlist), string(TrustModeAttested)},
			)
		}
	}
	return TrustModeSignedByAllowlist
}

// TrustConfig is the daemon-wide trust policy. Lives on Config (NOT on
// KitConfig) per audit § 1.2: the trust mode applies across plugin
// families per 015-plugin-spec.md § "Auth + trust", not just kits.
type TrustConfig struct {
	// Mode is one of permissive | signed-by-allowlist | attested.
	// Empty defaults to signed-by-allowlist (via resolveDefaultTrustMode).
	// Operators who need unsigned-kit installs can set mode: permissive
	// in daemon.yaml or export DONMAI_KIT_TRUST_MODE=permissive.
	Mode TrustMode `yaml:"mode,omitempty" json:"mode,omitempty"`

	// IssuerSet is the allowlist of OIDC subject identities (Fulcio SAN)
	// the operator considers trusted. Under signed-by-allowlist / attested
	// mode an EMPTY IssuerSet is treated as a misconfiguration: the trust
	// gate fails closed with ErrKitTrustGateRejected and a clear message
	// until the operator populates this list or switches to permissive.
	// In permissive mode an empty IssuerSet means "trust any signer the
	// embedded trust root can validate" (the bundle chain must still
	// verify; only the SAN-pattern filter is skipped).
	IssuerSet []string `yaml:"issuerSet,omitempty" json:"issuerSet,omitempty"`

	// Actor is the operator-declared identity used in the trustOverride
	// audit log entry. When empty the actor falls back to
	// fmt.Sprintf("uid:%d", os.Getuid()) per coordinator decision
	// Q-audit-2 (2026-05-07). The override is also timestamped and
	// names the kitId + signerId, so this field is best-effort.
	Actor string `yaml:"actor,omitempty" json:"actor,omitempty"`
}

// kitVerifier is the bundle-mode verifier wired into KitRegistry.
//
// One instance per registry; thread-safe (the underlying *verify.Verifier
// and *root.TrustedRoot are immutable after construction).
type kitVerifier struct {
	config       TrustConfig
	trustedRoot  *root.TrustedRoot
	trustedExtra root.TrustedMaterial // optional override (tests inject VirtualSigstore)
	rootSource   string               // "embedded" | "test-fixture"
}

// newKitVerifier constructs a verifier from the daemon's TrustConfig.
//
// The trust root is loaded from the embedded JSON; it will be replaced
// with Rensei-signed material in Wave 13+ once REN-1344 productionizes
// the signing CI. Callers that want a different trust root (e.g.,
// hermetic tests) construct via newKitVerifierWithMaterial.
//
// NOTE: this function does NOT call validateTrustConfig — callers that
// need fail-closed behaviour on misconfigured trust (e.g.,
// kitRegistryOrEmpty) must call validateTrustConfig themselves before
// calling NewKitRegistryWithTrust. newKitVerifier's error return is
// reserved for trust-root parse failures; mixing misconfiguration errors
// here would trigger the permissive-fallback in NewKitRegistryWithTrust,
// which is the opposite of fail-closed.
func newKitVerifier(cfg TrustConfig) (*kitVerifier, error) {
	tr, err := root.NewTrustedRootFromJSON(embeddedTrustRoot)
	if err != nil {
		return nil, fmt.Errorf("load embedded trust root: %w", err)
	}
	return &kitVerifier{
		config:      cfg,
		trustedRoot: tr,
		rootSource:  "embedded",
	}, nil
}

// validateTrustConfig returns a descriptive error when the trust
// configuration would produce an insecure outcome.
//
// Specifically: signed-by-allowlist / attested mode with an empty
// IssuerSet would fall through to WithoutIdentitiesUnsafe and accept any
// validly-signed bundle regardless of issuer. Callers must check this
// BEFORE constructing the registry so they can surface a clear operator
// action item rather than silently accepting over-permissive behaviour.
//
// This function is intentionally NOT called inside newKitVerifier because
// newKitVerifier's error triggers a permissive fallback inside
// NewKitRegistryWithTrust (kit_registry.go), which would be worse than
// the misconfiguration it is trying to prevent.
func validateTrustConfig(cfg TrustConfig) error {
	switch cfg.Mode {
	case TrustModeSignedByAllowlist, TrustModeAttested:
		if len(cfg.IssuerSet) == 0 {
			return fmt.Errorf(
				"kit trust: mode %q requires a non-empty trust.issuerSet "+
					"(an empty allowlist would accept any OIDC-signed kit); "+
					"populate trust.issuerSet in daemon.yaml or set %s=permissive "+
					"to explicitly opt in to permissive mode",
				cfg.Mode, envKitTrustMode,
			)
		}
	}
	return nil
}

// newKitVerifierWithMaterial constructs a verifier with caller-supplied
// trusted material — used by tests to inject a VirtualSigstore as the
// trust root, avoiding the embedded production root.
func newKitVerifierWithMaterial(cfg TrustConfig, material root.TrustedMaterial) *kitVerifier {
	return &kitVerifier{
		config:       cfg,
		trustedExtra: material,
		rootSource:   "test-fixture",
	}
}

// VerifyManifest reads <manifestPath>.sigstore alongside the manifest,
// validates it against the configured trust root + issuer set, and
// returns a populated afclient.KitSignatureResult.
//
// Bundle-not-found is NOT an error — the result reports KitTrustUnsigned
// with OK: true so that permissive trust mode can still continue with
// the install. Verification errors set Trust = KitTrustSignedUnverified
// with OK: true and a Details string explaining what failed.
//
// kitID is plumbed through purely so the result envelope carries it
// (the verifier doesn't otherwise need it).
func (v *kitVerifier) VerifyManifest(kitID, manifestPath string) (afclient.KitSignatureResult, error) {
	res := afclient.KitSignatureResult{KitID: kitID, Trust: afclient.KitTrustUnsigned, OK: true}

	manifestBytes, err := os.ReadFile(manifestPath) //nolint:gosec // operator-installed manifests
	if err != nil {
		return res, fmt.Errorf("read manifest %q: %w", manifestPath, err)
	}

	bundlePath := manifestPath + ".sigstore"
	if _, err := os.Stat(bundlePath); err != nil { //nolint:gosec // sibling of operator-installed manifest path
		if errors.Is(err, os.ErrNotExist) {
			res.Details = "no sibling .sigstore file; manifest is unsigned"
			return res, nil
		}
		return res, fmt.Errorf("stat bundle %q: %w", bundlePath, err)
	}

	b, err := bundle.LoadJSONFromPath(bundlePath)
	if err != nil {
		res.Trust = afclient.KitTrustSignedUnverified
		res.Details = fmt.Sprintf("parse bundle: %v", err)
		return res, nil
	}

	return v.verifyEntity(kitID, b, manifestBytes), nil
}

// verifyEntity is the algorithmic core: given a parsed entity and the
// manifest bytes, run sigstore-go's verifier and populate the result.
//
// Factored out so tests can drive the verifier with an in-memory
// VirtualSigstore TestEntity without round-tripping through bundle
// JSON serialization.
func (v *kitVerifier) verifyEntity(kitID string, entity verify.SignedEntity, manifestBytes []byte) afclient.KitSignatureResult {
	res := afclient.KitSignatureResult{KitID: kitID, OK: true}

	material := v.materialForVerify()
	if material == nil {
		res.Trust = afclient.KitTrustSignedUnverified
		res.Details = "no trusted material configured"
		return res
	}

	sev, err := verify.NewVerifier(material, verifierOptions()...)
	if err != nil {
		res.Trust = afclient.KitTrustSignedUnverified
		res.Details = fmt.Sprintf("init verifier: %v", err)
		return res
	}

	digest := sha256.Sum256(manifestBytes)
	policy := verify.NewPolicy(
		verify.WithArtifactDigest("sha256", digest[:]),
		buildIdentityPolicies(v.config.IssuerSet)...,
	)

	out, err := sev.Verify(entity, policy)
	if err != nil {
		res.Trust = afclient.KitTrustSignedUnverified
		res.Details = fmt.Sprintf("verify: %v", err)
		// Best-effort signer hint even on failure — the caller may want
		// to log who *claimed* to sign the manifest.
		res.SignerID = signerIDFromVerifyError(entity)
		return res
	}

	res.Trust = afclient.KitTrustSignedVerified
	if out.Signature != nil && out.Signature.Certificate != nil {
		res.SignerID = out.Signature.Certificate.SubjectAlternativeName
	}
	if len(out.VerifiedTimestamps) > 0 {
		res.SignedAt = out.VerifiedTimestamps[0].Timestamp.UTC().Format(time.RFC3339)
	}
	res.Details = fmt.Sprintf("trust root: %s; manifest sha256: %s", v.rootSource, hex.EncodeToString(digest[:]))
	return res
}

// materialForVerify returns whichever trusted material is configured.
// Tests inject via trustedExtra; production uses the embedded root.
func (v *kitVerifier) materialForVerify() root.TrustedMaterial {
	if v.trustedExtra != nil {
		return v.trustedExtra
	}
	if v.trustedRoot != nil {
		return v.trustedRoot
	}
	return nil
}

// verifierOptions returns the VerifierOption list for bundle-mode
// verification. Wave 12 scope (per WAVE12_PLAN § "Sigstore alignment
// research"): require an observer timestamp (RFC3161 OR Rekor SET) and
// a transparency-log inclusion proof, both threshold 1. SCT enforcement
// stays off for now since the embedded trust root may lag behind CT
// log key rotations and we'd rather report SignedUnverified than fail
// closed on the trust root's lifecycle.
func verifierOptions() []verify.VerifierOption {
	return []verify.VerifierOption{
		verify.WithObserverTimestamps(1),
		verify.WithTransparencyLog(1),
	}
}

// buildIdentityPolicies returns sigstore-go PolicyOptions for the
// configured issuer allowlist.
//
// Empty allowlist is only accepted in permissive mode; the caller
// (newKitVerifier / validateTrustConfig) already enforces that allowlist
// and attested modes require a non-empty IssuerSet. When called with an
// empty slice here we still defend by returning WithoutIdentitiesUnsafe,
// which skips the SAN-pattern filter but still requires the bundle chain
// to verify against the embedded trust root.
//
// All-malformed-entries: if every entry in a non-empty IssuerSet fails to
// parse we return a no-op policy that will cause all identity checks to
// fail (no WithCertificateIdentity option matched = rejection). We do NOT
// fall back to WithoutIdentitiesUnsafe in this case, which would
// incorrectly accept any signer.
func buildIdentityPolicies(issuerSet []string) []verify.PolicyOption {
	if len(issuerSet) == 0 {
		return []verify.PolicyOption{verify.WithoutIdentitiesUnsafe()}
	}
	out := make([]verify.PolicyOption, 0, len(issuerSet))
	for _, san := range issuerSet {
		// san-as-exact-match; regex deferred to a Wave 13+ extension
		// when ${REN-1344}'s Rensei issuer set materializes.
		ident, err := verify.NewShortCertificateIdentity("", "", san, "")
		if err != nil {
			slog.Warn("kit verifier: skipping malformed issuer entry", //nolint:gosec // structured slog handler escapes values
				"san", san,
				"err", err.Error(),
			)
			continue
		}
		out = append(out, verify.WithCertificateIdentity(ident))
	}
	if len(out) == 0 {
		// Every configured issuer entry was malformed. Return an empty
		// options slice so that sigstore-go's policy has no
		// WithCertificateIdentity option to match against — verification
		// will fail closed rather than silently accepting any signer.
		// The operator must fix their trust.issuerSet entries.
		slog.Warn("kit verifier: all trust.issuerSet entries were malformed; no certificate identities will be trusted")
		return []verify.PolicyOption{}
	}
	return out
}

// signerIDFromVerifyError extracts the SAN from a SignedEntity's leaf
// certificate when verify failed but the entity at least parsed. Best
// effort — returns "" on any error.
func signerIDFromVerifyError(entity verify.SignedEntity) string {
	vc, err := entity.VerificationContent()
	if err != nil || vc == nil {
		return ""
	}
	cert := vc.Certificate()
	if cert == nil {
		return ""
	}
	if len(cert.URIs) > 0 {
		return cert.URIs[0].String()
	}
	if len(cert.EmailAddresses) > 0 {
		return cert.EmailAddresses[0]
	}
	return ""
}

// trustGateAllows returns true when the given verifier outcome should
// be allowed to install under the configured trust mode.
//
// Permissive mode: always allow; emits a prominent slog.Warn so
// operators are aware that unsigned / any-issuer kits will execute
// shell commands.
//
// Allowlist / attested: only signed-verified outcomes pass.
//
// Empty mode: treated as signed-by-allowlist (the safe default). The
// empty-string case should not arise in production because both
// applyDefaults (config.go) and kitRegistryOrEmpty (handle_kit.go) call
// resolveDefaultTrustMode; it is handled here as a belt-and-suspenders
// guard.
func (v *kitVerifier) trustGateAllows(trust afclient.KitTrustState) bool {
	switch v.config.Mode {
	case TrustModePermissive:
		slog.Warn("kit trust: PERMISSIVE mode is active — unsigned kits and kits from any OIDC issuer will be allowed to execute shell commands; set trust.mode: signed-by-allowlist or remove DONMAI_KIT_TRUST_MODE=permissive to enforce signature verification",
			"trustState", string(trust),
		)
		return true
	case "", TrustModeSignedByAllowlist, TrustModeAttested:
		return trust == afclient.KitTrustSignedVerified
	default:
		// Unknown mode — fail-safe to allowlist semantics. validateConfig
		// rejects unknown modes at load time so this branch is defensive.
		return trust == afclient.KitTrustSignedVerified
	}
}

// resolveActor picks the actor string for trustOverride audit logs.
// Per Q-audit-2: daemon.yaml's `trust.actor` first, then
// "uid:<os.Getuid()>".
func (v *kitVerifier) resolveActor() string {
	if v.config.Actor != "" {
		return v.config.Actor
	}
	return "uid:" + strconv.Itoa(os.Getuid())
}

// auditTrustOverride emits the structured audit log entry for a
// trustOverride: "allowed-this-once" install. Audit-logged via slog.Info
// with kitId, signerId (may be empty when unsigned), actor, at.
func (v *kitVerifier) auditTrustOverride(kitID, signerID string) {
	slog.Info("kit install: trust gate bypassed via trustOverride=allowed-this-once", //nolint:gosec // structured slog handler escapes values
		"kitId", kitID,
		"signerId", signerID,
		"actor", v.resolveActor(),
		"at", time.Now().UTC().Format(time.RFC3339),
	)
}
