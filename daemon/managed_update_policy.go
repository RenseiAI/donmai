// Package daemon managed-update-policy store: one validated, durable policy
// snapshot the updater integration can consume without mixing signer
// identities and roots.
//
// Protocol bounds (documented here so producers and readers agree):
//
//   - Policy document is exact UTF-8 JSON:
//     {version:1, signers:[{issuer,subject}], publicRootsPem:[...]}.
//     The digest is the lowercase hex SHA-256 of those exact bytes.
//   - Policy size: 1 byte .. 64 KiB (65536 bytes) inclusive.
//   - Signers: 1 .. 32 entries. Each entry carries exactly {issuer,subject}.
//     Identities match exactly: unknown keys, wildcards ("*") and empty
//     identities are refused. Issuer must be an https:// URL.
//   - Public roots: 0 .. 16 PEM entries. Each entry must hold one or more
//     CERTIFICATE blocks parseable by the standard library. Any private-key
//     material is refused. Empty roots mean no additional roots; managed
//     mode never restores vendor trust when roots are empty.
//
// Durability:
//
//   - Policy bytes and digest live in ONE atomic record under a
//     daemon-owned managed configuration location. No destination path is
//     accepted from policy content or from any response: the path is fixed
//     at store construction (beside the daemon configuration file, or the
//     brand state directory when no configuration path is known).
//   - Replacement is validated with the production parser BEFORE any
//     durable write, persisted via temp-file-then-rename with content and
//     directory fsync, and only then published to readers under a
//     read/write lock. Identical digest is a no-op that performs no rewrite.
//   - On any validation or write failure the entire prior record is
//     retained and a typed refusal is returned. Startup load revalidates
//     persisted data; corrupt state refuses managed use until a subsequent
//     valid application recovers it.
//
// State table (doors: Apply, Load, Snapshot):
//
//	absent + valid           -> stored
//	stored + same digest     -> unchanged (no rewrite)
//	stored + valid new digest-> replaced
//	any + invalid/write fail -> prior record retained with refusal
package daemon

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/internal/statepath"
)

// Managed update-policy protocol bounds.
const (
	// ManagedUpdatePolicyVersion is the only accepted policy version.
	ManagedUpdatePolicyVersion = 1
	// ManagedUpdatePolicyMaxBytes bounds the exact policy JSON document.
	ManagedUpdatePolicyMaxBytes = 64 * 1024
	// ManagedUpdatePolicyMaxSigners bounds the signer allowlist.
	ManagedUpdatePolicyMaxSigners = 32
	// ManagedUpdatePolicyMaxRoots bounds the additional public roots.
	ManagedUpdatePolicyMaxRoots = 16
	// managedUpdatePolicyFileName is the daemon-owned managed configuration
	// location leaf. It lives beside the daemon configuration file so a
	// managed policy cannot be redirected by policy or response content.
	managedUpdatePolicyFileName = "managed-update-policy.json"
)

// ErrManagedUpdatePolicyRefused is the sentinel for every typed refusal from
// this store: wrong digest, oversize policy, malformed identity or root,
// private-key material, corrupt persisted state, or a durability failure.
// Callers use errors.Is to distinguish a refusal (prior record retained)
// from a programming error.
var ErrManagedUpdatePolicyRefused = errors.New("managed update policy refused")

// managedUpdatePolicyError is the typed refusal. It wraps
// ErrManagedUpdatePolicyRefused so errors.Is keeps working while the
// message names the deciding guard.
type managedUpdatePolicyError struct {
	reason string
	cause  error
}

func (e *managedUpdatePolicyError) Error() string {
	if e.cause != nil {
		return "managed update policy refused: " + e.reason + ": " + e.cause.Error()
	}
	return "managed update policy refused: " + e.reason
}

func (e *managedUpdatePolicyError) Unwrap() error { return ErrManagedUpdatePolicyRefused }

func managedPolicyRefused(reason string) error {
	return &managedUpdatePolicyError{reason: reason}
}

func managedPolicyRefusedCause(reason string, cause error) error {
	return &managedUpdatePolicyError{reason: reason, cause: cause}
}

// ManagedUpdatePolicySigner is one validated signer identity. Issuer and
// Subject match exactly at consumption time; no wildcard or pattern form
// survives parsing.
type ManagedUpdatePolicySigner struct {
	Issuer  string
	Subject string
}

// ManagedUpdatePolicy is the validated in-memory view of a policy document.
type ManagedUpdatePolicy struct {
	Version        int
	Signers        []ManagedUpdatePolicySigner
	PublicRootsPem []string
	Digest         string
	PolicyJSON     []byte
}

