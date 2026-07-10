package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/RenseiAI/donmai/afclient"
)

const (
	kitPackageSchema         = "donmai.dev/kit-package/v1"
	kitPackageDescriptorName = "kit.package.json"
	kitPackageSignatureName  = "kit.package.json.sigstore"
	officialKitPublisher     = "did:web:donmai.dev"
	kitRegistrySchema        = "donmai.dev/kit-registry-generation/v1"
)

var (
	// ErrKitPackageInvalid means package bytes failed structural, trust, or
	// complete-inventory validation.
	ErrKitPackageInvalid = errors.New("kit package invalid")
	// ErrKitPackageEquivocation means one id/version resolved to two digests.
	ErrKitPackageEquivocation = errors.New("kit package id/version equivocation")
	// ErrKitPackageConflict means the active generation changed before CAS.
	ErrKitPackageConflict = errors.New("kit package generation conflict")
)

type kitPackageLimits struct {
	MaxFiles           int
	MaxTotalBytes      int64
	MaxFileBytes       int64
	MaxPathBytes       int
	MaxDescriptorBytes int64
	MaxSignatureBytes  int64
}

func defaultKitPackageLimits() kitPackageLimits {
	return kitPackageLimits{
		MaxFiles:           1024,
		MaxTotalBytes:      64 << 20,
		MaxFileBytes:       16 << 20,
		MaxPathBytes:       1024,
		MaxDescriptorBytes: 1 << 20,
		MaxSignatureBytes:  4 << 20,
	}
}

func (l kitPackageLimits) withDefaults() kitPackageLimits {
	d := defaultKitPackageLimits()
	if l.MaxFiles <= 0 {
		l.MaxFiles = d.MaxFiles
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = d.MaxTotalBytes
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = d.MaxFileBytes
	}
	if l.MaxPathBytes <= 0 {
		l.MaxPathBytes = d.MaxPathBytes
	}
	if l.MaxDescriptorBytes <= 0 {
		l.MaxDescriptorBytes = d.MaxDescriptorBytes
	}
	if l.MaxSignatureBytes <= 0 {
		l.MaxSignatureBytes = d.MaxSignatureBytes
	}
	return l
}

type kitPackageDescriptor struct {
	Entries   []kitPackageEntry `json:"entries"`
	Kit       kitPackageID      `json:"kit"`
	Manifest  string            `json:"manifest"`
	Publisher string            `json:"publisher"`
	Schema    string            `json:"schema"`
}

