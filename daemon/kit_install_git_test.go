// Package daemon kit_install_git_test.go — Phase 4 / S3 git-source
// install coverage. The local-git fixture builds a real go-git
// repository on disk under t.TempDir(), commits a kit manifest (and an
// optional sibling .sigstore bundle), and exposes the file:// URL so
// KitRegistry.Install can clone it without touching the network.
//
// Tests covered (per Phase 4 prompt):
//   - install-from-local-git-fixture-permissive
//   - install-from-local-git-fixture-signed-by-allowlist
//   - install-rejects-unsigned-when-allowlist
//   - install-handles-bundle-not-found-permissive
//   - install-rejects-federation-source
//   - install-rejects-missing-manifest-after-clone
//   - install-trustOverride-allowed-this-once-bypasses-gate
//   - install-handles-fetch-failure
//   - install-rejects-unknown-source-kind
//   - install-handles-empty-source-url
//   - install-respects-explicit-manifestPath
//   - install-rejects-traversal-manifestPath
package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// fixtureFile is a path/body pair to seed into the local git fixture.
type fixtureFile struct {
	name string // path relative to repo root (may include subdirs)
	body string
}

// newLocalGitFixture initialises a git repo under t.TempDir(), commits
// the supplied files, and returns a `file://` URL pointing at it.
//
// Behaviour notes:
//   - Uses go-git's PlainInit + Worktree.Add + Commit, no shell-out.
//   - Sets a deterministic Author signature so commits hash predictably
//     enough for test debugging (the actual hash isn't asserted).
//   - Default branch is whatever go-git's PlainInit chooses (master).
//     The test plumbs no explicit Ref, so the fetcher defaults to HEAD.
func newLocalGitFixture(t *testing.T, files ...fixtureFile) string {
	t.Helper()

	if len(files) == 0 {
		t.Fatalf("newLocalGitFixture: must supply at least one fixture file (or use newEmptyGitFixture)")
	}
	repo := t.TempDir()
	r, err := gogit.PlainInit(repo, false)
	if err != nil {
		t.Fatalf("PlainInit %q: %v", repo, err)
	}

	wt, err := r.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}

	for _, f := range files {
		dst := filepath.Join(repo, f.name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			t.Fatalf("mkdir %q: %v", filepath.Dir(dst), err)
		}
		if err := os.WriteFile(dst, []byte(f.body), 0o600); err != nil {
			t.Fatalf("write %q: %v", dst, err)
		}
		if _, err := wt.Add(f.name); err != nil {
			t.Fatalf("Add %q: %v", f.name, err)
		}
	}

	if _, err := wt.Commit("seed: initial fixture commit", &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Test Fixture",
			Email: "fixture@rensei.dev",
			When:  time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC),
		},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	return "file://" + repo
}

// newEmptyGitFixture initialises an empty git repo (no commits) and
// returns its file:// URL. Used by the fetch-failure test case.
func newEmptyGitFixture(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if _, err := gogit.PlainInit(repo, false); err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	return "file://" + repo
}

// hermeticFetcher is a kitSourceFetcher that bypasses go-git entirely:
// it writes the manifest into a temp dir, optionally signs it via the
// VirtualSigstore, and returns the in-memory verify.SignedEntity. Used
// for tests that need the signed-by-allowlist gate path without
// round-tripping a sigstore bundle through protojson serialisation.
//
// The production gitKitFetcher always leaves fetchedKit.Entity nil —
// the registry's verify step reads `<manifest>.sigstore` from disk in
// that case. This fetcher is the test seam audit § 2.1 anticipated.
type hermeticFetcher struct {
	manifest      []byte
	manifestName  string
	entity        verify.SignedEntity
	bundleContent []byte // optional: written as <manifest>.sigstore on disk
}

func (h *hermeticFetcher) Fetch(_ context.Context, _ afclient.KitInstallSource) (*fetchedKit, func(), error) {
	dir, err := os.MkdirTemp("", "rensei-hermetic-fetcher-")
	if err != nil {
		return nil, func() {}, fmt.Errorf("temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	manifestPath := filepath.Join(dir, h.manifestName)
	if err := os.WriteFile(manifestPath, h.manifest, 0o600); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("write manifest: %w", err)
	}

	hasBundle := false
	if len(h.bundleContent) > 0 {
		bundlePath := manifestPath + ".sigstore"
		if err := os.WriteFile(bundlePath, h.bundleContent, 0o600); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("write bundle: %w", err)
		}
		hasBundle = true
	}

	return &fetchedKit{
		ManifestPath: manifestPath,
		HasBundle:    hasBundle,
		TempDir:      dir,
		Entity:       h.entity,
	}, cleanup, nil
}

