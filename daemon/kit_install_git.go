// Package daemon kit_install_git.go — git-source kit fetcher (Wave 12 /
// Theme C / S3).
//
// The fetcher clones the operator-provided git URL into a temp directory,
// locates the kit manifest (and its sibling `.sigstore` bundle, when
// present), and exposes both as on-disk paths so KitRegistry.Install can
// run the trust-gated verifier against the freshly-fetched material
// before persisting it into kit.scanPaths[0].
//
// Design notes
//
//   - Uses go-git/v5 (pure-Go) so the daemon does not depend on a
//     `git` binary on the operator's PATH. Public-host or file:// URLs
//     are both accepted; tests rely on file:// fixtures.
//   - When KitInstallSource.ManifestPath is empty the fetcher walks the
//     cloned tree for *.kit.toml files and selects the first one that
//     parses cleanly. This matches the audit § 2.1 step 3 contract:
//     "walk repo for *.kit.toml, pick the first; multi-manifest support
//     is a Wave 13+ extension per 005-kit-manifest-spec.md".
//   - Caller MUST defer the returned cleanup func; the temp tree is
//     persisted only long enough for the registry to copy what it needs
//     into the configured scanPath.
//
// Errors
//
//   - ErrKitInstallSourceFetchFailed — clone failed (network, auth, ref
//     not found, etc.). Wrapped with the underlying go-git error.
//   - ErrKitInstallManifestNotFound  — clone succeeded but no usable
//     `*.kit.toml` exists at the configured ManifestPath (or anywhere in
//     the tree when ManifestPath was empty).
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// kitSourceFetcher is the registry's fetch seam. Production wires the
// go-git-backed gitKitFetcher; tests can inject a fake that produces
// fixtures (including signed-bundle fixtures generated via the
// hermetic VirtualSigstore CA) without going through `git clone`.
type kitSourceFetcher interface {
	Fetch(ctx context.Context, source afclient.KitInstallSource) (*fetchedKit, func(), error)
}

// gitKitFetcher clones a git source URL into a temp directory and
// surfaces the path to the kit manifest (with optional sibling
// `.sigstore` bundle) so the registry can verify-then-persist.
type gitKitFetcher struct{}

// newGitKitFetcher constructs a fetcher. There is no per-instance state
// today; the type exists so tests that want to swap in a faux fetcher
// (Wave 13+) have a seam to substitute.
func newGitKitFetcher() *gitKitFetcher { return &gitKitFetcher{} }

// fetchedKit is the result of a successful Fetch. Paths are absolute
// inside the cloned tempDir; cleanup tears the whole tree down.
type fetchedKit struct {
	// ManifestPath is the absolute path to the cloned kit's manifest
	// file. The verifier reads this and looks for the sibling
	// `<ManifestPath>.sigstore` bundle automatically.
	ManifestPath string

	// HasBundle is true when a sibling `<ManifestPath>.sigstore` was
	// present in the cloned tree. The registry consults this to decide
	// whether to copy the bundle alongside the persisted manifest.
	HasBundle bool

	// TempDir is the root of the cloned repository. Exposed so callers
	// can resolve paths relative to it (e.g., the .sigstore sibling).
	TempDir string

	// Entity is an in-memory SignedEntity representation of the bundle.
	// Production fetchers leave this nil — the registry reads the
	// sibling .sigstore from disk in that case. Tests inject this
	// (e.g., from a hermetic VirtualSigstore TestEntity) so the trust
	// gate can be exercised end-to-end without round-tripping the
	// bundle through protojson serialisation. When non-nil, the
	// registry calls verifier.verifyEntity instead of VerifyManifest.
	Entity verify.SignedEntity
}

