package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/RenseiAI/donmai/afclient"
)

const containedLegacyManifest = `api = "donmai.dev/v1"
[kit]
id = "example/kit"
version = "1.0.0"
name = "Example kit"
`

func TestLegacyKitVerifierReleasedBundle(t *testing.T) {
	verifier, err := newKitVerifier(TrustConfig{Mode: TrustModeSignedByAllowlist, IssuerSet: defaultVendorIssuerSet()})
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join("testdata", "released-kit-packages", "go", "kit.toml")
	result, err := verifier.VerifyManifest("default/go", manifest)
	if err != nil || result.Trust != afclient.KitTrustSignedVerified {
		t.Fatalf("released legacy signature rejected: trust=%s details=%s err=%v", result.Trust, result.Details, err)
	}
}

func TestLegacyKitVerifierRejectsUnsafeBundle(t *testing.T) {
	for _, kind := range []string{"symlink", "dangling-symlink", "hardlink", "directory", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			manifest := filepath.Join(dir, "example.kit.toml")
			if err := os.WriteFile(manifest, []byte(containedLegacyManifest), 0o600); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "synthetic-bundle")
			if err := os.WriteFile(outside, []byte("synthetic outside sentinel"), 0o600); err != nil {
				t.Fatal(err)
			}
			bundle := manifest + ".sigstore"
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(outside, bundle)
			case "dangling-symlink":
				err = os.Symlink(outside+"-absent", bundle)
			case "hardlink":
				err = os.Link(outside, bundle)
			case "directory":
				err = os.Mkdir(bundle, 0o700)
			case "oversized":
				err = os.WriteFile(bundle, []byte(strings.Repeat("x", int(defaultKitPackageLimits().MaxSignatureBytes)+1)), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			verifier, _ := newHermeticVerifier(t, TrustModeSignedByAllowlist)
			if result, err := verifier.VerifyManifest("example/kit", manifest); err == nil {
				t.Fatalf("unsafe bundle reached trust parsing: trust=%s details=%s", result.Trust, result.Details)
			}
			if data, err := os.ReadFile(outside); err != nil || string(data) != "synthetic outside sentinel" {
				t.Fatalf("outside sentinel changed: %q, %v", data, err)
			}
		})
	}
}

func TestLegacyKitFetchRejectsSiblingBundleLink(t *testing.T) {
	repoURL := newLocalGitFixture(t, fixtureFile{name: "example.kit.toml", body: containedLegacyManifest})
	repoDir := strings.TrimPrefix(repoURL, "file://")
	outside := filepath.Join(t.TempDir(), "synthetic-bundle")
	if err := os.WriteFile(outside, []byte("synthetic outside sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repoDir, "example.kit.toml.sigstore")); err != nil {
		t.Fatal(err)
	}
	repo, err := gogit.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Add("example.kit.toml.sigstore"); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Commit("add synthetic linked bundle", &gogit.CommitOptions{Author: &object.Signature{
		Name: "Fixture", Email: "fixture@example.com", When: time.Unix(1, 0),
	}}); err != nil {
		t.Fatal(err)
	}
	for _, manifestPath := range []string{"", "example.kit.toml"} {
		t.Run("manifest="+manifestPath, func(t *testing.T) {
			_, cleanup, err := newGitKitFetcher().Fetch(context.Background(), afclient.KitInstallSource{
				Kind: "git", URL: repoURL, ManifestPath: manifestPath,
			}, "example/kit", "1.0.0")
			t.Cleanup(cleanup)
			if err == nil {
				t.Fatal("fetch accepted a sibling signature link outside the cloned source")
			}
		})
	}
}

func TestLegacyKitCopyRejectsLinkedSource(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "synthetic-bundle")
	if err := os.WriteFile(outside, []byte("synthetic outside sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "example.kit.toml.sigstore")
	if err := os.Symlink(outside, source); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "installed.sigstore")
	if err := atomicCopyFile(source, destination); err == nil {
		t.Fatal("copy accepted linked source")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("refused source was published: %v", err)
	}
}
