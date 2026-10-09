package pi

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/RenseiAI/donmai/runtime/harnessstate"
)

// Every write the harness's PARENT makes into a session's state — the state
// root, the boundary extension, injected extensions, the session temporary and
// cache directories, and the checkout's exclude entry — goes through
// sessionStateFS. The parent runs unconfined, and on Resume the state it
// writes into was writable by the confined seat for the whole previous run:
// the seat can leave a symbolic link where the parent expects a directory or
// a file. A parent that followed one would create, append to or overwrite a
// path outside the confined set before the confinement check that later
// refuses the spawn ever ran.
//
// So the parent never follows one. Every operation runs relative to a
// directory handle (os.Root) opened on the directory holding the state root —
// the session working directory — which the kernel will not let a path
// escape, and every path component under it is checked with Lstat first: a
// symbolic link anywhere on the way refuses the spawn before anything is
// written. Files are written fresh (a new file renamed into place), never
// through an existing inode, so a hard link the seat planted cannot carry the
// write out either; the one file written in place (the exclude file, which is
// appended to) is refused when it has a second link. For a confined session the working
// directory itself must be a real directory: the seat can replace its own
// leaf, and the handle must not open on a link it planted.

// errUnsafeStatePath marks a refusal: a parent-side write path under the
// session working directory is a symbolic link, is not the kind of entry
// expected, or is a file with a second hard link.
var errUnsafeStatePath = errors.New("pi: unsafe session state path")

// sessionStateFS writes inside the directory that holds a session's state
// root. Close it when done.
type sessionStateFS struct {
	base string
	root *os.Root
}

// openSessionStateFS opens the writer for layout. strict (a confined session)
// requires the base directory to exist and to be a real directory, not a
// symbolic link, and checks the opened handle is that same directory. An
// unconfined session's base is created when missing, as the state root's
// MkdirAll always did.
func openSessionStateFS(layout sessionLayout) (*sessionStateFS, error) {
	base := filepath.Dir(filepath.Clean(layout.root))
	var before fs.FileInfo
	if layout.strict {
		info, err := os.Lstat(base)
		if err != nil {
			return nil, fmt.Errorf("pi: session working directory: %w", err)
		}
		if err := requireKind(info, "session working directory", fs.ModeDir); err != nil {
			return nil, err
		}
		before = info
	} else if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, fmt.Errorf("pi: create session working directory: %w", err)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, fmt.Errorf("pi: open session working directory: %w", err)
	}
	if before != nil {
		opened, err := root.Stat(".")
		if err != nil || !os.SameFile(before, opened) {
			_ = root.Close()
			return nil, fmt.Errorf("%w: the session working directory changed while it was opened", errUnsafeStatePath)
		}
	}
	return &sessionStateFS{base: base, root: root}, nil
}

// Close releases the directory handle.
func (s *sessionStateFS) Close() error { return s.root.Close() }

// rel returns abs relative to the base. A path outside the base is a
// programming error and is refused.
func (s *sessionStateFS) rel(abs string) (string, error) {
	rel, err := filepath.Rel(s.base, filepath.Clean(abs))
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("pi: %q is not under the session working directory", abs)
	}
	return rel, nil
}

// mkdirAll creates abs and every missing parent under the base, refusing any
// component that is a symbolic link or not a directory.
func (s *sessionStateFS) mkdirAll(abs string, perm fs.FileMode) error {
	rel, err := s.rel(abs)
	if err != nil {
		return err
	}
	return s.mkdirAllRel(rel, perm)
}

func (s *sessionStateFS) mkdirAllRel(rel string, perm fs.FileMode) error {
	current := ""
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := s.root.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if mkErr := s.root.Mkdir(current, perm); mkErr != nil && !errors.Is(mkErr, fs.ErrExist) {
				return fmt.Errorf("pi: create %s: %w", current, mkErr)
			}
			info, err = s.root.Lstat(current)
		}
		if err != nil {
			return fmt.Errorf("pi: inspect %s: %w", current, err)
		}
		if err := requireKind(info, current, fs.ModeDir); err != nil {
			return err
		}
	}
	return nil
}

