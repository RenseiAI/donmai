package daemon

import (
	"fmt"
	"os"
	"path/filepath"
)

// readLegacyKitFile applies the package reader's file-type, identity and size
// checks to a legacy manifest or sibling signature. The parent is selected by
// source resolution or the operator's installed-kit scan path; the leaf must
// never redirect the read through a link or special file.
func readLegacyKitFile(name string, maxBytes int64) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(name))
	if err != nil {
		return nil, fmt.Errorf("open legacy kit directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	data, _, err := readPackageRegular(root, filepath.Base(name), maxBytes)
	if err != nil {
		return nil, fmt.Errorf("read legacy kit file: %w", err)
	}
	return data, nil
}
