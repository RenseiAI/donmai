package workarea

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ContextPackProtocol is the exact versioned carrier the runner materializes.
const ContextPackProtocol = "context-pack-v1"

const (
	// ContextPackDirName is the reserved session-owned directory under metadata.
	ContextPackDirName = "context"
	// ContextPackMarkerName records the published pack identity.
	ContextPackMarkerName = "pack.json"

	// MaxContextPackDocuments bounds one pack.
	MaxContextPackDocuments = 32
	// MaxContextPackBytesPerDocument bounds one decoded document.
	MaxContextPackBytesPerDocument = 256 * 1024
	// MaxContextPackTotalBytes bounds the decoded pack.
	MaxContextPackTotalBytes = 1024 * 1024
	// MaxContextPackPathBytes bounds one document path.
	MaxContextPackPathBytes    = 256
	maxContextPackCarrierBytes = 2 * 1024 * 1024
)

// ContextPackReason is the closed refusal code.
type ContextPackReason string

const (
	// ReasonContextPackInvalid marks a malformed carrier.
	ReasonContextPackInvalid ContextPackReason = "context_pack_invalid"
	// ReasonContextPackUnsafe marks an escaping path.
	ReasonContextPackUnsafe ContextPackReason = "context_pack_unsafe_path"
	// ReasonContextPackCollision marks duplicate destinations.
	ReasonContextPackCollision ContextPackReason = "context_pack_collision"
	// ReasonContextPackDigest marks digest mismatch.
	ReasonContextPackDigest ContextPackReason = "context_pack_digest_mismatch"
	// ReasonContextPackLimit marks bound violations.
	ReasonContextPackLimit ContextPackReason = "context_pack_limit_exceeded"
	// ReasonContextPackConflict marks a different occupying pack.
	ReasonContextPackConflict ContextPackReason = "context_pack_conflict"
	// ReasonContextPackSupport marks an unsupported version or executor.
	ReasonContextPackSupport ContextPackReason = "context_pack_unsupported"
)

// ContextPackError is a typed fail-closed refusal.
type ContextPackError struct {
	Reason ContextPackReason
	Path   string
	Detail string
}

func (e *ContextPackError) Error() string {
	msg := fmt.Sprintf("runtime/workarea: context pack refused (%s)", e.Reason)
	if e.Path != "" {
		msg += ": path=" + e.Path
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// ContextDocumentV1 is one versioned document.
type ContextDocumentV1 struct {
	Path          string `json:"path"`
	ContentBase64 string `json:"contentBase64"`
	Digest        string `json:"digest"`
}

// ContextPackV1 is the additive versioned carrier.
type ContextPackV1 struct {
	Version   string              `json:"version"`
	PackID    string              `json:"packId"`
	Documents []ContextDocumentV1 `json:"documents"`
}

// UnmarshalJSON rejects unknown fields.
func (p *ContextPackV1) UnmarshalJSON(data []byte) error {
	type alias ContextPackV1
	var decoded alias
	if err := decodeClosedJSON(data, &decoded); err != nil {
		return err
	}
	*p = ContextPackV1(decoded)
	return nil
}

// ContextPackIncluded is one materialized document.
type ContextPackIncluded struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	Digest string `json:"digest"`
}

// ContextPackExcluded is one rejected document.
type ContextPackExcluded struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// ContextPackReport is the sanitized materialization diagnostic.
type ContextPackReport struct {
	PackID   string                `json:"packId"`
	Version  string                `json:"version"`
	Dir      string                `json:"dir,omitempty"`
	Included []ContextPackIncluded `json:"included"`
	Excluded []ContextPackExcluded `json:"excluded,omitempty"`
}

// DecodeContextPack validates the raw carrier and returns the pack.
// It enforces the closed carrier gate (size, unknown fields, trailing data)
// plus the header and path shapes via ValidateContextPack. Per-document
// content integrity (base64 shape, digest shape and match, byte bounds) is
// enforced later at MaterializeContextPack, which owns the filesystem write.
func DecodeContextPack(data []byte) (ContextPackV1, error) {
	if len(data) == 0 || len(data) > maxContextPackCarrierBytes {
		return ContextPackV1{}, &ContextPackError{Reason: ReasonContextPackLimit, Detail: "carrier size out of bounds"}
	}
	var pack ContextPackV1
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pack); err != nil {
		return ContextPackV1{}, &ContextPackError{Reason: ReasonContextPackInvalid, Detail: "carrier is not a context pack"}
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return ContextPackV1{}, &ContextPackError{Reason: ReasonContextPackInvalid, Detail: "trailing carrier data"}
	}
	if err := ValidateContextPack(pack); err != nil {
		return ContextPackV1{}, err
	}
	return pack, nil
}

