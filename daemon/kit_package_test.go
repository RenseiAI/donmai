package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"

	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"

	"github.com/RenseiAI/donmai/afclient"
)

var releasedKitIDs = map[string]string{
	"go":         "default/go",
	"java":       "default/java",
	"python":     "default/python",
	"ruby":       "default/ruby",
	"rust":       "default/rust",
	"ts-nextjs":  "default/ts-nextjs",
	"typescript": "default/typescript",
}

type packageFixtureFetcher struct {
	roots map[string]string
}

func (f *packageFixtureFetcher) Fetch(_ context.Context, _ afclient.KitInstallSource, kitID, _ string) (*fetchedKit, func(), error) {
	root := f.roots[kitID]
	if root == "" {
		return nil, func() {}, fmt.Errorf("fixture missing for %s", kitID)
	}
	return &fetchedKit{
		ManifestPath:   filepath.Join(root, "kit.toml"),
		DescriptorPath: filepath.Join(root, kitPackageDescriptorName),
		TempDir:        root,
	}, func() {}, nil
}

func newPackageTestRegistry(t *testing.T, roots map[string]string) *KitRegistry {
	t.Helper()
	registry := NewKitRegistryWithTrust([]string{t.TempDir()}, TrustConfig{
		Mode:      TrustModeSignedByAllowlist,
		IssuerSet: defaultVendorIssuerSet(),
	})
	registry.fetcher = &packageFixtureFetcher{roots: roots}
	return registry
}

func TestReleasedKitPackagesVerifyAndInstall(t *testing.T) {
	names := make([]string, 0, len(releasedKitIDs))
	for name := range releasedKitIDs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		name := name
		t.Run(name, func(t *testing.T) {
			root := filepath.Join("testdata", "released-kit-packages", name)
			registry := newPackageTestRegistry(t, map[string]string{releasedKitIDs[name]: root})
			verified, err := registry.verifyKitPackage(root, kitPackageDescriptorName, releasedKitIDs[name], "1.0.0", "", nil)
			if err != nil {
				t.Fatalf("verify released package: %v", err)
			}
			if verified.Signature.Trust != afclient.KitTrustPackageVerified {
				t.Fatalf("trust = %q, want package-verified (details=%q signer=%q)", verified.Signature.Trust, verified.Signature.Details, verified.Signature.SignerID)
			}
			result, err := registry.Install(releasedKitIDs[name], afclient.KitInstallRequest{
				Version: "1.0.0",
				Source:  &afclient.KitInstallSource{Kind: "git", URL: "fixture"},
			})
			if err != nil {
				t.Fatalf("install released package: %v", err)
			}
			if result.Kit.InstallKind != afclient.KitInstallKindPackage || result.Kit.Trust != afclient.KitTrustPackageVerified || len(result.Kit.PackageDigest) != 64 {
				t.Fatalf("install evidence = %+v", result.Kit)
			}
			listed := registry.List()
			if len(listed) != 1 || listed[0].ID != releasedKitIDs[name] || listed[0].PackageDigest != result.Kit.PackageDigest {
				t.Fatalf("active registry = %+v", listed)
			}
		})
	}
}

