package codex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	codexHomePrefix       = "donmai-codex-home-"
	codexConfigFileName   = "config.toml"
	codexConfigBaseline   = "mcp_servers = {}\n"
	codexFileAuthConfig   = "cli_auth_credentials_store = \"file\"\n"
	codexHomeMode         = 0o700
	codexConfigMode       = 0o600
	codexMCPConfigKeyPath = "mcp_servers"
	codexAuthFileName     = "auth.json"
)

// codexConfigBoundary is a process-owned user-config layer. Codex app-server
// only permits config/batchWrite against its own user config.toml, so each
// Provider receives a private CODEX_HOME instead of ever targeting the
// operator's persistent config.
type codexConfigBoundary struct {
	home       string
	parent     string
	parentInfo os.FileInfo
	configPath string
	authPath   string
	// hostAuthPath/hostAuthInfo pin the regular host credential file that
	// authPath hard-links at the selected headless spawn boundary. In-place
	// file-backed refreshes remain coherent without copying the credential or
	// admitting the host's config.toml.
	hostAuthPath string
	hostAuthInfo os.FileInfo
	// pluginCacheDir is set only by enablePluginCacheReuse (plugin_cache.go).
	// Its zero value ("") means cache reuse was never opted into for this
	// boundary, and remove() skips the harvest step entirely — the default
	// for every construction path and test that predates this field.
	pluginCacheDir string
	cleanup        sync.Once
	cleanupErr     error
}

func newCodexConfigBoundary(tempDir string, fileAuth bool) (*codexConfigBoundary, error) {
	authMode := ""
	if fileAuth {
		authMode = "file"
	}
	return newCodexConfigBoundaryWithAuthMode(tempDir, authMode)
}

// newCodexConfigBoundaryWithAuthMode builds the private user layer while
// preserving the host's explicit credential-store selection. It copies only
// the one non-secret enum, never the host config or any credential bytes.
func newCodexConfigBoundaryWithAuthMode(tempDir, authMode string) (*codexConfigBoundary, error) {
	authMode = strings.ToLower(strings.TrimSpace(authMode))
	switch authMode {
	case "", "file", "keyring", "auto":
	default:
		return nil, fmt.Errorf("unsupported Codex credential store %q", authMode)
	}
	parent := tempDir
	if parent == "" {
		parent = os.TempDir()
	}
	parent, err := filepath.Abs(parent)
	if err != nil {
		return nil, fmt.Errorf("resolve isolated Codex parent: %w", err)
	}
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return nil, fmt.Errorf("resolve isolated Codex parent links: %w", err)
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return nil, fmt.Errorf("inspect isolated Codex parent: %w", err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return nil, errors.New("isolated Codex parent must be a non-symlink directory")
	}

	home, err := os.MkdirTemp(parent, codexHomePrefix)
	if err != nil {
		return nil, fmt.Errorf("create isolated Codex home: %w", err)
	}
	b := &codexConfigBoundary{
		home:       home,
		parent:     parent,
		parentInfo: parentInfo,
		configPath: filepath.Join(home, codexConfigFileName),
	}
	keep := false
	defer func() {
		if !keep {
			_ = b.remove()
		}
	}()

	if err := os.Chmod(home, codexHomeMode); err != nil {
		return nil, fmt.Errorf("secure isolated Codex home: %w", err)
	}
	if err := rejectSymlink(home, true); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(b.configPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, codexConfigMode)
	if err != nil {
		return nil, fmt.Errorf("create isolated Codex config: %w", err)
	}
	baseline := codexConfigBaseline
	if authMode != "" {
		// Pin the selected store before process initialization. File credential
		// bytes are linked separately, only on the selected Spawn/Resume path.
		baseline = "cli_auth_credentials_store = " + fmt.Sprintf("%q", authMode) + "\n" + baseline
	}
	if _, err := f.WriteString(baseline); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("initialize isolated Codex config: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close isolated Codex config: %w", err)
	}
	if err := os.Chmod(b.configPath, codexConfigMode); err != nil {
		return nil, fmt.Errorf("secure isolated Codex config: %w", err)
	}
	if err := rejectSymlink(b.configPath, false); err != nil {
		return nil, err
	}
	// Record this process as home's owner so a later orphan sweep (see
	// orphan_sweep.go), possibly running in a fresh daemon after this one is
	// long gone, can tell a live session apart from an orphan without
	// guessing from directory age alone. Best-effort — see
	// writeDonmaiOwnerManifest's doc comment.
	writeDonmaiOwnerManifest(home, sweepKindCodexHome)
	keep = true
	return b, nil
}

