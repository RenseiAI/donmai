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
	codexConfigBaseline   = "mcp_servers = {}\n"
	codexHomeMode         = 0o700
	codexConfigMode       = 0o600
	codexMCPConfigKeyPath = "mcp_servers"
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
	cleanup    sync.Once
	cleanupErr error
}

func newCodexConfigBoundary(tempDir string) (*codexConfigBoundary, error) {
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
		configPath: filepath.Join(home, "config.toml"),
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
	if _, err := f.WriteString(codexConfigBaseline); err != nil {
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

	keep = true
	return b, nil
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
