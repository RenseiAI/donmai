// Package skillfs writes a natively delivered skill (agent.NativeSkill) to
// disk and verifies it, for every harness materializer that delivers skills
// as directories (agent.SkillMaterializer).
//
// Two rules carry over from extension delivery (agent/extension_delivery.go):
//
//   - Nothing on the way is followed. Every operation runs through an
//     os.Root opened on the base directory, which the kernel will not let a
//     path escape, and every directory component is checked with Lstat: a
//     symbolic link where a directory belongs refuses the write. Files are
//     created exclusively, never written through an existing inode.
//   - Verification reads back what landed, after writing it, and checks each
//     file's sha256 and mode against the plan, never against the bytes this
//     process meant to write. A file changed between the write and the check
//     fails the materialization.
//
// A target that already exists is refused unless the caller owns the
// location (Options.Replace) or the target carries the runner's marker
// (Options.Marker): a repository's own skill directory is never overwritten.
package skillfs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/stagewire"
)

// Errors a caller can match.
var (
	// ErrTargetExists refuses to overwrite a directory the runner did not
	// write.
	ErrTargetExists = errors.New("skillfs: target exists and is not a runner-materialized skill")
	// ErrUnsafePath refuses a path component that is a symbolic link or
	// not the kind of entry expected.
	ErrUnsafePath = errors.New("skillfs: unsafe path")
	// ErrVerify reports a materialized file that does not match the plan.
	ErrVerify = errors.New("skillfs: materialized skill does not match its plan")
)

// Options tune one Write.
type Options struct {
	// Replace lets Write replace an existing target: the caller owns the
	// location (a harness state directory, a fresh temporary directory).
	Replace bool
	// Marker writes agent.SkillMarkerFile into the target and lets a later
	// Write replace a target that carries it.
	Marker bool
}

// afterWrite is a test seam called between writing and verifying.
var afterWrite func(base, dir string)

// Write materializes skill under base at dir, a forward-slash path of
// directories relative to base whose missing components are created, then
// verifies it. On any failure after the target directory is created, the
// target is removed.
func Write(base, dir string, skill agent.NativeSkill, opts Options) (err error) {
	if err := validateRel(dir); err != nil {
		return err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return fmt.Errorf("skillfs: open %s: %w", base, err)
	}
	defer func() { _ = root.Close() }()
	if parent := path.Dir(dir); parent != "." {
		if err := mkdirAll(root, parent); err != nil {
			return err
		}
	}
	if err := clearTarget(root, dir, opts); err != nil {
		return err
	}
	if err := root.Mkdir(filepath.FromSlash(dir), 0o755); err != nil {
		return fmt.Errorf("skillfs: create %s: %w", dir, err)
	}
	defer func() {
		if err != nil {
			_ = root.RemoveAll(filepath.FromSlash(dir))
		}
	}()
	for _, file := range skill.Entry.Files {
		if err := writeFile(root, dir, skill.Package, file); err != nil {
			return err
		}
	}
	if opts.Marker {
		marker := dir + "/" + agent.SkillMarkerFile
		if err := createFile(root, marker, []byte(skill.Entry.Digest+"\n"), 0o644); err != nil {
			return err
		}
	}
	if afterWrite != nil {
		afterWrite(base, dir)
	}
	return verify(root, dir, skill.Entry)
}

// Verify reads every planned file of entry back from base/dir and checks
// its content and mode against the plan.
func Verify(base, dir string, entry agent.SkillDeliveryEntry) error {
	if err := validateRel(dir); err != nil {
		return err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return fmt.Errorf("skillfs: open %s: %w", base, err)
	}
	defer func() { _ = root.Close() }()
	return verify(root, dir, entry)
}

// Remove deletes base/dir without following a link out of base.
func Remove(base, dir string) error {
	if err := validateRel(dir); err != nil {
		return err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return fmt.Errorf("skillfs: open %s: %w", base, err)
	}
	defer func() { _ = root.Close() }()
	if err := root.RemoveAll(filepath.FromSlash(dir)); err != nil {
		return fmt.Errorf("skillfs: remove %s: %w", dir, err)
	}
	return nil
}