// resolveHostSessionAuthFile returns the auth.json used by a normal Codex CLI
// invocation on this host. It intentionally resolves only the path; credential
// contents never enter Donmai memory or logs.
func resolveHostSessionAuthFile() (string, error) {
	codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if codexHome == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve host Codex home: %w", err)
		}
		codexHome = filepath.Join(userHome, ".codex")
	}
	if !filepath.IsAbs(codexHome) {
		return "", errors.New("host CODEX_HOME must be absolute")
	}
	return filepath.Join(codexHome, codexAuthFileName), nil
}

// linkHostSessionAuth projects exactly the host login into the private Codex
// home. A hard link is deliberate: unlike a copy it keeps refresh-token writes
// coherent, and unlike reusing the host CODEX_HOME it does not expose ambient
// MCP/project configuration. Cleanup removes only the private directory entry.
func (b *codexConfigBoundary) linkHostSessionAuth(hostAuthFile string) error {
	hostAuthFile = filepath.Clean(hostAuthFile)
	if !filepath.IsAbs(hostAuthFile) {
		return errors.New("host Codex auth file must be absolute")
	}
	if b.authPath != "" {
		if hostAuthFile != b.hostAuthPath {
			return errors.New("host Codex auth path changed")
		}
		return b.validate()
	}
	originalInfo, err := os.Lstat(hostAuthFile)
	if err != nil {
		return fmt.Errorf("inspect host Codex auth file: %w", err)
	}
	if originalInfo.Mode()&os.ModeSymlink != 0 || !originalInfo.Mode().IsRegular() {
		return errors.New("host Codex auth file must be a regular non-symlink file")
	}
	if originalInfo.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("host Codex auth file mode is %04o, must not grant group or other access", originalInfo.Mode().Perm())
	}
	resolved, err := filepath.EvalSymlinks(hostAuthFile)
	if err != nil {
		return fmt.Errorf("resolve host Codex auth file: %w", err)
	}
	resolvedInfo, err := os.Lstat(resolved)
	if err != nil {
		return fmt.Errorf("inspect resolved host Codex auth file: %w", err)
	}
	if resolvedInfo.Mode()&os.ModeSymlink != 0 || !resolvedInfo.Mode().IsRegular() {
		return errors.New("resolved host Codex auth file must be a regular file")
	}
	if resolvedInfo.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("resolved host Codex auth file mode is %04o, must not grant group or other access", resolvedInfo.Mode().Perm())
	}

	authPath := filepath.Join(b.home, codexAuthFileName)
	if err := os.Link(resolved, authPath); err != nil {
		return fmt.Errorf("link host Codex auth into isolated home (paths must share a filesystem): %w", err)
	}
	linkedInfo, err := os.Lstat(authPath)
	if err != nil {
		return errors.Join(fmt.Errorf("inspect isolated Codex auth link: %w", err), removeAuthLink(authPath))
	}
	if linkedInfo.Mode()&os.ModeSymlink != 0 || !linkedInfo.Mode().IsRegular() || !os.SameFile(resolvedInfo, linkedInfo) {
		return errors.Join(errors.New("isolated Codex auth is not the pinned host credential file"), removeAuthLink(authPath))
	}
	b.authPath = authPath
	b.hostAuthPath = resolved
	b.hostAuthInfo = resolvedInfo
	return nil
}

func removeAuthLink(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove incomplete isolated Codex auth link: %w", err)
	}
	return nil
}

func rejectSymlink(path string, wantDir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect isolated Codex path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("isolated Codex path must not be a symlink")
	}
	if wantDir && !info.IsDir() {
		return errors.New("isolated Codex home is not a directory")
	}
	if !wantDir && !info.Mode().IsRegular() {
		return errors.New("isolated Codex config is not a regular file")
	}
	return nil
}

