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
		stagedDocs = append(stagedDocs, staged{rel: clean, content: content})
		included = append(included, ContextPackIncluded{Path: clean, Bytes: len(content), Digest: doc.Digest})
	}
	rootHandle, err := openDeclarationRoot(root)
	if err != nil {
		return ContextPackReport{}, err
	}
	defer func() { _ = rootHandle.Close() }()
	finalRel := ContextPackDir(root, pack.PackID)
	if existing, readErr := readContextPackMarker(rootHandle, finalRel); readErr == nil {
		if existing.PackID == pack.PackID && sameIncludedDigests(existing.Included, included) {
			return existing, nil
		}
		return ContextPackReport{}, &ContextPackError{Reason: ReasonContextPackConflict, Path: pack.PackID, Detail: "different pack occupies context directory"}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return ContextPackReport{}, readErr
	}
	stagingName, err := contextStagingName()
	if err != nil {
		return ContextPackReport{}, err
	}
	stagingRel := DeclarationDirName + "/" + ContextPackDirName + "/" + stagingName
	if err := rootHandle.MkdirAll(DeclarationDirName+"/"+ContextPackDirName, 0o700); err != nil {
		return ContextPackReport{}, fmt.Errorf("runtime/workarea: create context directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = rootHandle.RemoveAll(stagingRel)
		}
	}()
	if err := rootHandle.MkdirAll(stagingRel, 0o700); err != nil {
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
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func readContextPackMarker(root *os.Root, finalRel string) (ContextPackReport, error) {
	metadata, err := root.OpenRoot(finalRel)
	if err != nil {
		return ContextPackReport{}, err
	}
	defer func() { _ = metadata.Close() }()
	file, err := metadata.Open(ContextPackMarkerName)
	if err != nil {
		return ContextPackReport{}, err
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, maxContextPackCarrierBytes))
	if err != nil {
		return ContextPackReport{}, err
	}
	var report ContextPackReport
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return ContextPackReport{}, err
	}
	return report, nil
}

func contextStagingName() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("runtime/workarea: staging name: %w", err)
	}
	return ".staging-" + hex.EncodeToString(random[:]), nil
}