// managedPolicyWire is the strict wire shape. Unknown keys are rejected by
// decoding with DisallowUnknownFields; the struct tags intentionally name
// every accepted key.
type managedPolicyWire struct {
	Version        int                 `json:"version"`
	Signers        []managedSignerWire `json:"signers"`
	PublicRootsPem []string            `json:"publicRootsPem"`
}

// managedSignerWire is the strict signer shape: exactly issuer + subject.
type managedSignerWire struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// ParseManagedUpdatePolicy validates policyJSON with the production parser
// and returns its validated view. digestHex must be the lowercase hex
// SHA-256 of the exact policyJSON bytes. Every guard refuses with a typed
// refusal wrapping ErrManagedUpdatePolicyRefused.
func ParseManagedUpdatePolicy(policyJSON []byte, digestHex string) (ManagedUpdatePolicy, error) {
	if len(policyJSON) == 0 {
		return ManagedUpdatePolicy{}, managedPolicyRefused("empty policy")
	}
	if len(policyJSON) > ManagedUpdatePolicyMaxBytes {
		return ManagedUpdatePolicy{}, managedPolicyRefused(fmt.Sprintf("policy exceeds %d bytes", ManagedUpdatePolicyMaxBytes))
	}
	if !utf8.Valid(policyJSON) {
		return ManagedUpdatePolicy{}, managedPolicyRefused("policy is not valid UTF-8")
	}
	normalizedDigest := strings.TrimSpace(digestHex)
	if len(normalizedDigest) != sha256.Size*2 {
		return ManagedUpdatePolicy{}, managedPolicyRefused("malformed digest")
	}
	decoded, err := hex.DecodeString(normalizedDigest)
	if err != nil || len(decoded) != sha256.Size {
		return ManagedUpdatePolicy{}, managedPolicyRefused("malformed digest")
	}
	if normalizedDigest != strings.ToLower(normalizedDigest) {
		return ManagedUpdatePolicy{}, managedPolicyRefused("digest must be lowercase hex")
	}
	sum := sha256.Sum256(policyJSON)
	actual := hex.EncodeToString(sum[:])
	if actual != normalizedDigest {
		return ManagedUpdatePolicy{}, managedPolicyRefused("digest mismatch")
	}

	dec := json.NewDecoder(bytes.NewReader(policyJSON))
	dec.DisallowUnknownFields()
	var wire managedPolicyWire
	if err := dec.Decode(&wire); err != nil {
		return ManagedUpdatePolicy{}, managedPolicyRefusedCause("malformed policy JSON", err)
	}
	// No trailing document after the single JSON object.
	if dec.More() {
		return ManagedUpdatePolicy{}, managedPolicyRefused("malformed policy JSON: trailing data")
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err == nil {
		// A second value decoded successfully: trailing data.
		_ = extra
		return ManagedUpdatePolicy{}, managedPolicyRefused("malformed policy JSON: trailing data")
	}

	if wire.Version != ManagedUpdatePolicyVersion {
		return ManagedUpdatePolicy{}, managedPolicyRefused(fmt.Sprintf("unsupported version %d", wire.Version))
	}
	if len(wire.Signers) < 1 || len(wire.Signers) > ManagedUpdatePolicyMaxSigners {
		return ManagedUpdatePolicy{}, managedPolicyRefused(fmt.Sprintf("signers must hold 1..%d entries", ManagedUpdatePolicyMaxSigners))
	}
	if wire.PublicRootsPem == nil {
		return ManagedUpdatePolicy{}, managedPolicyRefused("publicRootsPem is required (use [] when empty)")
	}
	if len(wire.PublicRootsPem) > ManagedUpdatePolicyMaxRoots {
		return ManagedUpdatePolicy{}, managedPolicyRefused(fmt.Sprintf("publicRootsPem must hold 0..%d entries", ManagedUpdatePolicyMaxRoots))
	}

	signers := make([]ManagedUpdatePolicySigner, 0, len(wire.Signers))
	for i, s := range wire.Signers {
		issuer := s.Issuer
		subject := s.Subject
		if strings.TrimSpace(issuer) == "" || strings.TrimSpace(subject) == "" {
			return ManagedUpdatePolicy{}, managedPolicyRefused(fmt.Sprintf("signers[%d]: empty issuer or subject", i))
		}
		if strings.Contains(issuer, "*") || strings.Contains(subject, "*") {
			return ManagedUpdatePolicy{}, managedPolicyRefused(fmt.Sprintf("signers[%d]: wildcards are not accepted", i))
		}
		if err := validatePolicyIssuer(issuer); err != nil {
			return ManagedUpdatePolicy{}, managedPolicyRefusedCause(fmt.Sprintf("signers[%d]: invalid issuer", i), err)
		}
		signers = append(signers, ManagedUpdatePolicySigner{Issuer: issuer, Subject: subject})
	}

	for i, entry := range wire.PublicRootsPem {
		if err := validatePolicyRoot(entry); err != nil {
			return ManagedUpdatePolicy{}, managedPolicyRefusedCause(fmt.Sprintf("publicRootsPem[%d]: invalid root", i), err)
		}
	}

	roots := append([]string(nil), wire.PublicRootsPem...)
	policyCopy := append([]byte(nil), policyJSON...)
	return ManagedUpdatePolicy{
		Version:        wire.Version,
		Signers:        signers,
		PublicRootsPem: roots,
		Digest:         normalizedDigest,
		PolicyJSON:     policyCopy,
	}, nil
}

// DigestManagedUpdatePolicy returns the lowercase hex SHA-256 of the exact
// policy bytes. It is the canonical digest computation shared by producers
// of synthetic test policies and the store itself.
func DigestManagedUpdatePolicy(policyJSON []byte) string {
	sum := sha256.Sum256(policyJSON)
	return hex.EncodeToString(sum[:])
}

// validatePolicyIssuer requires an exact https:// identity with no wildcard.
func validatePolicyIssuer(issuer string) error {
	if strings.Contains(issuer, "*") {
		return errors.New("wildcard is not accepted")
	}
	if !strings.HasPrefix(issuer, "https://") {
		return errors.New("issuer must use https")
	}
	parsed, err := url.Parse(issuer)
	if err != nil {
		return fmt.Errorf("malformed issuer URL: %w", err)
	}
	if parsed.Scheme != "https" {
		return errors.New("issuer must use https")
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return errors.New("issuer must carry a host")
	}
	if strings.Contains(issuer, " ") || strings.Contains(issuer, "\n") || strings.Contains(issuer, "\t") {
		return errors.New("issuer must not contain whitespace")
	}
	return nil
}

// validatePolicyRoot requires public CERTIFICATE PEM blocks only. Any
// private-key material is refused even when mixed with valid certificates.
func validatePolicyRoot(entry string) error {
	if strings.TrimSpace(entry) == "" {
		return errors.New("empty root entry")
	}
	if strings.Contains(entry, "PRIVATE KEY") {
		return errors.New("private-key material is not accepted")
	}
	rest := []byte(entry)
	certs := 0
	for {
		trimmed := bytes.TrimSpace(rest)
		if len(trimmed) == 0 {
			break
		}
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return errors.New("entry is not PEM encoded")
		}
		if block.Type != "CERTIFICATE" {
			return fmt.Errorf("unexpected PEM block %q (want CERTIFICATE)", block.Type)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return fmt.Errorf("malformed certificate: %w", err)
		}
		certs++
		if certs > ManagedUpdatePolicyMaxRoots+1 {
			return errors.New("too many certificates in one entry")
		}
	}
	if certs == 0 {
		return errors.New("entry holds no certificate")
	}
	return nil
}

