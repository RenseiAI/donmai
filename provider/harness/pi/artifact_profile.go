package pi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

const (
	preExecutionProfileID     = "pi/preexecution-refusal-origin/v1"
	preExecutionSidecarSHA256 = "b7978021217df3d3d5edc915072dc1fb69ad075e99ccdbd25d2c9bbf82cf9a5f"
	preExecutionTreeSHA256    = "a7aa27a451b47bc95d3981f21f6d45630cf29df29aff277a29fd6a15486eb003"
	preExecutionBinarySHA256  = "6a7668b2059b65851e2a7a94b9b3acdb942ee3e4ebef0a7833c0544b090397f2"
)

type artifactProfileFile struct {
	Path, SHA256 string
	Size         int64
	Mode         string
}
type artifactProfile struct {
	ProfileID, TreeSHA256, BinaryPath string
	Files                             []artifactProfileFile
}
type artifactLease struct {
	root, binary                           string
	files                                  []artifactProfileFile
	rootDev, rootIno, binaryDev, binaryIno uint64
}

func artifactIdentity(path string) (uint64, uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("pi artifact identity unavailable")
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// measureArtifactProfile returns nil for legacy Pi without a sidecar. A claimed
// sidecar that is not the compiled descriptor is a construction failure.
func measureArtifactProfile(binary string) (*artifactLease, error) {
	root := filepath.Dir(binary)
	sidecar := filepath.Join(root, "artifact-profile.json")
	if _, err := os.Lstat(sidecar); os.IsNotExist(err) {
		return nil, nil
	}
	if got, err := sha256File(sidecar); err != nil || got != preExecutionSidecarSHA256 {
		return nil, fmt.Errorf("pi artifact profile sidecar mismatch")
	}
	raw, err := os.ReadFile(sidecar)
	if err != nil {
		return nil, err
	}
	var p artifactProfile
	if err = json.Unmarshal(raw, &p); err != nil || p.ProfileID != preExecutionProfileID || p.TreeSHA256 != preExecutionTreeSHA256 || p.BinaryPath != "pi" {
		return nil, fmt.Errorf("pi artifact profile malformed")
	}
	seen := map[string]bool{}
	for _, entry := range p.Files {
		if entry.Path == "" || filepath.Clean(entry.Path) != entry.Path || strings.HasPrefix(entry.Path, "..") || seen[entry.Path] {
			return nil, fmt.Errorf("pi artifact profile file set malformed")
		}
		seen[entry.Path] = true
		path := filepath.Join(root, entry.Path)
		info, e := os.Lstat(path)
		if e != nil || !info.Mode().IsRegular() || info.Size() != entry.Size {
			return nil, fmt.Errorf("pi artifact file mismatch")
		}
		got, e := sha256File(path)
		if e != nil || got != entry.SHA256 {
			return nil, fmt.Errorf("pi artifact file digest mismatch")
		}
	}
	rootDev, rootIno, err := artifactIdentity(root)
	if err != nil {
		return nil, err
	}
	binaryDev, binaryIno, err := artifactIdentity(binary)
	if err != nil {
		return nil, err
	}
	got, err := sha256File(binary)
	if err != nil || got != preExecutionBinarySHA256 {
		return nil, fmt.Errorf("pi artifact binary mismatch")
	}
	sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].Path < p.Files[j].Path })
	return &artifactLease{root: root, binary: binary, files: p.Files, rootDev: rootDev, rootIno: rootIno, binaryDev: binaryDev, binaryIno: binaryIno}, nil
}

func (l *artifactLease) revalidate() error {
	rootDev, rootIno, err := artifactIdentity(l.root)
	if err != nil || rootDev != l.rootDev || rootIno != l.rootIno {
		return fmt.Errorf("pi artifact root changed")
	}
	binaryDev, binaryIno, err := artifactIdentity(l.binary)
	if err != nil || binaryDev != l.binaryDev || binaryIno != l.binaryIno {
		return fmt.Errorf("pi artifact binary changed")
	}
	for _, entry := range l.files {
		path := filepath.Join(l.root, entry.Path)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != entry.Size {
			return fmt.Errorf("pi artifact file changed")
		}
		got, err := sha256File(path)
		if err != nil || got != entry.SHA256 {
			return fmt.Errorf("pi artifact file digest changed")
		}
	}
	return nil
}
