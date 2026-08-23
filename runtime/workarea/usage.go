package workarea

import (
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
)

// PhysicalUsage returns allocated bytes for the complete root. Any walk,
// metadata, or platform-accounting failure is returned; callers must not report
// a partial total as authoritative disk usage.
func PhysicalUsage(root RootPath) (int64, error) {
	rootInfo, err := os.Lstat(root.String())
	if err != nil {
		return 0, fmt.Errorf("runtime/workarea: inspect physical usage root: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("runtime/workarea: physical usage root is not a real directory")
	}
	var total int64
	seen := make(map[FileIdentity]struct{})
	err = filepath.WalkDir(root.String(), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		identity, err := fileIdentity(info)
		if err != nil {
			return err
		}
		if _, duplicate := seen[identity]; duplicate {
			return nil
		}
		seen[identity] = struct{}{}
		allocated, err := allocatedFileBytes(info)
		if err != nil {
			return err
		}
		if allocated > 0 && total > math.MaxInt64-allocated {
			return fmt.Errorf("runtime/workarea: physical usage overflow at %q", path)
		}
		total += allocated
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("runtime/workarea: physical usage for %q: %w", root, err)
	}
	return total, nil
}