type kitPackageID struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type kitPackageEntry struct {
	Mode   string `json:"mode"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type verifiedKitPackage struct {
	Descriptor kitPackageDescriptor
	Digest     string
	Manifest   kitManifestTOML
	Signature  afclient.KitSignatureResult
}

// parseKitPackageDescriptor requires the exact RFC 8785 bytes signed by the
// publisher. Comparing the canonical transform also rejects whitespace,
// alternate key ordering, duplicate-key encodings, and trailing material.
func parseKitPackageDescriptor(raw []byte, limits kitPackageLimits) (kitPackageDescriptor, error) {
	var d kitPackageDescriptor
	limits = limits.withDefaults()
	if int64(len(raw)) > limits.MaxDescriptorBytes {
		return d, fmt.Errorf("%w: descriptor exceeds %d bytes", ErrKitPackageInvalid, limits.MaxDescriptorBytes)
	}
	canonical, err := jsoncanonicalizer.Transform(raw)
	if err != nil {
		return d, fmt.Errorf("%w: canonical descriptor: %v", ErrKitPackageInvalid, err)
	}
	if !bytes.Equal(raw, canonical) {
		return d, fmt.Errorf("%w: descriptor is not exact RFC 8785 canonical JSON", ErrKitPackageInvalid)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return d, fmt.Errorf("%w: decode descriptor: %v", ErrKitPackageInvalid, err)
	}
	if err := requireJSONEOF(dec); err != nil {
		return d, fmt.Errorf("%w: descriptor trailing data: %v", ErrKitPackageInvalid, err)
	}
	if err := validateKitPackageDescriptor(d, limits); err != nil {
		return d, err
	}
	return d, nil
}

func requireJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("additional JSON value")
		}
		return err
	}
	return nil
}

func validateKitPackageDescriptor(d kitPackageDescriptor, limits kitPackageLimits) error {
	if d.Schema != kitPackageSchema {
		return fmt.Errorf("%w: unsupported schema %q", ErrKitPackageInvalid, d.Schema)
	}
	if d.Kit.ID == "" || d.Kit.Version == "" || d.Publisher == "" {
		return fmt.Errorf("%w: kit id, version, and publisher are required", ErrKitPackageInvalid)
	}
	if d.Manifest != "kit.toml" {
		return fmt.Errorf("%w: v1 manifest must be kit.toml, got %q", ErrKitPackageInvalid, d.Manifest)
	}
	if len(d.Entries) == 0 || len(d.Entries) > limits.MaxFiles {
		return fmt.Errorf("%w: entry count %d outside 1..%d", ErrKitPackageInvalid, len(d.Entries), limits.MaxFiles)
	}
	seen := make(map[string]struct{}, len(d.Entries))
	collisions := make(map[string]string, len(d.Entries))
	var total int64
	previous := ""
	manifestSeen := false
	for i, entry := range d.Entries {
		if err := validatePackagePath(entry.Path, limits.MaxPathBytes); err != nil {
			return fmt.Errorf("%w: entries[%d]: %v", ErrKitPackageInvalid, i, err)
		}
		if i > 0 && entry.Path <= previous {
			return fmt.Errorf("%w: entries are not uniquely sorted by UTF-8 path bytes at %q", ErrKitPackageInvalid, entry.Path)
		}
		previous = entry.Path
		if _, ok := seen[entry.Path]; ok {
			return fmt.Errorf("%w: duplicate inventory path %q", ErrKitPackageInvalid, entry.Path)
		}
		seen[entry.Path] = struct{}{}
		key := portablePackagePathKey(entry.Path)
		if prior, ok := collisions[key]; ok {
			return fmt.Errorf("%w: portable path collision %q and %q", ErrKitPackageInvalid, prior, entry.Path)
		}
		collisions[key] = entry.Path
		if entry.Mode != "0644" && entry.Mode != "0755" {
			return fmt.Errorf("%w: %q has unsupported portable mode %q", ErrKitPackageInvalid, entry.Path, entry.Mode)
		}
		if entry.Size < 0 || entry.Size > limits.MaxFileBytes || total > limits.MaxTotalBytes-entry.Size {
			return fmt.Errorf("%w: %q violates package size limits", ErrKitPackageInvalid, entry.Path)
		}
		total += entry.Size
		if len(entry.SHA256) != sha256.Size*2 || strings.ToLower(entry.SHA256) != entry.SHA256 {
			return fmt.Errorf("%w: %q has non-canonical sha256", ErrKitPackageInvalid, entry.Path)
		}
		if _, err := hex.DecodeString(entry.SHA256); err != nil {
			return fmt.Errorf("%w: %q has invalid sha256: %v", ErrKitPackageInvalid, entry.Path, err)
		}
		manifestSeen = manifestSeen || entry.Path == d.Manifest
	}
	if !manifestSeen {
		return fmt.Errorf("%w: manifest %q is absent from inventory", ErrKitPackageInvalid, d.Manifest)
	}
	return nil
}

func validatePackagePath(name string, maxBytes int) error {
	if name == "" || len(name) > maxBytes || !utf8.ValidString(name) {
		return fmt.Errorf("invalid UTF-8 path or length: %q", name)
	}
	if !norm.NFC.IsNormalString(name) {
		return fmt.Errorf("path is not NFC: %q", name)
	}
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, "//") || strings.Contains(name, "\\") {
		return fmt.Errorf("path must be relative forward-slash form: %q", name)
	}
	segments := strings.Split(name, "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." || strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " ") {
			return fmt.Errorf("invalid path segment %q in %q", segment, name)
		}
		for _, r := range segment {
			if r < 0x20 || r == 0x7f || strings.ContainsRune(`:<>"|?*`, r) {
				return fmt.Errorf("forbidden character in path %q", name)
			}
		}
		base := segment
		if dot := strings.IndexByte(base, '.'); dot >= 0 {
			base = base[:dot]
		}
		folded := strings.ToUpper(base)
		if isWindowsDeviceBase(folded) || isReservedPackageSegment(segment) {
			return fmt.Errorf("reserved path segment %q", segment)
		}
	}
	if path.Clean(name) != name {
		return fmt.Errorf("path is not lexically normalized: %q", name)
	}
	return nil
}

func isWindowsDeviceBase(base string) bool {
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	for _, prefix := range []string{"COM", "LPT"} {
		if strings.HasPrefix(base, prefix) {
			suffix := strings.TrimPrefix(base, prefix)
			if len(suffix) == 1 && suffix[0] >= '1' && suffix[0] <= '9' {
				return true
			}
			if suffix == "¹" || suffix == "²" || suffix == "³" {
				return true
			}
		}
	}
	return false
}

func isReservedPackageSegment(segment string) bool {
	folded := portablePackagePathKey(segment)
	switch folded {
	case portablePackagePathKey(kitPackageDescriptorName), portablePackagePathKey(kitPackageSignatureName), ".git", ".hg", ".svn", ".state.json":
		return true
	}
	return strings.HasPrefix(folded, ".tmp") || strings.HasPrefix(folded, ".staging") || strings.HasPrefix(folded, ".install")
}

// Unicode 15.1 added no characters or case-fold mappings over 15.0, so the
// x/text 15.0 full default fold is byte-equivalent to CaseFolding-15.1 C+F.
func portablePackagePathKey(name string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(name)))
}

func packagePortableMode(mode fs.FileMode) string {
	if mode.Perm()&0o111 != 0 {
		return "0755"
	}
	return "0644"
}

func readPackageRegular(root *os.Root, name string, maxBytes int64) ([]byte, fs.FileInfo, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("%q is not a regular non-link file", name)
	}
	if nlink(info) != 1 {
		return nil, nil, fmt.Errorf("%q is hard-linked", name)
	}
	if info.Size() < 0 || info.Size() > maxBytes {
		return nil, nil, fmt.Errorf("%q exceeds %d bytes", name, maxBytes)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !os.SameFile(info, opened) || !opened.Mode().IsRegular() || nlink(opened) != 1 {
		return nil, nil, fmt.Errorf("%q changed type or identity while opening", name)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, nil, fmt.Errorf("%q exceeds %d bytes", name, maxBytes)
	}
	after, err := root.Lstat(name)
	if err != nil || !os.SameFile(opened, after) {
		return nil, nil, fmt.Errorf("%q changed while reading", name)
	}
	return data, opened, nil
}

