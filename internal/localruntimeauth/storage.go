package localruntimeauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

var (
	digestPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	lockTempPattern = regexp.MustCompile(`^writer\.lock\.tmp-[0-9a-f]{16}$`)
)

func validDigest(value string) bool { return digestPattern.MatchString(value) }

func syncDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

// Existing ancestors may be system directories; every newly created
// ancestor and its parent linkage are private and synced one level at a time.
func ensureParentDirectories(path string) error {
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return ErrCorruptState
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect auth parent: %w", err)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return ErrCorruptState
	}
	if err := ensureParentDirectories(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create auth parent: %w", err)
	}
	if err := inspectPrivateDirectory(path); err != nil {
		return err
	}
	return syncParentLink(path)
}

func inspectPrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return ErrCorruptState
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	if err := inspectPrivateDirectory(path); err == nil {
		// Reaffirm a prior mkdir whose parent-link fsync may have been lost.
		return syncParentLink(path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ensureParentDirectories(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create private auth directory: %w", err)
	}
	if err := inspectPrivateDirectory(path); err != nil {
		return err
	}
	return syncParentLink(path)
}

func syncParentLink(path string) error {
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open auth parent for sync: %w", err)
	}
	defer func() { _ = parent.Close() }()
	if err := syncDirectory(parent); err != nil {
		return fmt.Errorf("sync auth directory linkage: %w", err)
	}
	return nil
}

func privateRegular(info fs.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode().Perm() == 0o600 && fileLinkCount(info) == 1
}

func readRecord(root *os.Root, name string, target any) error {
	before, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !privateRegular(before) || before.Size() < 0 || before.Size() > maxProtectedBytes {
		return ErrCorruptState
	}
	file, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("open protected record: %w", err)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !privateRegular(opened) || !os.SameFile(before, opened) {
		return ErrCorruptState
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxProtectedBytes+1))
	if err != nil || len(raw) > maxProtectedBytes {
		return ErrCorruptState
	}
	after, err := root.Lstat(name)
	if err != nil || !privateRegular(after) || !os.SameFile(opened, after) {
		return ErrCorruptState
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return ErrCorruptState
	}
	// Our writer has exactly one encoding. Byte equality also rejects unknown
	// fields, duplicate members, trailing JSON and alternate spellings.
	reencoded, err := json.Marshal(target)
	if err != nil || !bytes.Equal(raw, reencoded) {
		return ErrCorruptState
	}
	return nil
}

func encodeRecord(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > maxProtectedBytes {
		return nil, ErrInvalidInput
	}
	return raw, nil
}

func openWriterLock(root *os.Root) (*os.File, bool, error) {
	info, err := root.Lstat(writerLockFileName)
	if err == nil {
		return openExistingWriterLock(root, info)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("inspect auth lock file: %w", err)
	}
	// Lock a private temporary inode BEFORE publishing the well-known name.
	// A concurrent opener can then only wait for this exact initialization or
	// race its own temporary link; it cannot see an unlocked new writer.lock.
	temp, err := randomTempName(writerLockFileName)
	if err != nil {
		return nil, false, err
	}
	file, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("create private auth lock: %w", err)
	}
	if err := lockWriter(file); err != nil {
		_ = file.Close()
		_ = root.Remove(temp)
		return nil, false, err
	}
	if err := file.Sync(); err != nil {
		_ = unlockWriter(file)
		_ = file.Close()
		_ = root.Remove(temp)
		return nil, false, fmt.Errorf("sync private auth lock: %w", err)
	}
	if err := root.Link(temp, writerLockFileName); err != nil {
		_ = unlockWriter(file)
		_ = file.Close()
		_ = root.Remove(temp)
		_ = syncDirectory(root)
		if errors.Is(err, fs.ErrExist) {
			info, err := root.Lstat(writerLockFileName)
			if err != nil {
				return nil, false, ErrCorruptState
			}
			return openExistingWriterLock(root, info)
		}
		return nil, false, fmt.Errorf("publish private auth lock: %w", err)
	}
	if err := root.Remove(temp); err != nil {
		_ = unlockWriter(file)
		_ = file.Close()
		return nil, false, fmt.Errorf("%w: unlink temporary auth lock: %v", ErrAmbiguousCommit, err)
	}
	if err := syncDirectory(root); err != nil {
		_ = unlockWriter(file)
		_ = file.Close()
		return nil, false, fmt.Errorf("%w: sync auth lock publication: %v", ErrAmbiguousCommit, err)
	}
	info, err = root.Lstat(writerLockFileName)
	if err != nil || !privateRegular(info) || info.Size() != 0 {
		_ = unlockWriter(file)
		_ = file.Close()
		return nil, false, ErrCorruptState
	}
	return file, true, nil // already locked
}