// ValidateContextPack checks the carrier header and document shapes.
func ValidateContextPack(pack ContextPackV1) error {
	if pack.Version != ContextPackProtocol {
		return &ContextPackError{Reason: ReasonContextPackSupport, Detail: "unsupported context pack version"}
	}
	if err := ValidateRepositoryLeaf(pack.PackID); err != nil {
		return &ContextPackError{Reason: ReasonContextPackInvalid, Path: pack.PackID, Detail: "pack identity is unsafe"}
	}
	if len(pack.Documents) > MaxContextPackDocuments {
		return &ContextPackError{Reason: ReasonContextPackLimit, Detail: "document count exceeds bound"}
	}
	seen := make(map[string]struct{}, len(pack.Documents))
	for _, doc := range pack.Documents {
		clean, err := cleanContextPackPath(doc.Path)
		if err != nil {
			return err
		}
		key := strings.ToLower(clean)
		if _, dup := seen[key]; dup {
			return &ContextPackError{Reason: ReasonContextPackCollision, Path: doc.Path, Detail: "duplicate destination"}
		}
		seen[key] = struct{}{}
	}
	return nil
}

func cleanContextPackPath(p string) (string, error) {
	if p == "" || len(p) > MaxContextPackPathBytes {
		return "", &ContextPackError{Reason: ReasonContextPackLimit, Path: p, Detail: "path length out of bounds"}
	}
	if path.IsAbs(p) {
		return "", &ContextPackError{Reason: ReasonContextPackUnsafe, Path: p, Detail: "absolute path"}
	}
	if strings.Contains(p, "\\") {
		return "", &ContextPackError{Reason: ReasonContextPackUnsafe, Path: p, Detail: "backslash in path"}
	}
	clean := path.Clean(p)
	if clean == "." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", &ContextPackError{Reason: ReasonContextPackUnsafe, Path: p, Detail: "path escapes context directory"}
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", &ContextPackError{Reason: ReasonContextPackUnsafe, Path: p, Detail: "bad path segment"}
		}
	}
	return clean, nil
}

func parseContextDigest(value, docPath string) ([]byte, error) {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return nil, &ContextPackError{Reason: ReasonContextPackDigest, Path: docPath, Detail: "bad digest"}
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil || len(raw) != sha256.Size {
		return nil, &ContextPackError{Reason: ReasonContextPackDigest, Path: docPath, Detail: "bad digest"}
	}
	return raw, nil
}

// SupportsContextPack reports whether the executor attests the pack protocol.
func SupportsContextPack(protocols []string) bool {
	for _, protocol := range protocols {
		if protocol == ContextPackProtocol {
			return true
		}
	}
	return false
}

// ContextPackDir returns the publish path for one pack under the root.
func ContextPackDir(_ RootPath, packID string) string {
	return DeclarationDirName + "/" + ContextPackDirName + "/" + packID
}