// DefaultManagedUpdatePolicyPath returns the brand state-directory location
// for the managed policy record. It is the fallback when no daemon
// configuration path is known.
func DefaultManagedUpdatePolicyPath() string {
	return statepath.Resolve(managedUpdatePolicyFileName, "/tmp/.donmai/"+managedUpdatePolicyFileName)
}

// ManagedUpdatePolicyStatePath derives the daemon-owned managed
// configuration location beside the daemon configuration file. An empty
// configuration path falls back to the brand state directory. No caller
// may supply any other destination: policy and response content never
// select the path.
func ManagedUpdatePolicyStatePath(configPath string) string {
	if strings.TrimSpace(configPath) == "" {
		return DefaultManagedUpdatePolicyPath()
	}
	return filepath.Join(filepath.Dir(configPath), managedUpdatePolicyFileName)
}

// managedUpdatePolicyRecord is the single atomic record: policy bytes
// (base64, preserving the exact document) plus its digest. One file, one
// rename: readers and restarts observe the complete old or the complete
// new pair, never a mixed pair.
type managedUpdatePolicyRecord struct {
	Digest string `json:"digest"`
	Policy string `json:"policyBase64"`
}

// ManagedUpdatePolicyStore holds one validated, durable policy snapshot.
// Safe for concurrent use: replacement is synchronized with readers and
// only published after durable persistence succeeds.
type ManagedUpdatePolicyStore struct {
	mu      sync.RWMutex
	path    string
	policy  []byte
	digest  string
	has     bool
	persist func(path string, data []byte) error
}

// NewManagedUpdatePolicyStore creates a store fixed to statePath. An empty
// statePath resolves to the brand state-directory location. The store does
// not touch disk until Apply or Load is called; the path never comes from
// policy or response content.
func NewManagedUpdatePolicyStore(statePath string) *ManagedUpdatePolicyStore {
	if strings.TrimSpace(statePath) == "" {
		statePath = DefaultManagedUpdatePolicyPath()
	}
	return &ManagedUpdatePolicyStore{path: statePath}
}

