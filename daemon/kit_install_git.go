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
//   - Resolution is identity-based. A complete matching package descriptor
//     wins; duplicate matching descriptors fail as equivocation. Legacy
//     manifest fallback happens only when the source has no descriptors and
//     exactly one manifest matches the requested id/version.
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

	"github.com/RenseiAI/donmai/afclient"
)

// kitSourceFetcher is the registry's fetch seam. Production wires the
// go-git-backed gitKitFetcher; tests can inject a fake that produces
// fixtures (including signed-bundle fixtures generated via the
// hermetic VirtualSigstore CA) without going through `git clone`.
type kitSourceFetcher interface {
	Fetch(ctx context.Context, source afclient.KitInstallSource, kitID, version string) (*fetchedKit, func(), error)
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
	// DescriptorPath is populated only for a complete package source. Its
	// absence means the fetch resolved an explicit legacy manifest.
	DescriptorPath string

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
	// PackageEntity is the descriptor-signature test seam. Production loads
	// kit.package.json.sigstore from the exact fetched package directory.
	PackageEntity verify.SignedEntity
}

// Fetch clones source.URL @ source.Ref into a fresh temp directory and
// resolves the manifest path. Returns a fetchedKit handle plus a
// cleanup func; callers MUST defer the cleanup to avoid leaking temp
// directories.
func (f *gitKitFetcher) Fetch(ctx context.Context, source afclient.KitInstallSource, kitID, version string) (*fetchedKit, func(), error) {
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

	manifestPath, descriptorPath, err := resolveKitSourcePaths(tempDir, source.ManifestPath, kitID, version)
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
		ManifestPath:   manifestPath,
		DescriptorPath: descriptorPath,
		HasBundle:      hasBundle,
		TempDir:        tempDir,
	}, cleanup, nil
}

// resolveKitSourcePaths resolves exactly the requested identity. Complete
// package descriptors are preferred and never selected by directory order.
// Legacy fallback is allowed only when the source contains no descriptors.
func resolveKitSourcePaths(cloneDir, requestedPath, kitID, version string) (string, string, error) {
	if requestedPath != "" {
		resolved, err := resolveManifestPath(cloneDir, requestedPath)
		if err != nil {
			return "", "", err
		}
		if filepath.Base(resolved) == kitPackageDescriptorName {
			descriptor, err := loadDescriptorForResolution(resolved)
			if err != nil {
				return "", "", err
			}
			if err := requireResolvedIdentity(descriptor, kitID, version); err != nil {
				return "", "", err
			}
			return filepath.Join(filepath.Dir(resolved), filepath.FromSlash(descriptor.Manifest)), resolved, nil
		}
		descriptorPath := filepath.Join(filepath.Dir(resolved), kitPackageDescriptorName)
		if info, err := os.Lstat(descriptorPath); err == nil && info.Mode().IsRegular() {
			descriptor, parseErr := loadDescriptorForResolution(descriptorPath)
			if parseErr != nil {
				return "", "", parseErr
			}
			if err := requireResolvedIdentity(descriptor, kitID, version); err != nil {
				return "", "", err
			}
			if filepath.Clean(resolved) != filepath.Join(filepath.Dir(descriptorPath), filepath.FromSlash(descriptor.Manifest)) {
				return "", "", fmt.Errorf("%w: requested manifest is not the package descriptor manifest", ErrKitInstallManifestNotFound)
			}
			return resolved, descriptorPath, nil
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("%w: inspect package descriptor: %v", ErrKitInstallManifestNotFound, err)
		}
		manifest, err := loadKitManifestFile(resolved)
		if err != nil || manifest.Kit.ID != kitID || (version != "" && manifest.Kit.Version != version) {
			return "", "", fmt.Errorf("%w: explicit legacy manifest identity does not match request", ErrKitInstallManifestNotFound)
		}
		descriptors, err := discoverPackageDescriptors(cloneDir)
		if err != nil {
			return "", "", err
		}
		if len(descriptors) > 0 {
			return "", "", fmt.Errorf("%w: explicit legacy manifest cannot downgrade a descriptor-bearing source", ErrKitInstallManifestNotFound)
		}
		return resolved, "", nil
	}

	descriptors, err := discoverPackageDescriptors(cloneDir)
	if err != nil {
		return "", "", err
	}
	var matches []string
	digests := map[string]struct{}{}
	for _, descriptorPath := range descriptors {
		descriptor, err := loadDescriptorForResolution(descriptorPath)
		if err != nil {
			return "", "", err
		}
		if descriptor.Kit.ID != kitID || (version != "" && descriptor.Kit.Version != version) {
			continue
		}
		raw, err := os.ReadFile(descriptorPath) //nolint:gosec // cloned source path
		if err != nil {
			return "", "", fmt.Errorf("%w: read descriptor: %v", ErrKitInstallManifestNotFound, err)
		}
		digests[sha256Hex(raw)] = struct{}{}
		matches = append(matches, descriptorPath)
	}
	if len(matches) > 1 || len(digests) > 1 {
		return "", "", fmt.Errorf("%w: source has %d package candidates for %s@%s", ErrKitPackageEquivocation, len(matches), kitID, version)
	}
	if len(matches) == 1 {
		descriptor, _ := loadDescriptorForResolution(matches[0])
		return filepath.Join(filepath.Dir(matches[0]), filepath.FromSlash(descriptor.Manifest)), matches[0], nil
	}
	if len(descriptors) > 0 {
		return "", "", fmt.Errorf("%w: no package descriptor matches %s@%s", ErrKitInstallManifestNotFound, kitID, version)
	}

	// Explicit legacy compatibility: select by parsed identity, never first
	// parseable or scan order.
	var legacy []string
	err = filepath.WalkDir(cloneDir, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return fs.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".kit.toml") {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		manifest, err := loadKitManifestFile(name)
		if err == nil && manifest.Kit.ID == kitID && (version == "" || manifest.Kit.Version == version) {
			legacy = append(legacy, name)
		}
		return nil
	})
	if err != nil {
		return "", "", fmt.Errorf("%w: walk legacy manifests: %v", ErrKitInstallManifestNotFound, err)
	}
	if len(legacy) != 1 {
		return "", "", fmt.Errorf("%w: expected exactly one legacy manifest for %s@%s, found %d", ErrKitInstallManifestNotFound, kitID, version, len(legacy))
	}
	return legacy[0], "", nil
}

