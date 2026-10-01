package localgithub

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	intentVersion  = 1
	maxIntentBytes = 4096
)

type publicationIntent struct {
	Version      int    `json:"version"`
	KeySHA256    string `json:"keySha256"`
	RepositoryID int64  `json:"repositoryId"`
	OwnerRepo    string `json:"ownerRepo"`
	IssueNumber  int    `json:"issueNumber"`
	SessionID    string `json:"sessionId"`
	Status       string `json:"status"`
	ResultSHA256 string `json:"resultSha256"`
	Marker       string `json:"marker"`
	BodySHA256   string `json:"bodySha256"`
	AuthorID     int64  `json:"authorId"`
}

func digestHex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func intentName(key string) string { return digestHex([]byte(key)) + ".json" }

func syncDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func inspectPrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return ErrCorruptPublicationRoot
	}
	return nil
}

func ensurePrivateRoot(path string) error {
	if err := inspectPrivateDirectory(path); err == nil {
		return syncParent(path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return ErrInvalidConfiguration
	}
	if err := ensureParentDirectories(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create private publication root: %w", err)
	}
	if err := inspectPrivateDirectory(path); err != nil {
		return err
	}
	return syncParent(path)
}

func ensureParentDirectories(path string) error {
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return ErrCorruptPublicationRoot
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect publication parent: %w", err)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return ErrCorruptPublicationRoot
	}
	if err := ensureParentDirectories(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create publication parent: %w", err)
	}
	if err := inspectPrivateDirectory(path); err != nil {
		return err
	}
	return syncParent(path)
}

func syncParent(path string) error {
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open publication parent: %w", err)
	}
	defer func() { _ = parent.Close() }()
	if err := syncDirectory(parent); err != nil {
		return fmt.Errorf("sync publication directory linkage: %w", err)
	}
	return nil
}

func validIntentFile(info fs.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode().Perm() == 0o600 && fileLinkCount(info) == 1 &&
		info.Size() >= 0 && info.Size() <= maxIntentBytes
}

func (s *Source) readIntent(name string) (publicationIntent, error) {
	var record publicationIntent
	before, err := s.root.Lstat(name)
	if err != nil {
		return record, err
	}
	if !validIntentFile(before) {
		return record, ErrCorruptPublicationRoot
	}
	file, err := s.root.Open(name)
	if err != nil {
		return record, fmt.Errorf("open private publication intent: %w", err)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !validIntentFile(opened) || !os.SameFile(before, opened) {
		return record, ErrCorruptPublicationRoot
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxIntentBytes+1))
	if err != nil || len(raw) > maxIntentBytes {
		return record, ErrCorruptPublicationRoot
	}
	after, err := s.root.Lstat(name)
	if err != nil || !validIntentFile(after) || !os.SameFile(opened, after) {
		return record, ErrCorruptPublicationRoot
	}
	if json.Unmarshal(raw, &record) != nil {
		return publicationIntent{}, ErrCorruptPublicationRoot
	}
	reencoded, err := json.Marshal(record)
	if err != nil || !bytes.Equal(raw, reencoded) || record.Version != intentVersion ||
		!digestPattern.MatchString(record.KeySHA256) || !digestPattern.MatchString(record.BodySHA256) ||
		!digestPattern.MatchString(record.ResultSHA256) || record.RepositoryID <= 0 || record.IssueNumber <= 0 ||
		!validOwnerRepo(record.OwnerRepo) || !localIDPattern.MatchString(record.SessionID) ||
		record.AuthorID <= 0 || record.Marker == "" {
		return publicationIntent{}, ErrCorruptPublicationRoot
	}
	return record, nil
}

func temporaryIntentName(final string) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("name private publication intent: %w", err)
	}
	return final + ".tmp-" + hex.EncodeToString(suffix[:]), nil
}

// publishIntent commits the possibility of one external POST before the POST
// itself. A visible but unconfirmed link poisons this Source; a new Source must
// fsync/read it and perform GET-only reconciliation, never mint another POST.
func (s *Source) publishIntent(ctx context.Context, name string, value publicationIntent) (bool, error) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > maxIntentBytes {
		return false, ErrCorruptPublicationRoot
	}
	temp, err := temporaryIntentName(name)
	if err != nil {
		return false, err
	}
	file, err := s.root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false, fmt.Errorf("create private publication intent: %w", err)
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
		_ = s.root.Remove(temp)
		return false, fmt.Errorf("sync private publication intent: %w", err)
	}
	if s.fault != nil {
		if err := s.fault("before_link"); err != nil {
			_ = s.root.Remove(temp)
			return false, fmt.Errorf("private publication interrupted before link: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		_ = s.root.Remove(temp)
		return false, err
	}
	if err := s.root.Link(temp, name); err != nil {
		_ = s.root.Remove(temp)
		if errors.Is(err, fs.ErrExist) {
			if syncErr := s.dirSync(s.root); syncErr != nil {
				return false, fmt.Errorf("%w: reaffirm prior intent", ErrAmbiguousPublication)
			}
			return false, nil
		}
		return false, fmt.Errorf("publish private intent: %w", err)
	}
	if err := s.root.Remove(temp); err != nil {
		s.poisoned = true
		return false, fmt.Errorf("%w: temporary intent unlink uncertain", ErrAmbiguousPublication)
	}
	if s.fault != nil {
		if err := s.fault("after_link"); err != nil {
			s.poisoned = true
			return false, fmt.Errorf("%w: intent link sync uncertain", ErrAmbiguousPublication)
		}
	}
	if err := s.dirSync(s.root); err != nil {
		s.poisoned = true
		return false, fmt.Errorf("%w: intent directory sync uncertain", ErrAmbiguousPublication)
	}
	return true, nil
}
