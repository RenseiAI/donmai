package repokeeper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// keeperDirName is the state-dir leaf holding every keeper mirror.
const keeperDirName = "repo-keeper"

// mirrorsDirName is the leaf under the keeper root holding one bare mirror
// per (canonical remote, credential scope).
const mirrorsDirName = "mirrors"

// identityFileName is the secret-free sidecar each mirror records its
// remote and scope in. It lives beside — never inside — the bare mirror so
// a foreign or hand-rolled mirror directory without one fails closed.
const identityFileName = "keeper.json"

// mirrorIdentityVersion is the only identity sidecar version this store
// reads or writes.
const mirrorIdentityVersion = 1

// maxIdentityBytes bounds the identity sidecar read.
const maxIdentityBytes = 1 << 20

// mirrorIdentity is the secret-free record each mirror carries beside its
// git directory: the canonical remote it mirrors and the credential scope
// that remote was fetched under. The digest repeats the directory name so
// a moved or misaddressed directory fails closed on re-verification.
type mirrorIdentity struct {
	Version         int    `json:"version"`
	RemoteDigest    string `json:"remoteDigest"`
	CanonicalRemote string `json:"canonicalRemote"`
	Scope           string `json:"scope"`
}

// writeMirrorIdentity persists the secret-free sidecar for a freshly
// created mirror directory. The file is created exclusively, written, and
// fsynced before the mirror is trusted; a pre-existing file aborts the
// write so two creators can never silently share one directory.
func writeMirrorIdentity(mirrorDir string, identity mirrorIdentity) error {
	body, err := json.Marshal(identity)
	if err != nil {
		return fmt.Errorf("repo keeper: encode mirror identity: %w", err)
	}
	// The sidecar name is a package constant joined to the mirror directory
	// this call created; neither component is external input.
	path := filepath.Join(mirrorDir, identityFileName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: constant sidecar name under the store's own mirror dir.
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("repo keeper: mirror identity already exists")
		}
		return fmt.Errorf("repo keeper: create mirror identity: %w", err)
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(body); err != nil {
		return fmt.Errorf("repo keeper: write mirror identity: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("repo keeper: fsync mirror identity: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("repo keeper: close mirror identity: %w", err)
	}
	if err := fsyncDir(mirrorDir); err != nil {
		return err
	}
	committed = true
	return nil
}

// readMirrorIdentity loads and validates the secret-free sidecar beside a
// mirror directory: the file must be a regular owner-only non-symlink
// within its size bound, carry exactly one JSON document at the known
// version, and be internally consistent — the recorded canonical remote
// must hash to the recorded remote digest. The directory binding (recorded
// remote plus recorded scope hash to the directory name) is checked
// separately by dirDigest so verifyMirror can report a remote or scope
// mismatch against the request before reporting a moved directory.
func readMirrorIdentity(mirrorDir string) (mirrorIdentity, error) {
	// The sidecar name is a package constant joined to a verified mirror
	// directory; neither component is external input.
	path := filepath.Join(mirrorDir, identityFileName)
	info, err := os.Lstat(path) //nolint:gosec // G304: constant sidecar name under the store's own mirror dir.
	if err != nil {
		return mirrorIdentity{}, fmt.Errorf("repo keeper: inspect mirror identity: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return mirrorIdentity{}, fmt.Errorf("%w: mirror identity is not a regular file", ErrOriginMismatch)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return mirrorIdentity{}, fmt.Errorf("%w: mirror identity mode is not owner-only", ErrOriginMismatch)
	}
	file, err := os.Open(path) //nolint:gosec // G304: constant sidecar name under the store's own mirror dir.
	if err != nil {
		return mirrorIdentity{}, fmt.Errorf("repo keeper: open mirror identity: %w", err)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return mirrorIdentity{}, fmt.Errorf("repo keeper: stat mirror identity: %w", err)
	}
	if !os.SameFile(info, opened) {
		return mirrorIdentity{}, fmt.Errorf("%w: mirror identity changed while opening", ErrOriginMismatch)
	}
	body, err := io.ReadAll(io.LimitReader(file, maxIdentityBytes+1))
	if err != nil {
		return mirrorIdentity{}, fmt.Errorf("repo keeper: read mirror identity: %w", err)
	}
	if len(body) > maxIdentityBytes {
		return mirrorIdentity{}, fmt.Errorf("%w: mirror identity exceeds size bound", ErrOriginMismatch)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var identity mirrorIdentity
	if err := decoder.Decode(&identity); err != nil {
		return mirrorIdentity{}, fmt.Errorf("%w: mirror identity is not valid JSON", ErrOriginMismatch)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return mirrorIdentity{}, fmt.Errorf("%w: mirror identity carries trailing data", ErrOriginMismatch)
		}
		return mirrorIdentity{}, fmt.Errorf("%w: mirror identity carries trailing data", ErrOriginMismatch)
	}
	if identity.Version != mirrorIdentityVersion {
		return mirrorIdentity{}, fmt.Errorf("%w: unsupported mirror identity version", ErrOriginMismatch)
	}
	if identity.CanonicalRemote == "" || !validDigest(identity.RemoteDigest) {
		return mirrorIdentity{}, fmt.Errorf("%w: mirror identity is incomplete", ErrOriginMismatch)
	}
	if _, recomputed, err := CanonicalRemote(identity.CanonicalRemote); err != nil || recomputed != identity.RemoteDigest {
		return mirrorIdentity{}, fmt.Errorf("%w: mirror identity remote does not hash to its digest", ErrOriginMismatch)
	}
	return identity, nil
}

// dirDigest recomputes the mirror directory name the identity belongs in:
// the recorded remote digest joined with the recorded scope. A sidecar
// that wandered into another directory fails closed at the caller.
func (identity mirrorIdentity) dirDigest() (string, error) {
	digest, err := ScopeDigest(identity.RemoteDigest, identity.Scope)
	if err != nil {
		return "", err
	}
	return digest, nil
}

// fsyncDir flushes a directory's entries so a freshly published file
// survives a crash before the caller trusts it.
func fsyncDir(dir string) error {
	handle, err := os.Open(dir) //nolint:gosec // G304: the store's own keeper/mirror dirs.
	if err != nil {
		return fmt.Errorf("repo keeper: open dir for fsync: %w", err)
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		return fmt.Errorf("repo keeper: fsync dir: %w", err)
	}
	if err := handle.Close(); err != nil {
		return fmt.Errorf("repo keeper: close dir for fsync: %w", err)
	}
	return nil
}