func discoverPackageDescriptors(cloneDir string) ([]string, error) {
	var descriptors []string
	err := filepath.WalkDir(cloneDir, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return fs.SkipDir
		}
		if !entry.IsDir() && entry.Name() == kitPackageDescriptorName {
			descriptors = append(descriptors, name)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%w: walk package descriptors: %v", ErrKitInstallManifestNotFound, err)
	}
	return descriptors, nil
}

func loadDescriptorForResolution(name string) (kitPackageDescriptor, error) {
	info, err := os.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || nlink(info) != 1 {
		return kitPackageDescriptor{}, fmt.Errorf("%w: descriptor %q is not a regular unlinked file", ErrKitInstallManifestNotFound, name)
	}
	raw, err := os.ReadFile(name) //nolint:gosec // cloned source path
	if err != nil {
		return kitPackageDescriptor{}, fmt.Errorf("%w: read descriptor: %v", ErrKitInstallManifestNotFound, err)
	}
	descriptor, err := parseKitPackageDescriptor(raw, defaultKitPackageLimits())
	if err != nil {
		return kitPackageDescriptor{}, fmt.Errorf("%w: %v", ErrKitInstallManifestNotFound, err)
	}
	return descriptor, nil
}

func requireResolvedIdentity(descriptor kitPackageDescriptor, kitID, version string) error {
	if descriptor.Kit.ID != kitID || (version != "" && descriptor.Kit.Version != version) {
		return fmt.Errorf("%w: descriptor identity %s@%s does not match request %s@%s", ErrKitInstallManifestNotFound, descriptor.Kit.ID, descriptor.Kit.Version, kitID, version)
	}
	return nil
}

// resolveManifestPath finds the manifest file inside cloneDir.
//
// When manifestPath is non-empty, the operator told us where the
// manifest lives — we resolve it relative to cloneDir and confirm the
// file exists. The package-aware caller performs identity-based discovery when
// manifestPath is empty; the local fallback below exists only for legacy
// helper compatibility.
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
		root, rootErr := os.OpenRoot(cloneDir)
		if rootErr != nil {
			return "", fmt.Errorf("%w: open source root: %v", ErrKitInstallManifestNotFound, rootErr)
		}
		defer func() { _ = root.Close() }()
		if err := ensureNoLinkComponents(root, filepath.ToSlash(clean)); err != nil {
			return "", fmt.Errorf("%w: %s: %v", ErrKitInstallManifestNotFound, manifestPath, err)
		}
		info, err := root.Lstat(filepath.ToSlash(clean))
		if err != nil || info.IsDir() || !info.Mode().IsRegular() || nlink(info) != 1 {
			return "", fmt.Errorf("%w: %s", ErrKitInstallManifestNotFound, manifestPath)
		}
		return full, nil
	}

	// Compatibility helper for callers that do not supply an identity. The
	// package-aware installer never uses this branch; resolveKitSourcePaths
	// performs exact identity resolution instead.
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