func nlink(info fs.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Nlink)
	}
	return 1
}

func ensureNoLinkComponents(root *os.Root, name string) error {
	current := ""
	for _, component := range strings.Split(filepath.ToSlash(name), "/") {
		if component == "" || component == "." {
			continue
		}
		current = path.Join(current, component)
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component %q is a symlink", current)
		}
	}
	return nil
}

func parseSigstoreBundle(raw []byte) (verify.SignedEntity, error) {
	var b bundle.Bundle
	if err := b.UnmarshalJSON(raw); err != nil {
		return nil, err
	}
	return &b, nil
}

func publisherAuthorized(publisher, signer string, cfg TrustConfig) bool {
	if publisher == officialKitPublisher {
		return signer == vendorSignerSAN
	}
	if publisher == "" || signer == "" || publisher != signer {
		return false
	}
	for _, allowed := range cfg.IssuerSet {
		if allowed == signer {
			return true
		}
	}
	return false
}

// verifyKitPackage verifies the descriptor before inspecting payloads, then
// proves exact inventory closure and optionally materializes those exact bytes
// into a private staging directory.
func (r *KitRegistry) verifyKitPackage(sourceRoot, descriptorRel, expectedID, expectedVersion, stageDir string, entity verify.SignedEntity) (verifiedKitPackage, error) {
	var out verifiedKitPackage
	limits := r.packageLimits.withDefaults()
	root, err := os.OpenRoot(sourceRoot)
	if err != nil {
		return out, fmt.Errorf("%w: open source root: %v", ErrKitPackageInvalid, err)
	}
	defer func() { _ = root.Close() }()
	if descriptorRel == "" {
		descriptorRel = kitPackageDescriptorName
	}
	descriptorRel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(descriptorRel)))
	if path.Base(descriptorRel) != kitPackageDescriptorName || strings.HasPrefix(descriptorRel, "../") {
		return out, fmt.Errorf("%w: invalid descriptor location %q", ErrKitPackageInvalid, descriptorRel)
	}
	if err := ensureNoLinkComponents(root, descriptorRel); err != nil {
		return out, fmt.Errorf("%w: descriptor containment: %v", ErrKitPackageInvalid, err)
	}
	descriptorBytes, _, err := readPackageRegular(root, descriptorRel, limits.MaxDescriptorBytes)
	if err != nil {
		return out, fmt.Errorf("%w: read descriptor: %v", ErrKitPackageInvalid, err)
	}
	signatureRel := path.Join(path.Dir(descriptorRel), kitPackageSignatureName)
	signatureBytes, _, sigReadErr := readPackageRegular(root, signatureRel, limits.MaxSignatureBytes)

	packageID := expectedID
	if packageID == "" {
		packageID = "unknown"
	}
	if entity == nil && sigReadErr == nil {
		entity, err = parseSigstoreBundle(signatureBytes)
		if err != nil {
			out.Signature = afclient.KitSignatureResult{KitID: packageID, Trust: afclient.KitTrustPackageSignedUnverified, OK: true, Details: fmt.Sprintf("parse package bundle: %v", err)}
		}
	}
	if entity != nil && out.Signature.Trust == "" && r.verifier != nil {
		out.Signature = r.verifier.verifyPackageEntity(packageID, entity, descriptorBytes)
	} else if out.Signature.Trust == "" {
		out.Signature = afclient.KitSignatureResult{KitID: packageID, Trust: afclient.KitTrustUnsigned, OK: true, Details: "package descriptor has no verifiable signature"}
	}

	descriptor, err := parseKitPackageDescriptor(descriptorBytes, limits)
	if err != nil {
		return out, err
	}
	out.Descriptor = descriptor
	out.Digest = sha256Hex(descriptorBytes)
	if expectedID != "" && descriptor.Kit.ID != expectedID {
		return out, fmt.Errorf("%w: requested id %q does not match descriptor id %q", ErrKitPackageInvalid, expectedID, descriptor.Kit.ID)
	}
	if expectedVersion != "" && descriptor.Kit.Version != expectedVersion {
		return out, fmt.Errorf("%w: requested version %q does not match descriptor version %q", ErrKitPackageInvalid, expectedVersion, descriptor.Kit.Version)
	}
	if out.Signature.Trust == afclient.KitTrustPackageVerified && !publisherAuthorized(descriptor.Publisher, out.Signature.SignerID, r.verifier.config) {
		out.Signature.Trust = afclient.KitTrustPackageSignedUnverified
		out.Signature.Details = fmt.Sprintf("signer %q is not authorized for publisher %q", out.Signature.SignerID, descriptor.Publisher)
	}

	packageRel := path.Dir(descriptorRel)
	if packageRel == "." {
		packageRel = ""
	}
	packageRoot := root
	if packageRel != "" {
		packageRoot, err = root.OpenRoot(packageRel)
		if err != nil {
			return out, fmt.Errorf("%w: open package root: %v", ErrKitPackageInvalid, err)
		}
		defer func() { _ = packageRoot.Close() }()
	}
	verifiedFiles, err := verifyPackageInventory(packageRoot, descriptor, limits, stageDir, descriptorBytes, signatureBytes, sigReadErr, r.packageFault)
	if err != nil {
		return out, err
	}
	if r.packageAfterInventory != nil {
		r.packageAfterInventory()
	}
	// Parse the exact bytes whose digest/size/mode were verified above. Never
	// reopen mutable source content after the inventory proof.
	manifestBytes := verifiedFiles[descriptor.Manifest]
	if err := tomlUnmarshalKit(manifestBytes, &out.Manifest); err != nil {
		return out, fmt.Errorf("%w: parse manifest: %v", ErrKitPackageInvalid, err)
	}
	if out.Manifest.API != "donmai.dev/v1" && out.Manifest.API != "rensei.dev/v1" {
		return out, fmt.Errorf("%w: unsupported manifest api %q", ErrKitPackageInvalid, out.Manifest.API)
	}
	if out.Manifest.Kit.ID != descriptor.Kit.ID || out.Manifest.Kit.Version != descriptor.Kit.Version {
		return out, fmt.Errorf("%w: descriptor/manifest identity mismatch", ErrKitPackageInvalid)
	}
	if out.Manifest.Kit.AuthorIdentity != "" && out.Manifest.Kit.AuthorIdentity != descriptor.Publisher {
		return out, fmt.Errorf("%w: manifest authorIdentity %q disagrees with publisher %q", ErrKitPackageInvalid, out.Manifest.Kit.AuthorIdentity, descriptor.Publisher)
	}
	if err := validateManifestPackageReferences(out.Manifest, descriptor); err != nil {
		return out, err
	}
	return out, nil
}

