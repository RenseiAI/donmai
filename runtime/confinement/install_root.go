package confinement

import (
	"fmt"
	"path/filepath"
	"strings"
)

// InstallRoot returns the directory an installed program reads at run time,
// for a read scope's ReadPaths: for a program inside a node_modules tree,
// the outermost node_modules directory on its resolved path, which holds
// its package and its dependencies whichever layout the package manager
// used (nested, hoisted, a virtual store); otherwise the directory holding
// the resolved binary. Symbolic links are resolved first.
func InstallRoot(binary string) (string, error) {
	resolved, err := filepath.EvalSymlinks(binary)
	if err != nil {
		return "", fmt.Errorf("confinement: install root: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("confinement: install root: %w", err)
	}
	sep := string(filepath.Separator)
	parts := strings.Split(resolved, sep)
	for i, part := range parts[:len(parts)-1] {
		if part == "node_modules" && i > 0 {
			return strings.Join(parts[:i+1], sep), nil
		}
	}
	return filepath.Dir(resolved), nil
}