func TestReleasedKitPackageInstallsThroughRealGitFetcher(t *testing.T) {
	fixtureRoot := filepath.Join("testdata", "released-kit-packages", "go")
	var files []fixtureFile
	if err := filepath.WalkDir(fixtureRoot, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(fixtureRoot, name)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(name) //nolint:gosec // immutable test fixture
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files = append(files, fixtureFile{name: rel, body: string(data), mode: info.Mode().Perm()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	repoURL := newLocalGitFixture(t, files...)
	registry := NewKitRegistryWithTrust([]string{t.TempDir()}, TrustConfig{Mode: TrustModeSignedByAllowlist, IssuerSet: defaultVendorIssuerSet()})
	result, err := registry.Install("default/go", afclient.KitInstallRequest{
		Version: "1.0.0",
		Source:  &afclient.KitInstallSource{Kind: "git", URL: repoURL},
	})
	if err != nil {
		t.Fatalf("real git install: %v", err)
	}
	if result.Kit.Trust != afclient.KitTrustPackageVerified || result.Kit.InstallKind != afclient.KitInstallKindPackage {
		t.Fatalf("real git install evidence = %+v", result.Kit)
	}
}

func TestKitPackageParsesExactVerifiedManifestSnapshot(t *testing.T) {
	root := copyReleasedFixture(t, "go")
	original, err := os.ReadFile(filepath.Join(root, "kit.toml"))
	if err != nil {
		t.Fatal(err)
	}
	registry := newPackageTestRegistry(t, map[string]string{"default/go": root})
	var once sync.Once
	registry.packageAfterInventory = func() {
		once.Do(func() {
			mutated := bytes.Replace(original, []byte(`name = "Go"`), []byte(`name = "MUTATED AFTER VERIFICATION"`), 1)
			mustWrite(t, filepath.Join(root, "kit.toml"), mutated, 0o644)
		})
	}
	result, err := registry.Install("default/go", afclient.KitInstallRequest{Version: "1.0.0", Source: &afclient.KitInstallSource{Kind: "git", URL: "fixture"}})
	if err != nil {
		t.Fatalf("install under source mutation: %v", err)
	}
	if strings.Contains(result.Kit.Name, "MUTATED") {
		t.Fatalf("install parsed mutable source bytes: %+v", result.Kit)
	}
	stored, err := os.ReadFile(filepath.Join(registry.packageStoreRoot(), "packages", "sha256", result.Kit.PackageDigest, "kit.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, original) {
		t.Fatal("immutable package did not retain the exact verified manifest snapshot")
	}
}

func TestKitPackageTamperAndClosureFailuresLeaveGenerationUnchanged(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{"payload-tamper", func(t *testing.T, root string) {
			appendFile(t, filepath.Join(root, "partials", "go-conventions.yaml"), []byte("tamper"))
		}},
		{"missing", func(t *testing.T, root string) {
			mustRemove(t, filepath.Join(root, "skills", "go-debugging", "SKILL.md"))
		}},
		{"extra", func(t *testing.T, root string) {
			mustWrite(t, filepath.Join(root, "extra.txt"), []byte("extra"), 0o644)
		}},
		{"mode", func(t *testing.T, root string) {
			mustChmod(t, filepath.Join(root, "partials", "go-conventions.yaml"), 0o755)
		}},
		{"symlink", func(t *testing.T, root string) {
			mustRemove(t, filepath.Join(root, "kit.toml"))
			if err := os.Symlink("partials/go-conventions.yaml", filepath.Join(root, "kit.toml")); err != nil {
				t.Fatal(err)
			}
		}},
		{"hardlink", func(t *testing.T, root string) {
			original := filepath.Join(root, "kit.toml")
			linked := filepath.Join(root, "kit-linked.toml")
			if err := os.Link(original, linked); err != nil {
				t.Fatal(err)
			}
		}},
		{"special", func(t *testing.T, root string) {
			if runtime.GOOS == "windows" {
				t.Skip("mkfifo unavailable")
			}
			mustRemove(t, filepath.Join(root, "kit.toml"))
			if err := syscall.Mkfifo(filepath.Join(root, "kit.toml"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := copyReleasedFixture(t, "go")
			test.mutate(t, root)
			registry := newPackageTestRegistry(t, map[string]string{"default/go": root})
			_, err := registry.Install("default/go", afclient.KitInstallRequest{Version: "1.0.0", Source: &afclient.KitInstallSource{Kind: "git", URL: "fixture"}})
			if !errors.Is(err, ErrKitPackageInvalid) {
				t.Fatalf("install error = %v, want ErrKitPackageInvalid", err)
			}
			if kits := registry.List(); len(kits) != 0 {
				t.Fatalf("failed install exposed kits: %+v", kits)
			}
		})
	}
}

func TestKitPackageSignatureFailureIsNotPackageVerified(t *testing.T) {
	root := copyReleasedFixture(t, "go")
	mustWrite(t, filepath.Join(root, kitPackageSignatureName), []byte(`{"invalid":true}`), 0o644)
	registry := newPackageTestRegistry(t, map[string]string{"default/go": root})
	_, err := registry.Install("default/go", afclient.KitInstallRequest{Source: &afclient.KitInstallSource{Kind: "git", URL: "fixture"}})
	if !errors.Is(err, ErrKitTrustGateRejected) {
		t.Fatalf("install error = %v, want trust rejection", err)
	}
	if kits := registry.List(); len(kits) != 0 {
		t.Fatalf("signature rejection exposed kits: %+v", kits)
	}
}

func TestActivePackageTamperFailsClosedWithoutLegacyFallback(t *testing.T) {
	root := filepath.Join("testdata", "released-kit-packages", "go")
	registry := newPackageTestRegistry(t, map[string]string{"default/go": root})
	result, err := registry.Install("default/go", afclient.KitInstallRequest{Version: "1.0.0", Source: &afclient.KitInstallSource{Kind: "git", URL: "fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	activePayload := filepath.Join(registry.packageStoreRoot(), "packages", "sha256", result.Kit.PackageDigest, "partials", "go-conventions.yaml")
	appendFile(t, activePayload, []byte("tamper after activation"))
	// A flat manifest with the same id must not impersonate the generation's
	// reserved package identity when re-verification fails.
	legacy := strings.Replace(minimalKitTOML, "rensei/example", "default/go", 1)
	legacy = strings.Replace(legacy, "0.1.0", "1.0.0", 1)
	mustWrite(t, filepath.Join(registry.scanPaths[0], "default__go.kit.toml"), []byte(legacy), 0o600)
	if kits := registry.List(); len(kits) != 0 {
		t.Fatalf("tampered active package or legacy fallback became visible: %+v", kits)
	}
	verification, err := registry.VerifySignature("default/go")
	if err != nil || verification.OK || verification.Trust != afclient.KitTrustPackageSignedUnverified || verification.PackageDigest != result.Kit.PackageDigest {
		t.Fatalf("tampered package verification evidence = %+v err=%v", verification, err)
	}
}

func TestActivePackageReevaluatesCurrentTrustPolicy(t *testing.T) {
	root := filepath.Join("testdata", "released-kit-packages", "go")
	registry := newPackageTestRegistry(t, map[string]string{"default/go": root})
	if _, err := registry.Install("default/go", afclient.KitInstallRequest{Version: "1.0.0", Source: &afclient.KitInstallSource{Kind: "git", URL: "fixture"}}); err != nil {
		t.Fatal(err)
	}
	verifier, err := newKitVerifier(TrustConfig{Mode: TrustModeSignedByAllowlist, IssuerSet: []string{"different-signer@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	registry.verifier = verifier
	if kits := registry.List(); len(kits) != 0 {
		t.Fatalf("package stayed active after signer policy removal: %+v", kits)
	}
}

func TestKitPackageDescriptorPathAndLimitVectors(t *testing.T) {
	root := copyReleasedFixture(t, "go")
	raw, err := os.ReadFile(filepath.Join(root, kitPackageDescriptorName))
	if err != nil {
		t.Fatal(err)
	}
	var base kitPackageDescriptor
	if err := json.Unmarshal(raw, &base); err != nil {
		t.Fatal(err)
	}
	vectors := []string{
		"../escape", "/absolute", `C:/drive`, `server\\share`, "a//b", "a/./b", "a/../b", "a:stream", "trail. ", "CON.txt", "com¹.log", "kit.package.json", ".git/config", "bad\x01name",
	}
	for _, vector := range vectors {
		t.Run(strings.ReplaceAll(vector, "/", "_"), func(t *testing.T) {
			descriptor := base
			descriptor.Entries = append([]kitPackageEntry(nil), base.Entries...)
			descriptor.Entries[0].Path = vector
			sort.Slice(descriptor.Entries, func(i, j int) bool { return descriptor.Entries[i].Path < descriptor.Entries[j].Path })
			encoded, err := json.Marshal(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := jsoncanonicalizer.Transform(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseKitPackageDescriptor(canonical, defaultKitPackageLimits()); !errors.Is(err, ErrKitPackageInvalid) {
				t.Fatalf("vector %q error = %v", vector, err)
			}
		})
	}

	tooFew := defaultKitPackageLimits()
	tooFew.MaxFiles = len(base.Entries) - 1
	if _, err := parseKitPackageDescriptor(raw, tooFew); !errors.Is(err, ErrKitPackageInvalid) {
		t.Fatalf("file-count limit error = %v", err)
	}
	tiny := defaultKitPackageLimits()
	tiny.MaxTotalBytes = 1
	if _, err := parseKitPackageDescriptor(raw, tiny); !errors.Is(err, ErrKitPackageInvalid) {
		t.Fatalf("total-size limit error = %v", err)
	}
}

func TestKitPackagePortableCollision(t *testing.T) {
	root := copyReleasedFixture(t, "go")
	raw, _ := os.ReadFile(filepath.Join(root, kitPackageDescriptorName))
	var descriptor kitPackageDescriptor
	if err := json.Unmarshal(raw, &descriptor); err != nil {
		t.Fatal(err)
	}
	clone := descriptor.Entries[0]
	clone.Path = strings.ToUpper(clone.Path)
	descriptor.Entries = append(descriptor.Entries, clone)
	sort.Slice(descriptor.Entries, func(i, j int) bool { return descriptor.Entries[i].Path < descriptor.Entries[j].Path })
	encoded, _ := json.Marshal(descriptor)
	canonical, _ := jsoncanonicalizer.Transform(encoded)
	if _, err := parseKitPackageDescriptor(canonical, defaultKitPackageLimits()); !errors.Is(err, ErrKitPackageInvalid) {
		t.Fatalf("portable collision error = %v", err)
	}
}

func TestKitPackageDescriptorRejectsDuplicateJSONMember(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "released-kit-packages", "go", kitPackageDescriptorName))
	if err != nil {
		t.Fatal(err)
	}
	duplicate := bytes.Replace(raw, []byte(`{"entries":`), []byte(`{"entries":[],"entries":`), 1)
	if _, err := parseKitPackageDescriptor(duplicate, defaultKitPackageLimits()); !errors.Is(err, ErrKitPackageInvalid) {
		t.Fatalf("duplicate JSON member error = %v", err)
	}
}

func TestKitPackageConcurrentActivationAndRollback(t *testing.T) {
	roots := map[string]string{
		"default/go":     filepath.Join("testdata", "released-kit-packages", "go"),
		"default/python": filepath.Join("testdata", "released-kit-packages", "python"),
	}
	registry := newPackageTestRegistry(t, roots)
	var wait sync.WaitGroup
	errCh := make(chan error, len(roots))
	for id := range roots {
		id := id
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := registry.Install(id, afclient.KitInstallRequest{Version: "1.0.0", Source: &afclient.KitInstallSource{Kind: "git", URL: "fixture"}})
			errCh <- err
		}()
	}
	wait.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent install: %v", err)
		}
	}
	if kits := registry.List(); len(kits) != 2 {
		t.Fatalf("concurrent generation kits = %+v", kits)
	}
	if err := registry.RollbackPackages(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if kits := registry.List(); len(kits) != 1 {
		t.Fatalf("rollback generation kits = %+v", kits)
	}
}

func TestKitPackageCrossRegistryRaceSerializesOnStoreLock(t *testing.T) {
	scan := t.TempDir()
	newRegistry := func(id, root string) *KitRegistry {
		registry := NewKitRegistryWithTrust([]string{scan}, TrustConfig{Mode: TrustModeSignedByAllowlist, IssuerSet: defaultVendorIssuerSet()})
		registry.fetcher = &packageFixtureFetcher{roots: map[string]string{id: root}}
		return registry
	}
	registries := map[string]*KitRegistry{
		"default/go":     newRegistry("default/go", filepath.Join("testdata", "released-kit-packages", "go")),
		"default/python": newRegistry("default/python", filepath.Join("testdata", "released-kit-packages", "python")),
	}
	start := make(chan struct{})
	errCh := make(chan error, len(registries))
	for id, registry := range registries {
		id, registry := id, registry
		go func() {
			<-start
			_, err := registry.Install(id, afclient.KitInstallRequest{Version: "1.0.0", Source: &afclient.KitInstallSource{Kind: "git", URL: "fixture"}})
			errCh <- err
		}()
	}
	close(start)
	for range registries {
		if err := <-errCh; err != nil {
			t.Fatalf("cross-registry activation: %v", err)
		}
	}
	if kits := registries["default/go"].List(); len(kits) != 2 {
		t.Fatalf("cross-registry generation lost an update: %+v", kits)
	}
}

func TestKitPackageResolutionIsIdentityBoundAndFailClosed(t *testing.T) {
	source := t.TempDir()
	if err := copyFixtureTree(filepath.Join("testdata", "released-kit-packages", "go"), filepath.Join(source, "a")); err != nil {
		t.Fatal(err)
	}
	if err := copyFixtureTree(filepath.Join("testdata", "released-kit-packages", "python"), filepath.Join(source, "b")); err != nil {
		t.Fatal(err)
	}
	manifest, descriptor, err := resolveKitSourcePaths(source, "", "default/python", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(manifest) != filepath.Dir(descriptor) || filepath.Base(filepath.Dir(descriptor)) != "b" {
		t.Fatalf("resolved wrong identity: manifest=%s descriptor=%s", manifest, descriptor)
	}
	// A descriptor-bearing source never falls back to an otherwise matching
	// legacy manifest when the requested package descriptor is absent.
	mustWrite(t, filepath.Join(source, "legacy.kit.toml"), []byte(strings.Replace(minimalKitTOML, "rensei/example", "missing/id", 1)), 0o644)
	if _, _, err := resolveKitSourcePaths(source, "", "missing/id", "0.1.0"); !errors.Is(err, ErrKitInstallManifestNotFound) {
		t.Fatalf("descriptor source legacy fallback error = %v", err)
	}
	if _, _, err := resolveKitSourcePaths(source, "legacy.kit.toml", "missing/id", "0.1.0"); !errors.Is(err, ErrKitInstallManifestNotFound) {
		t.Fatalf("explicit legacy downgrade error = %v", err)
	}
	// Two descriptor locations for one identity are ambiguous even when their
	// canonical bytes are identical: the source must name one exact package.
	if err := copyFixtureTree(filepath.Join("testdata", "released-kit-packages", "go"), filepath.Join(source, "c")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveKitSourcePaths(source, "", "default/go", "1.0.0"); !errors.Is(err, ErrKitPackageEquivocation) {
		t.Fatalf("duplicate package resolution error = %v", err)
	}
}

func TestKitPackageHistoricalIdentityEquivocationRejected(t *testing.T) {
	first := filepath.Join("testdata", "released-kit-packages", "go")
	second := copyReleasedFixture(t, "go")
	payload := filepath.Join(second, "partials", "go-conventions.yaml")
	appendFile(t, payload, []byte("\n# changed without version bump\n"))
	descriptorPath := filepath.Join(second, kitPackageDescriptorName)
	raw, err := os.ReadFile(descriptorPath)
	if err != nil {
		t.Fatal(err)
	}
	var descriptor kitPackageDescriptor
	if err := json.Unmarshal(raw, &descriptor); err != nil {
		t.Fatal(err)
	}
	changed, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	for i := range descriptor.Entries {
		if descriptor.Entries[i].Path == "partials/go-conventions.yaml" {
			descriptor.Entries[i].Size = int64(len(changed))
			descriptor.Entries[i].SHA256 = sha256Hex(changed)
		}
	}
	encoded, _ := json.Marshal(descriptor)
	canonical, _ := jsoncanonicalizer.Transform(encoded)
	mustWrite(t, descriptorPath, canonical, 0o644)

	scan := t.TempDir()
	registry := NewKitRegistryWithTrust([]string{scan}, TrustConfig{Mode: TrustModePermissive, IssuerSet: defaultVendorIssuerSet()})
	fetcher := &packageFixtureFetcher{roots: map[string]string{"default/go": first}}
	registry.fetcher = fetcher
	request := afclient.KitInstallRequest{Version: "1.0.0", Source: &afclient.KitInstallSource{Kind: "git", URL: "fixture"}}
	if _, err := registry.Install("default/go", request); err != nil {
		t.Fatalf("first install: %v", err)
	}
	fetcher.roots["default/go"] = second
	if _, err := registry.Install("default/go", request); !errors.Is(err, ErrKitPackageEquivocation) {
		t.Fatalf("equivocation error = %v", err)
	}
	if kits := registry.List(); len(kits) != 1 || kits[0].PackageDigest == sha256Hex(canonical) {
		t.Fatalf("equivocation changed active package: %+v", kits)
	}
}

func TestKitPackageGenerationCASAndCrashCleanup(t *testing.T) {
	registry := newPackageTestRegistry(t, nil)
	if err := registry.withPackageStoreLock(func(store string) error {
		stale := filepath.Join(store, "staging", "stale", "partial")
		mustWrite(t, stale, []byte("partial"), 0o600)
		generation := kitRegistryGeneration{Schema: kitRegistrySchema}
		if _, err := persistPackageGeneration(store, "unexpected", generation, nil, syncDirectory); !errors.Is(err, ErrKitPackageConflict) {
			t.Fatalf("CAS error = %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Reacquiring the store lock performs deterministic crash cleanup.
	if err := registry.withPackageStoreLock(func(store string) error {
		entries, err := os.ReadDir(filepath.Join(store, "staging"))
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			t.Fatalf("stale staging survived cleanup: %+v", entries)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestKitPackageDurabilityFaultsLeavePriorGenerationActive(t *testing.T) {
	points := []string{"before-stage-sync", "after-stage-sync", "after-package-rename", "before-current-switch"}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			roots := map[string]string{
				"default/go":     filepath.Join("testdata", "released-kit-packages", "go"),
				"default/python": filepath.Join("testdata", "released-kit-packages", "python"),
			}
			registry := newPackageTestRegistry(t, roots)
			request := afclient.KitInstallRequest{Version: "1.0.0", Source: &afclient.KitInstallSource{Kind: "git", URL: "fixture"}}
			if _, err := registry.Install("default/go", request); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected durability fault")
			registry.packageFault = func(at string) error {
				if at == point {
					return injected
				}
				return nil
			}
			if _, err := registry.Install("default/python", request); !errors.Is(err, injected) {
				t.Fatalf("install error = %v, want injected fault at %s", err, point)
			}
			if kits := registry.List(); len(kits) != 1 || kits[0].ID != "default/go" {
				t.Fatalf("fault %s changed active generation: %+v", point, kits)
			}
			registry.packageFault = nil
			if _, err := registry.Install("default/python", request); err != nil {
				t.Fatalf("retry after %s: %v", point, err)
			}
			if kits := registry.List(); len(kits) != 2 {
				t.Fatalf("retry after %s did not recover: %+v", point, kits)
			}
		})
	}
}

func TestKitPackageAncestorSyncsPrecedeCurrentSwitch(t *testing.T) {
	root := filepath.Join("testdata", "released-kit-packages", "go")
	registry := newPackageTestRegistry(t, map[string]string{"default/go": root})
	var synced []string
	registry.packageSyncObserver = func(name string) { synced = append(synced, filepath.Clean(name)) }
	injected := errors.New("stop before current switch")
	registry.packageFault = func(point string) error {
		if point != "before-current-switch" {
			return nil
		}
		store := registry.packageStoreRoot()
		packagesDir := filepath.Join(store, "packages")
		shaDir := filepath.Join(packagesDir, "sha256")
		packagesIndex, shaIndex := -1, -1
		for i, name := range synced {
			if name == packagesDir && packagesIndex < 0 {
				packagesIndex = i
			}
			if name == shaDir && shaIndex < 0 {
				shaIndex = i
			}
		}
		if packagesIndex < 0 || shaIndex < 0 || packagesIndex >= shaIndex {
			t.Fatalf("package ancestor sync order before current switch = %+v", synced)
		}
		return injected
	}
	_, err := registry.Install("default/go", afclient.KitInstallRequest{Version: "1.0.0", Source: &afclient.KitInstallSource{Kind: "git", URL: "fixture"}})
	if !errors.Is(err, injected) {
		t.Fatalf("install error = %v, want current-switch fault", err)
	}
	if kits := registry.List(); len(kits) != 0 {
		t.Fatalf("current pointer changed despite pre-switch fault: %+v", kits)
	}
}

func TestKitPackageRollbackStrictlyValidatesHistoricalGeneration(t *testing.T) {
	roots := map[string]string{
		"default/go":     filepath.Join("testdata", "released-kit-packages", "go"),
		"default/python": filepath.Join("testdata", "released-kit-packages", "python"),
	}
	registry := newPackageTestRegistry(t, roots)
	request := afclient.KitInstallRequest{Version: "1.0.0", Source: &afclient.KitInstallSource{Kind: "git", URL: "fixture"}}
	if _, err := registry.Install("default/go", request); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Install("default/python", request); err != nil {
		t.Fatal(err)
	}
	store := registry.packageStoreRoot()
	currentDigest, current, err := loadCurrentGeneration(store)
	if err != nil {
		t.Fatal(err)
	}
	malicious := kitRegistryGeneration{
		Schema: kitRegistrySchema,
		Packages: []kitRegistryGenerationEntry{{
			ID: "default/go", Version: "1.0.0", Digest: "../../outside", Trust: afclient.KitTrustPackageVerified,
		}},
	}
	maliciousRaw, maliciousDigest, err := canonicalGeneration(malicious)
	if err != nil {
		t.Fatal(err)
	}
	if err := durableWriteOnce(filepath.Join(store, "generations"), maliciousDigest+".json", maliciousRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	current.Previous = maliciousDigest
	currentRaw, replacementDigest, err := canonicalGeneration(current)
	if err != nil {
		t.Fatal(err)
	}
	if replacementDigest == currentDigest {
		t.Fatal("test failed to change current generation")
	}
	if err := durableWriteOnce(filepath.Join(store, "generations"), replacementDigest+".json", currentRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := durableAtomicReplace(store, "current", []byte(replacementDigest+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = registry.RollbackPackages()
	if err == nil || !strings.Contains(err.Error(), "invalid package digest") {
		t.Fatalf("rollback error = %v, want strict historical digest rejection", err)
	}
}

func copyReleasedFixture(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join("testdata", "released-kit-packages", name)
	dst := filepath.Join(t.TempDir(), name)
	if err := copyFixtureTree(src, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}

func copyFixtureTree(src, dst string) error {
	if err := rootedMkdirAll(dst, 0o700); err != nil {
		return err
	}
	dstRoot, err := os.OpenRoot(dst)
	if err != nil {
		return err
	}
	defer func() { _ = dstRoot.Close() }()
	return filepath.WalkDir(src, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, name)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return dstRoot.MkdirAll(rel, 0o700)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(name) //nolint:gosec // test fixture
		if err != nil {
			return err
		}
		return dstRoot.WriteFile(rel, data, info.Mode().Perm())
	})
}

func mustWrite(t *testing.T, name string, data []byte, mode fs.FileMode) {
	t.Helper()
	dir := filepath.Dir(name)
	if err := rootedMkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile(filepath.Base(name), data, mode); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, name string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // test fixture
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
}

func mustRemove(t *testing.T, name string) {
	t.Helper()
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
}

func mustChmod(t *testing.T, name string, mode fs.FileMode) {
	t.Helper()
	if err := os.Chmod(name, mode); err != nil {
		t.Fatal(err)
	}
}

func TestImportedReleasedFixtureDescriptorsAreExact(t *testing.T) {
	for name := range releasedKitIDs {
		raw, err := os.ReadFile(filepath.Join("testdata", "released-kit-packages", name, kitPackageDescriptorName))
		if err != nil {
			t.Fatal(err)
		}
		canonical, err := jsoncanonicalizer.Transform(raw)
		if err != nil || !bytes.Equal(raw, canonical) {
			t.Fatalf("%s descriptor is not exact canonical fixture: %v", name, err)
		}
	}
}