func tomlUnmarshalKit(data []byte, out *kitManifestTOML) error {
	return toml.Unmarshal(data, out)
}

func verifyPackageInventory(root *os.Root, descriptor kitPackageDescriptor, limits kitPackageLimits, stageDir string, descriptorBytes, signatureBytes []byte, signatureErr error, fault func(string) error) (map[string][]byte, error) {
	expected := make(map[string]kitPackageEntry, len(descriptor.Entries))
	requiredDirs := map[string]struct{}{":root": {}}
	for _, entry := range descriptor.Entries {
		expected[entry.Path] = entry
		for dir := path.Dir(entry.Path); dir != "."; dir = path.Dir(dir) {
			requiredDirs[dir] = struct{}{}
			if dir == "." {
				break
			}
		}
	}
	seen := make(map[string]struct{}, len(expected))
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		name = strings.TrimPrefix(filepath.ToSlash(name), "./")
		if name == ".git" && entry.IsDir() {
			return fs.SkipDir
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink %q", name)
		}
		if entry.IsDir() {
			if _, ok := requiredDirs[name]; !ok {
				return fmt.Errorf("extra directory %q", name)
			}
			return nil
		}
		if !info.Mode().IsRegular() || nlink(info) != 1 {
			return fmt.Errorf("special or hard-linked file %q", name)
		}
		if name == kitPackageDescriptorName || name == kitPackageSignatureName {
			return nil
		}
		if _, ok := expected[name]; !ok {
			return fmt.Errorf("extra file %q", name)
		}
		seen[name] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%w: inventory closure: %v", ErrKitPackageInvalid, err)
	}
	if signatureErr != nil {
		if !errors.Is(signatureErr, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: signature envelope: %v", ErrKitPackageInvalid, signatureErr)
		}
		signatureBytes = nil
	}
	var stageRoot *os.Root
	if stageDir != "" {
		if err := rootedMkdirAll(stageDir, 0o700); err != nil {
			return nil, fmt.Errorf("%w: create stage: %v", ErrKitPackageInvalid, err)
		}
		stageRoot, err = os.OpenRoot(stageDir)
		if err != nil {
			return nil, fmt.Errorf("%w: open stage: %v", ErrKitPackageInvalid, err)
		}
		defer func() { _ = stageRoot.Close() }()
		if err := writeAndSyncRootFile(stageRoot, kitPackageDescriptorName, descriptorBytes, 0o600); err != nil {
			return nil, fmt.Errorf("%w: stage descriptor: %v", ErrKitPackageInvalid, err)
		}
		if len(signatureBytes) > 0 {
			if err := writeAndSyncRootFile(stageRoot, kitPackageSignatureName, signatureBytes, 0o600); err != nil {
				return nil, fmt.Errorf("%w: stage signature: %v", ErrKitPackageInvalid, err)
			}
		}
	}
	verifiedFiles := make(map[string][]byte, len(descriptor.Entries))
	for _, entry := range descriptor.Entries {
		if _, ok := seen[entry.Path]; !ok {
			return nil, fmt.Errorf("%w: missing inventory file %q", ErrKitPackageInvalid, entry.Path)
		}
		data, info, err := readPackageRegular(root, entry.Path, limits.MaxFileBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: read %q: %v", ErrKitPackageInvalid, entry.Path, err)
		}
		if int64(len(data)) != entry.Size || info.Size() != entry.Size {
			return nil, fmt.Errorf("%w: size mismatch for %q", ErrKitPackageInvalid, entry.Path)
		}
		if sha256Hex(data) != entry.SHA256 {
			return nil, fmt.Errorf("%w: digest mismatch for %q", ErrKitPackageInvalid, entry.Path)
		}
		if packagePortableMode(info.Mode()) != entry.Mode {
			return nil, fmt.Errorf("%w: mode mismatch for %q", ErrKitPackageInvalid, entry.Path)
		}
		verifiedFiles[entry.Path] = data
		if stageRoot != nil {
			dst := filepath.FromSlash(entry.Path)
			if err := stageRoot.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return nil, fmt.Errorf("%w: create staged parent: %v", ErrKitPackageInvalid, err)
			}
			mode := fs.FileMode(0o644)
			if entry.Mode == "0755" {
				mode = 0o755
			}
			if err := writeAndSyncRootFile(stageRoot, dst, data, mode); err != nil {
				return nil, fmt.Errorf("%w: stage %q: %v", ErrKitPackageInvalid, entry.Path, err)
			}
		}
	}
	if stageRoot != nil {
		if fault != nil {
			if err := fault("before-stage-sync"); err != nil {
				return nil, err
			}
		}
		if err := syncPackageStage(stageRoot); err != nil {
			return nil, fmt.Errorf("%w: sync package stage: %v", ErrKitPackageInvalid, err)
		}
	}
	return verifiedFiles, nil
}