// StatePath reports the daemon-owned file this store reads and writes.
func (s *ManagedUpdatePolicyStore) StatePath() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Apply validates policyJSON against digestHex with the production parser
// and, when valid and new, durably persists the single atomic record before
// publishing the new immutable snapshot. Identical digest is a no-op that
// performs no rewrite. On any validation or durability failure the entire
// prior record is retained and a typed refusal is returned.
func (s *ManagedUpdatePolicyStore) Apply(policyJSON []byte, digestHex string) (bool, error) {
	parsed, err := ParseManagedUpdatePolicy(policyJSON, digestHex)
	if err != nil {
		return false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.has && s.digest == parsed.Digest {
		return false, nil
	}

	record := managedUpdatePolicyRecord{
		Digest: parsed.Digest,
		Policy: base64.StdEncoding.EncodeToString(parsed.PolicyJSON),
	}
	data, err := json.Marshal(record)
	if err != nil {
		return false, managedPolicyRefusedCause("encode durable record", err)
	}

	persist := s.persist
	if persist == nil {
		persist = writeManagedUpdatePolicyFile
	}
	if err := persist(s.path, data); err != nil {
		if errors.Is(err, ErrManagedUpdatePolicyRefused) {
			return false, err
		}
		return false, managedPolicyRefusedCause("durable persistence failed", err)
	}

	s.policy = append([]byte(nil), parsed.PolicyJSON...)
	s.digest = parsed.Digest
	s.has = true
	return true, nil
}

// Load revalidates the persisted record from disk with the production
// parser. A missing file means absent (no policy, no error). Any other
// read, decode, digest or validation failure clears the in-memory snapshot
// and returns a typed refusal: corrupt state refuses managed use until a
// subsequent valid Apply recovers it.
func (s *ManagedUpdatePolicyStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path) //nolint:gosec // path is daemon-owned, never request-derived
	if err != nil {
		if os.IsNotExist(err) {
			s.policy = nil
			s.digest = ""
			s.has = false
			return nil
		}
		s.policy = nil
		s.digest = ""
		s.has = false
		return managedPolicyRefusedCause("durable state unreadable", err)
	}
	var record managedUpdatePolicyRecord
	if err := json.Unmarshal(data, &record); err != nil {
		s.policy = nil
		s.digest = ""
		s.has = false
		return managedPolicyRefusedCause("durable state corrupt", err)
	}
	if strings.TrimSpace(record.Digest) == "" || strings.TrimSpace(record.Policy) == "" {
		// An empty-roots policy still carries a non-empty base64 document
		// (the JSON object itself), so an empty field is corruption, not
		// an empty policy.
		if strings.TrimSpace(record.Digest) == "" {
			s.policy = nil
			s.digest = ""
			s.has = false
			return managedPolicyRefused("durable state corrupt: missing digest")
		}
		s.policy = nil
		s.digest = ""
		s.has = false
		return managedPolicyRefused("durable state corrupt: missing policy")
	}
	raw, err := base64.StdEncoding.DecodeString(record.Policy)
	if err != nil {
		s.policy = nil
		s.digest = ""
		s.has = false
		return managedPolicyRefusedCause("durable state corrupt: policy encoding", err)
	}
	parsed, err := ParseManagedUpdatePolicy(raw, record.Digest)
	if err != nil {
		s.policy = nil
		s.digest = ""
		s.has = false
		return err
	}
	s.policy = append([]byte(nil), parsed.PolicyJSON...)
	s.digest = parsed.Digest
	s.has = true
	return nil
}

// Snapshot returns the current immutable snapshot: a copy of the exact
// policy bytes and its digest. ok is false when no valid policy is stored
// (absent or refused). Every caller observes a complete old or complete
// new pair, never a mixture.
func (s *ManagedUpdatePolicyStore) Snapshot() (policy []byte, digest string, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.has {
		return nil, "", false
	}
	return append([]byte(nil), s.policy...), s.digest, true
}

// writeManagedUpdatePolicyFile durably writes one atomic record: temp file
// in the same directory, content fsync, rename into place, directory
// fsync. A crash mid-write leaves the prior record intact. It reuses the
// repository's supported filesystem primitives (no external durability
// dependency).
func writeManagedUpdatePolicyFile(path string, data []byte) error {
	if strings.TrimSpace(path) == "" {
		return managedPolicyRefused("no durable state path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".managed-update-policy-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename temp file: %w", err)
	}
	dirFile, err := os.Open(dir) //nolint:gosec // daemon-owned state directory
	if err != nil {
		return fmt.Errorf("open state dir: %w", err)
	}
	syncErr := dirFile.Sync()
	closeErr := dirFile.Close()
	if syncErr != nil {
		return fmt.Errorf("sync state dir: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("sync state dir: %w", closeErr)
	}
	return nil
}
