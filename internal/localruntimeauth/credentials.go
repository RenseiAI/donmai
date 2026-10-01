package localruntimeauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/RenseiAI/donmai/internal/localqueue"
)

const attemptsDirectory = "attempts"

var handlePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{1,127}$`)

// Credential deliberately has no exported secret-bearing fields. Explicit
// BearerToken extraction is required; routine formatting/JSON is redacted.
type Credential struct{ token *string }

func newCredential(token string) Credential { return Credential{token: &token} }

// BearerToken is the explicit, trusted extraction path for a private key.
func (c Credential) BearerToken() string {
	if c.token == nil {
		return ""
	}
	return *c.token
}

func (Credential) String() string { return "[REDACTED]" }

// GoString redacts diagnostic %#v formatting.
func (Credential) GoString() string { return "[REDACTED]" }

// MarshalJSON emits only a redaction marker, never the bearer.
func (Credential) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// Format redacts every alternate fmt verb supported by fmt.Formatter.
func (Credential) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, "[REDACTED]") }

type attemptRecord struct {
	Version int                   `json:"version"`
	Handle  string                `json:"handle"`
	Ref     localqueue.AttemptRef `json:"ref"`
	Secret  string                `json:"secret"`
}

func validHandle(handle string) bool {
	if !handlePattern.MatchString(handle) {
		return false
	}
	for _, prefix := range []string{"rsp_", "rsk_", "ghp_", "github_pat_", operatorTokenPrefix, attemptTokenPrefix} {
		if strings.HasPrefix(handle, prefix) {
			return false
		}
	}
	return true
}

func (s *Store) validRef(ref localqueue.AttemptRef) bool {
	return ref.ScopeID == s.identity.ScopeID && ref.WorkerID == s.identity.WorkerID &&
		localIDPattern.MatchString(ref.SessionID) && localIDPattern.MatchString(ref.AttemptID) &&
		ref.Generation > 0
}

func attemptName(handle string) string { return handle + ".json" }

func (s *Store) openAttempts(create bool) (*os.Root, error) {
	path := filepath.Join(s.rootPath, attemptsDirectory)
	if create {
		if err := ensurePrivateDirectory(path); err != nil {
			return nil, err
		}
	} else if err := inspectPrivateDirectory(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrMissingCredential
		}
		return nil, err
	}
	root, err := s.root.OpenRoot(attemptsDirectory)
	if err != nil {
		return nil, fmt.Errorf("open private attempts directory: %w", err)
	}
	// A prior successful directory sync cannot certify a later publication by
	// another Store handle. Reaffirm each read under the caller's file lock.
	if err := s.dirSync(root); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("sync private attempts directory: %w", err)
	}
	return root, nil
}

func (s *Store) loadAttemptLocked(handle string, ref localqueue.AttemptRef) (Credential, error) {
	if !validHandle(handle) || !s.validRef(ref) {
		return Credential{}, ErrBindingMismatch
	}
	attempts, err := s.openAttempts(false)
	if err != nil {
		return Credential{}, err
	}
	defer func() { _ = attempts.Close() }()
	var record attemptRecord
	if err := readRecord(attempts, attemptName(handle), &record); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Credential{}, ErrMissingCredential
		}
		return Credential{}, err
	}
	if record.Version != formatVersion || !validHandle(record.Handle) || record.Handle != handle ||
		!validToken(record.Secret, attemptTokenPrefix) || !s.validRef(record.Ref) {
		return Credential{}, ErrCorruptState
	}
	if record.Ref != ref {
		return Credential{}, ErrBindingMismatch
	}
	return newCredential(record.Secret), nil
}

// CreateAttempt persists an independently random exact-attempt credential.
// The trusted controller must first prove this is the authoritative prelaunch
// attempt; this package neither claims nor retires work.
func (s *Store) CreateAttempt(ctx context.Context, handle string, ref localqueue.AttemptRef) (Credential, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(true); err != nil {
		return Credential{}, err
	}
	if !validHandle(handle) || !s.validRef(ref) {
		return Credential{}, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	if err := lockWriter(s.lockFile); err != nil {
		return Credential{}, err
	}
	defer func() { _ = unlockWriter(s.lockFile) }()
	if err := s.ensureBootstrapLocked(); err != nil {
		return Credential{}, err
	}
	// Existing exact retry returns the original key. A changed ref under the
	// same handle is a binding conflict, never a rotation.
	if credential, err := s.loadAttemptLocked(handle, ref); err == nil {
		return credential, nil
	} else if !errors.Is(err, ErrMissingCredential) {
		return Credential{}, err
	}
	attempts, err := s.openAttempts(true)
	if err != nil {
		return Credential{}, err
	}
	defer func() { _ = attempts.Close() }()
	secret, err := randomToken(attemptTokenPrefix)
	if err != nil {
		return Credential{}, err
	}
	raw, err := encodeRecord(attemptRecord{Version: formatVersion, Handle: handle, Ref: ref, Secret: secret})
	if err != nil {
		return Credential{}, err
	}
	created, err := s.writeImmutable(ctx, attempts, attemptName(handle), raw)
	if err != nil {
		return Credential{}, err
	}
	if !created {
		return s.loadAttemptLocked(handle, ref)
	}
	return newCredential(secret), nil
}

// LoadAttempt never creates or regenerates a missing key. Exact terminal
// response-loss replay may use this same retained key after a restart.
func (s *Store) LoadAttempt(handle string, ref localqueue.AttemptRef) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(false); err != nil {
		return Credential{}, err
	}
	if err := lockReader(s.lockFile); err != nil {
		return Credential{}, err
	}
	defer func() { _ = unlockWriter(s.lockFile) }()
	if err := s.ensureBootstrapLocked(); err != nil {
		return Credential{}, err
	}
	return s.loadAttemptLocked(handle, ref)
}

// OperatorCredential is a trusted local-controller extraction. The returned
// Credential stays redacted under ordinary formatting and JSON encoding.
func (s *Store) OperatorCredential() (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpenLocked(false); err != nil {
		return Credential{}, err
	}
	if err := lockReader(s.lockFile); err != nil {
		return Credential{}, err
	}
	defer func() { _ = unlockWriter(s.lockFile) }()
	if err := s.dirSync(s.root); err != nil {
		return Credential{}, fmt.Errorf("reaffirm private auth root: %w", err)
	}
	if err := s.ensureBootstrapLocked(); err != nil {
		return Credential{}, err
	}
	return s.operator, nil
}

func equalSecret(left, right string) bool {
	lhs := sha256.Sum256([]byte(left))
	rhs := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(lhs[:], rhs[:]) == 1
}

// VerifyOperator is separate from attempt auth. Every wrong or missing
// presented credential receives the same outward refusal.
func (s *Store) VerifyOperator(bearer string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checkOpenLocked(false) != nil || lockReader(s.lockFile) != nil {
		return ErrUnauthorized
	}
	defer func() { _ = unlockWriter(s.lockFile) }()
	if s.dirSync(s.root) != nil || s.ensureBootstrapLocked() != nil ||
		!equalSecret(bearer, s.operator.BearerToken()) {
		return ErrUnauthorized
	}
	return nil
}

// VerifyAttempt authenticates only the supplied exact binding. The HTTP
// receiver must select the allowed current/replay ref from queue authority,
// never trust a ref supplied by a caller as lifecycle authorization.
func (s *Store) VerifyAttempt(handle string, ref localqueue.AttemptRef, bearer string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checkOpenLocked(false) != nil || lockReader(s.lockFile) != nil {
		return ErrUnauthorized
	}
	defer func() { _ = unlockWriter(s.lockFile) }()
	if s.ensureBootstrapLocked() != nil {
		return ErrUnauthorized
	}
	credential, err := s.loadAttemptLocked(handle, ref)
	if err != nil || !equalSecret(bearer, credential.BearerToken()) {
		return ErrUnauthorized
	}
	return nil
}

// Ensure credentials cannot accidentally acquire a second JSON shape through
// encoding/json's default struct traversal if their implementation changes.
var _ json.Marshaler = Credential{}