func writeAndSyncRootFile(root *os.Root, name string, data []byte, mode fs.FileMode) error {
	file, err := root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func syncPackageStage(root *os.Root) error {
	directories := []string{"."}
	if err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name != "." && entry.IsDir() {
			directories = append(directories, name)
		}
		return nil
	}); err != nil {
		return err
	}
	sort.Slice(directories, func(i, j int) bool {
		return strings.Count(filepath.ToSlash(directories[i]), "/") > strings.Count(filepath.ToSlash(directories[j]), "/")
	})
	for _, directory := range directories {
		file, err := root.Open(directory)
		if err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}

func validateManifestPackageReferences(manifest kitManifestTOML, descriptor kitPackageDescriptor) error {
	inventory := make(map[string]struct{}, len(descriptor.Entries))
	for _, entry := range descriptor.Entries {
		inventory[entry.Path] = struct{}{}
	}
	var refs []string
	if manifest.Detect.Exec != "" {
		refs = append(refs, manifest.Detect.Exec)
	}
	for _, fragment := range manifest.Provide.PromptFragments {
		refs = append(refs, fragment.File)
	}
	for _, skill := range manifest.Provide.Skills {
		refs = append(refs, skill.File)
	}
	for _, agent := range manifest.Provide.Agents {
		refs = append(refs, agent.Template)
	}
	for _, skill := range manifest.Provide.A2ASkills {
		if skill.Endpoint != "" {
			refs = append(refs, skill.Endpoint)
		}
	}
	for _, hook := range []string{manifest.Provide.Hooks.PostAcquire, manifest.Provide.Hooks.PreRelease} {
		if hook != "" {
			refs = append(refs, hook)
		}
	}
	for _, hooks := range manifest.Provide.Hooks.OS {
		for _, hook := range []string{hooks.PostAcquire, hooks.PreRelease} {
			if hook != "" {
				refs = append(refs, strings.ReplaceAll(hook, "\\", "/"))
			}
		}
	}
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		if _, ok := inventory[ref]; !ok {
			return fmt.Errorf("%w: manifest package path %q is absent from inventory", ErrKitPackageInvalid, ref)
		}
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type kitRegistryGeneration struct {
	Schema                string                       `json:"schema"`
	Previous              string                       `json:"previous,omitempty"`
	CatalogSnapshotDigest string                       `json:"catalogSnapshotDigest,omitempty"`
	Packages              []kitRegistryGenerationEntry `json:"packages"`
}

type kitRegistryGenerationEntry struct {
	ID       string                 `json:"id"`
	Version  string                 `json:"version"`
	Digest   string                 `json:"digest"`
	Trust    afclient.KitTrustState `json:"trust"`
	SignerID string                 `json:"signerId,omitempty"`
	SignedAt string                 `json:"signedAt,omitempty"`
}

func (r *KitRegistry) packageStoreRoot() string {
	if len(r.scanPaths) == 0 {
		return ""
	}
	return filepath.Join(r.scanPaths[0], ".package-store")
}

func (r *KitRegistry) withPackageStoreLock(fn func(store string) error) error {
	store := r.packageStoreRoot()
	if store == "" {
		return errors.New("no package store configured")
	}
	if err := durableMkdirAll(store, 0o700, r.syncPackageDirectory); err != nil {
		return fmt.Errorf("create package store: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(store, ".install.lock"), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // private operator store
	if err != nil {
		return fmt.Errorf("open package installer lock: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil { //nolint:gosec // Flock requires int; OS file descriptors are int-sized
		return fmt.Errorf("lock package store: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck,gosec // best-effort release; Flock requires an int fd
	if err := cleanupPackageStoreCrashDebris(store, r.syncPackageDirectory); err != nil {
		return err
	}
	return fn(store)
}

func cleanupPackageStoreCrashDebris(store string, syncFn func(string) error) error {
	staging := filepath.Join(store, "staging")
	if err := durableMkdirAll(staging, 0o700, syncFn); err != nil {
		return fmt.Errorf("create package staging root: %w", err)
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		return fmt.Errorf("read package staging root: %w", err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(staging, entry.Name())); err != nil { //nolint:gosec // path is direct ReadDir child
			return fmt.Errorf("clean stale package staging %q: %w", entry.Name(), err)
		}
	}
	for _, dir := range []string{filepath.Join(store, "generations"), store} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read package store directory: %w", err)
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.Contains(entry.Name(), ".tmp-") {
				if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil { //nolint:gosec // direct child
					return fmt.Errorf("clean package temp file: %w", err)
				}
			}
		}
	}
	return nil
}

func loadCurrentGeneration(store string) (string, kitRegistryGeneration, error) {
	var generation kitRegistryGeneration
	pointer, err := os.ReadFile(filepath.Join(store, "current")) //nolint:gosec // private package store
	if errors.Is(err, os.ErrNotExist) {
		generation.Schema = kitRegistrySchema
		return "", generation, nil
	}
	if err != nil {
		return "", generation, fmt.Errorf("read current package generation: %w", err)
	}
	digest := strings.TrimSpace(string(pointer))
	if !isCanonicalSHA256(digest) {
		return "", generation, fmt.Errorf("current package generation has invalid digest %q", digest)
	}
	generation, err = loadGenerationByDigest(store, digest)
	if err != nil {
		return "", generation, fmt.Errorf("load current package generation: %w", err)
	}
	return digest, generation, nil
}

func loadGenerationByDigest(store, digest string) (kitRegistryGeneration, error) {
	var generation kitRegistryGeneration
	if !isCanonicalSHA256(digest) {
		return generation, fmt.Errorf("invalid generation digest %q", digest)
	}
	raw, err := os.ReadFile(filepath.Join(store, "generations", digest+".json")) //nolint:gosec // digest is validated hex below
	if err != nil {
		return generation, fmt.Errorf("read generation %s: %w", digest, err)
	}
	if sha256Hex(raw) != digest {
		return generation, errors.New("generation digest mismatch")
	}
	canonical, err := jsoncanonicalizer.Transform(raw)
	if err != nil || !bytes.Equal(raw, canonical) {
		return generation, errors.New("generation is not canonical JSON")
	}
	if err := json.Unmarshal(raw, &generation); err != nil {
		return generation, fmt.Errorf("parse generation: %w", err)
	}
	if generation.Schema != kitRegistrySchema {
		return generation, fmt.Errorf("generation has unsupported schema %q", generation.Schema)
	}
	seen := make(map[string]struct{}, len(generation.Packages))
	for _, entry := range generation.Packages {
		if !isCanonicalSHA256(entry.Digest) {
			return generation, fmt.Errorf("generation has invalid package digest %q", entry.Digest)
		}
		if entry.ID == "" || entry.Version == "" {
			return generation, errors.New("generation has empty identity")
		}
		if _, ok := seen[entry.ID]; ok {
			return generation, fmt.Errorf("generation has duplicate id %q", entry.ID)
		}
		seen[entry.ID] = struct{}{}
		switch entry.Trust {
		case afclient.KitTrustPackageVerified, afclient.KitTrustPackageSignedUnverified, afclient.KitTrustUnsigned:
		default:
			return generation, fmt.Errorf("generation has invalid package trust %q", entry.Trust)
		}
	}
	if generation.Previous != "" && !isCanonicalSHA256(generation.Previous) {
		return generation, fmt.Errorf("generation has invalid previous digest %q", generation.Previous)
	}
	if generation.CatalogSnapshotDigest != "" && !isCanonicalSHA256(generation.CatalogSnapshotDigest) {
		return generation, fmt.Errorf("generation has invalid catalog snapshot digest %q", generation.CatalogSnapshotDigest)
	}
	return generation, nil
}

func isCanonicalSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func canonicalGeneration(generation kitRegistryGeneration) ([]byte, string, error) {
	sort.Slice(generation.Packages, func(i, j int) bool {
		if generation.Packages[i].ID != generation.Packages[j].ID {
			return generation.Packages[i].ID < generation.Packages[j].ID
		}
		if generation.Packages[i].Version != generation.Packages[j].Version {
			return generation.Packages[i].Version < generation.Packages[j].Version
		}
		return generation.Packages[i].Digest < generation.Packages[j].Digest
	})
	raw, err := json.Marshal(generation)
	if err != nil {
		return nil, "", err
	}
	raw, err = jsoncanonicalizer.Transform(raw)
	if err != nil {
		return nil, "", err
	}
	return raw, sha256Hex(raw), nil
}

func persistPackageGeneration(store, expectedCurrent string, generation kitRegistryGeneration, beforeSwitch func(string) error, syncFn func(string) error) (string, error) {
	actual, _, err := loadCurrentGeneration(store)
	if err != nil {
		return "", err
	}
	if actual != expectedCurrent {
		return "", fmt.Errorf("%w: expected generation %q, found %q", ErrKitPackageConflict, expectedCurrent, actual)
	}
	raw, digest, err := canonicalGeneration(generation)
	if err != nil {
		return "", fmt.Errorf("canonicalize package generation: %w", err)
	}
	genDir := filepath.Join(store, "generations")
	if err := durableMkdirAll(genDir, 0o700, syncFn); err != nil {
		return "", err
	}
	if err := durableWriteOnce(genDir, digest+".json", raw, 0o600); err != nil {
		return "", fmt.Errorf("publish package generation: %w", err)
	}
	if beforeSwitch != nil {
		if err := beforeSwitch("before-current-switch"); err != nil {
			return "", err
		}
	}
	pointer := []byte(digest + "\n")
	if err := durableAtomicReplace(store, "current", pointer, 0o600); err != nil {
		return "", fmt.Errorf("switch package generation: %w", err)
	}
	return digest, nil
}

func durableWriteOnce(dir, name string, data []byte, mode fs.FileMode) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if existing, err := root.ReadFile(name); err == nil {
		if !bytes.Equal(existing, data) {
			return errors.New("immutable file already exists with different bytes")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d", name, os.Getpid())
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = root.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		if existing, readErr := root.ReadFile(name); readErr == nil && bytes.Equal(existing, data) {
			_ = root.Remove(tmp)
			ok = true
			return nil
		}
		return err
	}
	ok = true
	return syncDirectory(dir)
}

func durableAtomicReplace(dir, name string, data []byte, mode fs.FileMode) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tmp := fmt.Sprintf("%s.tmp-%d", name, os.Getpid())
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = root.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		return err
	}
	ok = true
	return syncDirectory(dir)
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir) //nolint:gosec // private package store
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

func rootedMkdirAll(name string, mode fs.FileMode) error {
	abs, err := filepath.Abs(name)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	rel := strings.TrimPrefix(filepath.ToSlash(abs), "/")
	return root.MkdirAll(filepath.FromSlash(rel), mode)
}

func durableMkdirAll(name string, mode fs.FileMode, syncFn func(string) error) error {
	abs, err := filepath.Abs(name)
	if err != nil {
		return err
	}
	var missing []string
	ancestor := filepath.Clean(abs)
	for {
		info, statErr := os.Lstat(ancestor)
		if statErr == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("durable mkdir ancestor %q is not a non-link directory", ancestor)
			}
			break
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		missing = append(missing, ancestor)
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return fmt.Errorf("durable mkdir cannot find existing ancestor for %q", abs)
		}
		ancestor = parent
	}
	for i := len(missing) - 1; i >= 0; i-- {
		directory := missing[i]
		parent := filepath.Dir(directory)
		root, err := os.OpenRoot(parent)
		if err != nil {
			return err
		}
		err = root.Mkdir(filepath.Base(directory), mode)
		if errors.Is(err, os.ErrExist) {
			info, statErr := root.Lstat(filepath.Base(directory))
			if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				_ = root.Close()
				return fmt.Errorf("durable mkdir raced with non-directory %q", directory)
			}
		}
		closeErr := root.Close()
		if err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if syncFn != nil {
			if err := syncFn(parent); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *KitRegistry) syncPackageDirectory(name string) error {
	if err := syncDirectory(name); err != nil {
		return err
	}
	if r.packageSyncObserver != nil {
		r.packageSyncObserver(filepath.Clean(name))
	}
	return nil
}

func (r *KitRegistry) installFetchedPackage(id string, req afclient.KitInstallRequest, fetched *fetchedKit) (afclient.KitInstallResult, error) {
	var result afclient.KitInstallResult
	r.mu.Lock()
	defer r.mu.Unlock()
	err := r.withPackageStoreLock(func(store string) error {
		currentDigest, current, err := loadCurrentGeneration(store)
		if err != nil {
			return err
		}
		stage, err := os.MkdirTemp(filepath.Join(store, "staging"), "package-")
		if err != nil {
			return fmt.Errorf("create package stage: %w", err)
		}
		defer os.RemoveAll(stage) //nolint:errcheck // stale staging is ignored and cleaned on next lock
		descriptorRel, err := filepath.Rel(fetched.TempDir, fetched.DescriptorPath)
		if err != nil || strings.HasPrefix(descriptorRel, "..") {
			return fmt.Errorf("%w: descriptor is outside fetched source", ErrKitPackageInvalid)
		}
		verified, err := r.verifyKitPackage(fetched.TempDir, filepath.ToSlash(descriptorRel), id, req.Version, stage, fetched.PackageEntity)
		if err != nil {
			return err
		}
		if r.packageFault != nil {
			if err := r.packageFault("after-stage-sync"); err != nil {
				return err
			}
		}
		if r.verifier != nil && !r.verifier.trustGateAllows(verified.Signature.Trust) {
			if req.TrustOverride == afclient.TrustOverrideAllowedThisOnce {
				r.verifier.auditTrustOverride(id, verified.Signature.SignerID)
			} else {
				return trustGateRejectionError(id, verified.Signature, r.verifier.config.Mode)
			}
		}
		if err := rejectPackageEquivocation(store, verified.Descriptor.Kit.ID, verified.Descriptor.Kit.Version, verified.Digest); err != nil {
			return err
		}

		packageParent := filepath.Join(store, "packages", "sha256")
		if err := durableMkdirAll(packageParent, 0o700, r.syncPackageDirectory); err != nil {
			return fmt.Errorf("create immutable package root: %w", err)
		}
		packageDir := filepath.Join(packageParent, verified.Digest)
		storeRoot, err := os.OpenRoot(store)
		if err != nil {
			return fmt.Errorf("open package store root: %w", err)
		}
		defer func() { _ = storeRoot.Close() }()
		packageRel := filepath.Join("packages", "sha256", verified.Digest)
		if info, statErr := storeRoot.Lstat(packageRel); statErr == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%w: digest destination is not a directory", ErrKitPackageInvalid)
			}
			stagedTree, treeErr := packageTreeDigest(stage)
			if treeErr != nil {
				return fmt.Errorf("digest staged package tree: %w", treeErr)
			}
			existingTree, treeErr := packageTreeDigest(packageDir)
			if treeErr != nil || existingTree != stagedTree {
				return fmt.Errorf("%w: existing immutable package bytes differ: %v", ErrKitPackageInvalid, treeErr)
			}
			rechecked, verifyErr := r.verifyKitPackage(packageDir, kitPackageDescriptorName, id, req.Version, "", nil)
			if verifyErr != nil || rechecked.Digest != verified.Digest {
				return fmt.Errorf("%w: existing immutable package failed re-verification: %v", ErrKitPackageInvalid, verifyErr)
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect immutable package destination: %w", statErr)
		} else if stageRel, relErr := filepath.Rel(store, stage); relErr != nil {
			return fmt.Errorf("resolve package stage: %w", relErr)
		} else if err := storeRoot.Rename(stageRel, packageRel); err != nil {
			return fmt.Errorf("atomically publish immutable package: %w", err)
		} else if err := r.syncPackageDirectory(packageParent); err != nil {
			return fmt.Errorf("durably publish immutable package: %w", err)
		}
		if r.packageFault != nil {
			if err := r.packageFault("after-package-rename"); err != nil {
				return err
			}
		}

		entry := kitRegistryGenerationEntry{
			ID:       verified.Descriptor.Kit.ID,
			Version:  verified.Descriptor.Kit.Version,
			Digest:   verified.Digest,
			Trust:    verified.Signature.Trust,
			SignerID: verified.Signature.SignerID,
			SignedAt: verified.Signature.SignedAt,
		}
		next := kitRegistryGeneration{Schema: kitRegistrySchema, Previous: currentDigest, CatalogSnapshotDigest: current.CatalogSnapshotDigest}
		for _, existing := range current.Packages {
			if existing.ID != entry.ID {
				next.Packages = append(next.Packages, existing)
			}
		}
		next.Packages = append(next.Packages, entry)
		if _, err := persistPackageGeneration(store, currentDigest, next, r.packageFault, r.syncPackageDirectory); err != nil {
			return err
		}

		kit := manifestToKit(verified.Manifest)
		kit.Status = afclient.KitStatusActive
		kit.InstallKind = afclient.KitInstallKindPackage
		kit.PackageDigest = verified.Digest
		kit.CatalogSnapshotDigest = next.CatalogSnapshotDigest
		kit.Trust = verified.Signature.Trust
		kit.SignerID = verified.Signature.SignerID
		kit.SignedAt = verified.Signature.SignedAt
		result = afclient.KitInstallResult{Kit: kit, Message: "immutable kit package installed and activated"}
		return nil
	})
	return result, err
}