// ─────────────────────────────────────────────────────────────────────
//
//	install-from-local-git-fixture-permissive
//
// ─────────────────────────────────────────────────────────────────────

func TestKitRegistry_InstallFromGit_PermissiveUnsigned(t *testing.T) {
	repoURL := newLocalGitFixture(t, fixtureFile{name: "rensei-example.kit.toml", body: minimalKitTOML})
	scan := t.TempDir()

	r := NewKitRegistryWithTrust([]string{scan}, TrustConfig{Mode: TrustModePermissive})
	res, err := r.Install("rensei/example", afclient.KitInstallRequest{
		Source: &afclient.KitInstallSource{Kind: "git", URL: repoURL},
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Kit.ID != "rensei/example" {
		t.Errorf("Kit.ID: want rensei/example, got %q", res.Kit.ID)
	}
	if res.Kit.Trust != afclient.KitTrustUnsigned {
		t.Errorf("Kit.Trust: want unsigned, got %q", res.Kit.Trust)
	}
	if res.Message == "" {
		t.Errorf("Message: want non-empty, got empty")
	}

	// Manifest persisted into scanPaths[0] under the sanitized filename.
	want := filepath.Join(scan, "rensei__example.kit.toml")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("persisted manifest %q missing: %v", want, err)
	}

	// Fresh registry sees the kit on the next List() call.
	r2 := NewKitRegistryWithTrust([]string{scan}, TrustConfig{Mode: TrustModePermissive})
	listed := r2.List()
	if len(listed) != 1 || listed[0].ID != "rensei/example" {
		t.Errorf("List after Install: want [rensei/example], got %+v", listed)
	}
}

// ─────────────────────────────────────────────────────────────────────
//
//	install-from-local-git-fixture-signed-by-allowlist
//
// ─────────────────────────────────────────────────────────────────────

func TestKitRegistry_InstallFromGit_SignedByAllowlistAccepts(t *testing.T) {
	// VirtualSigstore (hermetic CA) signs the manifest; the same VS is
	// the trust material the verifier accepts. We use the hermeticFetcher
	// seam because round-tripping a TestEntity through protojson bundle
	// JSON requires constructing the protobundle.Bundle from scratch
	// (audit § 1.6's "auditor caveat"). The hermeticFetcher gives the
	// same end-to-end exercise of fetch → verify → gate → persist while
	// staying offline.
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("NewVirtualSigstore: %v", err)
	}
	manifestBytes := []byte(minimalKitTOML)
	entity, err := vs.Sign("kit-publisher@rensei.dev", "https://issuer.example", manifestBytes)
	if err != nil {
		t.Fatalf("vs.Sign: %v", err)
	}

	scan := t.TempDir()
	r := &KitRegistry{
		scanPaths: []string{scan},
		verifier:  newKitVerifierWithMaterial(TrustConfig{Mode: TrustModeSignedByAllowlist}, vs),
		fetcher: &hermeticFetcher{
			manifest:      manifestBytes,
			manifestName:  "rensei-example.kit.toml",
			entity:        entity,
			bundleContent: []byte(`{"placeholder":"sibling .sigstore presence flag"}`),
		},
	}

	res, err := r.Install("rensei/example", afclient.KitInstallRequest{
		Source: &afclient.KitInstallSource{Kind: "git", URL: "ignored-by-hermetic-fetcher"},
	})
	if err != nil {
		t.Fatalf("Install: want success for signed kit under allowlist, got %v", err)
	}
	if res.Kit.Trust != afclient.KitTrustSignedVerified {
		t.Errorf("Kit.Trust: want signed-verified, got %q", res.Kit.Trust)
	}

	// Bundle persisted alongside the manifest so subsequent
	// verify-signature calls keep returning signed-verified.
	wantBundle := filepath.Join(scan, "rensei__example.kit.toml.sigstore")
	if _, err := os.Stat(wantBundle); err != nil {
		t.Errorf("persisted bundle %q missing: %v", wantBundle, err)
	}
	// Manifest persisted under the sanitized name.
	wantManifest := filepath.Join(scan, "rensei__example.kit.toml")
	if _, err := os.Stat(wantManifest); err != nil {
		t.Errorf("persisted manifest %q missing: %v", wantManifest, err)
	}
}