func verify(root *os.Root, dir string, entry agent.SkillDeliveryEntry) error {
	for _, file := range entry.Files {
		rel := filepath.FromSlash(dir + "/" + file.Path)
		info, err := root.Lstat(rel)
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrVerify, file.Path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s is not a regular file", ErrVerify, file.Path)
		}
		if info.Mode().Perm() != fs.FileMode(file.Mode) {
			return fmt.Errorf("%w: %s has mode %04o, want %04o", ErrVerify, file.Path, info.Mode().Perm(), file.Mode)
		}
		got, err := hashFile(root, rel)
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrVerify, file.Path, err)
		}
		if got != file.SHA256 {
			return fmt.Errorf("%w: %s content changed after it was written", ErrVerify, file.Path)
		}
	}
	return nil
}

func hashFile(root *os.Root, rel string) (string, error) {
	f, err := root.Open(rel)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func clearTarget(root *os.Root, dir string, opts Options) error {
	rel := filepath.FromSlash(dir)
	info, err := root.Lstat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("skillfs: inspect %s: %w", dir, err)
	case info.Mode()&fs.ModeSymlink != 0 || !info.IsDir():
		return fmt.Errorf("%w: %s is not a directory", ErrUnsafePath, dir)
	}
	if !opts.Replace && !(opts.Marker && hasMarker(root, dir)) {
		return fmt.Errorf("%w: %s", ErrTargetExists, dir)
	}
	if err := root.RemoveAll(rel); err != nil {
		return fmt.Errorf("skillfs: replace %s: %w", dir, err)
	}
	return nil
}

func hasMarker(root *os.Root, dir string) bool {
	info, err := root.Lstat(filepath.FromSlash(dir + "/" + agent.SkillMarkerFile))
	return err == nil && info.Mode().IsRegular()
}

func writeFile(root *os.Root, dir string, pkg stagewire.Skill, file agent.SkillPlannedFile) error {
	if err := agent.ValidateSkillPath(file.Path); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnsafePath, file.Path, err)
	}
	content, err := agent.SkillFileContent(pkg, file.Path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(content)
	if hex.EncodeToString(sum[:]) != file.SHA256 {
		return fmt.Errorf("%w: %s content does not match the plan", ErrVerify, file.Path)
	}
	target := dir + "/" + file.Path
	if parent := path.Dir(target); parent != dir {
		if err := mkdirAll(root, parent); err != nil {
			return err
		}
	}
	return createFile(root, target, content, fs.FileMode(file.Mode))
}

func createFile(root *os.Root, target string, content []byte, perm fs.FileMode) error {
	rel := filepath.FromSlash(target)
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("skillfs: create %s: %w", target, err)
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("skillfs: write %s: %w", target, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("skillfs: close %s: %w", target, err)
	}
	// The process umask may have narrowed perm; the plan states the exact
	// mode, so set it.
	if err := root.Chmod(rel, perm); err != nil {
		return fmt.Errorf("skillfs: chmod %s: %w", target, err)
	}
	return nil
}

// mkdirAll creates rel and every missing parent under root, refusing any
// component that is a symbolic link or not a directory.
func mkdirAll(root *os.Root, rel string) error {
	current := ""
	for _, part := range strings.Split(rel, "/") {
		current = path.Join(current, part)
		native := filepath.FromSlash(current)
		info, err := root.Lstat(native)
		if errors.Is(err, fs.ErrNotExist) {
			if mkErr := root.Mkdir(native, 0o755); mkErr != nil && !errors.Is(mkErr, fs.ErrExist) {
				return fmt.Errorf("skillfs: create %s: %w", current, mkErr)
			}
			info, err = root.Lstat(native)
		}
		if err != nil {
			return fmt.Errorf("skillfs: inspect %s: %w", current, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: %s is not a directory", ErrUnsafePath, current)
		}
	}
	return nil
}

// validateRel accepts a forward-slash relative directory path with no
// empty, ".", or ".." segment.
func validateRel(dir string) error {
	if dir == "" || strings.HasPrefix(dir, "/") || strings.Contains(dir, "\\") {
		return fmt.Errorf("%w: %q", ErrUnsafePath, dir)
	}
	for _, segment := range strings.Split(dir, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("%w: %q", ErrUnsafePath, dir)
		}
	}
	return nil
}
