package pi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
)

const (
	preExecutionProfileID     = "pi/preexecution-refusal-origin/v1"
	preExecutionSidecarSHA256 = "d6927c50dedd6283bbba7009eddff80cc4cf3b0d9df5b172cb82ca39ba9b8082"
	preExecutionTreeSHA256    = "0f6dcf512b6ce0e50135b3bfb8795e5c71d5548fd38118a0d40376cda8ebf928"
	preExecutionBinarySHA256  = "73cab7d9cc76535c5d9035a9e9eb68544cb6271b8171329396150785509605fd"
	artifactSidecarName       = "artifact-profile.json"
	artifactSidecarMode       = "0444"
	artifactSidecarSize       = 35480
)

// TrustedExtensionIdentity is one exact same-process Pi extension identity
// reviewed and compiled into an embedding binary. Options.TrustedExtensions is
// the complete ordered list; a session Spec cannot add trust at runtime.
type TrustedExtensionIdentity struct {
	ID     string
	Digest string
}

type artifactProfileFile struct {
	Mode   string `json:"mode"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type artifactProfile struct {
	BinaryPath string `json:"binaryPath"`
	Build      struct {
		CompileAutoload struct {
			Bunfig      bool     `json:"bunfig"`
			Dotenv      bool     `json:"dotenv"`
			Flags       []string `json:"flags"`
			PackageJSON bool     `json:"packageJson"`
			TSConfig    bool     `json:"tsconfig"`
		} `json:"compileAutoload"`
	} `json:"build"`
	Files           []artifactProfileFile `json:"files"`
	ProfileID       string                `json:"profileId"`
	ReceiptContract struct {
		Mode          string   `json:"mode"`
		Origins       []string `json:"origins"`
		SchemaVersion int      `json:"schemaVersion"`
		Transport     string   `json:"transport"`
	} `json:"receiptContract"`
	SameProcessExtensionPolicy string `json:"sameProcessExtensionPolicy"`
	SchemaVersion              int    `json:"schemaVersion"`
	Target                     struct {
		Arch string `json:"arch"`
		OS   string `json:"os"`
	} `json:"target"`
	TreeSHA256 string `json:"treeSha256"`
}

type artifactFileLease struct {
	entry artifactProfileFile
	info  os.FileInfo
}

// artifactLease retains the opened artifact root and every measured file
// identity. Revalidation reads through that root and also checks the canonical
// path still names the same root, closing ordinary path/root replacement.
type artifactLease struct {
	root      *os.Root
	rootPath  string
	rootInfo  os.FileInfo
	sidecar   artifactFileLease
	files     []artifactFileLease
	closeOnce sync.Once
	closeErr  error
}

type extensionFileLease struct {
	id, path, digest string
	info             os.FileInfo
	size             int64
	mode             os.FileMode
}

type receiptAdmission struct {
	artifact   *artifactLease
	extensions []extensionFileLease // policy boundary first, then trusted additions
	startup    *startupLease
}

type agentExtensionIdentity struct{ id, digest, path string }

var receiptUnsafeStartupEnv = map[string]bool{
	"BUN_OPTIONS":                  true,
	"BUN_BE_BUN":                   true,
	"NODE_OPTIONS":                 true,
	"DYLD_INSERT_LIBRARIES":        true,
	"DYLD_LIBRARY_PATH":            true,
	"DYLD_FRAMEWORK_PATH":          true,
	"DYLD_FALLBACK_LIBRARY_PATH":   true,
	"DYLD_FALLBACK_FRAMEWORK_PATH": true,
	"LD_PRELOAD":                   true,
	"LD_LIBRARY_PATH":              true,
}

var preExecutionCompileAutoloadFlags = []string{
	"--no-compile-autoload-dotenv",
	"--no-compile-autoload-bunfig",
	"--no-compile-autoload-tsconfig",
	"--no-compile-autoload-package-json",
}

// startupLease binds receipt trust to the exact exec.Cmd environment and
// workarea identity. The compiled artifact descriptor separately proves all
// Bun config autoload classes are disabled, so ordinary workarea dotenv and
// bunfig files are inert and do not need to be absent.
type startupLease struct {
	cwd     string
	cwdInfo os.FileInfo
	env     []string
}

func measureReceiptStartupContext(cwd string, env []string) *startupLease {
	for _, entry := range env {
		if receiptUnsafeStartupEnv[startupEnvKey(entry)] {
			return nil
		}
	}
	cwdInfo, err := os.Lstat(cwd)
	if err != nil || !cwdInfo.IsDir() || cwdInfo.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	return &startupLease{
		cwd: cwd, cwdInfo: cwdInfo,
		env: append([]string(nil), env...),
	}
}

func startupEnvKey(entry string) string {
	if index := strings.IndexByte(entry, '='); index >= 0 {
		return entry[:index]
	}
	return entry
}

func withoutUnsafeStartupEnv(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		if !receiptUnsafeStartupEnv[startupEnvKey(entry)] {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func (l *startupLease) revalidate() error {
	if l == nil {
		return fmt.Errorf("pi receipt startup context unavailable")
	}
	cwdInfo, err := os.Lstat(l.cwd)
	if err != nil || !cwdInfo.IsDir() || !os.SameFile(l.cwdInfo, cwdInfo) {
		return fmt.Errorf("pi receipt workarea changed")
	}
	return nil
}

func (l *startupLease) matchesEnv(env []string) bool {
	return l != nil && reflect.DeepEqual(l.env, env)
}

func sha256Reader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is the already-resolved artifact or compiled-embedder extension path; callers hash it before trust.
	if err != nil {
		return "", err
	}
	digest, hashErr := sha256Reader(f)
	closeErr := f.Close()
	if hashErr != nil {
		return "", hashErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return digest, nil
}

func fileLinkCount(info os.FileInfo) (uint64, bool) {
	v := reflect.ValueOf(info.Sys())
	if !v.IsValid() {
		return 0, false
	}
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return 0, false
	}
	n := v.FieldByName("Nlink")
	if !n.IsValid() {
		return 0, false
	}
	switch n.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return n.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if n.Int() < 0 {
			return 0, false
		}
		return uint64(n.Int()), true //nolint:gosec // G115: negative values are rejected immediately above.
	default:
		return 0, false
	}
}

func validateArtifactPath(path string) bool {
	if path == "" || strings.Contains(path, "\\") || filepath.IsAbs(path) {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(path))) == path
}

func canonicalJSON(raw []byte) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return false
	}
	return bytes.Equal(raw, append(canonical, '\n'))
}

func readMeasuredRootFile(root *os.Root, entry artifactProfileFile, prior os.FileInfo) (os.FileInfo, string, error) {
	before, err := root.Lstat(entry.Path)
	if err != nil || !before.Mode().IsRegular() {
		return nil, "", fmt.Errorf("pi artifact file %q is not regular", entry.Path)
	}
	if links, ok := fileLinkCount(before); !ok || links != 1 {
		return nil, "", fmt.Errorf("pi artifact file %q is multiply linked", entry.Path)
	}
	if before.Size() != entry.Size || fmt.Sprintf("%04o", before.Mode().Perm()) != entry.Mode {
		return nil, "", fmt.Errorf("pi artifact file %q metadata mismatch", entry.Path)
	}
	if prior != nil && !os.SameFile(prior, before) {
		return nil, "", fmt.Errorf("pi artifact file %q identity changed", entry.Path)
	}
	f, err := root.Open(entry.Path)
	if err != nil {
		return nil, "", fmt.Errorf("pi artifact file %q open: %w", entry.Path, err)
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		_ = f.Close()
		return nil, "", fmt.Errorf("pi artifact file %q changed while opening", entry.Path)
	}
	digest, err := sha256Reader(f)
	if err != nil {
		_ = f.Close()
		return nil, "", fmt.Errorf("pi artifact file %q hash: %w", entry.Path, err)
	}
	if err := f.Close(); err != nil {
		return nil, "", fmt.Errorf("pi artifact file %q close: %w", entry.Path, err)
	}
	after, err := root.Lstat(entry.Path)
	if err != nil || !os.SameFile(opened, after) {
		return nil, "", fmt.Errorf("pi artifact file %q changed while hashing", entry.Path)
	}
	return before, digest, nil
}

func verifyClosedArtifactSet(root *os.Root, expected map[string]bool) error {
	seen := make(map[string]bool, len(expected))
	err := fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." {
			return nil
		}
		rel := strings.TrimPrefix(filepath.ToSlash(path), "./")
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("pi artifact directory %q is a symlink", rel)
			}
			return nil
		}
		if rel == artifactSidecarName {
			return nil
		}
		if !info.Mode().IsRegular() || !expected[rel] || seen[rel] {
			return fmt.Errorf("pi artifact contains unexpected entry %q", rel)
		}
		seen[rel] = true
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("pi artifact closed file set mismatch")
	}
	return nil
}

// measureArtifactProfile returns nil for legacy Pi without a sidecar. Once an
// adjacent sidecar exists, every mismatch is a hard construction failure.
func measureArtifactProfile(binary string) (*artifactLease, error) {
	rootPath := filepath.Dir(binary)
	sidecarPath := filepath.Join(rootPath, artifactSidecarName)
	sideInfo, err := os.Lstat(sidecarPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil || !sideInfo.Mode().IsRegular() || sideInfo.Size() != artifactSidecarSize || fmt.Sprintf("%04o", sideInfo.Mode().Perm()) != artifactSidecarMode {
		return nil, fmt.Errorf("pi artifact profile sidecar is not a regular file")
	}
	if links, ok := fileLinkCount(sideInfo); !ok || links != 1 {
		return nil, fmt.Errorf("pi artifact profile sidecar is multiply linked")
	}
	raw, err := os.ReadFile(sidecarPath) //nolint:gosec // G304: adjacent to the canonical resolved Pi binary; exact bytes are digest-pinned below.
	if err != nil {
		return nil, fmt.Errorf("pi artifact profile sidecar read: %w", err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != preExecutionSidecarSHA256 || !canonicalJSON(raw) {
		return nil, fmt.Errorf("pi artifact profile sidecar mismatch")
	}
	var profile artifactProfile
	if err := json.Unmarshal(raw, &profile); err != nil {
		return nil, fmt.Errorf("pi artifact profile decode: %w", err)
	}
	if profile.SchemaVersion != 1 || profile.ProfileID != preExecutionProfileID ||
		profile.TreeSHA256 != preExecutionTreeSHA256 || profile.BinaryPath != "pi" ||
		profile.Target.OS != "darwin" || profile.Target.Arch != "arm64" ||
		profile.ReceiptContract.SchemaVersion != 1 || profile.ReceiptContract.Mode != "rpc" ||
		profile.ReceiptContract.Transport != "tool_execution_end.preExecutionRefusal" ||
		!reflect.DeepEqual(profile.ReceiptContract.Origins, []string{"invalid_arguments", "unknown_tool"}) ||
		profile.SameProcessExtensionPolicy != "consumer-bound-exact-set" {
		return nil, fmt.Errorf("pi artifact profile closed fields mismatch")
	}
	compileAutoload := profile.Build.CompileAutoload
	if compileAutoload.Bunfig || compileAutoload.Dotenv || compileAutoload.PackageJSON || compileAutoload.TSConfig ||
		!reflect.DeepEqual(compileAutoload.Flags, preExecutionCompileAutoloadFlags) {
		return nil, fmt.Errorf("pi artifact profile compile autoload mismatch")
	}
	if runtime.GOOS != profile.Target.OS || runtime.GOARCH != profile.Target.Arch {
		return nil, fmt.Errorf("pi artifact profile target mismatch")
	}
	if filepath.Clean(filepath.Join(rootPath, filepath.FromSlash(profile.BinaryPath))) != binary {
		return nil, fmt.Errorf("pi artifact profile binary path mismatch")
	}
	expected := make(map[string]bool, len(profile.Files))
	for i, entry := range profile.Files {
		if !validateArtifactPath(entry.Path) || expected[entry.Path] || (i > 0 && profile.Files[i-1].Path >= entry.Path) {
			return nil, fmt.Errorf("pi artifact profile file set malformed")
		}
		expected[entry.Path] = true
	}
	treeJSON, err := json.Marshal(profile.Files)
	if err != nil {
		return nil, fmt.Errorf("pi artifact profile tree encode: %w", err)
	}
	treeSum := sha256.Sum256(append(treeJSON, '\n'))
	if hex.EncodeToString(treeSum[:]) != preExecutionTreeSHA256 {
		return nil, fmt.Errorf("pi artifact profile tree digest mismatch")
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("pi artifact root open: %w", err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = root.Close()
		}
	}()
	rootInfo, err := root.Stat(".")
	pathRootInfo, pathErr := os.Lstat(rootPath)
	if err != nil || pathErr != nil || !rootInfo.IsDir() || !pathRootInfo.IsDir() || !os.SameFile(rootInfo, pathRootInfo) {
		return nil, fmt.Errorf("pi artifact root identity mismatch")
	}
	if err := verifyClosedArtifactSet(root, expected); err != nil {
		return nil, err
	}
	files := make([]artifactFileLease, 0, len(profile.Files))
	for _, entry := range profile.Files {
		info, digest, err := readMeasuredRootFile(root, entry, nil)
		if err != nil || digest != entry.SHA256 {
			return nil, fmt.Errorf("pi artifact file digest mismatch: %s", entry.Path)
		}
		files = append(files, artifactFileLease{entry: entry, info: info})
	}
	if !expected[profile.BinaryPath] {
		return nil, fmt.Errorf("pi artifact binary missing from closed file set")
	}
	for _, file := range files {
		if file.entry.Path == profile.BinaryPath && file.entry.SHA256 != preExecutionBinarySHA256 {
			return nil, fmt.Errorf("pi artifact binary mismatch")
		}
	}
	sideEntry := artifactProfileFile{Mode: artifactSidecarMode, Path: artifactSidecarName, SHA256: preExecutionSidecarSHA256, Size: artifactSidecarSize}
	sideMeasured, digest, err := readMeasuredRootFile(root, sideEntry, nil)
	if err != nil || digest != preExecutionSidecarSHA256 {
		return nil, fmt.Errorf("pi artifact profile sidecar changed during measurement")
	}
	lease := &artifactLease{
		root: root, rootPath: rootPath, rootInfo: rootInfo,
		sidecar: artifactFileLease{entry: sideEntry, info: sideMeasured}, files: files,
	}
	closeOnError = false
	return lease, nil
}

func (l *artifactLease) expectedSet() map[string]bool {
	expected := make(map[string]bool, len(l.files))
	for _, file := range l.files {
		expected[file.entry.Path] = true
	}
	return expected
}

func (l *artifactLease) revalidate() error {
	pathInfo, err := os.Lstat(l.rootPath)
	handleInfo, handleErr := l.root.Stat(".")
	if err != nil || handleErr != nil || !os.SameFile(l.rootInfo, pathInfo) || !os.SameFile(l.rootInfo, handleInfo) {
		return fmt.Errorf("pi artifact root changed")
	}
	if err := verifyClosedArtifactSet(l.root, l.expectedSet()); err != nil {
		return err
	}
	if _, digest, err := readMeasuredRootFile(l.root, l.sidecar.entry, l.sidecar.info); err != nil || digest != l.sidecar.entry.SHA256 {
		return fmt.Errorf("pi artifact profile sidecar changed")
	}
	for _, file := range l.files {
		_, digest, err := readMeasuredRootFile(l.root, file.entry, file.info)
		if err != nil || digest != file.entry.SHA256 {
			return fmt.Errorf("pi artifact file changed: %s", file.entry.Path)
		}
	}
	return nil
}

func (l *artifactLease) close() error {
	l.closeOnce.Do(func() { l.closeErr = l.root.Close() })
	return l.closeErr
}

func validateTrustedExtensionIdentities(ids []TrustedExtensionIdentity) error {
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id.ID == "" || seen[id.ID] {
			return fmt.Errorf("pi trusted extension identity has empty or duplicate id")
		}
		decoded, err := hex.DecodeString(id.Digest)
		if err != nil || len(decoded) != sha256.Size || strings.ToLower(id.Digest) != id.Digest {
			return fmt.Errorf("pi trusted extension %q has malformed digest", id.ID)
		}
		seen[id.ID] = true
	}
	return nil
}

func trustedExtensionsMatch(trusted []TrustedExtensionIdentity, actual []agentExtensionIdentity) bool {
	if len(trusted) != len(actual) {
		return false
	}
	for i := range trusted {
		if trusted[i].ID != actual[i].id || trusted[i].Digest != actual[i].digest {
			return false
		}
	}
	return true
}

func measureExtensionFile(id, path, digest string) (extensionFileLease, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return extensionFileLease{}, fmt.Errorf("pi extension %q is not a regular file", id)
	}
	f, err := os.Open(path) //nolint:gosec // G304: compiled embedder delivery path; opened bytes are identity- and digest-checked before trust.
	if err != nil {
		return extensionFileLease{}, fmt.Errorf("pi extension %q open: %w", id, err)
	}
	opened, statErr := f.Stat()
	got, hashErr := sha256Reader(f)
	closeErr := f.Close()
	after, afterErr := os.Lstat(path)
	if statErr != nil || hashErr != nil || closeErr != nil || afterErr != nil ||
		!os.SameFile(before, opened) || !os.SameFile(opened, after) || got != digest {
		return extensionFileLease{}, fmt.Errorf("pi extension %q measurement mismatch", id)
	}
	return extensionFileLease{id: id, path: path, digest: digest, info: before, size: before.Size(), mode: before.Mode()}, nil
}

func newReceiptAdmission(artifact *artifactLease, layout sessionLayout, actual []agentExtensionIdentity, trusted []TrustedExtensionIdentity, startup *startupLease) (*receiptAdmission, error) {
	if artifact == nil || startup == nil || !trustedExtensionsMatch(trusted, actual) {
		return nil, nil
	}
	policy, err := measureExtensionFile("donmai-policy", layout.extension, extensionSHA())
	if err != nil {
		return nil, err
	}
	extensions := []extensionFileLease{policy}
	for _, id := range actual {
		lease, err := measureExtensionFile(id.id, id.path, id.digest)
		if err != nil {
			return nil, err
		}
		extensions = append(extensions, lease)
	}
	return &receiptAdmission{artifact: artifact, extensions: extensions, startup: startup}, nil
}

func (a *receiptAdmission) revalidate() error {
	if a == nil || a.artifact == nil {
		return fmt.Errorf("pi receipt admission unavailable")
	}
	if err := a.startup.revalidate(); err != nil {
		return err
	}
	if err := a.artifact.revalidate(); err != nil {
		return err
	}
	for _, ext := range a.extensions {
		measured, err := measureExtensionFile(ext.id, ext.path, ext.digest)
		if err != nil || !os.SameFile(ext.info, measured.info) || measured.size != ext.size || measured.mode != ext.mode {
			return fmt.Errorf("pi extension %q changed", ext.id)
		}
	}
	return nil
}