// ─────────────────────────────────────────────────────────────────────
//
//	install-tampered-bundle-allowlist (signed-but-unverified rejected)
//
// ─────────────────────────────────────────────────────────────────────

func TestKitRegistry_InstallFromGit_TamperedBundleAllowlistRejected(t *testing.T) {
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("NewVirtualSigstore: %v", err)
	}
	signedBytes := []byte(minimalKitTOML)
	tamperedBytes := []byte(strings.Replace(minimalKitTOML, "0.1.0", "9.9.9", 1))
	// Sign one body, deliver a different body to the verifier — bundle
	// digest mismatch → signed-unverified → allowlist rejects.
	entity, err := vs.Sign("kit-publisher@rensei.dev", "https://issuer.example", signedBytes)
	if err != nil {
		t.Fatalf("vs.Sign: %v", err)
	}

	scan := t.TempDir()
	r := &KitRegistry{
		scanPaths: []string{scan},
		verifier:  newKitVerifierWithMaterial(TrustConfig{Mode: TrustModeSignedByAllowlist}, vs),
		fetcher: &hermeticFetcher{
			manifest:      tamperedBytes,
			manifestName:  "rensei-example.kit.toml",
			entity:        entity,
			bundleContent: []byte(`{"placeholder":"present"}`),
		},
	}

	_, err = r.Install("rensei/example", afclient.KitInstallRequest{
		Source: &afclient.KitInstallSource{Kind: "git", URL: "ignored"},
	})
	if !errors.Is(err, ErrKitTrustGateRejected) {
		t.Fatalf("Install: want ErrKitTrustGateRejected for tampered bundle + allowlist, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────
//
//	install-rejects-unsigned-when-allowlist
//
// ─────────────────────────────────────────────────────────────────────

func TestKitRegistry_InstallFromGit_RejectsUnsignedAllowlist(t *testing.T) {
	repoURL := newLocalGitFixture(t, fixtureFile{name: "rensei-example.kit.toml", body: minimalKitTOML})
	scan := t.TempDir()

	r := NewKitRegistryWithTrust([]string{scan}, TrustConfig{Mode: TrustModeSignedByAllowlist})
	_, err := r.Install("rensei/example", afclient.KitInstallRequest{
		Source: &afclient.KitInstallSource{Kind: "git", URL: repoURL},
	})
	if !errors.Is(err, ErrKitTrustGateRejected) {
		t.Fatalf("Install: want ErrKitTrustGateRejected, got %v", err)
	}

	// Nothing got persisted on rejection.
	if entries, _ := os.ReadDir(scan); len(entries) != 0 {
		t.Errorf("scan dir: want empty after rejection, got %d entries", len(entries))
	}
}

// ─────────────────────────────────────────────────────────────────────
//
//	install-handles-bundle-not-found-permissive
//
// ─────────────────────────────────────────────────────────────────────

func TestKitRegistry_InstallFromGit_BundleNotFoundPermissive(t *testing.T) {
	repoURL := newLocalGitFixture(t, fixtureFile{name: "rensei-example.kit.toml", body: minimalKitTOML})
	scan := t.TempDir()

	r := NewKitRegistryWithTrust([]string{scan}, TrustConfig{Mode: TrustModePermissive})
	res, err := r.Install("rensei/example", afclient.KitInstallRequest{
		Source: &afclient.KitInstallSource{Kind: "git", URL: repoURL},
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Kit.Trust != afclient.KitTrustUnsigned {
		t.Errorf("Trust: want unsigned for missing bundle in permissive mode, got %q", res.Kit.Trust)
	}

	// No sibling .sigstore was copied (because there wasn't one).
	bundle := filepath.Join(scan, "rensei__example.kit.toml.sigstore")
	if _, err := os.Stat(bundle); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("bundle %q: want non-existent, got %v", bundle, err)
	}
}

// ─────────────────────────────────────────────────────────────────────
//
//	install-rejects-federation-source
//
// ─────────────────────────────────────────────────────────────────────

func TestKitRegistry_InstallFromGit_RejectsFederationSource(t *testing.T) {
	scan := t.TempDir()
	r := NewKitRegistryWithTrust([]string{scan}, TrustConfig{Mode: TrustModePermissive})
	cases := []string{"tessl", "agentskills", "community"}
	for _, kind := range cases {
		t.Run(kind, func(t *testing.T) {
			_, err := r.Install("rensei/example", afclient.KitInstallRequest{
				Source: &afclient.KitInstallSource{Kind: kind, URL: "https://example.com/source"},
			})
			if !errors.Is(err, ErrKitSourceFederationUnimplemented) {
				t.Errorf("Install kind=%s: want ErrKitSourceFederationUnimplemented, got %v", kind, err)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
//
//	install-rejects-missing-manifest-after-clone
//
// ─────────────────────────────────────────────────────────────────────

func TestKitRegistry_InstallFromGit_RejectsMissingManifest(t *testing.T) {
	// Repo with a non-manifest file — the walker won't find a *.kit.toml.
	repoURL := newLocalGitFixture(t, fixtureFile{name: "README.md", body: "not a kit"})
	scan := t.TempDir()

	r := NewKitRegistryWithTrust([]string{scan}, TrustConfig{Mode: TrustModePermissive})
	_, err := r.Install("rensei/example", afclient.KitInstallRequest{
		Source: &afclient.KitInstallSource{Kind: "git", URL: repoURL},
	})
	if !errors.Is(err, ErrKitInstallManifestNotFound) {
		t.Fatalf("Install: want ErrKitInstallManifestNotFound, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────
//
//	install-trustOverride-allowed-this-once-bypasses-gate
//
// ─────────────────────────────────────────────────────────────────────

func TestKitRegistry_InstallFromGit_TrustOverrideBypassesGate(t *testing.T) {
	repoURL := newLocalGitFixture(t, fixtureFile{name: "rensei-example.kit.toml", body: minimalKitTOML})
	scan := t.TempDir()

	buf := captureSlogTrust(t)

	r := NewKitRegistryWithTrust([]string{scan}, TrustConfig{
		Mode:  TrustModeSignedByAllowlist,
		Actor: "operator@rensei.dev",
	})

	res, err := r.Install("rensei/example", afclient.KitInstallRequest{
		Source:        &afclient.KitInstallSource{Kind: "git", URL: repoURL},
		TrustOverride: afclient.TrustOverrideAllowedThisOnce,
	})
	if err != nil {
		t.Fatalf("Install with override: want success, got %v", err)
	}
	if res.Kit.ID != "rensei/example" {
		t.Errorf("Kit.ID: want rensei/example, got %q", res.Kit.ID)
	}

	// Audit log line emitted with the override-trail fields.
	if !strings.Contains(buf.String(), "trust gate bypassed") {
		t.Errorf("audit log: want 'trust gate bypassed' line, got %s", buf.String())
	}
	if !strings.Contains(buf.String(), "operator@rensei.dev") {
		t.Errorf("audit log: want actor 'operator@rensei.dev', got %s", buf.String())
	}

	// Manifest persisted despite the gate having rejected it under
	// signed-by-allowlist; this is the audited behaviour of
	// trustOverride: "allowed-this-once".
	want := filepath.Join(scan, "rensei__example.kit.toml")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("persisted manifest %q missing: %v", want, err)
	}
}

// ─────────────────────────────────────────────────────────────────────
//
//	install-handles-fetch-failure
//
// ─────────────────────────────────────────────────────────────────────

func TestKitRegistry_InstallFromGit_HandlesFetchFailure(t *testing.T) {
	scan := t.TempDir()
	r := NewKitRegistryWithTrust([]string{scan}, TrustConfig{Mode: TrustModePermissive})

	// Empty repo (no HEAD) — clone surfaces a remote-empty error.
	repoURL := newEmptyGitFixture(t)
	_, err := r.Install("rensei/example", afclient.KitInstallRequest{
		Source: &afclient.KitInstallSource{Kind: "git", URL: repoURL},
	})
	if !errors.Is(err, ErrKitInstallSourceFetchFailed) {
		t.Fatalf("Install: want ErrKitInstallSourceFetchFailed for empty repo, got %v", err)
	}
}

func TestKitRegistry_InstallFromGit_RejectsEmptySourceURL(t *testing.T) {
	scan := t.TempDir()
	r := NewKitRegistryWithTrust([]string{scan}, TrustConfig{Mode: TrustModePermissive})
	_, err := r.Install("rensei/example", afclient.KitInstallRequest{
		Source: &afclient.KitInstallSource{Kind: "git", URL: ""},
	})
	if !errors.Is(err, ErrKitInstallSourceFetchFailed) {
		t.Fatalf("Install: want ErrKitInstallSourceFetchFailed for empty URL, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────
//
//	install-rejects-unknown-source-kind
//
// ─────────────────────────────────────────────────────────────────────

func TestKitRegistry_InstallFromGit_RejectsUnknownSourceKind(t *testing.T) {
	scan := t.TempDir()
	r := NewKitRegistryWithTrust([]string{scan}, TrustConfig{Mode: TrustModePermissive})
	_, err := r.Install("rensei/example", afclient.KitInstallRequest{
		Source: &afclient.KitInstallSource{Kind: "wat", URL: "file:///irrelevant"},
	})
	// Unknown kind is a 400-level "bad request" — no sentinel since the
	// handler maps that to InternalServerError without a specific code
	// today. Just confirm the error message includes the kind.
	if err == nil {
		t.Fatalf("Install: want error for unknown source kind, got nil")
	}
	if errors.Is(err, ErrKitSourceFederationUnimplemented) {
		t.Errorf("Install: unknown kind 'wat' should NOT be classified as federation, got %v", err)
	}
	if !strings.Contains(err.Error(), "wat") {
		t.Errorf("Install: error should reference the unknown kind, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────
//
//	install-respects-explicit-manifestPath
//
// ─────────────────────────────────────────────────────────────────────

func TestKitRegistry_InstallFromGit_RespectsManifestPath(t *testing.T) {
	repoURL := newLocalGitFixture(t,
		fixtureFile{name: "README.md", body: "not a kit"},
		fixtureFile{name: "kits/rensei-example.kit.toml", body: minimalKitTOML},
	)
	scan := t.TempDir()
	r := NewKitRegistryWithTrust([]string{scan}, TrustConfig{Mode: TrustModePermissive})

	_, err := r.Install("rensei/example", afclient.KitInstallRequest{
		Source: &afclient.KitInstallSource{
			Kind:         "git",
			URL:          repoURL,
			ManifestPath: "kits/rensei-example.kit.toml",
		},
	})
	if err != nil {
		t.Fatalf("Install with explicit ManifestPath: %v", err)
	}

	// Manifest persisted under sanitized name in scanPaths[0].
	if _, err := os.Stat(filepath.Join(scan, "rensei__example.kit.toml")); err != nil {
		t.Errorf("persisted manifest missing: %v", err)
	}
}

func TestKitRegistry_InstallFromGit_RejectsTraversalManifestPath(t *testing.T) {
	repoURL := newLocalGitFixture(t,
		fixtureFile{name: "rensei-example.kit.toml", body: minimalKitTOML},
	)
	scan := t.TempDir()
	r := NewKitRegistryWithTrust([]string{scan}, TrustConfig{Mode: TrustModePermissive})

	_, err := r.Install("rensei/example", afclient.KitInstallRequest{
		Source: &afclient.KitInstallSource{
			Kind:         "git",
			URL:          repoURL,
			ManifestPath: "../escape.kit.toml",
		},
	})
	if !errors.Is(err, ErrKitInstallManifestNotFound) {
		t.Fatalf("Install: want ErrKitInstallManifestNotFound for traversal, got %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────
//
//	sanitizeKitFilename direct coverage
//
// ─────────────────────────────────────────────────────────────────────

func TestSanitizeKitFilename(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"spring/java", "spring__java"},
		{"a/b/c", "a__b__c"},
		{"plain", "plain"},
		{"", "kit"},
		{"with:colon", "with_colon"},
		{"win\\style", "win__style"},
		{"with\x00null", "withnull"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := sanitizeKitFilename(tc.in); got != tc.want {
				t.Errorf("sanitizeKitFilename(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