func (b *codexConfigBoundary) validate() error {
	if b == nil {
		return errors.New("isolated Codex config boundary is missing")
	}
	if err := b.validateParent(); err != nil {
		return err
	}
	if err := rejectSymlink(b.home, true); err != nil {
		return err
	}
	if err := rejectSymlink(b.configPath, false); err != nil {
		return err
	}
	homeInfo, err := os.Stat(b.home)
	if err != nil {
		return fmt.Errorf("stat isolated Codex home: %w", err)
	}
	if homeInfo.Mode().Perm() != codexHomeMode {
		return fmt.Errorf("isolated Codex home mode is %04o, want %04o", homeInfo.Mode().Perm(), codexHomeMode)
	}
	configInfo, err := os.Stat(b.configPath)
	if err != nil {
		return fmt.Errorf("stat isolated Codex config: %w", err)
	}
	if configInfo.Mode().Perm() != codexConfigMode {
		return fmt.Errorf("isolated Codex config mode is %04o, want %04o", configInfo.Mode().Perm(), codexConfigMode)
	}
	if b.authPath != "" {
		if b.hostAuthPath == "" || b.hostAuthInfo == nil {
			return errors.New("isolated Codex host auth identity is missing")
		}
		linkedInfo, err := os.Lstat(b.authPath)
		if err != nil {
			return fmt.Errorf("inspect isolated Codex auth: %w", err)
		}
		if linkedInfo.Mode()&os.ModeSymlink != 0 || !linkedInfo.Mode().IsRegular() {
			return errors.New("isolated Codex auth must remain a regular non-symlink file")
		}
		if linkedInfo.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("isolated Codex auth mode is %04o, must not grant group or other access", linkedInfo.Mode().Perm())
		}
		currentHostInfo, err := os.Lstat(b.hostAuthPath)
		if err != nil {
			return fmt.Errorf("inspect pinned host Codex auth: %w", err)
		}
		if currentHostInfo.Mode()&os.ModeSymlink != 0 || !currentHostInfo.Mode().IsRegular() ||
			!os.SameFile(b.hostAuthInfo, currentHostInfo) || !os.SameFile(currentHostInfo, linkedInfo) {
			return errors.New("host Codex auth identity changed")
		}
	}
	return nil
}

func (b *codexConfigBoundary) validateParent() error {
	if b.parent == "" || b.parentInfo == nil {
		return errors.New("isolated Codex parent identity is missing")
	}
	current, err := os.Lstat(b.parent)
	if err != nil {
		return fmt.Errorf("inspect isolated Codex parent: %w", err)
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(b.parentInfo, current) {
		return errors.New("isolated Codex parent identity changed")
	}
	return nil
}

func (b *codexConfigBoundary) remove() error {
	if b == nil || b.home == "" {
		return nil
	}
	b.cleanup.Do(func() {
		clean := filepath.Clean(b.home)
		parent := filepath.Clean(filepath.Dir(clean))
		if parent != b.parent || !strings.HasPrefix(filepath.Base(clean), codexHomePrefix) {
			b.cleanupErr = errors.New("refusing to remove unowned Codex home")
			return
		}
		if err := b.validateParent(); err != nil {
			b.cleanupErr = fmt.Errorf("refusing to remove Codex home: %w", err)
			return
		}
		// Harvest whatever this session's own cache/ subtree fetched that the
		// host-level cache did not already have, so the NEXT session skips
		// that fetch too. Best-effort and skipped entirely when
		// enablePluginCacheReuse was never called (pluginCacheDir == "").
		b.harvestPluginCache()
		// os.RemoveAll removes a symlink itself rather than following it. The
		// path is nevertheless Lstat-validated before process start, and its
		// 0700 mode prevents other users from replacing children.
		if err := os.RemoveAll(clean); err != nil {
			b.cleanupErr = fmt.Errorf("remove isolated Codex home: %w", err)
		}
	})
	return b.cleanupErr
}

func sameResolvedPath(a, b string) bool {
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	if errA == nil && errB == nil {
		return filepath.Clean(resolvedA) == filepath.Clean(resolvedB)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