func ensureContextPackParent(root *os.Root) (os.FileInfo, error) {
	// The session-owned context directory must stay an independent
	// metadata directory: an operator-planted symlink at either
	// metadata ancestor would redirect publication into a repository
	// checkout (including a declared read-only repository). MkdirAll
	// follows such links, so each ancestor is created level by level
	// and proved to be a real directory before use.
	if _, err := root.Lstat(DeclarationDirName); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err := root.Mkdir(DeclarationDirName, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	declaredIdent, err := root.Lstat(DeclarationDirName)
	if err != nil {
		return nil, err
	}
	if declaredIdent.Mode()&os.ModeSymlink != 0 || !declaredIdent.IsDir() {
		return nil, &ContextPackError{Reason: ReasonContextPackUnsafe, Path: DeclarationDirName, Detail: "metadata directory is not a real directory"}
	}
	contextRel := DeclarationDirName + "/" + ContextPackDirName
	if _, err := root.Lstat(contextRel); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err := root.Mkdir(contextRel, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	contextIdent, err := root.Lstat(contextRel)
	if err != nil {
		return nil, err
	}
	if contextIdent.Mode()&os.ModeSymlink != 0 || !contextIdent.IsDir() {
		return nil, &ContextPackError{Reason: ReasonContextPackUnsafe, Path: contextRel, Detail: "context directory is not a real directory"}
	}
	return contextIdent, nil
}

// MaterializeContextPack publishes the pack atomically under the session-owned directory.
func MaterializeContextPack(root RootPath, pack ContextPackV1) (ContextPackReport, error) {
	if err := ValidateContextPack(pack); err != nil {
		return ContextPackReport{}, err
	}
	var total int64
	included := make([]ContextPackIncluded, 0, len(pack.Documents))
	type staged struct {
		rel     string
		content []byte
	}
	stagedDocs := make([]staged, 0, len(pack.Documents))
	for _, doc := range pack.Documents {
		clean, _ := cleanContextPackPath(doc.Path)
		want, err := parseContextDigest(doc.Digest, doc.Path)
		if err != nil {
			return ContextPackReport{}, err
		}
		content, err := base64.StdEncoding.DecodeString(doc.ContentBase64)
		if err != nil {
			return ContextPackReport{}, &ContextPackError{Reason: ReasonContextPackInvalid, Path: doc.Path, Detail: "bad base64"}
		}
		if len(content) > MaxContextPackBytesPerDocument {
			return ContextPackReport{}, &ContextPackError{Reason: ReasonContextPackLimit, Path: doc.Path, Detail: "document too large"}
		}
		total += int64(len(content))
		if total > MaxContextPackTotalBytes {
			return ContextPackReport{}, &ContextPackError{Reason: ReasonContextPackLimit, Path: doc.Path, Detail: "pack too large"}
		}
		sum := sha256.Sum256(content)
		if !bytes.Equal(sum[:], want) {
			return ContextPackReport{}, &ContextPackError{Reason: ReasonContextPackDigest, Path: doc.Path, Detail: "digest mismatch"}
		}
		canonical := "sha256:" + hex.EncodeToString(sum[:])
		stagedDocs = append(stagedDocs, staged{rel: clean, content: content})
		included = append(included, ContextPackIncluded{Path: clean, Bytes: len(content), Digest: canonical})
	}
	rootHandle, err := openDeclarationRoot(root)
	if err != nil {
		return ContextPackReport{}, err
	}
	defer func() { _ = rootHandle.Close() }()
	contextIdent, err := ensureContextPackParent(rootHandle)
	if err != nil {
		var packErr *ContextPackError
		if errors.As(err, &packErr) {
			return ContextPackReport{}, err
		}
		return ContextPackReport{}, fmt.Errorf("runtime/workarea: create context directory: %w", err)
	}
	finalRel := ContextPackDir(root, pack.PackID)
	if existing, readErr := readContextPackMarker(rootHandle, finalRel); readErr == nil {
		// Redelivery trusts nothing it has not re-proved: the marker
		// must name exactly this pack (identity, version, directory
		// and digest set), and every published document must still be
		// a regular file whose bytes match the recorded digest. An
		// operator-planted or tampered marker that skips publication,
		// or a redelivery over deleted, modified or symlinked
		// documents, refuses instead of reporting the stale receipt.
		if existing.PackID != pack.PackID ||
			existing.Version != ContextPackProtocol ||
			existing.Dir != finalRel ||
			!sameIncludedDigests(existing.Included, included) {
			return ContextPackReport{}, &ContextPackError{Reason: ReasonContextPackConflict, Path: pack.PackID, Detail: "different pack occupies context directory"}
		}
		if err := verifyContextPackDocuments(rootHandle, finalRel, included); err != nil {
			return ContextPackReport{}, err
		}
		return ContextPackReport{PackID: pack.PackID, Version: ContextPackProtocol, Dir: finalRel, Included: included}, nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return ContextPackReport{}, readErr
	}
	stagingName, err := contextStagingName()
	if err != nil {
		return ContextPackReport{}, err
	}
	stagingRel := DeclarationDirName + "/" + ContextPackDirName + "/" + stagingName
	committed := false
	defer func() {
		if !committed {
			_ = rootHandle.RemoveAll(stagingRel)
		}
	}()
	if err := rootHandle.Mkdir(stagingRel, 0o700); err != nil {
		return ContextPackReport{}, fmt.Errorf("runtime/workarea: create staging: %w", err)
	}
	stagingRoot, err := rootHandle.OpenRoot(stagingRel)
	if err != nil {
		return ContextPackReport{}, fmt.Errorf("runtime/workarea: open staging: %w", err)
	}
	defer func() { _ = stagingRoot.Close() }()
	for _, doc := range stagedDocs {
		if dir := path.Dir(doc.rel); dir != "." {
			if err := stagingRoot.MkdirAll(dir, 0o700); err != nil {
				return ContextPackReport{}, fmt.Errorf("runtime/workarea: create parent: %w", err)
			}
		}
		if _, err := stagingRoot.Lstat(doc.rel); err == nil {
			return ContextPackReport{}, &ContextPackError{Reason: ReasonContextPackCollision, Path: doc.rel, Detail: "duplicate destination"}
		}
		file, err := stagingRoot.OpenFile(doc.rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return ContextPackReport{}, &ContextPackError{Reason: ReasonContextPackCollision, Path: doc.rel, Detail: "duplicate destination"}
			}
			return ContextPackReport{}, fmt.Errorf("runtime/workarea: create document: %w", err)
		}
		if _, err := file.Write(doc.content); err != nil {
			_ = file.Close()
			return ContextPackReport{}, fmt.Errorf("runtime/workarea: write document: %w", err)
		}
		if err := file.Close(); err != nil {
			return ContextPackReport{}, fmt.Errorf("runtime/workarea: close document: %w", err)
		}
	}
	report := ContextPackReport{PackID: pack.PackID, Version: pack.Version, Dir: finalRel, Included: included}
	marker, _ := json.Marshal(report)
	markerFile, err := stagingRoot.OpenFile(ContextPackMarkerName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return ContextPackReport{}, fmt.Errorf("runtime/workarea: create marker: %w", err)
	}
	if _, err := markerFile.Write(marker); err != nil {
		_ = markerFile.Close()
		return ContextPackReport{}, fmt.Errorf("runtime/workarea: write marker: %w", err)
	}
	_ = markerFile.Close()
	_ = stagingRoot.Close()
	// Re-prove the context parent did not change while staging: an
	// ancestor swapped for a symlink after validation must not carry
	// the atomic rename into a repository checkout.
	currentIdent, err := rootHandle.Lstat(DeclarationDirName + "/" + ContextPackDirName)
	if err != nil {
		return ContextPackReport{}, fmt.Errorf("runtime/workarea: recheck context directory: %w", err)
	}
	if currentIdent.Mode()&os.ModeSymlink != 0 || !currentIdent.IsDir() || !os.SameFile(contextIdent, currentIdent) {
		return ContextPackReport{}, &ContextPackError{Reason: ReasonContextPackUnsafe, Path: DeclarationDirName + "/" + ContextPackDirName, Detail: "context directory changed during publication"}
	}
	stagingAbs := filepath.Join(string(root), filepath.FromSlash(stagingRel))
	finalAbs := filepath.Join(string(root), filepath.FromSlash(finalRel))
	if err := RenameNoReplace(stagingAbs, finalAbs); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ContextPackReport{}, &ContextPackError{Reason: ReasonContextPackConflict, Path: pack.PackID, Detail: "different pack occupies context directory"}
		}
		return ContextPackReport{}, fmt.Errorf("runtime/workarea: publish pack: %w", err)
	}
	committed = true
	return report, nil
}

