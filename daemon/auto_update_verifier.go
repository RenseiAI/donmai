// Package daemon auto_update_verifier.go — sigstore bundle-mode
// binary-signature verifier for the auto-update flow.
//
// Mirrors kit_trust.go: the release pipeline publishes a sigstore bundle
// (`<binary>.sigstore`, e.g. `cosign sign-blob --bundle … --new-bundle-format`)
// next to each binary on the CDN; before any swap the daemon verifies the
// downloaded binary's SHA-256 digest against the bundle, the embedded (or
// operator-supplied) trust root, and the operator-pinned signer allowlist.
//
// Fail-closed: with no `autoUpdate.signers` configured the daemon refuses
// every swap. Unlike the kit verifier there is deliberately no
// identity-less fallback — "any keyless signer the public trust root
// validates" is far too weak a policy for swapping the daemon's own
// binary, so at least one (SAN, issuer) pair must be pinned.
package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// failClosedVerifier is a BinaryVerifier that refuses every swap with a
// fixed reason. It is the production default whenever a real sigstore
// verifier cannot be constructed (no signers configured, unreadable trust
// root, malformed signer entry) — auto-update stays fail-closed rather
// than degrading to unsigned swaps.
type failClosedVerifier struct{ reason string }

// Verify implements BinaryVerifier. It always refuses the swap.
func (v failClosedVerifier) Verify(_ context.Context, _, _ string) (bool, string) {
	return false, v.reason
}

// newAutoUpdateVerifier returns the production BinaryVerifier for the
// given auto-update config: a sigstore bundle verifier when a signer
// allowlist is configured, a fail-closed verifier otherwise.
func newAutoUpdateVerifier(cfg AutoUpdateConfig) BinaryVerifier {
	if len(cfg.Signers) == 0 {
		return failClosedVerifier{reason: "no update signers configured (autoUpdate.signers); refusing swap"}
	}
	v, err := newSigstoreBinaryVerifier(cfg)
	if err != nil {
		return failClosedVerifier{reason: fmt.Sprintf("sigstore verifier init failed: %v; refusing swap", err)}
	}
	return v
}

// sigstoreBinaryVerifier verifies release binaries against sigstore
// bundles. Immutable after construction; safe for concurrent use.
type sigstoreBinaryVerifier struct {
	material   root.TrustedMaterial
	identities []verify.PolicyOption
	rootSource string // "embedded" | trust-root file path | "test-fixture"
}

// newSigstoreBinaryVerifier constructs the production verifier from the
// daemon's auto-update config. Any malformed signer entry or trust-root
// problem is an error — callers fall back to failClosedVerifier.
func newSigstoreBinaryVerifier(cfg AutoUpdateConfig) (*sigstoreBinaryVerifier, error) {
	identities, err := buildUpdateIdentityPolicies(cfg.Signers)
	if err != nil {
		return nil, err
	}
	material, source, err := loadUpdateTrustRoot(cfg.TrustRootPath)
	if err != nil {
		return nil, err
	}
	return &sigstoreBinaryVerifier{material: material, identities: identities, rootSource: source}, nil
}

// newSigstoreBinaryVerifierWithMaterial is the hermetic-test constructor
// (mirrors newKitVerifierWithMaterial): the caller supplies the trusted
// material (e.g. a VirtualSigstore) instead of the embedded root.
func newSigstoreBinaryVerifierWithMaterial(signers []UpdateSigner, material root.TrustedMaterial) (*sigstoreBinaryVerifier, error) {
	identities, err := buildUpdateIdentityPolicies(signers)
	if err != nil {
		return nil, err
	}
	return &sigstoreBinaryVerifier{material: material, identities: identities, rootSource: "test-fixture"}, nil
}

// buildUpdateIdentityPolicies maps the signer allowlist to sigstore-go
// identity policies. Unlike the kit verifier's permissive
// buildIdentityPolicies, ANY malformed entry is an error and an empty
// allowlist is never valid — binary swaps must pin at least one signer.
func buildUpdateIdentityPolicies(signers []UpdateSigner) ([]verify.PolicyOption, error) {
	if len(signers) == 0 {
		return nil, errors.New("no update signers configured")
	}
	out := make([]verify.PolicyOption, 0, len(signers))
	for i, s := range signers {
		ident, err := verify.NewShortCertificateIdentity(s.Issuer, "", s.SAN, s.SANRegex)
		if err != nil {
			return nil, fmt.Errorf("autoUpdate.signers[%d] (san=%q, sanRegex=%q): %w", i, s.SAN, s.SANRegex, err)
		}
		out = append(out, verify.WithCertificateIdentity(ident))
	}
	return out, nil
}

// loadUpdateTrustRoot loads the sigstore trusted root: the
// operator-supplied JSON at path when set, the embedded public Sigstore
// production root (shared with kit verification) otherwise.
func loadUpdateTrustRoot(path string) (root.TrustedMaterial, string, error) {
	if path == "" {
		tr, err := root.NewTrustedRootFromJSON(embeddedTrustRoot)
		if err != nil {
			return nil, "", fmt.Errorf("load embedded trust root: %w", err)
		}
		return tr, "embedded", nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // operator-configured trust root path
	if err != nil {
		return nil, "", fmt.Errorf("read trust root %q: %w", path, err)
	}
	tr, err := root.NewTrustedRootFromJSON(data)
	if err != nil {
		return nil, "", fmt.Errorf("parse trust root %q: %w", path, err)
	}
	return tr, path, nil
}

// Verify implements BinaryVerifier. signatureValue is the sigstore bundle
// JSON downloaded from `<binaryURL>.sigstore`; contentHash is the
// lowercase hex SHA-256 of the downloaded binary (already checked against
// the manifest by RunUpdate before this point).
func (v *sigstoreBinaryVerifier) Verify(_ context.Context, contentHash, signatureValue string) (bool, string) {
	var b bundle.Bundle
	if err := b.UnmarshalJSON([]byte(signatureValue)); err != nil {
		return false, fmt.Sprintf("parse sigstore bundle: %v", err)
	}
	return v.verifyEntity(&b, contentHash)
}

// verifyEntity is the algorithmic core, factored out (like the kit
// verifier's verifyEntity) so hermetic tests can drive it with an
// in-memory VirtualSigstore TestEntity without round-tripping through
// bundle JSON serialization.
func (v *sigstoreBinaryVerifier) verifyEntity(entity verify.SignedEntity, contentHash string) (bool, string) {
	digest, err := hex.DecodeString(contentHash)
	if err != nil || len(digest) != sha256.Size {
		return false, fmt.Sprintf("malformed binary sha256 %q", contentHash)
	}
	sev, err := verify.NewVerifier(v.material, verifierOptions()...)
	if err != nil {
		return false, fmt.Sprintf("init sigstore verifier: %v", err)
	}
	policy := verify.NewPolicy(
		verify.WithArtifactDigest("sha256", digest),
		v.identities...,
	)
	out, err := sev.Verify(entity, policy)
	if err != nil {
		return false, fmt.Sprintf("sigstore verify: %v", err)
	}
	signer := ""
	if out.Signature != nil && out.Signature.Certificate != nil {
		signer = out.Signature.Certificate.SubjectAlternativeName
	}
	return true, fmt.Sprintf("sigstore-verified (signer %q, trust root: %s)", signer, v.rootSource)
}