func packageTreeDigest(name string) (string, error) {
	root, err := os.OpenRoot(name)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	hash := sha256.New()
	err = fs.WalkDir(root.FS(), ".", func(fileName string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if fileName == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!entry.IsDir() && (!info.Mode().IsRegular() || nlink(info) != 1)) {
			return fmt.Errorf("non-regular package tree entry %q", fileName)
		}
		_, _ = fmt.Fprintf(hash, "%s\x00%v\x00", filepath.ToSlash(fileName), info.Mode().Perm())
		if info.Mode().IsRegular() {
			data, err := root.ReadFile(fileName)
			if err != nil {
				return err
			}
			_, _ = hash.Write(data)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func rejectPackageEquivocation(store, id, version, digest string) error {
	genDir := filepath.Join(store, "generations")
	entries, err := os.ReadDir(genDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect package generations for equivocation: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		generationDigest := strings.TrimSuffix(entry.Name(), ".json")
		generation, err := loadGenerationByDigest(store, generationDigest)
		if err != nil {
			return fmt.Errorf("validate package generation for equivocation: %w", err)
		}
		for _, prior := range generation.Packages {
			if prior.ID == id && prior.Version == version && prior.Digest != digest {
				return fmt.Errorf("%w: %s@%s was %s, source claims %s", ErrKitPackageEquivocation, id, version, prior.Digest, digest)
			}
		}
	}
	return nil
}

// RollbackPackages atomically selects the previous verified registry
// generation. It is intentionally not exposed through the daemon HTTP API in
// this substrate wave; callers must make rollback an explicit operator action.
func (r *KitRegistry) RollbackPackages() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.withPackageStoreLock(func(store string) error {
		currentDigest, current, err := loadCurrentGeneration(store)
		if err != nil {
			return err
		}
		if current.Previous == "" {
			return errors.New("package rollback: no previous generation")
		}
		previous, err := loadGenerationByDigest(store, current.Previous)
		if err != nil {
			return fmt.Errorf("package rollback: validate previous generation: %w", err)
		}
		for _, entry := range previous.Packages {
			packageDir := filepath.Join(store, "packages", "sha256", entry.Digest)
			verified, err := r.verifyKitPackage(packageDir, kitPackageDescriptorName, entry.ID, entry.Version, "", nil)
			if err != nil {
				return fmt.Errorf("package rollback: reverify %s@%s: %w", entry.ID, entry.Version, err)
			}
			if verified.Digest != entry.Digest || verified.Signature.Trust != afclient.KitTrustPackageVerified {
				return fmt.Errorf("package rollback: %s@%s no longer matches a package-verified digest", entry.ID, entry.Version)
			}
		}
		actual, _, err := loadCurrentGeneration(store)
		if err != nil {
			return err
		}
		if actual != currentDigest {
			return fmt.Errorf("%w: rollback current changed", ErrKitPackageConflict)
		}
		return durableAtomicReplace(store, "current", []byte(current.Previous+"\n"), 0o600)
	})
}