// writeFile writes content to abs as a new file renamed into place: the
// parent directories are checked (and created) like mkdirAll, an existing
// entry at abs must be a regular file, and the old inode is never opened, so
// neither a symbolic link nor a hard link at abs can redirect the bytes.
func (s *sessionStateFS) writeFile(abs string, content []byte, perm fs.FileMode) error {
	rel, err := s.rel(abs)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(rel); dir != "." {
		if err := s.mkdirAllRel(dir, 0o700); err != nil {
			return err
		}
	}
	info, err := s.root.Lstat(rel)
	switch {
	case err == nil:
		if err := requireKind(info, rel, 0); err != nil {
			return err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("pi: inspect %s: %w", rel, err)
	}
	tmp, err := s.createTemp(rel, perm)
	if err != nil {
		return err
	}
	if _, err := tmp.file.Write(content); err != nil {
		_ = tmp.file.Close()
		_ = s.root.Remove(tmp.rel)
		return fmt.Errorf("pi: write %s: %w", rel, err)
	}
	if err := tmp.file.Close(); err != nil {
		_ = s.root.Remove(tmp.rel)
		return fmt.Errorf("pi: write %s: %w", rel, err)
	}
	if err := s.root.Rename(tmp.rel, rel); err != nil {
		_ = s.root.Remove(tmp.rel)
		return fmt.Errorf("pi: place %s: %w", rel, err)
	}
	return nil
}

type stateTemp struct {
	file *os.File
	rel  string
}

// createTemp creates a fresh, exclusively-created sibling of rel.
func (s *sessionStateFS) createTemp(rel string, perm fs.FileMode) (stateTemp, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return stateTemp{}, fmt.Errorf("pi: temp name for %s: %w", rel, err)
	}
	name := filepath.Join(filepath.Dir(rel), "."+filepath.Base(rel)+".tmp-"+hex.EncodeToString(suffix[:]))
	f, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return stateTemp{}, fmt.Errorf("pi: create %s: %w", rel, err)
	}
	return stateTemp{file: f, rel: name}, nil
}

// ensureGitExcluded adds entry to the exclude file of the repository rooted
// at the base, read from the base's own .git directory. It never asks git:
// git follows a .git file or a commondir pointer anywhere, and both are seat
// content. A base with no .git directory of its own (none, or a .git file)
// gets no entry, which only makes `git status` noisier. A symbolic link or a
// second hard link on the way returns errUnsafeStatePath; any other failure
// is best effort and returns nil, exactly as the unconfined exclude write is.
func (s *sessionStateFS) ensureGitExcluded(entry string) error {
	info, err := s.root.Lstat(".git")
	if err != nil {
		return nil
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: .git is a symbolic link", errUnsafeStatePath)
	}
	if !info.IsDir() {
		return nil
	}
	if err := s.mkdirAllRel(filepath.Join(".git", "info"), 0o755); err != nil {
		if errors.Is(err, errUnsafeStatePath) {
			return err
		}
		return nil
	}
	rel := filepath.Join(".git", "info", "exclude")
	if info, err := s.root.Lstat(rel); err == nil {
		if err := requireKind(info, rel, 0); err != nil {
			return err
		}
	}
	f, err := s.root.OpenFile(rel, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return nil
	}
	if err := requireKind(opened, rel, 0); err != nil {
		return err
	}
	if err := requireSingleLink(opened, rel); err != nil {
		return err
	}
	existing, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	if appendix := harnessstate.ExcludeAppendix(existing, entry); len(appendix) > 0 {
		_, _ = f.Write(appendix)
	}
	return nil
}

// requireKind refuses a symbolic link and an entry of the wrong kind (want is
// fs.ModeDir for a directory, 0 for a regular file).
func requireKind(info fs.FileInfo, name string, want fs.FileMode) error {
	mode := info.Mode()
	if mode&fs.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symbolic link", errUnsafeStatePath, name)
	}
	if want == fs.ModeDir {
		if !mode.IsDir() {
			return fmt.Errorf("%w: %s is not a directory", errUnsafeStatePath, name)
		}
		return nil
	}
	if !mode.IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", errUnsafeStatePath, name)
	}
	return nil
}

// requireSingleLink refuses a file with a second hard link: a write through
// it would land in an inode another path, possibly outside the confined set,
// also names. Only a file written in place needs this; writeFile replaces the
// entry and never opens the old inode.
func requireSingleLink(info fs.FileInfo, name string) error {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && uint64(st.Nlink) > 1 {
		return fmt.Errorf("%w: %s has another hard link", errUnsafeStatePath, name)
	}
	return nil
}

// writeStateFile writes one file into layout's state through a fresh
// sessionStateFS.
func writeStateFile(layout sessionLayout, abs string, content []byte, perm fs.FileMode) error {
	state, err := openSessionStateFS(layout)
	if err != nil {
		return err
	}
	defer func() { _ = state.Close() }()
	return state.writeFile(abs, content, perm)
}
