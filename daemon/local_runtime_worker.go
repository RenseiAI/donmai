package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

const localWorkerArtifactLimit int64 = 512 << 20

// localWorkerArtifactDigest samples a regular executable without exposing its
// path/bytes in diagnostics. It is not protection against a same-user replace
// after verification; daemon and worker must be deployed as one artifact.
func localWorkerArtifactDigest(path string) (string, error) {
	resolved, err := exec.LookPath(path)
	if err != nil {
		return "", errors.New("local worker executable is unavailable")
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", errors.New("local worker executable cannot be resolved")
	}
	root, err := os.OpenRoot(filepath.Dir(resolved))
	if err != nil {
		return "", errors.New("local worker executable parent is unavailable")
	}
	defer func() { _ = root.Close() }()
	name := filepath.Base(resolved)
	before, err := root.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0o111 == 0 || before.Size() <= 0 || before.Size() > localWorkerArtifactLimit {
		return "", errors.New("local worker executable is not a bounded regular artifact")
	}
	file, err := root.Open(name)
	if err != nil {
		return "", errors.New("local worker executable cannot be read")
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", errors.New("local worker executable changed while opening")
	}
	digest := sha256.New()
	size, err := io.Copy(digest, io.LimitReader(file, localWorkerArtifactLimit+1))
	if err != nil {
		return "", errors.New("local worker executable could not be hashed")
	}
	after, err := root.Lstat(name)
	if err != nil || size != before.Size() || size > localWorkerArtifactLimit || !os.SameFile(opened, after) || !after.Mode().IsRegular() || after.Mode().Perm() != before.Mode().Perm() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return "", errors.New("local worker executable changed while reading")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func (l *localRuntime) bindWorkerCommand(command []string) error {
	executable, err := os.Executable()
	if err != nil {
		return errors.New("local daemon executable is unavailable")
	}
	digest, err := localWorkerArtifactDigest(executable)
	if err != nil {
		return err
	}
	if len(command) == 0 {
		return errors.New("local worker command is absent")
	}
	l.workerCommand = append([]string(nil), command...)
	l.workerArtifactDigest = digest
	return l.validateCoupledWorker()
}

func (l *localRuntime) validateCoupledWorker() error {
	if len(l.workerCommand) == 0 || l.workerArtifactDigest == "" {
		return errors.New("local worker artifact coupling is not configured")
	}
	digest, err := localWorkerArtifactDigest(l.workerCommand[0])
	if err != nil {
		return err
	}
	if digest != l.workerArtifactDigest {
		return errors.New("local worker must use the same executable artifact as its daemon")
	}
	return nil
}
