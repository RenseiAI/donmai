package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const trailerDataFile = "commit-coauthor"

// User values live only in the private data file, never in shell source.
const prepareCommitTrailerHook = `#!/bin/sh
set -eu
data="$(dirname "$0")/../commit-coauthor"
if grep -F -x -q -f "$data" -- "$1"; then
    exit 0
else
    status=$?
    [ "$status" -eq 1 ] || exit "$status"
fi
printf '\n' >> "$1"
cat "$data" >> "$1"
`

func installTrailerHook(gitDir, name, email string) error {
	name, email = ValidateCoAuthorPair("", name, email)
	if name == "" {
		return nil
	}
	hooks := filepath.Join(gitDir, "hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		return fmt.Errorf("create commit hooks directory: %w", err)
	}
	data := filepath.Join(gitDir, trailerDataFile)
	if err := writePrivateHookFile(data, "Co-Authored-By: "+name+" <"+email+">\n", 0o600); err != nil {
		return err
	}
	if err := writePrivateHookFile(filepath.Join(hooks, "prepare-commit-msg"), prepareCommitTrailerHook, 0o700); err != nil {
		return errors.Join(err, os.Remove(data))
	}
	return nil
}

// Exclusive creation avoids overwriting a repository's existing hook or data.
func writePrivateHookFile(path, content string, mode os.FileMode) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open commit hook directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(filepath.Base(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create commit hook file: %w", err)
	}
	_, writeErr := f.WriteString(content)
	err = errors.Join(writeErr, f.Chmod(mode), f.Close())
	if err != nil {
		return errors.Join(fmt.Errorf("write commit hook file: %w", err), os.Remove(path))
	}
	return nil
}

func removeTrailerHook(gitDir string) error {
	var errs []error
	for _, path := range []string{filepath.Join(gitDir, "hooks", "prepare-commit-msg"), filepath.Join(gitDir, trailerDataFile)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove commit hook file: %w", err))
		}
	}
	return errors.Join(errs...)
}
