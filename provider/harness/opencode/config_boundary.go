package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/RenseiAI/donmai/agent"
)

const (
	openCodeConfigPrefix = "donmai-opencode-config-"
	openCodeConfigMode   = 0o600
	openCodeHomeMode     = 0o700
)

var (
	errOpenCodeConfigCleanup  = errors.New("provider/opencode: owned session config cleanup failed")
	errOpenCodeResourceClosed = errors.New("provider/opencode: owned session resource already closed")
	errOpenCodeShutdown       = errors.New("provider/opencode: provider is shut down")
)

// openCodeConfigBoundary owns one binary-backed config outside the session worktree.
// The file may contain remote MCP headers, so it is never placed in the repo,
// reused by another session, or retained after its run/serve handle stops.
type openCodeConfigBoundary struct {
	home       string
	homeInfo   os.FileInfo
	parent     string
	parentInfo os.FileInfo
	configPath string
	configInfo os.FileInfo

	cleanup    sync.Once
	cleanupErr error
}

func newOpenCodeConfigBoundary(tempDir string, spec agent.Spec) (_ *openCodeConfigBoundary, resultErr error) {
	parent := tempDir
	if parent == "" {
		parent = os.TempDir()
	}
	parent, err := filepath.Abs(parent)
	if err != nil {
		return nil, fmt.Errorf("provider/opencode: resolve config parent: %w", err)
	}
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return nil, fmt.Errorf("provider/opencode: resolve config parent links: %w", err)
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return nil, fmt.Errorf("provider/opencode: inspect config parent: %w", err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return nil, errors.New("provider/opencode: config parent must be a non-symlink directory")
	}

	home, err := os.MkdirTemp(parent, openCodeConfigPrefix)
	if err != nil {
		return nil, fmt.Errorf("provider/opencode: create config boundary: %w", err)
	}
	b := &openCodeConfigBoundary{
		home:       home,
		parent:     parent,
		parentInfo: parentInfo,
		configPath: filepath.Join(home, "opencode.json"),
	}
	defer func() {
		if resultErr != nil {
			if cleanupErr := b.remove(); cleanupErr != nil {
				resultErr = errors.Join(resultErr, errOpenCodeConfigCleanup)
			}
		}
	}()

	b.homeInfo, err = os.Lstat(home)
	if err != nil {
		return nil, fmt.Errorf("provider/opencode: inspect config boundary: %w", err)
	}
	if b.homeInfo.Mode()&os.ModeSymlink != 0 || !b.homeInfo.IsDir() {
		return nil, errors.New("provider/opencode: config boundary must be a non-symlink directory")
	}
	if err := os.Chmod(home, openCodeHomeMode); err != nil {
		return nil, fmt.Errorf("provider/opencode: secure config boundary: %w", err)
	}

	data, err := json.MarshalIndent(buildConfig(spec), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("provider/opencode: marshal config: %w", err)
	}
	f, err := os.OpenFile(b.configPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, openCodeConfigMode)
	if err != nil {
		return nil, fmt.Errorf("provider/opencode: create config: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("provider/opencode: write config: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("provider/opencode: close config: %w", err)
	}
	if err := os.Chmod(b.configPath, openCodeConfigMode); err != nil {
		return nil, fmt.Errorf("provider/opencode: secure config: %w", err)
	}
	b.configInfo, err = os.Lstat(b.configPath)
	if err != nil {
		return nil, fmt.Errorf("provider/opencode: inspect config: %w", err)
	}
	if b.configInfo.Mode()&os.ModeSymlink != 0 || !b.configInfo.Mode().IsRegular() {
		return nil, errors.New("provider/opencode: config must be a regular non-symlink file")
	}
	if err := b.validate(); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *openCodeConfigBoundary) validate() error {
	if b == nil {
		return errors.New("provider/opencode: config boundary is missing")
	}
	if err := b.validateParent(); err != nil {
		return err
	}
	homeInfo, err := os.Lstat(b.home)
	if err != nil {
		return fmt.Errorf("provider/opencode: inspect config boundary: %w", err)
	}
	if homeInfo.Mode()&os.ModeSymlink != 0 || !homeInfo.IsDir() || b.homeInfo == nil || !os.SameFile(b.homeInfo, homeInfo) {
		return errors.New("provider/opencode: config boundary identity changed")
	}
	if homeInfo.Mode().Perm() != openCodeHomeMode {
		return fmt.Errorf("provider/opencode: config boundary mode is %04o, want %04o", homeInfo.Mode().Perm(), openCodeHomeMode)
	}
	configInfo, err := os.Lstat(b.configPath)
	if err != nil {
		return fmt.Errorf("provider/opencode: inspect config: %w", err)
	}
	if configInfo.Mode()&os.ModeSymlink != 0 || !configInfo.Mode().IsRegular() || b.configInfo == nil || !os.SameFile(b.configInfo, configInfo) {
		return errors.New("provider/opencode: config identity changed")
	}
	if configInfo.Mode().Perm() != openCodeConfigMode {
		return fmt.Errorf("provider/opencode: config mode is %04o, want %04o", configInfo.Mode().Perm(), openCodeConfigMode)
	}
	return nil
}

func (b *openCodeConfigBoundary) validateParent() error {
	if b.parent == "" || b.parentInfo == nil {
		return errors.New("provider/opencode: config parent identity is missing")
	}
	current, err := os.Lstat(b.parent)
	if err != nil {
		return fmt.Errorf("provider/opencode: inspect config parent: %w", err)
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(b.parentInfo, current) {
		return errors.New("provider/opencode: config parent identity changed")
	}
	return nil
}

func (b *openCodeConfigBoundary) remove() error {
	if b == nil || b.home == "" {
		return nil
	}
	b.cleanup.Do(func() {
		clean := filepath.Clean(b.home)
		if filepath.Clean(filepath.Dir(clean)) != b.parent || !strings.HasPrefix(filepath.Base(clean), openCodeConfigPrefix) {
			b.cleanupErr = errors.New("provider/opencode: refusing to remove unowned config boundary")
			return
		}
		if err := b.validateParent(); err != nil {
			b.cleanupErr = fmt.Errorf("provider/opencode: refusing config cleanup: %w", err)
			return
		}
		current, err := os.Lstat(clean)
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			b.cleanupErr = fmt.Errorf("provider/opencode: inspect config cleanup target: %w", err)
			return
		}
		if current.Mode()&os.ModeSymlink != 0 {
			// Removing the link itself is safe and never traverses its target.
			if err := os.Remove(clean); err != nil {
				b.cleanupErr = fmt.Errorf("provider/opencode: remove config boundary link: %w", err)
			}
			return
		}
		if b.homeInfo == nil || !current.IsDir() || !os.SameFile(b.homeInfo, current) {
			b.cleanupErr = errors.New("provider/opencode: refusing to remove replaced config boundary")
			return
		}
		// RemoveAll does not follow a symlink stored inside the owned directory.
		if err := os.RemoveAll(clean); err != nil {
			b.cleanupErr = fmt.Errorf("provider/opencode: remove config boundary: %w", err)
		}
	})
	return b.cleanupErr
}

// openCodeServerResource couples a serve child to the config it consumes.
// attachChild and close may race with Provider.Shutdown; exactly one close path
// wins, and a late child is stopped instead of escaping provider ownership.
type openCodeServerResource struct {
	mu     sync.Mutex
	child  *serveChild
	config *openCodeConfigBoundary
	closed bool

	closeOnce sync.Once
	closeErr  error
}

func (r *openCodeServerResource) attachChild(child *serveChild) error {
	if r == nil || child == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		child.stop()
		return errOpenCodeResourceClosed
	}
	r.child = child
	r.mu.Unlock()
	return nil
}

func (r *openCodeServerResource) close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		child := r.child
		r.mu.Unlock()
		if child != nil {
			child.stop()
		}
		if r.config != nil {
			if err := r.config.remove(); err != nil {
				r.closeErr = errOpenCodeConfigCleanup
			}
		}
	})
	return r.closeErr
}

func openCodeConfigCleanupEvent() agent.ErrorEvent {
	return agent.ErrorEvent{
		Message: "opencode owned session configuration cleanup failed",
		Code:    "config_cleanup_failed",
	}
}
