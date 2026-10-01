package localqueue

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
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/executioncell"
)

var temporaryTransactionName = regexp.MustCompile(`^[0-9]{20}\.json\.tmp-[0-9a-f]{16}$`)

const (
	journalVersion      = 1
	maxTransactionBytes = 16 << 20
	kindAuthority       = "authority"
	kindAdmit           = "admit"
	kindClaim           = "claim"
	kindLaunchIntent    = "launch_intent"
	kindObserveLaunch   = "observe_launch"
	kindObservation     = "observation"
	kindTerminal        = "terminal"
	kindPublication     = "publication"
)

type journalTransaction struct {
	Version  int             `json:"version"`
	Revision Revision        `json:"revision"`
	Previous string          `json:"previousSha256"`
	Kind     string          `json:"kind"`
	Data     json.RawMessage `json:"data"`
	SHA256   string          `json:"sha256"`
}

func transactionName(revision Revision) string { return fmt.Sprintf("%020d.json", revision) }

func canonicalTransaction(tx journalTransaction) ([]byte, error) {
	return executioncell.CanonicalJSON(tx)
}

func transactionDigest(tx journalTransaction) (string, error) {
	tx.SHA256 = ""
	canonical, err := canonicalTransaction(tx)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func decodeCanonicalTransaction(raw []byte) (journalTransaction, error) {
	var tx journalTransaction
	canonical, err := executioncell.CanonicalJSON(json.RawMessage(raw))
	if err != nil || !bytes.Equal(canonical, raw) {
		return tx, fmt.Errorf("%w: transaction is not canonical JSON", ErrCorruptStore)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&tx); err != nil {
		return tx, fmt.Errorf("%w: decode transaction: %v", ErrCorruptStore, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return tx, fmt.Errorf("%w: trailing transaction bytes", ErrCorruptStore)
	}
	if tx.Version != journalVersion || !validSHA256(tx.SHA256) || !validSHA256(tx.Previous) {
		return tx, fmt.Errorf("%w: transaction header invalid", ErrCorruptStore)
	}
	digest, err := transactionDigest(tx)
	if err != nil || digest != tx.SHA256 {
		return tx, fmt.Errorf("%w: transaction hash mismatch", ErrCorruptStore)
	}
	return tx, nil
}

func syncDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func randomSuffix() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (s *Store) recoverJournal() error {
	dir, err := s.journalRoot.Open(".")
	if err != nil {
		return fmt.Errorf("%w: open journal directory: %v", ErrCorruptStore, err)
	}
	entries, err := dir.ReadDir(-1)
	closeErr := dir.Close()
	if err != nil || closeErr != nil {
		return fmt.Errorf("%w: read journal: %v", ErrCorruptStore, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if temporaryTransactionName.MatchString(name) {
			// A pre-link crash leaves only an unpublished temporary file.
			info, statErr := s.journalRoot.Lstat(name)
			if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maxTransactionBytes {
				return fmt.Errorf("%w: unsafe orphan temporary transaction", ErrCorruptStore)
			}
			continue
		}
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			return fmt.Errorf("%w: unexpected journal member", ErrCorruptStore)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for index, name := range names {
		expected := Revision(index + 1)
		if name != transactionName(expected) {
			return fmt.Errorf("%w: revision gap or unexpected filename", ErrCorruptStore)
		}
		info, err := s.journalRoot.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maxTransactionBytes {
			return fmt.Errorf("%w: journal member is unsafe or oversized", ErrCorruptStore)
		}
		file, err := s.journalRoot.Open(name)
		if err != nil {
			return fmt.Errorf("%w: open journal member: %v", ErrCorruptStore, err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, maxTransactionBytes+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(raw) > maxTransactionBytes {
			return fmt.Errorf("%w: read journal member", ErrCorruptStore)
		}
		tx, err := decodeCanonicalTransaction(raw)
		if err != nil {
			return err
		}
		if tx.Revision != expected || tx.Previous != s.previousHash() {
			return fmt.Errorf("%w: noncontiguous revision/hash chain", ErrCorruptStore)
		}
		if err := s.applyTransaction(tx); err != nil {
			if errors.Is(err, ErrBindingMismatch) {
				return err
			}
			return fmt.Errorf("%w: replay revision %d: %v", ErrCorruptStore, expected, err)
		}
		s.revision = expected
		s.digest = tx.SHA256
	}
	if s.revision != 0 && s.authority.ScopeID == "" {
		return fmt.Errorf("%w: missing authority transaction", ErrCorruptStore)
	}
	return nil
}

func (s *Store) previousHash() string {
	if s.revision == 0 {
		return strings.Repeat("0", 64)
	}
	return s.digest
}

func (s *Store) commitLocked(ctx context.Context, expected Revision, kind string, payload any) error {
	if s.closed || s.poisoned {
		return ErrCorruptStore
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if expected != s.revision {
		return ErrConflict
	}
	data, err := executioncell.CanonicalJSON(payload)
	if err != nil {
		return fmt.Errorf("localqueue: encode transaction payload: %w", err)
	}
	tx := journalTransaction{
		Version: journalVersion, Revision: expected + 1, Previous: s.previousHash(),
		Kind: kind, Data: data,
	}
	tx.SHA256, err = transactionDigest(tx)
	if err != nil {
		return fmt.Errorf("localqueue: hash transaction: %w", err)
	}
	raw, err := canonicalTransaction(tx)
	if err != nil || len(raw) > maxTransactionBytes {
		return fmt.Errorf("localqueue: transaction is too large or not canonical")
	}
	suffix, err := randomSuffix()
	if err != nil {
		return fmt.Errorf("localqueue: name temporary transaction: %w", err)
	}
	finalName := transactionName(tx.Revision)
	tempName := finalName + ".tmp-" + suffix
	file, err := s.journalRoot.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("localqueue: create temporary transaction: %w", err)
	}
	writeCount, writeErr := file.Write(raw)
	if writeErr == nil && writeCount != len(raw) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = s.fileSync(file)
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = s.journalRoot.Remove(tempName)
		return fmt.Errorf("localqueue: sync temporary transaction: %w", errors.Join(writeErr, closeErr))
	}
	if s.fault != nil {
		if err := s.fault("before_link"); err != nil {
			_ = s.journalRoot.Remove(tempName)
			return fmt.Errorf("localqueue: pre-publication transaction fault: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		_ = s.journalRoot.Remove(tempName)
		return err
	}
	if err := s.journalRoot.Link(tempName, finalName); err != nil {
		_ = s.journalRoot.Remove(tempName)
		return fmt.Errorf("localqueue: publish transaction revision %d: %w", tx.Revision, err)
	}
	// From this point a durable reader may see the transaction. Even if the
	// caller context expires, finish the directory fsync before answering.
	if s.fault != nil {
		if err := s.fault("after_link"); err != nil {
			s.poisoned = true
			return fmt.Errorf("%w: %v", ErrAmbiguousCommit, err)
		}
	}
	if err := s.dirSync(s.journalRoot); err != nil {
		s.poisoned = true
		return fmt.Errorf("%w: sync transaction directory: %v", ErrAmbiguousCommit, err)
	}
	if err := s.applyTransaction(tx); err != nil {
		s.poisoned = true
		return fmt.Errorf("%w: committed transition replay: %v", ErrAmbiguousCommit, err)
	}
	s.revision = tx.Revision
	s.digest = tx.SHA256
	_ = s.journalRoot.Remove(tempName) // orphan temps are ignored on recovery.
	_ = s.dirSync(s.journalRoot)
	return nil
}