func sameIncludedDigests(a, b []ContextPackIncluded) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Path != b[i].Path || a[i].Bytes != b[i].Bytes || !strings.EqualFold(a[i].Digest, b[i].Digest) {
			return false
		}
	}
	return true
}

func readContextPackMarker(root *os.Root, finalRel string) (ContextPackReport, error) {
	// Fail closed before opening anything: a FIFO, socket or other
	// special file at either path would block Open indefinitely, and a
	// symlink could redirect the trust read. Only a real directory
	// holding a regular marker file is readable.
	dirIdent, err := root.Lstat(finalRel)
	if err != nil {
		return ContextPackReport{}, err
	}
	if dirIdent.Mode()&os.ModeSymlink != 0 || !dirIdent.IsDir() {
		return ContextPackReport{}, &ContextPackError{Reason: ReasonContextPackUnsafe, Path: finalRel, Detail: "pack directory is not a real directory"}
	}
	metadata, err := root.OpenRoot(finalRel)
	if err != nil {
		return ContextPackReport{}, err
	}
	defer func() { _ = metadata.Close() }()
	markerIdent, err := metadata.Lstat(ContextPackMarkerName)
	if err != nil {
		return ContextPackReport{}, err
	}
	if !markerIdent.Mode().IsRegular() {
		return ContextPackReport{}, &ContextPackError{Reason: ReasonContextPackUnsafe, Path: ContextPackMarkerName, Detail: "pack marker is not a regular file"}
	}
	if markerIdent.Size() > maxContextPackCarrierBytes+1 {
		return ContextPackReport{}, &ContextPackError{Reason: ReasonContextPackLimit, Detail: "pack marker exceeds bound"}
	}
	file, err := metadata.Open(ContextPackMarkerName)
	if err != nil {
		return ContextPackReport{}, err
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, maxContextPackCarrierBytes+1))
	if err != nil {
		return ContextPackReport{}, err
	}
	if len(body) > maxContextPackCarrierBytes {
		return ContextPackReport{}, &ContextPackError{Reason: ReasonContextPackLimit, Detail: "pack marker exceeds bound"}
	}
	var report ContextPackReport
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return ContextPackReport{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return ContextPackReport{}, err
	}
	return report, nil
}