func openExistingWriterLock(root *os.Root, info fs.FileInfo) (*os.File, bool, error) {
	links := fileLinkCount(info)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != 0 || (links != 1 && links != 2) {
		return nil, false, ErrCorruptState
	}
	file, err := root.OpenFile(writerLockFileName, os.O_RDWR, 0)
	if err != nil {
		return nil, false, fmt.Errorf("open auth lock file: %w", err)
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Mode().Perm() != 0o600 || opened.Size() != 0 {
		_ = file.Close()
		return nil, false, ErrCorruptState
	}
	if err := lockWriter(file); err != nil {
		_ = file.Close()
		return nil, false, err
	}
	after, err := root.Lstat(writerLockFileName)
	if err != nil || !privateRegular(after) || !os.SameFile(opened, after) || after.Size() != 0 {
		_ = unlockWriter(file)
		_ = file.Close()
		return nil, false, ErrCorruptState
	}
	return file, false, nil // already locked
}

func openReaderLock(root *os.Root) (*os.File, error) {
	info, err := root.Lstat(writerLockFileName)
	if errors.Is(err, os.ErrNotExist) {
		prior, inspectErr := authRootHasEntries(root)
		if inspectErr != nil {
			return nil, inspectErr
		}
		if prior {
			return nil, fmt.Errorf("%w: existing auth root lacks writer lock; preserve it for explicit recovery", ErrCorruptState)
		}
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, fmt.Errorf("inspect private auth read lock: %w", err)
	}
	links := fileLinkCount(info)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != 0 || (links != 1 && links != 2) {
		return nil, ErrCorruptState
	}
	file, err := root.OpenFile(writerLockFileName, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open private auth read lock: %w", err)
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Mode().Perm() != 0o600 || opened.Size() != 0 {
		_ = file.Close()
		return nil, ErrCorruptState
	}
	if err := lockReader(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	after, err := root.Lstat(writerLockFileName)
	if err != nil || !privateRegular(after) || !os.SameFile(opened, after) || after.Size() != 0 {
		_ = unlockWriter(file)
		_ = file.Close()
		return nil, ErrCorruptState
	}
	return file, nil // shared lock held through caller readback
}

func authRootHasEntries(root *os.Root) (bool, error) {
	dir, err := root.Open(".")
	if err != nil {
		return false, fmt.Errorf("inspect existing auth root: %w", err)
	}
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(1)
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("list existing auth root: %w", err)
	}
	return len(entries) != 0, nil
}

func requireFreshRoot(root *os.Root, queueRoot string) error {
	dir, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("inspect fresh auth root: %w", err)
	}
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("list fresh auth root: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() != writerLockFileName && !lockTempPattern.MatchString(entry.Name()) {
			return ErrCorruptState
		}
	}
	// A vanished bootstrap alongside an existing queue journal must never
	// mint a new identity that disagrees with the queue's durable authority.
	// A fresh queue root may be absent or an empty private directory only.
	info, err := os.Lstat(queueRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return ErrCorruptState
	}
	queue, err := os.OpenRoot(queueRoot)
	if err != nil {
		return fmt.Errorf("inspect fresh queue root: %w", err)
	}
	defer func() { _ = queue.Close() }()
	queueDir, err := queue.Open(".")
	if err != nil {
		return fmt.Errorf("open fresh queue root: %w", err)
	}
	defer func() { _ = queueDir.Close() }()
	queueEntries, err := queueDir.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("list fresh queue root: %w", err)
	}
	if len(queueEntries) != 0 {
		return ErrCorruptState
	}
	return nil
}

func randomTempName(final string) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generate private temporary name: %w", err)
	}
	return final + ".tmp-" + hex.EncodeToString(suffix[:]), nil
}

func (s *Store) writeTemp(root *os.Root, final string, raw []byte) (string, error) {
	temp, err := randomTempName(final)
	if err != nil {
		return "", err
	}
	file, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create protected temporary record: %w", err)
	}
	written, writeErr := file.Write(raw)
	if writeErr == nil && written != len(raw) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = s.fileSync(file)
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = root.Remove(temp)
		return "", fmt.Errorf("sync protected temporary record: %w", err)
	}
	return temp, nil
}

// writeImmutable publishes only after file fsync, then fsyncs the final link.
// A link visible without a confirmed directory sync poisons this handle.
func (s *Store) writeImmutable(ctx context.Context, root *os.Root, final string, raw []byte) (bool, error) {
	temp, err := s.writeTemp(root, final, raw)
	if err != nil {
		return false, err
	}
	if s.fault != nil {
		if err := s.fault("before_link"); err != nil {
			_ = root.Remove(temp)
			return false, fmt.Errorf("private write interrupted before publication: %w", err)
		}
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			_ = root.Remove(temp)
			return false, err
		}
	}
	if err := root.Link(temp, final); err != nil {
		_ = root.Remove(temp)
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("publish immutable private record: %w", err)
	}
	if err := root.Remove(temp); err != nil {
		s.poisoned = true
		return false, fmt.Errorf("%w: unlink published temporary record: %v", ErrAmbiguousCommit, err)
	}
	if s.fault != nil {
		if err := s.fault("after_link"); err != nil {
			s.poisoned = true
			return false, fmt.Errorf("%w: %v", ErrAmbiguousCommit, err)
		}
	}
	if err := s.dirSync(root); err != nil {
		s.poisoned = true
		return false, fmt.Errorf("%w: sync private record directory: %v", ErrAmbiguousCommit, err)
	}
	return true, nil
}