// Fetch clones source.URL @ source.Ref into a fresh temp directory and
// resolves the manifest path. Returns a fetchedKit handle plus a
// cleanup func; callers MUST defer the cleanup to avoid leaking temp
// directories.
func (f *gitKitFetcher) Fetch(ctx context.Context, source afclient.KitInstallSource) (*fetchedKit, func(), error) {
	if source.URL == "" {
		return nil, func() {}, fmt.Errorf("%w: source URL empty", ErrKitInstallSourceFetchFailed)
	}

	tempDir, err := os.MkdirTemp("", "rensei-kit-install-")
	if err != nil {
		return nil, func() {}, fmt.Errorf("%w: temp dir: %w", ErrKitInstallSourceFetchFailed, err)
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }

	cloneOpts := &gogit.CloneOptions{
		URL:          source.URL,
		SingleBranch: source.Ref != "",
		Depth:        1,
	}
	if source.Ref != "" {
		cloneOpts.ReferenceName = plumbing.ReferenceName(source.Ref)
		// Allow short refs — let go-git resolve unqualified branch / tag
		// names without the operator having to write
		// "refs/heads/<branch>".
		if !strings.HasPrefix(source.Ref, "refs/") {
			cloneOpts.ReferenceName = plumbing.NewBranchReferenceName(source.Ref)
		}
	}

	if _, err := gogit.PlainCloneContext(ctx, tempDir, false, cloneOpts); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("%w: clone %s: %w", ErrKitInstallSourceFetchFailed, source.URL, err)
	}

	manifestPath, err := resolveManifestPath(tempDir, source.ManifestPath)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}

	bundlePath := manifestPath + ".sigstore"
	hasBundle := false
	if _, err := os.Stat(bundlePath); err == nil {
		hasBundle = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		// Non-not-found stat error is unusual; surface it but don't fail
		// the fetch — the verifier reports SignedUnverified in that case.
		hasBundle = false
	}

	return &fetchedKit{
		ManifestPath: manifestPath,
		HasBundle:    hasBundle,
		TempDir:      tempDir,
	}, cleanup, nil
}

// resolveManifestPath finds the manifest file inside cloneDir.
//
// When manifestPath is non-empty, the operator told us where the
// manifest lives — we resolve it relative to cloneDir and confirm the
// file exists. When manifestPath is empty we walk the tree for the
// first `*.kit.toml` file (per audit § 2.1 step 3).
//
// Path traversal protection: a non-empty manifestPath that resolves
// outside cloneDir is rejected as ErrKitInstallManifestNotFound rather
// than ErrKitInstallSourceFetchFailed — operators occasionally pass
// "../../../etc/passwd"-style paths and the daemon should treat that
// as "no manifest here" rather than a fetch failure.
func resolveManifestPath(cloneDir, manifestPath string) (string, error) {
	if manifestPath != "" {
		clean := filepath.Clean(manifestPath)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			return "", fmt.Errorf("%w: manifestPath %q escapes source root", ErrKitInstallManifestNotFound, manifestPath)
		}
		full := filepath.Join(cloneDir, clean)
		// Defence-in-depth: confirm the cleaned path stays under cloneDir
		// even after Join's normalisation. (filepath.Join can resolve
		// `a/../b` cases that filepath.Clean missed.)
		if rel, relErr := filepath.Rel(cloneDir, full); relErr != nil || strings.HasPrefix(rel, "..") {
			return "", fmt.Errorf("%w: manifestPath %q escapes source root", ErrKitInstallManifestNotFound, manifestPath)
		}
		info, err := os.Stat(full)
		if err != nil || info.IsDir() {
			return "", fmt.Errorf("%w: %s", ErrKitInstallManifestNotFound, manifestPath)
		}
		return full, nil
	}

	// Walk for the first *.kit.toml. Sorted via filepath.WalkDir's
	// lexical iteration so behaviour is deterministic across runs.
	var found string
	walkErr := filepath.WalkDir(cloneDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip the .git folder for performance — never holds manifests.
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".kit.toml") && found == "" {
			found = path
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("%w: walk clone tree: %w", ErrKitInstallManifestNotFound, walkErr)
	}
	if found == "" {
		return "", fmt.Errorf("%w: no *.kit.toml found in cloned source", ErrKitInstallManifestNotFound)
	}
	return found, nil
}