// verifyContextPackDocuments re-proves every published document before a
// redelivery may reuse it: each entry must still be a regular file (never
// a symlink, directory, FIFO or other special file) whose byte count and
// digest match the recorded receipt. Anything else refuses fail-closed so
// a reused workarea can never launch with missing or altered documents
// while reporting the original digest.
func verifyContextPackDocuments(root *os.Root, finalRel string, included []ContextPackIncluded) error {
	packRoot, err := root.OpenRoot(finalRel)
	if err != nil {
		return err
	}
	defer func() { _ = packRoot.Close() }()
	for _, inc := range included {
		clean, err := cleanContextPackPath(inc.Path)
		if err != nil {
			return err
		}
		ident, err := packRoot.Lstat(clean)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return &ContextPackError{Reason: ReasonContextPackInvalid, Path: inc.Path, Detail: "published document missing"}
			}
			return err
		}
		if !ident.Mode().IsRegular() {
			return &ContextPackError{Reason: ReasonContextPackUnsafe, Path: inc.Path, Detail: "published document is not a regular file"}
		}
		file, err := packRoot.Open(clean)
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(io.LimitReader(file, MaxContextPackBytesPerDocument+1))
		closeErr := file.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(body) != inc.Bytes {
			return &ContextPackError{Reason: ReasonContextPackDigest, Path: inc.Path, Detail: "published document changed"}
		}
		sum := sha256.Sum256(body)
		if !strings.EqualFold("sha256:"+hex.EncodeToString(sum[:]), inc.Digest) {
			return &ContextPackError{Reason: ReasonContextPackDigest, Path: inc.Path, Detail: "published document digest mismatch"}
		}
	}
	return nil
}

func contextStagingName() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("runtime/workarea: staging name: %w", err)
	}
	return ".staging-" + hex.EncodeToString(random[:]), nil
}
